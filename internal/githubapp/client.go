package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// Installation is a GitHub App installation as seen by the authenticated
// user (GET /user/installations) — just enough to let the user pick which
// org/account to connect.
type Installation struct {
	ID    int64
	Login string
	Type  string
}

// GitHubAPI is the fakeable surface manyforge needs from GitHub during App
// setup and OAuth login: converting a manifest into App credentials,
// exchanging an OAuth code for a user token, and listing the installations
// that token can see.
type GitHubAPI interface {
	ConvertManifest(ctx context.Context, code string) (AppCreds, error)
	ExchangeOAuthCode(ctx context.Context, clientID, clientSecret, code string) (string, error)
	ListUserInstallations(ctx context.Context, userToken string) ([]Installation, error)
}

// Client is the real GitHubAPI implementation. HTTP, APIBase, and WebBase
// are exported so unit tests can point it at an httptest server instead of
// the live GitHub hosts.
type Client struct {
	HTTP    *http.Client
	APIBase string
	WebBase string
}

// NewClient returns a Client wired to the real GitHub hosts over the
// SSRF-guarded netsafe client — every outbound call this package makes is
// influenced by data GitHub itself returns (installation IDs, OAuth codes),
// so it must go through the locked-down dialer.
func NewClient(timeout time.Duration) *Client {
	return &Client{
		HTTP:    netsafe.NewClient(timeout),
		APIBase: "https://api.github.com",
		WebBase: "https://github.com",
	}
}

// do executes req and decodes a JSON body into out (if non-nil). Upstream
// response bodies are never surfaced in the returned error — only the
// status code — so a GitHub error page/body never leaks to a manyforge
// caller; log the detail server-side if needed.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("github request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github status %d", resp.StatusCode) // never surface upstream body
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("github decode: %w", err)
		}
	}
	return nil
}

// ConvertManifest exchanges a just-created App manifest's temporary code for
// the permanent App identity + secrets (POST /app-manifests/{code}/conversions).
func (c *Client) ConvertManifest(ctx context.Context, code string) (AppCreds, error) {
	u := fmt.Sprintf("%s/app-manifests/%s/conversions", c.APIBase, url.PathEscape(code))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return AppCreds{}, fmt.Errorf("github request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	var r struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := c.do(req, &r); err != nil {
		return AppCreds{}, err
	}
	return AppCreds{
		AppID:         r.ID,
		Slug:          r.Slug,
		ClientID:      r.ClientID,
		ClientSecret:  r.ClientSecret,
		PrivateKeyPEM: r.PEM,
		WebhookSecret: r.WebhookSecret,
	}, nil
}

// ExchangeOAuthCode trades a user-facing OAuth code for an access token
// (POST /login/oauth/access_token). Accept: application/json is required —
// without it GitHub replies form-encoded instead of JSON.
func (c *Client) ExchangeOAuthCode(ctx context.Context, clientID, clientSecret, code string) (string, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.WebBase+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("github request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json") // force JSON, not form-encoded
	var r struct {
		AccessToken string `json:"access_token"`
	}
	if err := c.do(req, &r); err != nil {
		return "", err
	}
	if r.AccessToken == "" {
		return "", fmt.Errorf("github oauth: no access token")
	}
	return r.AccessToken, nil
}

// ListUserInstallations lists the App installations visible to userToken
// (GET /user/installations).
//
// Known limitation: per_page=100 without pagination — a user with more than
// 100 installations will silently see only the first page. Acceptable for
// Slice 1; revisit if/when that becomes reachable.
func (c *Client) ListUserInstallations(ctx context.Context, userToken string) ([]Installation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBase+"/user/installations?per_page=100", nil)
	if err != nil {
		return nil, fmt.Errorf("github request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	var r struct {
		Installations []struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
		} `json:"installations"`
	}
	if err := c.do(req, &r); err != nil {
		return nil, err
	}
	out := make([]Installation, 0, len(r.Installations))
	for _, in := range r.Installations {
		out = append(out, Installation{ID: in.ID, Login: in.Account.Login, Type: in.Account.Type})
	}
	return out, nil
}
