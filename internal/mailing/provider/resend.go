package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strings"

	"github.com/manyforge/manyforge/internal/platform/notify"
)

const defaultResendBaseURL = "https://api.resend.com"

// Resend delivers structured messages through the Resend HTTP API. An empty
// BaseURL selects the public Resend endpoint.
type Resend struct {
	APIKey    string
	FromEmail string
	BaseURL   string
	Client    *http.Client
}

type ResendWebhook struct {
	ID            string
	SigningSecret string
}

type ResendWebhookProvisioner interface {
	EnsureWebhook(context.Context, string, string) (ResendWebhook, bool, error)
	CleanupWebhooks(context.Context, string, string, bool) error
	DeleteWebhook(context.Context, string) error
}

type resendWebhookResponse struct {
	ID            string   `json:"id"`
	SigningSecret string   `json:"signing_secret"`
	Status        string   `json:"status"`
	Endpoint      string   `json:"endpoint"`
	Events        []string `json:"events"`
}

// Send validates message headers and submits a structured Resend API request.
func (r *Resend) Send(ctx context.Context, mail notify.Mail) (SendResult, error) {
	// Resend accepts structured headers rather than raw MIME, but it must still
	// pass the shared CR/LF injection chokepoint used by SMTP and SES.
	if _, err := notify.BuildMIME(mail); err != nil {
		return SendResult{}, err
	}
	payload := map[string]any{
		"from": mail.From, "to": []string{mail.To}, "subject": mail.Subject,
		"text": mail.BodyText, "html": mail.BodyHTML,
	}
	if mail.ReplyTo != "" {
		payload["reply_to"] = mail.ReplyTo
	}
	headers := make(map[string]string, len(mail.ExtraHeaders)+2)
	for key, value := range mail.ExtraHeaders {
		headers[key] = value
	}
	if mail.MessageID != "" {
		headers["Message-ID"] = "<" + mail.MessageID + ">"
	}
	if mail.AutoSubmitted != "" {
		headers["Auto-Submitted"] = mail.AutoSubmitted
	}
	if len(headers) > 0 {
		payload["headers"] = headers
	}
	if deliveryID := strings.TrimSpace(mail.ExtraHeaders["X-MF-Delivery"]); deliveryID != "" {
		payload["tags"] = []map[string]string{{"name": "mf_delivery", "value": deliveryID}}
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := r.do(ctx, http.MethodPost, "/emails", payload, mail.MessageID, &response); err != nil {
		return SendResult{}, err
	}
	return SendResult{ProviderID: response.ID}, nil
}

// Verify confirms the configured From domain is verified in Resend.
func (r *Resend) Verify(ctx context.Context) error {
	address, err := mail.ParseAddress(r.FromEmail)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSenderAddress, err)
	}
	at := strings.LastIndex(address.Address, "@")
	if at < 0 {
		return ErrSenderAddress
	}
	domain := strings.ToLower(address.Address[at+1:])
	var response struct {
		Data []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := r.do(ctx, http.MethodGet, "/domains?limit=100", nil, "", &response); err != nil {
		return err
	}
	for _, candidate := range response.Data {
		if strings.EqualFold(candidate.Name, domain) {
			if strings.EqualFold(candidate.Status, "verified") {
				return nil
			}
			return ErrSenderDomain
		}
	}
	return ErrSenderDomain
}

func (r *Resend) EnsureWebhook(ctx context.Context, endpoint, existingID string) (ResendWebhook, bool, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ResendWebhook{}, false, ErrPublicURL
	}
	if webhook, found, err := r.reconcileWebhooks(ctx, endpoint, existingID); err != nil || found {
		return webhook, false, err
	}
	payload := struct {
		Endpoint string   `json:"endpoint"`
		Events   []string `json:"events"`
	}{
		Endpoint: endpoint,
		Events:   []string{"email.bounced", "email.complained"},
	}
	var created resendWebhookResponse
	createErr := r.do(ctx, http.MethodPost, "/webhooks", payload, "", &created)
	webhook, found, reconcileErr := r.reconcileWebhooks(ctx, endpoint, created.ID)
	if reconcileErr != nil {
		return ResendWebhook{}, false, reconcileErr
	}
	if found {
		return webhook, true, nil
	}
	if createErr != nil {
		return ResendWebhook{}, false, createErr
	}
	return ResendWebhook{}, false, ErrResendWebhook
}

