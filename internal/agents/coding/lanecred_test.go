package coding

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// TestResolveLaneCred pins per-dimension lane resolution (manyforge-azy): a blank provider
// inherits the review default (model overridden); a distinct provider resolves its own
// credential; a down primary falls back; a down primary with no fallback still returns the
// primary (retry path); an unresolvable primary with no fallback is skipped with a reason.
func TestResolveLaneCred(t *testing.T) {
	def := AICredential{Provider: "anthropic", BaseURL: "https://api.anthropic.com", Model: "def", MaxConcurrentLanes: 4}
	svc := &CodeReviewService{
		Creds: &FakeCredResolver{ByProvider: map[string]AICredential{
			"openrouter": {Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "or", APIKey: "k"},
			"vllm":       {Provider: "vllm", BaseURL: "http://192.168.2.241:1234/v1", Model: "orn", AllowPrivateBaseURL: true, APIKey: "k"},
		}},
		Prober: stubProbe{
			"https://api.anthropic.com":    true,
			"https://openrouter.ai/api/v1": false, // down
			"http://192.168.2.241:1234/v1": true,
		},
		// 192.168.2.241 is the vllm fixture's private-LAN host below — now that laneCredFor
		// requires allowlist membership for every provider (manyforge-9er Task 3), it must be
		// listed here too (its AllowPrivateBaseURL:true satisfies the private-host guard).
		EgressAllow: netsafe.ParseHostAllowlist("api.anthropic.com,openrouter.ai,192.168.2.241"),
	}
	ctx := context.Background()
	p, b := uuid.New(), uuid.New()

	// Blank provider ⇒ inherit the default, model overridden.
	if lc, _, reason := svc.resolveLaneCred(ctx, p, b, def, Dimension{Key: "x", Model: "m2"}); reason != "" || lc.Provider != "anthropic" || lc.Model != "m2" {
		t.Fatalf("blank provider: %+v reason=%q", lc, reason)
	}

	// Distinct provider, primary live ⇒ that provider's cred.
	if lc, _, reason := svc.resolveLaneCred(ctx, p, b, def, Dimension{Key: "x", Provider: "vllm", Model: "orn"}); reason != "" || lc.Provider != "vllm" {
		t.Fatalf("vllm primary live: %+v reason=%q", lc, reason)
	}

	// Primary down + NO fallback ⇒ still returns the primary (let the real call fail → retry).
	if lc, _, reason := svc.resolveLaneCred(ctx, p, b, def, Dimension{Key: "x", Provider: "openrouter", Model: "or"}); reason != "" || lc.Provider != "openrouter" {
		t.Fatalf("down no-fallback ⇒ primary: %+v reason=%q", lc, reason)
	}

	// Primary down ⇒ fallback provider chosen.
	if lc, _, reason := svc.resolveLaneCred(ctx, p, b, def, Dimension{Key: "x", Provider: "openrouter", Model: "or", FallbackChain: []FallbackEntry{{Provider: "vllm", Model: "orn"}}}); reason != "" || lc.Provider != "vllm" {
		t.Fatalf("down ⇒ fallback vllm: %+v reason=%q", lc, reason)
	}

	// Unresolvable primary (no credential) + no fallback ⇒ skipped with a reason.
	if lc, _, reason := svc.resolveLaneCred(ctx, p, b, def, Dimension{Key: "docs", Provider: "openai"}); reason == "" {
		t.Fatalf("unknown provider must skip with a reason, got %+v", lc)
	}
}
