// Package zendesk implements connectors.TicketingConnector for the Zendesk REST API v2.
// The client is SSRF-safe (backed by netsafe) and uses HTTP Basic API-token auth, whose
// Zendesk form is username "<email>/token", password "<api_token>". Errors from non-2xx
// responses NEVER contain the upstream response body (Spec 004 Principle II / §7).
package zendesk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
)

// sentinel errors — never wrap upstream bodies.
var (
	ErrUpstream    = errors.New("zendesk: upstream error")
	ErrUnreachable = errors.New("zendesk: service unreachable")
	ErrBadSig      = errors.New("zendesk: invalid webhook signature")
	ErrBadTicketID = errors.New("zendesk: invalid ticket id")
)

const bodyLimit = 8 << 20 // 8 MiB

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
		// Network failure, timeout, or SSRF dial-refusal (netsafe) all land here.
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

// ── Stub methods — implemented in T2–T4 ──

// PostComment implements connectors.TicketingConnector (T2).
func (c *client) PostComment(_ context.Context, _, _ string) (connectors.ExternalComment, error) {
	return connectors.ExternalComment{}, fmt.Errorf("zendesk: PostComment: %w", ErrUpstream)
}

// TransitionStatus implements connectors.TicketingConnector (T3).
func (c *client) TransitionStatus(_ context.Context, _, _ string) error {
	return fmt.Errorf("zendesk: TransitionStatus: %w", ErrUpstream)
}

// ListUpdatedSince implements connectors.TicketingConnector (T3).
func (c *client) ListUpdatedSince(_ context.Context, _ time.Time) ([]string, error) {
	return nil, fmt.Errorf("zendesk: ListUpdatedSince: %w", ErrUpstream)
}

// VerifyWebhook implements connectors.TicketingConnector (T4).
func (c *client) VerifyWebhook(_ http.Header, _ []byte) error {
	return fmt.Errorf("zendesk: VerifyWebhook: %w", ErrUpstream)
}

// DecodeWebhook implements connectors.TicketingConnector (T4).
func (c *client) DecodeWebhook(_ []byte) (connectors.WebhookEvent, error) {
	return connectors.WebhookEvent{}, fmt.Errorf("zendesk: DecodeWebhook: %w", ErrUpstream)
}

// CreateIssue implements connectors.TicketingConnector (T2).
func (c *client) CreateIssue(_ context.Context, _ connectors.ExternalIssueDraft) (connectors.ExternalIssue, error) {
	return connectors.ExternalIssue{}, fmt.Errorf("zendesk: CreateIssue: %w", ErrUpstream)
}

// ── Zendesk REST v2 response shapes (timestamps are RFC3339, decoded by time.Time) ──

type zendeskTicketResponse struct {
	Ticket zendeskTicket `json:"ticket"`
	Users  []zendeskUser `json:"users"`
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
