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

// TestHTTPProber_HuggingFaceAssumedLive: a ZeroGPU Space sleeps after inactivity and takes
// 30-60s to cold-start, which no probe budget can accommodate (defaultProbeTimeout is 3s).
// Probing it would therefore skip the lane on a perfectly healthy-but-idle Space, every time.
// It must short-circuit to live without any network call — the reactive worker retry and the
// dimension's fallback_chain cover a Space that is genuinely down. See manyforge-bhx.
func TestHTTPProber_HuggingFaceAssumedLive(t *testing.T) {
	p := httpProber{Timeout: time.Millisecond}
	for _, provider := range []string{"huggingface", "HuggingFace"} { // case-insensitive, like anthropic
		if !p.Live(context.Background(), AICredential{Provider: provider, BaseURL: "https://josh-reviewbot.hf.space/v1"}) {
			t.Fatalf("%q must be assumed live", provider)
		}
	}
}

// TestHTTPProber_HuggingFaceLiveWhenColdStarting is the regression this fix exists for: an
// endpoint that takes longer than the probe timeout to answer (a waking Space) must still be
// treated as live. A constrained-but-probeable provider like vllm correctly reports not-live
// in exactly the same situation — that contrast is the point.
func TestHTTPProber_HuggingFaceLiveWhenColdStarting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // stands in for a 30-60s cold start
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := httpProber{Timeout: 5 * time.Millisecond}

	cold := AICredential{Provider: "huggingface", BaseURL: srv.URL + "/v1", AllowPrivateBaseURL: true}
	if !p.Live(context.Background(), cold) {
		t.Fatal("a cold-starting ZeroGPU Space must still be considered live (lane would be skipped forever)")
	}
	// Same slow endpoint, a provider that IS probed: times out, reports not-live.
	probed := AICredential{Provider: "vllm", BaseURL: srv.URL + "/v1", AllowPrivateBaseURL: true}
	if p.Live(context.Background(), probed) {
		t.Fatal("vllm is probed, so a timeout must report not-live — otherwise this test proves nothing")
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