func (r *Resend) reconcileWebhooks(ctx context.Context, endpoint, existingID string) (ResendWebhook, bool, error) {
	var listed struct {
		HasMore bool                    `json:"has_more"`
		Data    []resendWebhookResponse `json:"data"`
	}
	if err := r.do(ctx, http.MethodGet, "/webhooks?limit=100", nil, "", &listed); err != nil {
		return ResendWebhook{}, false, err
	}
	if listed.HasMore || len(listed.Data) > 100 {
		return ResendWebhook{}, false, ErrResendWebhookLimit
	}
	candidates := make([]resendWebhookResponse, 0, 1)
	for _, summary := range listed.Data {
		if summary.Endpoint != endpoint && summary.ID != existingID {
			continue
		}
		var detail resendWebhookResponse
		if err := r.do(ctx, http.MethodGet, "/webhooks/"+url.PathEscape(summary.ID), nil, "", &detail); err != nil {
			return ResendWebhook{}, false, err
		}
		valid := detail.ID == summary.ID && detail.Status == "enabled" &&
			detail.Endpoint == endpoint &&
			containsResendEvents(detail.Events, "email.bounced", "email.complained") &&
			validResendSigningSecret(detail.SigningSecret)
		if !valid {
			if err := r.DeleteWebhook(ctx, summary.ID); err != nil {
				return ResendWebhook{}, false, err
			}
			continue
		}
		candidates = append(candidates, detail)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	if len(candidates) == 0 {
		return ResendWebhook{}, false, nil
	}
	for _, duplicate := range candidates[1:] {
		if err := r.DeleteWebhook(ctx, duplicate.ID); err != nil {
			return ResendWebhook{}, false, err
		}
	}
	return ResendWebhook{ID: candidates[0].ID, SigningSecret: candidates[0].SigningSecret}, true, nil
}
func (r *Resend) CleanupWebhooks(ctx context.Context, endpoint, existingID string, requireMatch bool) error {
	matches, err := r.listCleanupMatches(ctx, endpoint, existingID)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		if requireMatch {
			return fmt.Errorf("provider: replacement Resend key did not find the webhook cleanup target")
		}
		return nil
	}
	for _, match := range matches {
		if err := r.DeleteWebhook(ctx, match.ID); err != nil {
			return err
		}
	}
	remaining, err := r.listCleanupMatches(ctx, endpoint, existingID)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return fmt.Errorf("provider: Resend webhook cleanup was not confirmed")
	}
	return nil
}

func (r *Resend) listCleanupMatches(ctx context.Context, endpoint, existingID string) ([]resendWebhookResponse, error) {
	var listed struct {
		HasMore bool                    `json:"has_more"`
		Data    []resendWebhookResponse `json:"data"`
	}
	if err := r.do(ctx, http.MethodGet, "/webhooks?limit=100", nil, "", &listed); err != nil {
		return nil, err
	}
	if listed.HasMore || len(listed.Data) > 100 {
		return nil, fmt.Errorf("provider: resend webhook cleanup exceeds the bounded page")
	}
	matches := make([]resendWebhookResponse, 0)
	for _, summary := range listed.Data {
		if summary.Endpoint == endpoint || summary.ID == existingID {
			matches = append(matches, summary)
		}
	}
	return matches, nil
}

func (r *Resend) DeleteWebhook(ctx context.Context, webhookID string) error {
	if strings.TrimSpace(webhookID) == "" {
		return nil
	}
	err := r.do(ctx, http.MethodDelete, "/webhooks/"+url.PathEscape(webhookID), nil, "", nil)
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func containsResendEvents(events []string, required ...string) bool {
	present := make(map[string]bool, len(events))
	for _, event := range events {
		present[event] = true
	}
	for _, event := range required {
		if !present[event] {
			return false
		}
	}
	return true
}

func validResendSigningSecret(secret string) bool {
	if !strings.HasPrefix(secret, "whsec_") {
		return false
	}
	raw := strings.TrimPrefix(secret, "whsec_")
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(raw)
	}
	valid := err == nil && len(key) >= 16
	clear(key)
	return valid
}

func (r *Resend) do(ctx context.Context, method, path string, body any, idempotencyKey string, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("provider: resend request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	base := strings.TrimRight(r.BaseURL, "/")
	if base == "" {
		base = defaultResendBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return fmt.Errorf("provider: resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("provider: resend transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("provider: resend response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var detail struct {
			Name, Message string
			StatusCode    int `json:"statusCode"`
		}
		_ = json.Unmarshal(raw, &detail)
		return &HTTPError{StatusCode: resp.StatusCode, Code: detail.Name}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("provider: resend decode: %w", err)
		}
	}
	return nil
}
