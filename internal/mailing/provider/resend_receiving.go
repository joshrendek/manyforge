package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// maxRawRedirects bounds how many redirects a raw download follows.
const maxRawRedirects = 3

// errRawRedirectRejected carries no URL: redirect targets may be signed.
var errRawRedirectRejected = errors.New("provider: raw download redirect rejected")

var (
	// ErrReceivedEmailTooLarge marks a received message (or its metadata) larger
	// than MANYFORGE_INBOUND_MAX_BYTES allows. Callers acknowledge, never retry (R6).
	ErrReceivedEmailTooLarge = errors.New("provider: received email exceeds the size cap")
	// ErrReceivedEmailGone marks a received email Resend no longer has (HTTP 404).
	ErrReceivedEmailGone = errors.New("provider: received email not found")
	// ErrReceivedRawUnavailable marks a missing or unusable raw download URL.
	// Callers treat it as transient and let Resend retry.
	ErrReceivedRawUnavailable = errors.New("provider: received email raw message unavailable")
)

// ReceivedEmail is the subset of Resend's GET /emails/receiving/{id} that
// ManyForge uses. Shape confirmed against
// https://resend.com/docs/api-reference/emails/retrieve-received-email (2026-10-10).
type ReceivedEmail struct {
	ID          string
	To          []string
	ReceivedFor []string
	MessageID   string
	RawURL      string
}

type resendReceivedEmailResponse struct {
	ID          string   `json:"id"`
	To          []string `json:"to"`
	ReceivedFor []string `json:"received_for"`
	MessageID   string   `json:"message_id"`
	Raw         *struct {
		DownloadURL string `json:"download_url"`
	} `json:"raw"`
}

// GetReceivedEmail loads a received email's metadata with the profile's API key.
// html_format=cid keeps inline images out of the JSON. The response is bounded
// by 1 MiB + 2*maxRawBytes (text and html are each at most the raw size); a
// larger response returns ErrReceivedEmailTooLarge.
func (r *Resend) GetReceivedEmail(ctx context.Context, id string, maxRawBytes int64) (ReceivedEmail, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 200 {
		return ReceivedEmail{}, ErrReceivedEmailGone
	}
	limit := int64(1 << 20)
	if maxRawBytes > 0 {
		limit += 2 * maxRawBytes
	}
	var resp resendReceivedEmailResponse
	err := r.doLimited(ctx, http.MethodGet, "/emails/receiving/"+url.PathEscape(id)+"?html_format=cid", nil, "", &resp, limit)
	var httpErr *HTTPError
	switch {
	case errors.Is(err, errResendResponseTooLarge):
		return ReceivedEmail{}, ErrReceivedEmailTooLarge
	case errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound:
		return ReceivedEmail{}, ErrReceivedEmailGone
	case err != nil:
		return ReceivedEmail{}, err
	}
	out := ReceivedEmail{ID: resp.ID, To: resp.To, ReceivedFor: resp.ReceivedFor, MessageID: resp.MessageID}
	if resp.Raw != nil {
		out.RawURL = strings.TrimSpace(resp.Raw.DownloadURL)
	}
	if out.RawURL == "" {
		return ReceivedEmail{}, ErrReceivedRawUnavailable
	}
	return out, nil
}

