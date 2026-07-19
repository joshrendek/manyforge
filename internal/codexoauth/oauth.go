package codexoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

const (
	// clientID is OpenAI's public Codex/ChatGPT OAuth client (same one the codex CLI uses).
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// scope requests offline_access so the token endpoint returns a refresh token.
	scope = "openid profile email offline_access"
	// redirectURI matches the codex CLI loopback the PKCE paste-redirect flow reproduces.
	redirectURI = "http://localhost:1455/auth/callback"

	// Paths on AuthBase. OPEN ITEM: confirm deviceAuthPath against codex-rs /
	// tumf/opencode-openai-device-auth before shipping; the value is isolated here and the
	// tests inject AuthBase, so only the prod constant depends on it.
	deviceAuthPath = "/oauth/device/code"
	tokenPath      = "/oauth/token"
	authorizePath  = "/oauth/authorize"
)

// ErrInvalidGrant wraps a token-endpoint invalid_grant (dead/rotated refresh token, expired
// device code). Task 6 uses errors.Is to disconnect the credential.
var ErrInvalidGrant = errors.New("codexoauth: invalid_grant")

// Client talks to auth.openai.com over the SSRF-screened netsafe client. AuthBase is exported
// so tests point it at an httptest server.
type Client struct {
	HTTP     *http.Client
	AuthBase string
}

// NewClient wires a Client to auth.openai.com through netsafe (IP-screened; the host reaches this
// fixed public host with no allowlist change — there is no host hostname allowlist).
func NewClient(timeout time.Duration) *Client {
	return &Client{HTTP: netsafe.NewClient(timeout), AuthBase: "https://auth.openai.com"}
}

// TokenSet is a decoded token-endpoint success response plus the parsed id_token claims.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	Expiry       time.Time
	Claims       Claims
}

// DeviceAuth is the device-authorization response.
type DeviceAuth struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                int
	ExpiresIn               int
}

// PollStatus is the outcome of one device-token poll.
type PollStatus int

const (
	PollPending PollStatus = iota
	PollSlowDown
	PollApproved
	PollDenied
	PollExpired
)

// tokenResp is the shared token-endpoint success shape.
type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// errResp is the OAuth error shape (error field only — never surface description to callers).
type errResp struct {
	Error string `json:"error"`
}

// postForm posts an application/x-www-form-urlencoded body. It reads the body once and, on a
// non-2xx, returns (rawBody, statusErr) so callers can classify the OAuth `error` field WITHOUT
// leaking the upstream body into the returned error text (mirrors githubapp.do()).
func (c *Client) postForm(ctx context.Context, path string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AuthBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("codexoauth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codexoauth request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("codexoauth status %d", resp.StatusCode) // body returned for classification, not for the error text
	}
	return body, nil
}

// decodeToken parses a token success body into a TokenSet (incl. id_token claims + expiry).
func decodeToken(body []byte) (TokenSet, error) {
	var r tokenResp
	if err := json.Unmarshal(body, &r); err != nil {
		return TokenSet{}, fmt.Errorf("codexoauth: decode token: %w", err)
	}
	if r.AccessToken == "" {
		return TokenSet{}, fmt.Errorf("codexoauth: empty access_token")
	}
	ts := TokenSet{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		IDToken:      r.IDToken,
		Expiry:       time.Now().Add(time.Duration(r.ExpiresIn) * time.Second),
	}
	if r.IDToken != "" {
		c, err := parseIDTokenClaims(r.IDToken)
		if err != nil {
			return TokenSet{}, err
		}
		ts.Claims = c
	}
	return ts, nil
}

// StartDeviceAuth initiates the device-authorization flow.
func (c *Client) StartDeviceAuth(ctx context.Context) (DeviceAuth, error) {
	body, err := c.postForm(ctx, deviceAuthPath, url.Values{
		"client_id": {clientID}, "scope": {scope},
	})
	if err != nil {
		return DeviceAuth{}, err
	}
	var r struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return DeviceAuth{}, fmt.Errorf("codexoauth: decode device auth: %w", err)
	}
	if r.DeviceCode == "" {
		return DeviceAuth{}, fmt.Errorf("codexoauth: empty device_code")
	}
	if r.Interval == 0 {
		r.Interval = 5
	}
	return DeviceAuth(r), nil
}

// PollDeviceToken polls once for the device token. A pending/slow_down/expired/denied maps to a
// PollStatus with a nil error; a real transport error returns (…, 0, err).
func (c *Client) PollDeviceToken(ctx context.Context, deviceCode string) (TokenSet, PollStatus, error) {
	body, err := c.postForm(ctx, tokenPath, url.Values{
		"client_id":   {clientID},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
	})
	if err != nil {
		var oe errResp
		_ = json.Unmarshal(body, &oe)
		switch oe.Error {
		case "authorization_pending":
			return TokenSet{}, PollPending, nil
		case "slow_down":
			return TokenSet{}, PollSlowDown, nil
		case "expired_token":
			return TokenSet{}, PollExpired, nil
		case "access_denied":
			return TokenSet{}, PollDenied, nil
		default:
			return TokenSet{}, 0, err
		}
	}
	ts, derr := decodeToken(body)
	if derr != nil {
		return TokenSet{}, 0, derr
	}
	return ts, PollApproved, nil
}

// ExchangePKCE trades an authorization code (from the pasted redirect) for tokens.
func (c *Client) ExchangePKCE(ctx context.Context, code, verifier string) (TokenSet, error) {
	body, err := c.postForm(ctx, tokenPath, url.Values{
		"client_id":     {clientID},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if err != nil {
		return TokenSet{}, classifyGrant(body, err)
	}
	return decodeToken(body)
}

// Refresh exchanges a refresh token for a new token set (rotating the refresh token).
func (c *Client) Refresh(ctx context.Context, refreshToken string) (TokenSet, error) {
	body, err := c.postForm(ctx, tokenPath, url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {scope},
	})
	if err != nil {
		return TokenSet{}, classifyGrant(body, err)
	}
	return decodeToken(body)
}

// classifyGrant maps an invalid_grant error body to ErrInvalidGrant; otherwise returns the
// status error unchanged (no upstream body leaked).
func classifyGrant(body []byte, statusErr error) error {
	var oe errResp
	_ = json.Unmarshal(body, &oe)
	if oe.Error == "invalid_grant" {
		return ErrInvalidGrant
	}
	return statusErr
}

// NewPKCE returns a fresh (verifier, S256-challenge) pair for the paste-redirect flow.
func NewPKCE() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("codexoauth: pkce rand: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf) // 43 chars
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// AuthorizeURL builds the browser authorize URL for the PKCE flow. The two OpenAI-specific params
// (id_token_add_organizations / codex_cli_simplified_flow) match the codex CLI.
func (c *Client) AuthorizeURL(challenge, state string) string {
	q := url.Values{
		"response_type":              {"code"},
		"client_id":                  {clientID},
		"redirect_uri":               {redirectURI},
		"scope":                      {scope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return c.AuthBase + authorizePath + "?" + q.Encode()
}
