package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
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
		return fmt.Errorf("provider: resend from address: %w", err)
	}
	at := strings.LastIndex(address.Address, "@")
	if at < 0 {
		return fmt.Errorf("provider: resend from address has no domain")
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
			return fmt.Errorf("provider: resend domain %s is %s", domain, candidate.Status)
		}
	}
	return fmt.Errorf("provider: resend domain %s was not found", domain)
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
		return &HTTPError{StatusCode: resp.StatusCode, Code: detail.Name, Message: detail.Message}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("provider: resend decode: %w", err)
		}
	}
	return nil
}
