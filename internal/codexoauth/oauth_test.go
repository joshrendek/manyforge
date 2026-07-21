package codexoauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newTestClient points a Client at an httptest server (mirrors githubapp's client_test).
func newTestClient(srv *httptest.Server) *Client {
	return &Client{HTTP: srv.Client(), AuthBase: srv.URL}
}

func TestRefresh_invalidGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}))
	defer srv.Close()
	_, err := newTestClient(srv).Refresh(context.Background(), "dead")
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("want ErrInvalidGrant, got %v", err)
	}
}

func TestRefresh_ok(t *testing.T) {
	idTok := makeIDToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc_9"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "r1" {
			t.Errorf("form = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "a2", "refresh_token": "r2", "id_token": idTok, "expires_in": 3600,
		})
	}))
	defer srv.Close()
	ts, err := newTestClient(srv).Refresh(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if ts.AccessToken != "a2" || ts.RefreshToken != "r2" {
		t.Fatalf("got %+v", ts)
	}
}

func TestExchangePKCE(t *testing.T) {
	idTok := makeIDToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc_9"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pin the token endpoint to the codex CLI's path (openai/codex, codex-rs/login).
		if r.URL.Path != tokenPath {
			t.Errorf("token path = %s, want %s", r.URL.Path, tokenPath)
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") != "ver" {
			t.Errorf("form = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "a", "refresh_token": "r", "id_token": idTok, "expires_in": 3600,
		})
	}))
	defer srv.Close()
	ts, err := newTestClient(srv).ExchangePKCE(context.Background(), "code123", "ver")
	if err != nil || ts.AccessToken != "a" {
		t.Fatalf("ts=%+v err=%v", ts, err)
	}
}

func TestNewPKCE_and_AuthorizeURL(t *testing.T) {
	v, ch, err := NewPKCE()
	if err != nil || len(v) < 43 || ch == "" {
		t.Fatalf("v=%q ch=%q err=%v", v, ch, err)
	}
	u := (&Client{AuthBase: "https://auth.openai.com"}).AuthorizeURL(ch, "state123")
	parsed, _ := url.Parse(u)
	if parsed.Path != authorizePath {
		t.Fatalf("authorize path = %s, want %s", parsed.Path, authorizePath)
	}
	q := parsed.Query()
	if q.Get("code_challenge") != ch || q.Get("state") != "state123" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize url query = %v", q)
	}
	if !strings.Contains(u, "auth.openai.com") {
		t.Fatalf("authorize url host wrong: %s", u)
	}
}
