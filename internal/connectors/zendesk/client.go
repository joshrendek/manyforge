// Package zendesk implements connectors.TicketingConnector for the Zendesk REST API v2.
// The client is SSRF-safe (backed by netsafe) and uses HTTP Basic API-token auth, whose
// Zendesk form is username "<email>/token", password "<api_token>". Errors from non-2xx
// responses NEVER contain the upstream response body (Spec 004 Principle II / §7).
package zendesk

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
)

// logSafeURL returns scheme://host/path with the query stripped, for safe diagnostic logging.
// Zendesk auth rides the Basic-auth header, not the URL, but stripping the query keeps any future
// credential-bearing param out of the logs (CLAUDE.md logging rule).
func logSafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// sentinel errors — never wrap upstream bodies.
var (
	ErrUpstream    = errors.New("zendesk: upstream error")
	ErrUnreachable = errors.New("zendesk: service unreachable")
	ErrBadSig      = errors.New("zendesk: invalid webhook signature")
	ErrBadTicketID = errors.New("zendesk: invalid ticket id")
)

// compile-time check
var _ connectors.TicketingConnector = (*client)(nil)

const bodyLimit = 8 << 20 // 8 MiB

// searchPageSize is the per-request page size for the paginated Zendesk search.
const searchPageSize = 100

// zendeskStatuses is the closed set of valid Zendesk ticket statuses (clamps the target
// before any write, mirroring Jira's transition-name lookup).
var zendeskStatuses = map[string]bool{
	"new": true, "open": true, "pending": true, "hold": true, "solved": true, "closed": true,
}

// ticketIDRe matches a Zendesk ticket id (one or more ASCII digits). Validated before any URL
// build so a crafted id from an inbound webhook (DecodeWebhook lifts detail.id) cannot
// smuggle path-traversal segments through url.JoinPath.
var ticketIDRe = regexp.MustCompile(`^[0-9]+$`)

func validateTicketID(id string) error {
	if !ticketIDRe.MatchString(id) {
		return fmt.Errorf("zendesk: ticket id %q: %w", id, ErrBadTicketID)
	}
	return nil
}

// client is a live Zendesk REST client bound to one business's credential.
type client struct {
	httpClient    *http.Client
	baseURL       string // e.g. "https://acme.zendesk.com"
	email         string
	apiToken      string
	webhookSecret string
}

// doJSON executes an HTTP request and JSON-decodes the response into out (out may be nil
// for requests whose body we ignore, e.g. PUT status). Non-2xx responses return a sentinel
// error that NEVER contains the upstream body.
func (c *client) doJSON(ctx context.Context, method, rawURL string, body []byte, out any) error {
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reqBody)
	if err != nil {
		slog.Default().WarnContext(ctx, "zendesk: build request error",
			"method", method, "url", logSafeURL(rawURL), "err", err)
		return fmt.Errorf("zendesk: build request: %w", ErrUnreachable)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// Zendesk API-token auth: HTTP Basic, username "<email>/token", password = api_token.
	req.SetBasicAuth(c.email+"/token", c.apiToken)

	res, err := c.httpClient.Do(req)
	if err != nil {
		// Network failure, timeout, or SSRF dial-refusal (netsafe) all land here. The caller only
		// ever sees the opaque ErrUnreachable (Principle II), so log the REAL cause server-side —
		// otherwise a firewall block is indistinguishable from a genuine outage (manyforge-zci).
		slog.Default().WarnContext(ctx, "zendesk: transport error",
			"method", method, "url", logSafeURL(rawURL), "err", err)
		return fmt.Errorf("zendesk: transport: %w", ErrUnreachable)
	}
	defer func() { _ = res.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, bodyLimit))

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return zendeskHTTPError(res.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("zendesk: decode response: %w", ErrUpstream)
	}
	return nil
}

// zendeskHTTPError maps a non-2xx status onto a sentinel (status code only, never body).
func zendeskHTTPError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("zendesk: auth rejected (%d): %w", status, ErrUpstream)
	case http.StatusNotFound:
		return fmt.Errorf("zendesk: not found (%d): %w", status, ErrUpstream)
	case http.StatusTooManyRequests:
		return fmt.Errorf("zendesk: rate limited (%d): %w", status, ErrUpstream)
	default:
		return fmt.Errorf("zendesk: upstream status %d: %w", status, ErrUpstream)
	}
}

