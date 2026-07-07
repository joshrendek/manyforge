package coding

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// stubProbe reports liveness by base_url — a pure test double for reviewbotProber.
type stubProbe map[string]bool

func (s stubProbe) Live(_ context.Context, c AICredential) bool { return s[c.BaseURL] }

func resolverFor(m map[uuid.UUID]AICredential) resolveFn {
	return func(_ context.Context, id uuid.UUID) (AICredential, error) {
		c, ok := m[id]
		if !ok {
			return AICredential{}, errs.ErrNotFound
		}
		return c, nil
	}
}

func TestChooseReviewbot(t *testing.T) {
	a1, a2 := uuid.New(), uuid.New()
	creds := map[uuid.UUID]AICredential{
		a1: {Provider: "vllm", BaseURL: "http://lan/v1"},
		a2: {Provider: "openrouter", BaseURL: "http://cloud/v1"},
	}
	chain := []uuid.UUID{a1, a2}

	// Primary live ⇒ primary chosen.
	got, err := chooseReviewbot(context.Background(), chain, resolverFor(creds), stubProbe{"http://lan/v1": true, "http://cloud/v1": true})
	if err != nil || got.BaseURL != "http://lan/v1" {
		t.Fatalf("primary-live: got %q err=%v, want lan", got.BaseURL, err)
	}

	// Primary dead ⇒ secondary chosen.
	got, err = chooseReviewbot(context.Background(), chain, resolverFor(creds), stubProbe{"http://cloud/v1": true})
	if err != nil || got.BaseURL != "http://cloud/v1" {
		t.Fatalf("primary-dead: got %q err=%v, want cloud", got.BaseURL, err)
	}

	// All dead but resolvable ⇒ last resolvable (let the real call fail → retry).
	got, err = chooseReviewbot(context.Background(), chain, resolverFor(creds), stubProbe{})
	if err != nil || got.BaseURL != "http://cloud/v1" {
		t.Fatalf("all-dead: got %q err=%v, want last resolvable (cloud)", got.BaseURL, err)
	}

	// A stale entry is skipped; the next resolvable+live one wins.
	got, err = chooseReviewbot(context.Background(), []uuid.UUID{uuid.New(), a2}, resolverFor(creds), stubProbe{"http://cloud/v1": true})
	if err != nil || got.BaseURL != "http://cloud/v1" {
		t.Fatalf("stale-then-live: got %q err=%v, want cloud", got.BaseURL, err)
	}

	// Nothing resolves ⇒ terminal validation error.
	if _, err := chooseReviewbot(context.Background(), []uuid.UUID{uuid.New()}, resolverFor(creds), stubProbe{}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("all-stale: want ErrValidation, got %v", err)
	}
}

// TestResolveReviewChain_DBErrorDegradesToNil pins the intentional degradation: a DB
// failure loading review_config must NOT brick reviews — resolveReviewChain logs and
// returns nil so runJob falls back to the single enqueued agent (no chain, no error).
func TestResolveReviewChain_DBErrorDegradesToNil(t *testing.T) {
	s := &CodeReviewService{DB: fakeServiceDB{}} // WithPrincipal returns errFakeDB without running fn
	if got := s.resolveReviewChain(context.Background(), uuid.New(), uuid.New()); got != nil {
		t.Fatalf("a DB error must degrade to a nil chain, got %v", got)
	}
}

// TestPrivateBaseURLBlocked pins privateBaseURLBlocked's classification: it is deliberately
// narrower than localBaseURLBlocked (the direct-POST SSRF guard) — a DNS hostname or a public
// IP always passes here (the egress allowlist is what governs those), while an IP-literal
// private/ULA host requires the AllowPrivateBaseURL opt-in, loopback always passes, and
// cloud-metadata/link-local stay blocked even with the opt-in.
func TestPrivateBaseURLBlocked(t *testing.T) {
	cases := []struct {
		name        string
		host        string
		allowPriv   bool
		wantBlocked bool
	}{
		{"dns hostname, no opt-in", "api.anthropic.com", false, false},
		{"dns hostname, opt-in", "api.anthropic.com", true, false},
		{"public ip", "8.8.8.8", false, false},
		{"private ip, opt-in", "192.168.2.241", true, false},
		{"private ip, no opt-in", "192.168.2.241", false, true},
		{"rfc1918 10/8, no opt-in", "10.0.0.5", false, true},
		{"loopback", "127.0.0.1", false, false},
		{"metadata, opt-in still blocked", "169.254.169.254", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := privateBaseURLBlocked(c.host, c.allowPriv); got != c.wantBlocked {
				t.Errorf("privateBaseURLBlocked(%q, allowPrivate=%v) = %v, want %v", c.host, c.allowPriv, got, c.wantBlocked)
			}
		})
	}
}

