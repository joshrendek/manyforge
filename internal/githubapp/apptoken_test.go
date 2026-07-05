package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testRSAKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

func TestAppJWTClaims(t *testing.T) {
	pemStr := testRSAKeyPEM(t)
	now := time.Unix(1_700_000_000, 0)
	tok, err := AppJWT(42, pemStr, now)
	if err != nil {
		t.Fatalf("AppJWT: %v", err)
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(tok, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := parsed.Claims.(jwt.MapClaims)
	if c["iss"] != "42" && c["iss"] != float64(42) {
		t.Errorf("iss = %v", c["iss"])
	}
	iat := int64(c["iat"].(float64))
	exp := int64(c["exp"].(float64))
	if iat != now.Add(-60*time.Second).Unix() {
		t.Errorf("iat not backdated 60s: %d", iat)
	}
	if exp-iat > 600 {
		t.Errorf("exp-iat = %d, want <= 600", exp-iat)
	}
}

func TestMintInstallationTokenPerRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/77/access_tokens" || r.Method != http.MethodPost {
			t.Errorf("req %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer appjwt" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Repositories []string `json:"repositories"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Repositories) != 1 || body.Repositories[0] != "name" {
			t.Errorf("repos %v", body.Repositories)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_abc", "expires_at": "2026-07-05T13:00:00Z"})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), APIBase: srv.URL, WebBase: srv.URL}
	tok, exp, err := c.MintInstallationToken(context.Background(), 77, "appjwt", "owner/name")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "ghs_abc" || exp.IsZero() {
		t.Fatalf("tok=%q exp=%v", tok, exp)
	}
}

// fakeTokenMinter is a stub tokenMinter recording the arguments it was
// called with, for InstallationTokenSource.Token tests below.
type fakeTokenMinter struct {
	gotInstallationID int64
	gotAppJWT         string
	gotRepoFullName   string
	token             string
	expiresAt         time.Time
	err               error
}

func (f *fakeTokenMinter) MintInstallationToken(ctx context.Context, installationID int64, appJWT, repoFullName string) (string, time.Time, error) {
	f.gotInstallationID = installationID
	f.gotAppJWT = appJWT
	f.gotRepoFullName = repoFullName
	return f.token, f.expiresAt, f.err
}

// stubAppConfigGetter is a stub appConfigGetter returning a fixed AppConfig.
type stubAppConfigGetter struct {
	cfg AppConfig
	err error
}

func (s *stubAppConfigGetter) Get(ctx context.Context) (AppConfig, error) {
	return s.cfg, s.err
}

func TestInstallationTokenSourceToken(t *testing.T) {
	pemStr := testRSAKeyPEM(t)
	store := &stubAppConfigGetter{cfg: AppConfig{AppID: 42, PrivateKeyPEM: pemStr}}
	minter := &fakeTokenMinter{token: "ghs_fresh", expiresAt: time.Now().Add(time.Hour)}
	now := time.Unix(1_700_000_000, 0)
	src := &InstallationTokenSource{
		Store: store,
		API:   minter,
		Now:   func() time.Time { return now },
	}

	tok, err := src.Token(context.Background(), 99, "owner/repo")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_fresh" {
		t.Errorf("tok = %q, want ghs_fresh", tok)
	}
	if minter.gotInstallationID != 99 {
		t.Errorf("installationID = %d, want 99", minter.gotInstallationID)
	}
	if minter.gotRepoFullName != "owner/repo" {
		t.Errorf("repoFullName = %q, want owner/repo", minter.gotRepoFullName)
	}
	if minter.gotAppJWT == "" {
		t.Error("appJWT passed to minter was empty")
	}
	// Confirm the JWT handed to the minter really is derived from the
	// store's AppConfig (iss=42) rather than some hardcoded value.
	parsed, _, err := jwt.NewParser().ParseUnverified(minter.gotAppJWT, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse minted appJWT: %v", err)
	}
	c := parsed.Claims.(jwt.MapClaims)
	if c["iss"] != "42" {
		t.Errorf("appJWT iss = %v, want 42", c["iss"])
	}
}

func TestInstallationTokenSourceTokenMintsFreshEveryCall(t *testing.T) {
	pemStr := testRSAKeyPEM(t)
	store := &stubAppConfigGetter{cfg: AppConfig{AppID: 42, PrivateKeyPEM: pemStr}}
	calls := 0
	minter := &countingMinter{fakeTokenMinter: fakeTokenMinter{token: "ghs_x"}, calls: &calls}
	src := &InstallationTokenSource{
		Store: store,
		API:   minter,
		Now:   func() time.Time { return time.Unix(1_700_000_000, 0) },
	}

	if _, err := src.Token(context.Background(), 1, "o/r"); err != nil {
		t.Fatalf("Token 1: %v", err)
	}
	if _, err := src.Token(context.Background(), 1, "o/r"); err != nil {
		t.Fatalf("Token 2: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (no caching — fresh mint every call)", calls)
	}
}

// countingMinter wraps fakeTokenMinter to assert Token never caches: two
// calls must hit the minter twice.
type countingMinter struct {
	fakeTokenMinter
	calls *int
}

func (c *countingMinter) MintInstallationToken(ctx context.Context, installationID int64, appJWT, repoFullName string) (string, time.Time, error) {
	*c.calls++
	return c.fakeTokenMinter.MintInstallationToken(ctx, installationID, appJWT, repoFullName)
}
