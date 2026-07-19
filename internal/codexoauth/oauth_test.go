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
	"time"
)

// newTestClient points a Client at an httptest server (mirrors githubapp's client_test).
func newTestClient(srv *httptest.Server) *Client {
	return &Client{HTTP: srv.Client(), AuthBase: srv.URL}
}

func TestStartDeviceAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deviceAuthPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.Form.Get("client_id") != clientID {
			t.Errorf("client_id = %s", r.Form.Get("client_id"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dev123", "user_code": "WXYZ-1234",
			"verification_uri":          "https://auth.openai.com/codex/device",
			"verification_uri_complete": "https://auth.openai.com/codex/device?user_code=WXYZ-1234",
			"interval":                  5, "expires_in": 900,
		})
	}))
	defer srv.Close()
	da, err := newTestClient(srv).StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if da.DeviceCode != "dev123" || da.UserCode != "WXYZ-1234" || da.Interval != 5 {
		t.Fatalf("got %+v", da)
	}
}

func TestPollDeviceToken_pendingThenApproved(t *testing.T) {
	idTok := makeIDToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc_9", "chatgpt_plan_type": "plus"},
	})
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "acc-tok", "refresh_token": "ref-tok", "id_token": idTok, "expires_in": 3600,
		})
	}))
	defer srv.Close()
	c := newTestClient(srv)
	if _, st, _ := c.PollDeviceToken(context.Background(), "dev123"); st != PollPending {
		t.Fatalf("first poll status = %v", st)
	}
	ts, st, err := c.PollDeviceToken(context.Background(), "dev123")
	if err != nil || st != PollApproved {
		t.Fatalf("second poll: st=%v err=%v", st, err)
	}
	if ts.AccessToken != "acc-tok" || ts.RefreshToken != "ref-tok" || ts.Claims.AccountID != "acc_9" {
		t.Fatalf("got %+v", ts)
	}
	if ts.Expiry.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("expiry not ~1h out: %v", ts.Expiry)
	}
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
	q := parsed.Query()
	if q.Get("code_challenge") != ch || q.Get("state") != "state123" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize url query = %v", q)
	}
	if !strings.Contains(u, "auth.openai.com") {
		t.Fatalf("authorize url host wrong: %s", u)
	}
}
