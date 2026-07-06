package coding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHTTPProber_Live2xx: an OpenAI-compat endpoint answering 200 on /models is live.
func TestHTTPProber_Live2xx(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := httpProber{Timeout: 2 * time.Second}
	// httptest binds 127.0.0.1 → loopback must be permitted (AllowPrivateBaseURL).
	if !p.Live(context.Background(), AICredential{Provider: "vllm", BaseURL: srv.URL + "/v1", AllowPrivateBaseURL: true}) {
		t.Fatal("expected live for a 2xx /models endpoint")
	}
	if gotPath != "/v1/models" {
		t.Fatalf("prober hit %q, want /v1/models", gotPath)
	}
}

// TestHTTPProber_DeadConnRefused: a closed port is not live (connection refused).
func TestHTTPProber_DeadConnRefused(t *testing.T) {
	p := httpProber{Timeout: 500 * time.Millisecond}
	// Port 9 (discard) is reliably not listening for HTTP on loopback.
	if p.Live(context.Background(), AICredential{Provider: "vllm", BaseURL: "http://127.0.0.1:9/v1", AllowPrivateBaseURL: true}) {
		t.Fatal("expected not-live for a refused connection")
	}
}

// TestHTTPProber_Non2xx: a 500 from /models is treated as not live.
func TestHTTPProber_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := httpProber{Timeout: 2 * time.Second}
	if p.Live(context.Background(), AICredential{Provider: "openrouter", BaseURL: srv.URL + "/v1", AllowPrivateBaseURL: true}) {
		t.Fatal("expected not-live for a 5xx /models response")
	}
}

// TestHTTPProber_AnthropicAssumedLive: anthropic has no cheap probe → assumed live,
// short-circuiting without any network call (so a tiny timeout still returns true).
func TestHTTPProber_AnthropicAssumedLive(t *testing.T) {
	p := httpProber{Timeout: time.Millisecond}
	if !p.Live(context.Background(), AICredential{Provider: "anthropic", BaseURL: "https://api.anthropic.com"}) {
		t.Fatal("anthropic must be assumed live")
	}
}

// TestHTTPProber_PrivateBlockedWithoutFlag: a private LAN host is not live when the
// credential lacks allow_private_base_url — netsafe refuses the dial (no SSRF regression),
// so the probe fails closed regardless of whether the host is actually reachable.
func TestHTTPProber_PrivateBlockedWithoutFlag(t *testing.T) {
	p := httpProber{Timeout: 500 * time.Millisecond}
	if p.Live(context.Background(), AICredential{Provider: "vllm", BaseURL: "http://192.168.2.241:1234/v1", AllowPrivateBaseURL: false}) {
		t.Fatal("expected not-live: private host must be blocked without allow_private_base_url")
	}
}

// TestHTTPProber_EmptyBaseURL: no base_url ⇒ not live (can't probe).
func TestHTTPProber_EmptyBaseURL(t *testing.T) {
	p := httpProber{Timeout: time.Millisecond}
	if p.Live(context.Background(), AICredential{Provider: "vllm", BaseURL: ""}) {
		t.Fatal("expected not-live for an empty base_url")
	}
}