// FetchIssue returns the external ticket plus its comments. The requester is sideloaded
// (?include=users) so the reporter email is resolved in a single call.
func (c *client) FetchIssue(ctx context.Context, externalID string) (connectors.ExternalIssue, error) {
	if err := validateTicketID(externalID); err != nil {
		return connectors.ExternalIssue{}, err
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return connectors.ExternalIssue{}, fmt.Errorf("zendesk: invalid base_url: %w", ErrUpstream)
	}

	ticketURL := base.JoinPath("api", "v2", "tickets", externalID+".json")
	q := ticketURL.Query()
	q.Set("include", "users")
	ticketURL.RawQuery = q.Encode()

	var tr zendeskTicketResponse
	if err := c.doJSON(ctx, http.MethodGet, ticketURL.String(), nil, &tr); err != nil {
		return connectors.ExternalIssue{}, err
	}

	commURL := base.JoinPath("api", "v2", "tickets", externalID, "comments.json")
	var cr zendeskCommentsResponse
	if err := c.doJSON(ctx, http.MethodGet, commURL.String(), nil, &cr); err != nil {
		return connectors.ExternalIssue{}, err
	}

	reporterEmail, reporterName := "", ""
	for _, u := range tr.Users {
		if u.ID == tr.Ticket.RequesterID {
			reporterEmail, reporterName = u.Email, u.Name
			break
		}
	}

	issue := connectors.ExternalIssue{
		ExternalID:    strconv.FormatInt(tr.Ticket.ID, 10),
		URL:           base.JoinPath("agent", "tickets", externalID).String(),
		Title:         tr.Ticket.Subject,
		Status:        tr.Ticket.Status,
		Priority:      tr.Ticket.Priority,
		ReporterEmail: reporterEmail,
		ReporterName:  reporterName,
		UpdatedAt:     tr.Ticket.UpdatedAt,
	}
	for _, cm := range cr.Comments {
		issue.Comments = append(issue.Comments, connectors.ExternalComment{
			ExternalID: strconv.FormatInt(cm.ID, 10),
			// Zendesk's comment list returns only author_id; the display name is a
			// non-identity UI field and is left empty for the thin connector.
			Author:    "",
			Body:      cm.PlainBody,
			CreatedAt: cm.CreatedAt,
		})
	}
	return issue, nil
}

// PostComment appends a comment to the external ticket by updating it; the created comment's
// id comes from the response audit's "Comment" event. When internal is true the comment is
// posted as a Zendesk PRIVATE/internal comment (public=false) — visible to agents only, never
// to the requester.
func (c *client) PostComment(ctx context.Context, externalID, body string, internal bool) (connectors.ExternalComment, error) {
	if err := validateTicketID(externalID); err != nil {
		return connectors.ExternalComment{}, err
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return connectors.ExternalComment{}, fmt.Errorf("zendesk: invalid base_url: %w", ErrUpstream)
	}
	ticketURL := base.JoinPath("api", "v2", "tickets", externalID+".json")
	// A Zendesk comment is internal when public=false; an internal note maps to public=!internal.
	payload, err := json.Marshal(map[string]any{
		"ticket": map[string]any{
			"comment": map[string]any{"body": body, "public": !internal},
		},
	})
	if err != nil {
		return connectors.ExternalComment{}, fmt.Errorf("zendesk: marshal comment: %w", ErrUpstream)
	}
	var resp zendeskUpdateResponse
	if err := c.doJSON(ctx, http.MethodPut, ticketURL.String(), payload, &resp); err != nil {
		return connectors.ExternalComment{}, err
	}
	commentID := resp.Audit.ID // fall back to the audit id if no Comment event is present
	for _, ev := range resp.Audit.Events {
		if ev.Type == "Comment" {
			commentID = ev.ID
			break
		}
	}
	return connectors.ExternalComment{
		ExternalID: strconv.FormatInt(commentID, 10),
		Author:     "",
		Body:       body,
		CreatedAt:  resp.Audit.CreatedAt,
	}, nil
}

