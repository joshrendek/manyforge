// Finding: US8 / Spec 003 §3.5 — the per-credential allow_private_base_url trust flag
// loosens netsafe for loopback + RFC1918 ONLY. Cloud-metadata IPs stay blocked even
// under full trust, and a trusted credential reaches a loopback base_url (the hatch
// works). See manyforge-deo.9.
package security_regression

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// Metadata IPs must never be reachable, even when a credential is fully trusted.
func TestUS8_MetadataBlockedUnderTrust(t *testing.T) {
	full := netsafe.Options{AllowLoopback: true, AllowPrivate: true}
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254"} {
		if !netsafe.IsBlocked(net.ParseIP(ip), full) {
			t.Fatalf("metadata %s must stay blocked under AllowLoopback+AllowPrivate", ip)
		}
	}
	// Behavioral: a trusted credential pointed at the metadata endpoint is still refused.
	p, err := ai.New(ai.Credential{
		Provider: "ollama", BaseURL: "http://169.254.169.254/v1", Model: "m", AllowPrivateBaseURL: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.Complete(ctx, ai.Request{MaxTokens: 8, Messages: []ai.Message{{Role: ai.RoleUser, Text: "x"}}}); !errors.Is(err, ai.ErrProviderUnavailable) {
		t.Fatalf("trusted metadata base_url -> %v, want ErrProviderUnavailable (refused)", err)
	}
}

// A trusted credential reaches a loopback base_url — the self-host escape hatch works.
func TestUS8_TrustedCredentialReachesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()
	p, err := ai.New(ai.Credential{Provider: "ollama", BaseURL: srv.URL + "/v1", Model: "m", AllowPrivateBaseURL: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.Complete(context.Background(), ai.Request{MaxTokens: 8, Messages: []ai.Message{{Role: ai.RoleUser, Text: "x"}}})
	if err != nil {
		t.Fatalf("trusted loopback Complete: %v", err)
	}
	if resp.Text != "ok" {
		t.Fatalf("resp.Text = %q, want ok", resp.Text)
	}
}
