// Package jira implements connectors.TicketingConnector for Jira Cloud REST API v3.
// The client is SSRF-safe (backed by netsafe) and uses HTTP Basic auth (email:api_token).
// Errors from non-2xx responses NEVER contain the upstream response body (Principle II).
package jira

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
)

// sentinel errors — never wrap upstream bodies.
var (
	ErrUpstream    = errors.New("jira: upstream error")
	ErrUnreachable = errors.New("jira: service unreachable")
	ErrBadSig      = errors.New("jira: invalid webhook signature")
	ErrBadIssueKey = errors.New("jira: invalid issue key")
)

const bodyLimit = 8 << 20 // 8 MiB

// issueKeyRe matches a Jira issue key (e.g. "PROJ-42"): an uppercase project key
// followed by a numeric id. Validated before any URL build so a crafted key from
// an inbound webhook (DecodeWebhook lifts issue.key) cannot smuggle path-traversal
// segments through url.JoinPath (which collapses "..").
var issueKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]+-[0-9]+$`)

// validateIssueKey rejects any externalID that is not a well-formed Jira issue key.
func validateIssueKey(key string) error {
	if !issueKeyRe.MatchString(key) {
		return fmt.Errorf("jira: issue key %q: %w", key, ErrBadIssueKey)
	}
	return nil
}

// client is a live Jira Cloud REST client bound to one business's credential.
type client struct {
	httpClient    *http.Client
	baseURL       string // e.g. "https://mycompany.atlassian.net"
	email         string
	apiToken      string
	webhookSecret string
}

// compile-time check
var _ connectors.TicketingConnector = (*client)(nil)

// FetchIssue returns the external issue plus its comments.
func (c *client) FetchIssue(ctx context.Context, externalID string) (connectors.ExternalIssue, error) {
	if err := validateIssueKey(externalID); err != nil {
		return connectors.ExternalIssue{}, err
	}
	// Fetch the issue fields.
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return connectors.ExternalIssue{}, fmt.Errorf("jira: invalid base_url: %w", ErrUpstream)
	}

	issueURL := base.JoinPath("rest", "api", "3", "issue", externalID)
	q := issueURL.Query()
	q.Set("fields", "summary,status,priority,reporter,updated")
	issueURL.RawQuery = q.Encode()

	var issueResp jiraIssueResponse
	if err := c.doJSON(ctx, http.MethodGet, issueURL.String(), nil, &issueResp); err != nil {
		return connectors.ExternalIssue{}, err
	}

	// Fetch comments.
	commURL := base.JoinPath("rest", "api", "3", "issue", externalID, "comment")
	var commResp jiraCommentsResponse
	if err := c.doJSON(ctx, http.MethodGet, commURL.String(), nil, &commResp); err != nil {
		return connectors.ExternalIssue{}, err
	}

	issue := connectors.ExternalIssue{
		ExternalID:    issueResp.Key,
		URL:           base.JoinPath("browse", externalID).String(),
		Title:         issueResp.Fields.Summary,
		Status:        issueResp.Fields.Status.Name,
		Priority:      issueResp.Fields.Priority.Name,
		ReporterEmail: issueResp.Fields.Reporter.EmailAddress,
		ReporterName:  issueResp.Fields.Reporter.DisplayName,
		UpdatedAt:     issueResp.Fields.Updated.Time,
	}

	for _, c := range commResp.Comments {
		issue.Comments = append(issue.Comments, connectors.ExternalComment{
			ExternalID: c.ID,
			Author:     c.Author.DisplayName,
			Body:       extractADFText(c.Body),
			CreatedAt:  c.Created.Time,
		})
	}

	return issue, nil
}

// PostComment appends a plain-text comment to the external issue.
func (c *client) PostComment(ctx context.Context, externalID, body string) (connectors.ExternalComment, error) {
	if err := validateIssueKey(externalID); err != nil {
		return connectors.ExternalComment{}, err
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return connectors.ExternalComment{}, fmt.Errorf("jira: invalid base_url: %w", ErrUpstream)
	}

	commURL := base.JoinPath("rest", "api", "3", "issue", externalID, "comment")

	adfBody := buildADFDoc(body)
	payload, err := json.Marshal(map[string]any{"body": adfBody})
	if err != nil {
		return connectors.ExternalComment{}, fmt.Errorf("jira: marshal comment: %w", ErrUpstream)
	}

	var resp jiraComment
	if err := c.doJSON(ctx, http.MethodPost, commURL.String(), payload, &resp); err != nil {
		return connectors.ExternalComment{}, err
	}

	return connectors.ExternalComment{
		ExternalID: resp.ID,
		Author:     resp.Author.DisplayName,
		Body:       body,
		CreatedAt:  resp.Created.Time,
	}, nil
}

// TransitionStatus moves the external issue to the target status name.
func (c *client) TransitionStatus(ctx context.Context, externalID, status string) error {
	if err := validateIssueKey(externalID); err != nil {
		return err
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("jira: invalid base_url: %w", ErrUpstream)
	}

	transURL := base.JoinPath("rest", "api", "3", "issue", externalID, "transitions")

	var transResp jiraTransitionsResponse
	if err := c.doJSON(ctx, http.MethodGet, transURL.String(), nil, &transResp); err != nil {
		return err
	}

	var transitionID string
	for _, t := range transResp.Transitions {
		if strings.EqualFold(t.To.Name, status) || strings.EqualFold(t.Name, status) {
			transitionID = t.ID
			break
		}
	}
	if transitionID == "" {
		return fmt.Errorf("jira: no transition found for status %q: %w", status, ErrUpstream)
	}

	payload, err := json.Marshal(map[string]any{
		"transition": map[string]string{"id": transitionID},
	})
	if err != nil {
		return fmt.Errorf("jira: marshal transition: %w", ErrUpstream)
	}

	return c.doJSON(ctx, http.MethodPost, transURL.String(), payload, nil)
}

// searchPageSize is the per-request page size for the paginated search. Jira's
// default (50) silently truncates; we page explicitly so reconcile never misses
// an updated issue.
const searchPageSize = 100

// ListUpdatedSince returns the keys of issues updated at or after since (reconcile),
// paging through ALL results (Jira's default page size of 50 would otherwise
// silently truncate the reconcile set).
func (c *client) ListUpdatedSince(ctx context.Context, since time.Time) ([]string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("jira: invalid base_url: %w", ErrUpstream)
	}
	// Format as Jira expects: "2006-01-02 15:04"
	ts := since.UTC().Format("2006-01-02 15:04")
	jql := fmt.Sprintf(`updated >= "%s" ORDER BY updated ASC`, ts)

	var keys []string
	startAt := 0
	for {
		searchURL := base.JoinPath("rest", "api", "3", "search")
		q := url.Values{}
		q.Set("jql", jql)
		q.Set("fields", "updated")
		q.Set("startAt", fmt.Sprintf("%d", startAt))
		q.Set("maxResults", fmt.Sprintf("%d", searchPageSize))
		searchURL.RawQuery = q.Encode()

		var page jiraSearchResponse
		if err := c.doJSON(ctx, http.MethodGet, searchURL.String(), nil, &page); err != nil {
			return nil, err
		}

		for _, issue := range page.Issues {
			keys = append(keys, issue.Key)
		}

		// Stop on an empty page (defensive) or once we've covered the total.
		if len(page.Issues) == 0 {
			break
		}
		startAt += len(page.Issues)
		if startAt >= page.Total {
			break
		}
	}
	return keys, nil
}

// VerifyWebhook checks the X-Hub-Signature header (sha256=<hex>) against the
// HMAC-SHA256 of the body using the per-connector webhook secret.
// Returns ErrBadSig if the signature is missing, malformed, or does not match.
func (c *client) VerifyWebhook(headers http.Header, body []byte) error {
	// Fail closed: an unconfigured secret means an empty HMAC key, which makes
	// EVERY signature forgeable. Reject before computing the MAC.
	if c.webhookSecret == "" {
		return fmt.Errorf("jira: webhook secret not configured: %w", ErrBadSig)
	}
	sig := headers.Get("X-Hub-Signature")
	if sig == "" {
		return fmt.Errorf("jira: missing X-Hub-Signature header: %w", ErrBadSig)
	}
	after, ok := strings.CutPrefix(sig, "sha256=")
	if !ok {
		return fmt.Errorf("jira: malformed X-Hub-Signature (no sha256= prefix): %w", ErrBadSig)
	}
	got, err := hex.DecodeString(after)
	if err != nil {
		return fmt.Errorf("jira: malformed X-Hub-Signature hex: %w", ErrBadSig)
	}
	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	mac.Write(body)
	want := mac.Sum(nil)
	if !hmac.Equal(got, want) {
		return fmt.Errorf("jira: signature mismatch: %w", ErrBadSig)
	}
	return nil
}

// DecodeWebhook parses a Jira webhook JSON body into a WebhookEvent.
// DeliveryID is derived from timestamp+issue.key when not explicitly present.
func (c *client) DecodeWebhook(body []byte) (connectors.WebhookEvent, error) {
	var payload struct {
		Timestamp    int64  `json:"timestamp"`
		WebhookEvent string `json:"webhookEvent"`
		Issue        struct {
			Key string `json:"key"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return connectors.WebhookEvent{}, fmt.Errorf("jira: decode webhook: %w", ErrUpstream)
	}

	externalID := payload.Issue.Key
	// Reject a malformed issue key so a crafted webhook can never push a
	// path-traversal key into the sync pipeline (and downstream URL builds).
	if err := validateIssueKey(externalID); err != nil {
		return connectors.WebhookEvent{}, err
	}
	// Derive a stable delivery ID from the timestamp + issue key (Jira does not
	// always provide a unique delivery header).
	deliveryID := fmt.Sprintf("%s:%d", externalID, payload.Timestamp)
	kind := mapWebhookEventKind(payload.WebhookEvent)

	return connectors.WebhookEvent{
		DeliveryID: deliveryID,
		ExternalID: externalID,
		Kind:       kind,
	}, nil
}