// CreateIssue creates a new Zendesk ticket from the draft, returning its id + agent URL.
// Zendesk has no project/issue-type; a recognized IssueType maps onto the ticket "type".
func (c *client) CreateIssue(ctx context.Context, draft connectors.ExternalIssueDraft) (connectors.ExternalIssue, error) {
	if draft.Summary == "" {
		return connectors.ExternalIssue{}, fmt.Errorf("zendesk: summary required: %w", ErrUpstream)
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return connectors.ExternalIssue{}, fmt.Errorf("zendesk: invalid base_url: %w", ErrUpstream)
	}
	ticketsURL := base.JoinPath("api", "v2", "tickets.json")

	commentBody := draft.Description
	if commentBody == "" {
		commentBody = draft.Summary
	}
	ticket := map[string]any{
		"subject": draft.Summary,
		"comment": map[string]any{"body": commentBody},
	}
	switch strings.ToLower(draft.IssueType) {
	case "problem", "incident", "question", "task":
		ticket["type"] = strings.ToLower(draft.IssueType)
	}
	if draft.ReporterEmail != "" {
		ticket["requester"] = map[string]any{"email": draft.ReporterEmail}
	}
	payload, err := json.Marshal(map[string]any{"ticket": ticket})
	if err != nil {
		return connectors.ExternalIssue{}, fmt.Errorf("zendesk: marshal create: %w", ErrUpstream)
	}
	var resp zendeskTicketResponse
	if err := c.doJSON(ctx, http.MethodPost, ticketsURL.String(), payload, &resp); err != nil {
		return connectors.ExternalIssue{}, err
	}
	idStr := strconv.FormatInt(resp.Ticket.ID, 10)
	return connectors.ExternalIssue{
		ExternalID: idStr,
		URL:        base.JoinPath("agent", "tickets", idStr).String(),
		Title:      draft.Summary,
	}, nil
}

// ── T3 implementations ──

// TransitionStatus sets the ticket's status (Zendesk applies it directly; there is no
// transition graph). The target is clamped to the valid status set before the write.
func (c *client) TransitionStatus(ctx context.Context, externalID, status string) error {
	if err := validateTicketID(externalID); err != nil {
		return err
	}
	target := strings.ToLower(status)
	if !zendeskStatuses[target] {
		return fmt.Errorf("zendesk: unknown status %q: %w", status, ErrUpstream)
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("zendesk: invalid base_url: %w", ErrUpstream)
	}
	ticketURL := base.JoinPath("api", "v2", "tickets", externalID+".json")
	payload, err := json.Marshal(map[string]any{
		"ticket": map[string]any{"status": target},
	})
	if err != nil {
		return fmt.Errorf("zendesk: marshal transition: %w", ErrUpstream)
	}
	return c.doJSON(ctx, http.MethodPut, ticketURL.String(), payload, nil)
}

// ListUpdatedSince returns the ids of tickets updated at/after since (reconcile), paging
// through ALL results. We page with explicit page/per_page rather than following the
// response's next_page URL, so we never dial an upstream-controlled absolute URL through
// the SSRF client. Zendesk search "updated>" is date-granular; over-fetch is harmless
// because the inbound upsert is idempotent.
func (c *client) ListUpdatedSince(ctx context.Context, since time.Time) ([]string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("zendesk: invalid base_url: %w", ErrUpstream)
	}
	query := fmt.Sprintf("type:ticket updated>%s", since.UTC().Format("2006-01-02"))

	var ids []string
	page := 1
	for {
		searchURL := base.JoinPath("api", "v2", "search.json")
		q := url.Values{}
		q.Set("query", query)
		q.Set("sort_by", "updated_at")
		q.Set("sort_order", "asc")
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", strconv.Itoa(searchPageSize))
		searchURL.RawQuery = q.Encode()

		var pageResp zendeskSearchResponse
		if err := c.doJSON(ctx, http.MethodGet, searchURL.String(), nil, &pageResp); err != nil {
			return nil, err
		}
		for _, r := range pageResp.Results {
			ids = append(ids, strconv.FormatInt(r.ID, 10))
		}
		if len(pageResp.Results) == 0 || len(ids) >= pageResp.Count {
			break
		}
		page++
	}
	return ids, nil
}

