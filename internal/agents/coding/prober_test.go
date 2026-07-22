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

// TestHTTPProber_CodexAssumedLive: openai_codex's ChatGPT backend (chatgpt.com/backend-api/codex)
// answers only to a request carrying ChatGPT-Account-Id + originator + a versioned codex User-Agent
// + a fresh OAuth token — a bare GET /models 403s — so there is no cheap unauthenticated probe. Its
// token validity is instead enforced at credential-resolve time (a dead codex resolves to an
// unresolvable candidate the fallback chain already skips), so codex is assumed live here and
// short-circuits without any network call (a 1ms timeout still returns true). manyforge-6fx.
func TestHTTPProber_CodexAssumedLive(t *testing.T) {
	p := httpProber{Timeout: time.Millisecond}
	if !p.Live(context.Background(), AICredential{Provider: "openai_codex", BaseURL: "https://chatgpt.com/backend-api/codex"}) {
		t.Fatal("openai_codex must be assumed live (no cheap unauthenticated probe; liveness enforced at resolve time)")
	}
}

// TestHTTPProber_HuggingFaceIsProbed: unlike anthropic, the HF router serves GET /v1/models
// publicly and fast, so huggingface takes the normal probe path — a 200 is live, a timeout or
// non-2xx is not. Only anthropic and openai_codex are assume-live (neither has a cheap
// unauthenticated probe); keep the assume-live set that small unless a provider genuinely
// has no cheap probe.
func TestHTTPProber_HuggingFaceIsProbed(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := httpProber{Timeout: time.Second}
	if !p.Live(context.Background(), AICredential{Provider: "huggingface", BaseURL: srv.URL + "/v1", AllowPrivateBaseURL: true}) {
		t.Fatal("huggingface answering 200 on /models must be live")
	}
	if gotPath != "/v1/models" {
		t.Fatalf("probe path = %q, want /v1/models (a real network probe must have happened)", gotPath)
	}

	// A dead router endpoint reports not-live rather than being assumed up.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	if p.Live(context.Background(), AICredential{Provider: "huggingface", BaseURL: dead.URL + "/v1", AllowPrivateBaseURL: true}) {
		t.Fatal("huggingface returning 500 on /models must NOT be live")
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

// TestHTTPProber_DefaultTimeout: a zero Timeout falls back to defaultProbeTimeout and the
// probe still succeeds against a reachable endpoint (exercises the to<=0 branch).
func TestHTTPProber_DefaultTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := httpProber{Timeout: 0} // ⇒ defaultProbeTimeout
	if !p.Live(context.Background(), AICredential{Provider: "vllm", BaseURL: srv.URL + "/v1", AllowPrivateBaseURL: true}) {
		t.Fatal("expected live with the default timeout")
	}
}