// doJSON executes an HTTP request and JSON-decodes the response body into out
// (out may be nil for requests that return no body, e.g. POST /transitions).
// Non-2xx responses return a sentinel error that NEVER contains the upstream body.
func (c *client) doJSON(ctx context.Context, method, rawURL string, body []byte, out any) error {
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, reqBody)
	if err != nil {
		return fmt.Errorf("jira: build request: %w", ErrUnreachable)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.email, c.apiToken)

	res, err := c.httpClient.Do(req)
	if err != nil {
		// Network failure, timeout, or SSRF dial-refusal (netsafe) all land here.
		return fmt.Errorf("jira: transport: %w", ErrUnreachable)
	}
	defer func() { _ = res.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, bodyLimit))

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return jiraHTTPError(res.StatusCode)
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("jira: decode response: %w", ErrUpstream)
	}
	return nil
}

// jiraHTTPError maps a non-2xx status onto a sentinel.
// The raw upstream body is NEVER surfaced (Principle II).
func jiraHTTPError(status int) error {
	if status >= 500 || status == http.StatusTooManyRequests {
		return fmt.Errorf("jira: upstream status %d: %w", status, ErrUnreachable)
	}
	return fmt.Errorf("jira: upstream status %d: %w", status, ErrUpstream)
}

// mapWebhookEventKind maps a Jira webhookEvent value to a canonical kind string.
func mapWebhookEventKind(event string) string {
	switch event {
	case "jira:issue_created":
		return "issue.created"
	case "jira:issue_updated":
		return "issue.updated"
	case "jira:issue_deleted":
		return "issue.deleted"
	case "comment_created":
		return "comment.created"
	case "comment_updated":
		return "comment.updated"
	case "comment_deleted":
		return "comment.deleted"
	default:
		return event
	}
}