// VerifyWebhook checks the X-Zendesk-Webhook-Signature header (base64 of
// HMAC-SHA256(secret, timestamp + body)) using the per-connector webhook secret and the
// X-Zendesk-Webhook-Signature-Timestamp header. Fails closed: an unconfigured secret, a
// missing header/timestamp, malformed base64, or a mismatch all return ErrBadSig.
func (c *client) VerifyWebhook(headers http.Header, body []byte) error {
	if c.webhookSecret == "" {
		return fmt.Errorf("zendesk: webhook secret not configured: %w", ErrBadSig)
	}
	sig := headers.Get("X-Zendesk-Webhook-Signature")
	if sig == "" {
		return fmt.Errorf("zendesk: missing signature header: %w", ErrBadSig)
	}
	ts := headers.Get("X-Zendesk-Webhook-Signature-Timestamp")
	if ts == "" {
		return fmt.Errorf("zendesk: missing signature timestamp: %w", ErrBadSig)
	}
	got, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("zendesk: malformed signature base64: %w", ErrBadSig)
	}
	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	mac.Write([]byte(ts))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return fmt.Errorf("zendesk: signature mismatch: %w", ErrBadSig)
	}
	return nil
}

// DecodeWebhook parses a Zendesk event-format webhook body into a WebhookEvent. The
// ticket id is validated so a crafted payload cannot push a path-traversal id downstream.
func (c *client) DecodeWebhook(body []byte) (connectors.WebhookEvent, error) {
	var payload struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Time   string `json:"time"`
		Detail struct {
			ID string `json:"id"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return connectors.WebhookEvent{}, fmt.Errorf("zendesk: decode webhook: %w", ErrUpstream)
	}
	externalID := payload.Detail.ID
	if err := validateTicketID(externalID); err != nil {
		return connectors.WebhookEvent{}, err
	}
	deliveryID := payload.ID
	if deliveryID == "" {
		deliveryID = fmt.Sprintf("%s:%s", externalID, payload.Time)
	}
	return connectors.WebhookEvent{
		DeliveryID: deliveryID,
		ExternalID: externalID,
		Kind:       mapZendeskEventKind(payload.Type),
	}, nil
}

// mapZendeskEventKind maps a Zendesk event "type" onto a canonical kind string.
func mapZendeskEventKind(t string) string {
	switch t {
	case "zen:event-type:ticket.created":
		return "issue.created"
	case "zen:event-type:ticket.updated",
		"zen:event-type:ticket.status_changed",
		"zen:event-type:ticket.priority_changed",
		"zen:event-type:ticket.subject_changed":
		return "issue.updated"
	case "zen:event-type:ticket.comment_added":
		return "comment.created"
	default:
		return t
	}
}

// ── Zendesk REST v2 response shapes (timestamps are RFC3339, decoded by time.Time) ──

type zendeskTicketResponse struct {
	Ticket zendeskTicket `json:"ticket"`
	Users  []zendeskUser `json:"users"`
}

// zendeskUpdateResponse is the PUT /tickets/{id} response carrying the audit of the change.
type zendeskUpdateResponse struct {
	Ticket zendeskTicket `json:"ticket"`
	Audit  struct {
		ID        int64     `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		Events    []struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"events"`
	} `json:"audit"`
}

type zendeskTicket struct {
	ID          int64     `json:"id"`
	Subject     string    `json:"subject"`
	Status      string    `json:"status"`
	Priority    string    `json:"priority"`
	RequesterID int64     `json:"requester_id"`
	URL         string    `json:"url"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type zendeskUser struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type zendeskCommentsResponse struct {
	Comments []zendeskComment `json:"comments"`
}

type zendeskComment struct {
	ID        int64     `json:"id"`
	AuthorID  int64     `json:"author_id"`
	PlainBody string    `json:"plain_body"`
	CreatedAt time.Time `json:"created_at"`
}

// zendeskSearchResponse is the GET /api/v2/search.json response page consumed by
// ListUpdatedSince; only ticket ids and the total count are needed for pagination.
type zendeskSearchResponse struct {
	Results []struct {
		ID int64 `json:"id"`
	} `json:"results"`
	Count int `json:"count"`
}
