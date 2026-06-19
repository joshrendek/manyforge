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
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
)

// logSafeURL returns scheme://host/path with the query stripped, for safe diagnostic logging.
// Our connector URLs carry credentials in headers (Basic auth), not the query, but stripping
// the query keeps any future credential-bearing param out of the logs (CLAUDE.md logging rule).
func logSafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.Path
}

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
	projectKey    string // optional: scope reconcile/search to this Jira project (config.project_key)
}

// projectKeyRe bounds a Jira project key to injection-safe characters before it is
// interpolated into a JQL `project = "<key>"` clause.
var projectKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

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
	q.Set("fields", "summary,status,priority,reporter,updated,created,description")
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
		// description is ADF (same shape as comment bodies); extract plain text. A null/absent
		// description yields "" (extractADFText's fallback handles the `null` literal).
		Description: extractADFText(issueResp.Fields.Description),
		UpdatedAt:   issueResp.Fields.Updated.Time,
		CreatedAt:   issueResp.Fields.Created.Time,
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

// PostComment appends a plain-text comment to the external issue. When internal is true the
// comment is posted as a Jira Service Management INTERNAL comment (agent-only, not visible to
// the requester) by attaching the sd.public.comment property with internal=true.
func (c *client) PostComment(ctx context.Context, externalID, body string, internal bool) (connectors.ExternalComment, error) {
	if err := validateIssueKey(externalID); err != nil {
		return connectors.ExternalComment{}, err
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return connectors.ExternalComment{}, fmt.Errorf("jira: invalid base_url: %w", ErrUpstream)
	}

	commURL := base.JoinPath("rest", "api", "3", "issue", externalID, "comment")

	adfBody := buildADFDoc(body)
	commentPayload := map[string]any{"body": adfBody}
	if internal {
		// JSM internal comment: the sd.public.comment property with internal=true marks the
		// comment visible to agents only. On a non-Service-Management project Jira ignores it.
		commentPayload["properties"] = []map[string]any{
			{"key": "sd.public.comment", "value": map[string]any{"internal": true}},
		}
	}
	payload, err := json.Marshal(commentPayload)
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

// CreateIssue creates a new Jira issue in the draft's project, returning its key + URL.
func (c *client) CreateIssue(ctx context.Context, draft connectors.ExternalIssueDraft) (connectors.ExternalIssue, error) {
	if draft.ProjectKey == "" || draft.IssueType == "" {
		return connectors.ExternalIssue{}, fmt.Errorf("jira: project key and issue type required: %w", ErrUpstream)
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return connectors.ExternalIssue{}, fmt.Errorf("jira: invalid base_url: %w", ErrUpstream)
	}
	issueURL := base.JoinPath("rest", "api", "3", "issue")

	fields := map[string]any{
		"project":   map[string]any{"key": draft.ProjectKey},
		"issuetype": map[string]any{"name": draft.IssueType},
		"summary":   draft.Summary,
	}
	if draft.Description != "" {
		fields["description"] = buildADFDoc(draft.Description)
	}
	payload, err := json.Marshal(map[string]any{"fields": fields})
	if err != nil {
		return connectors.ExternalIssue{}, fmt.Errorf("jira: marshal create: %w", ErrUpstream)
	}

	var resp jiraCreateIssueResponse
	if err := c.doJSON(ctx, http.MethodPost, issueURL.String(), payload, &resp); err != nil {
		return connectors.ExternalIssue{}, err
	}
	return connectors.ExternalIssue{
		ExternalID: resp.Key,
		URL:        base.JoinPath("browse", resp.Key).String(),
		Title:      draft.Summary,
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
		// Jira lists only transitions valid FROM the issue's current status, so an issue that is
		// ALREADY in the target status has no transition reaching it. Treat that as an idempotent
		// no-op rather than a hard failure — otherwise an agent re-issuing "→Done" on an
		// already-Done issue creates a permanently-failed outbound op that pins the connector to
		// "degraded" (manyforge-zal / manyforge-xfj). A genuine name mismatch or missing workflow
		// path (current status != target) is a real misconfiguration and stays a loud error.
		current, cerr := c.currentStatus(ctx, externalID)
		if cerr != nil {
			return cerr
		}
		if strings.EqualFold(current, status) {
			slog.Default().DebugContext(ctx, "jira: issue already in target status; skipping transition",
				"issue", externalID, "status", status)
			return nil
		}
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

// currentStatus fetches just the issue's current status name via a lightweight ?fields=status
// GET. Used by TransitionStatus to distinguish an already-in-target no-op from a genuine
// missing-transition error. Callers validate externalID before reaching here.
func (c *client) currentStatus(ctx context.Context, externalID string) (string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("jira: invalid base_url: %w", ErrUpstream)
	}
	issueURL := base.JoinPath("rest", "api", "3", "issue", externalID)
	q := issueURL.Query()
	q.Set("fields", "status")
	issueURL.RawQuery = q.Encode()

	var resp jiraIssueResponse
	if err := c.doJSON(ctx, http.MethodGet, issueURL.String(), nil, &resp); err != nil {
		return "", err
	}
	return resp.Fields.Status.Name, nil
}

// searchPageSize is the per-request page size for the paginated search. Jira's
// default (50) silently truncates; we page explicitly so reconcile never misses
// an updated issue.
const searchPageSize = 100

// maxSearchPages bounds ListUpdatedSince so a pathological/malicious upstream that returns
// non-empty pages while perpetually issuing a nextPageToken cannot loop forever (defense-in-
// depth; reconcile is idempotent, so a truncated sweep is safe — the next run resumes).
// ~100k issues at searchPageSize=100, far above any real reconcile window. A var so tests
// can lower it.
var maxSearchPages = 1000

// reconcileOverlapMinutes pads the incremental sweep window so an issue updated right on a
// poll boundary isn't dropped; the idempotent upsert dedupes the re-fetched overlap.
const reconcileOverlapMinutes = 2

// ListUpdatedSince returns the keys of issues updated at or after since (reconcile),
// paging through ALL results via the new JQL search endpoint
// (GET /rest/api/3/search/jql). The legacy /rest/api/3/search was removed by
// Atlassian (returns 410 Gone), so this uses cursor pagination (nextPageToken) —
// there is no startAt/total; the loop stops when nextPageToken is absent.
func (c *client) ListUpdatedSince(ctx context.Context, since time.Time) ([]string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("jira: invalid base_url: %w", ErrUpstream)
	}
	// Build the lower time bound. Jira evaluates a bare datetime literal in the
	// AUTHENTICATED ACCOUNT's timezone, NOT UTC — so a UTC-formatted absolute `since`
	// silently shifts the window by the account's UTC offset. For accounts behind UTC
	// (e.g. the Americas) that pushes the bound into the future, so the incremental query
	// returns NOTHING and freshly-updated issues never sync until a full pull (the cause of
	// manyforge-7kf). Use Jira's timezone-independent relative-time syntax instead:
	// "-Nm" = updated within the last N minutes, a duration evaluated against Jira's own
	// clock (also immune to host/Jira clock skew). N spans since→now plus an overlap buffer.
	// A zero `since` (the "Sync now" full pull) keeps an ancient absolute bound so it still
	// fetches everything. The bound is an integer or a fixed literal — injection-safe.
	updatedBound := `updated >= "1970-01-01"`
	if !since.IsZero() {
		mins := int(time.Since(since).Minutes()) + reconcileOverlapMinutes
		if mins < reconcileOverlapMinutes {
			mins = reconcileOverlapMinutes
		}
		updatedBound = fmt.Sprintf(`updated >= "-%dm"`, mins)
	}
	// Scope to the connector's project when configured (config.project_key) so inbound
	// reconcile only pulls that project, not every project the token can see. The key is
	// validated against a strict pattern to stay injection-safe inside the JQL.
	jql := updatedBound + ` ORDER BY updated ASC`
	if pk := c.projectKey; pk != "" && projectKeyRe.MatchString(pk) {
		jql = fmt.Sprintf(`project = "%s" AND %s ORDER BY updated ASC`, pk, updatedBound)
	}

	var keys []string
	nextPageToken := ""
	pageCount := 0
	for {
		searchURL := base.JoinPath("rest", "api", "3", "search", "jql")
		q := url.Values{}
		q.Set("jql", jql)
		q.Set("fields", "updated")
		q.Set("maxResults", fmt.Sprintf("%d", searchPageSize))
		if nextPageToken != "" {
			q.Set("nextPageToken", nextPageToken)
		}
		searchURL.RawQuery = q.Encode()

		var page jiraSearchResponse
		if err := c.doJSON(ctx, http.MethodGet, searchURL.String(), nil, &page); err != nil {
			return nil, err
		}

		for _, issue := range page.Issues {
			keys = append(keys, issue.Key)
		}

		// The new API signals "no more pages" by omitting nextPageToken. Stop on an
		// empty token, or an empty page (defensive — a page with a token but no issues).
		if page.NextPageToken == "" || len(page.Issues) == 0 {
			break
		}
		pageCount++
		if pageCount >= maxSearchPages {
			slog.Default().WarnContext(ctx, "jira: ListUpdatedSince hit page cap; truncating sweep (next reconcile resumes)",
				"max_pages", maxSearchPages, "keys_so_far", len(keys))
			break
		}
		nextPageToken = page.NextPageToken
	}
	return keys, nil
}

// VerifyAuth confirms the stored credential authenticates against this Jira site via a
// cheap, side-effect-free GET /rest/api/3/myself. Returns nil on 2xx; a non-2xx (e.g. a
// 401/403 from a bad email/token) returns ErrUpstream — the body is never surfaced. Backs
// the connector Test action and create/rotate-time credential verification.
func (c *client) VerifyAuth(ctx context.Context) error {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("jira: invalid base_url: %w", ErrUpstream)
	}
	return c.doJSON(ctx, http.MethodGet, base.JoinPath("rest", "api", "3", "myself").String(), nil, nil)
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
		slog.Default().WarnContext(ctx, "jira: build request error",
			"method", method, "url", logSafeURL(rawURL), "err", err)
		return fmt.Errorf("jira: build request: %w", ErrUnreachable)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.email, c.apiToken)

	res, err := c.httpClient.Do(req)
	if err != nil {
		// Network failure, timeout, or SSRF dial-refusal (netsafe) all land here. The caller only
		// ever sees the opaque ErrUnreachable (Principle II), so log the REAL cause server-side —
		// otherwise a firewall block is indistinguishable from a genuine outage (manyforge-zci).
		slog.Default().WarnContext(ctx, "jira: transport error",
			"method", method, "url", logSafeURL(rawURL), "err", err)
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
		// Description is the issue's main body in ADF (same encoding as a comment body),
		// or a plain string on older API versions, or null when absent. extractADFText
		// handles all three; a null/absent description yields "" (its fallback handles the
		// `null` literal).
		Description json.RawMessage `json:"description"`
		Updated     jiraTime        `json:"updated"`
		Created     jiraTime        `json:"created"`
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

type jiraCreateIssueResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type jiraCommentsResponse struct {
	Comments []jiraComment `json:"comments"`
}

type jiraSearchResponse struct {
	// NextPageToken is the cursor for the next page (search/jql endpoint). It is
	// absent/empty on the last page — that is the only stop signal (the new API
	// returns no startAt/total).
	NextPageToken string `json:"nextPageToken"`
	Issues        []struct {
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