// DownloadReceivedRaw fetches the original RFC 822 message from Resend's
// pre-signed raw.download_url. The API key is never sent. Only https URLs are
// fetched (http only when AllowInsecureRawURL is set, for tests). The body is
// capped at maxBytes while streaming, so a missing or lying Content-Length cannot
// exhaust memory. Transport errors never include the signed URL.
func (r *Resend) DownloadReceivedRaw(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && (!r.AllowInsecureRawURL || u.Scheme != "http")) {
		return nil, ErrReceivedRawUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrReceivedRawUnavailable
	}
	// Fail closed: the pre-signed URL is caller-influenced, so production must
	// supply an SSRF-guarded RawClient. Only the test opt-in may fall back.
	base := r.RawClient
	if base == nil {
		if !r.AllowInsecureRawURL {
			return nil, ErrReceivedRawUnavailable
		}
		base = r.Client
		if base == nil {
			base = http.DefaultClient
		}
	}
	// Shallow copy: the shared client is never mutated. Redirects stay https-only
	// and bounded, so a signed URL cannot be bounced to http or around forever.
	client := *base
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRawRedirects {
			return errRawRedirectRejected
		}
		if req.URL.Scheme != "https" && (!r.AllowInsecureRawURL || req.URL.Scheme != "http") {
			return errRawRedirectRejected
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errRawRedirectRejected) {
			return nil, ErrReceivedRawUnavailable
		}
		var netErr net.Error
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// ctx.Err() is nil when http.Client.Timeout (not ctx) expired.
			cause := ctx.Err()
			if cause == nil {
				cause = context.DeadlineExceeded
			}
			return nil, fmt.Errorf("provider: resend raw transport: %w", cause)
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) && errors.As(urlErr.Err, &netErr) {
			return nil, fmt.Errorf("provider: resend raw transport: %w", urlErr.Err)
		}
		// Anything else (e.g. a malformed Location) may embed the signed URL.
		return nil, errors.New("provider: resend raw transport failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode}
	}
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		return nil, ErrReceivedEmailTooLarge
	}
	reader := io.Reader(resp.Body)
	if maxBytes > 0 {
		reader = io.LimitReader(resp.Body, maxBytes+1)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, errors.New("provider: resend raw read failed")
	}
	if maxBytes > 0 && int64(len(raw)) > maxBytes {
		return nil, ErrReceivedEmailTooLarge
	}
	return raw, nil
}

// EnsureReceivingEvents makes an existing webhook subscribe to every event in
// ResendWebhookEvents, keeping any other events it already has, without touching
// its endpoint or identity. It is read-only when nothing is missing. It returns
// the signing secret as read after any change so the caller can confirm the
// stored secret still matches.
func (r *Resend) EnsureReceivingEvents(ctx context.Context, webhookID string) (string, bool, error) {
	webhookID = strings.TrimSpace(webhookID)
	if webhookID == "" {
		return "", false, ErrResendWebhook
	}
	detail, err := r.webhookDetail(ctx, webhookID)
	if err != nil {
		return "", false, err
	}
	if containsResendEvents(detail.Events, ResendWebhookEvents...) {
		return detail.SigningSecret, false, nil
	}
	if err := r.patchWebhookEvents(ctx, webhookID, unionEvents(detail.Events, ResendWebhookEvents)); err != nil {
		return "", false, err
	}
	detail, err = r.webhookDetail(ctx, webhookID)
	if err != nil {
		return "", false, err
	}
	if !containsResendEvents(detail.Events, ResendWebhookEvents...) || !validResendSigningSecret(detail.SigningSecret) {
		return "", false, ErrResendWebhook
	}
	return detail.SigningSecret, true, nil
}

func (r *Resend) webhookDetail(ctx context.Context, webhookID string) (resendWebhookResponse, error) {
	var detail resendWebhookResponse
	err := r.do(ctx, http.MethodGet, "/webhooks/"+url.PathEscape(webhookID), nil, "", &detail)
	return detail, err
}

func (r *Resend) patchWebhookEvents(ctx context.Context, webhookID string, events []string) error {
	payload := struct {
		Events []string `json:"events"`
	}{Events: events}
	return r.do(ctx, http.MethodPatch, "/webhooks/"+url.PathEscape(webhookID), payload, "", nil)
}

func unionEvents(existing, required []string) []string {
	seen := make(map[string]bool, len(existing)+len(required))
	out := make([]string, 0, len(existing)+len(required))
	for _, list := range [][]string{existing, required} {
		for _, event := range list {
			if event != "" && !seen[event] {
				seen[event] = true
				out = append(out, event)
			}
		}
	}
	return out
}