// ── Jira REST v3 response shapes ────────────────────────────────────────────

// jiraTime wraps a Jira timestamp string for JSON unmarshaling.
type jiraTime struct {
	Time time.Time
}

func (t *jiraTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	// Jira uses RFC 3339 with milliseconds: "2026-06-01T10:30:00.000+0000"
	parsed, err := time.Parse("2006-01-02T15:04:05.999Z0700", s)
	if err != nil {
		// Fallback to RFC 3339
		parsed, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("jira: parse time %q: %w", s, err)
		}
	}
	t.Time = parsed
	return nil
}

type jiraIssueResponse struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
		Status  struct {
			Name string `json:"name"`
		} `json:"status"`
		Priority struct {
			Name string `json:"name"`
		} `json:"priority"`
		Reporter struct {
			EmailAddress string `json:"emailAddress"`
			DisplayName  string `json:"displayName"`
		} `json:"reporter"`
		Updated jiraTime `json:"updated"`
	} `json:"fields"`
}

type jiraComment struct {
	ID     string `json:"id"`
	Author struct {
		DisplayName  string `json:"displayName"`
		EmailAddress string `json:"emailAddress"`
	} `json:"author"`
	Body    json.RawMessage `json:"body"`
	Created jiraTime        `json:"created"`
}

type jiraCommentsResponse struct {
	Comments []jiraComment `json:"comments"`
}

type jiraSearchResponse struct {
	StartAt    int `json:"startAt"`
	MaxResults int `json:"maxResults"`
	Total      int `json:"total"`
	Issues     []struct {
		Key    string `json:"key"`
		Fields struct {
			Updated jiraTime `json:"updated"`
		} `json:"fields"`
	} `json:"issues"`
}

type jiraTransitionsResponse struct {
	Transitions []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		To   struct {
			Name string `json:"name"`
		} `json:"to"`
	} `json:"transitions"`
}

// ── ADF (Atlassian Document Format) helpers ──────────────────────────────────

// adfNode is a recursive ADF node for text extraction.
type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text,omitempty"`
	Content []adfNode `json:"content,omitempty"`
}

// extractADFText extracts plain text from a Jira ADF body (which may also be a
// plain string in older API versions). Best-effort: walks the content tree for
// text nodes. Falls back to the raw string if the body is not valid ADF JSON.
func extractADFText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try ADF (object with content array).
	var node adfNode
	if err := json.Unmarshal(raw, &node); err == nil && node.Type != "" {
		return strings.TrimSpace(walkADF(node))
	}
	// Fallback: it might be a plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func walkADF(node adfNode) string {
	if node.Type == "text" {
		return node.Text
	}
	var sb strings.Builder
	for i, child := range node.Content {
		sb.WriteString(walkADF(child))
		// Insert a space between paragraph-level nodes.
		if i < len(node.Content)-1 && node.Type == "doc" {
			sb.WriteString(" ")
		}
	}
	return sb.String()
}

// buildADFDoc wraps plain text in a minimal ADF document for outbound comments.
func buildADFDoc(text string) map[string]any {
	return map[string]any{
		"version": 1,
		"type":    "doc",
		"content": []map[string]any{
			{
				"type": "paragraph",
				"content": []map[string]any{
					{"type": "text", "text": text},
				},
			},
		},
	}
}