// TestLaneCredForEgressAndPrivateBaseURL pins laneCredFor's two-part sandbox lane guard
// (manyforge-9er Task 3): EVERY provider's host must be in the sandbox egress allowlist —
// local providers are no longer exempt now that Task 4 routes them through the sandbox too —
// and an IP-literal private host additionally requires the credential's AllowPrivateBaseURL
// opt-in. The cloud/anthropic case is a regression guard: an earlier draft of this gate
// reused localBaseURLBlocked (the INVERTED direct-POST guard, which blocks every public/DNS
// host) and would have wrongly rejected every cloud provider — this proves it still resolves.
func TestLaneCredForEgressAndPrivateBaseURL(t *testing.T) {
	def := AICredential{Provider: "anthropic", BaseURL: "https://api.anthropic.com", Model: "def"}
	ctx := context.Background()
	p, b := uuid.New(), uuid.New()

	t.Run("cloud provider without opt-in still resolves (regression guard)", func(t *testing.T) {
		svc := &CodeReviewService{
			Creds:       &FakeCredResolver{ByProvider: map[string]AICredential{}},
			EgressAllow: netsafe.ParseHostAllowlist("api.anthropic.com"),
		}
		lc, err := svc.laneCredFor(ctx, p, b, def, "", "")
		if err != nil {
			t.Fatalf("cloud cred with AllowPrivateBaseURL=false must resolve: %v", err)
		}
		if lc.Provider != "anthropic" || lc.Host() != "api.anthropic.com" {
			t.Fatalf("unexpected resolved cred: %+v", lc)
		}
	})

	t.Run("local private host with opt-in and allowlist membership resolves", func(t *testing.T) {
		svc := &CodeReviewService{
			Creds: &FakeCredResolver{ByProvider: map[string]AICredential{
				"vllm": {Provider: "vllm", BaseURL: "http://192.168.2.241:1234/v1", Model: "orn", AllowPrivateBaseURL: true, APIKey: "k"},
			}},
			EgressAllow: netsafe.ParseHostAllowlist("192.168.2.241"),
		}
		lc, err := svc.laneCredFor(ctx, p, b, def, "vllm", "orn")
		if err != nil {
			t.Fatalf("local cred with opt-in + allowlist membership must resolve: %v", err)
		}
		if lc.Provider != "vllm" {
			t.Fatalf("unexpected resolved cred: %+v", lc)
		}
	})

	t.Run("local private host without opt-in is rejected", func(t *testing.T) {
		svc := &CodeReviewService{
			Creds: &FakeCredResolver{ByProvider: map[string]AICredential{
				"vllm": {Provider: "vllm", BaseURL: "http://192.168.2.241:1234/v1", Model: "orn", AllowPrivateBaseURL: false, APIKey: "k"},
			}},
			EgressAllow: netsafe.ParseHostAllowlist("192.168.2.241"),
		}
		_, err := svc.laneCredFor(ctx, p, b, def, "vllm", "orn")
		if err == nil || !strings.Contains(err.Error(), "allow_private_base_url") {
			t.Fatalf("local cred without opt-in: want error mentioning allow_private_base_url, got %v", err)
		}
	})

	t.Run("local host not in allowlist is rejected regardless of opt-in", func(t *testing.T) {
		svc := &CodeReviewService{
			Creds: &FakeCredResolver{ByProvider: map[string]AICredential{
				"vllm": {Provider: "vllm", BaseURL: "http://192.168.2.241:1234/v1", Model: "orn", AllowPrivateBaseURL: true, APIKey: "k"},
			}},
			EgressAllow: netsafe.ParseHostAllowlist("api.anthropic.com"), // 192.168.2.241 NOT included
		}
		_, err := svc.laneCredFor(ctx, p, b, def, "vllm", "orn")
		if err == nil || !strings.Contains(err.Error(), "egress allowlist") {
			t.Fatalf("host not in allowlist: want error mentioning egress allowlist, got %v", err)
		}
	})
}
