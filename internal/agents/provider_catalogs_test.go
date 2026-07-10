package agents

import (
	"context"
	"errors"
	"testing"
)

// fakeCatalog records the provider it was dispatched for.
type fakeCatalog struct {
	models []ModelInfo
	cents  int64
	err    error
	calls  int
}

func (f *fakeCatalog) ProviderModels(_ context.Context, _ string) ([]ModelInfo, error) {
	f.calls++
	return f.models, f.err
}

func (f *fakeCatalog) CostCents(_ context.Context, _, _ string, _, _ int64) (int64, error) {
	f.calls++
	return f.cents, f.err
}

func TestProviderCatalogs_DispatchesByProvider(t *testing.T) {
	or := &fakeCatalog{models: []ModelInfo{{Provider: "openrouter", ModelID: "a/b"}}, cents: 7}
	hf := &fakeCatalog{models: []ModelInfo{{Provider: "huggingface", ModelID: "c/d:groq"}}, cents: 11}
	pc := &ProviderCatalogs{byProvider: map[string]providerCatalog{"openrouter": or, "huggingface": hf}}
	ctx := context.Background()

	got, err := pc.ProviderModels(ctx, "huggingface")
	if err != nil || len(got) != 1 || got[0].ModelID != "c/d:groq" {
		t.Fatalf("ProviderModels(huggingface) = (%v, %v)", got, err)
	}
	if or.calls != 0 {
		t.Error("openrouter catalog must not be consulted for a huggingface lookup")
	}

	c, err := pc.CostCents(ctx, "openrouter", "a/b", 1, 1)
	if err != nil || c != 7 {
		t.Fatalf("CostCents(openrouter) = (%d, %v), want (7, nil)", c, err)
	}
}

// Providers with no live catalog must degrade quietly: an empty model list (the form falls
// back to free-text) and zero cost (the caller falls back to the static pricing catalog).
// Returning an error here would fail a review over a cosmetic lookup.
func TestProviderCatalogs_UnregisteredProviderDegrades(t *testing.T) {
	pc := NewProviderCatalogs(nil) // no HTTP client needed: nothing should dial
	ctx := context.Background()
	for _, provider := range []string{"anthropic", "openai", "ollama", "vllm", "nope"} {
		models, err := pc.ProviderModels(ctx, provider)
		if err != nil || len(models) != 0 {
			t.Errorf("ProviderModels(%q) = (%v, %v), want (empty, nil)", provider, models, err)
		}
		c, err := pc.CostCents(ctx, provider, "m", 1_000_000, 1_000_000)
		if err != nil || c != 0 {
			t.Errorf("CostCents(%q) = (%d, %v), want (0, nil)", provider, c, err)
		}
	}
}

// A catalog fetch error must propagate so the handler can log it, not be swallowed into an
// empty list that looks like "this provider has no models".
func TestProviderCatalogs_PropagatesCatalogError(t *testing.T) {
	boom := errors.New("upstream 502")
	pc := &ProviderCatalogs{byProvider: map[string]providerCatalog{"huggingface": &fakeCatalog{err: boom}}}
	if _, err := pc.ProviderModels(context.Background(), "huggingface"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

// cmd/manyforge/main.go passes *ProviderCatalogs to two seams. This pins the one declared in
// this package; the other (coding.CostEstimator) is pinned from there, since coding imports
// agents and not the reverse.
func TestProviderCatalogs_SatisfiesModelListerSeam(t *testing.T) {
	var _ providerModelLister = (*ProviderCatalogs)(nil)
	pc := NewProviderCatalogs(nil)
	if _, ok := pc.byProvider["openrouter"]; !ok {
		t.Error("openrouter catalog not registered")
	}
	if _, ok := pc.byProvider["huggingface"]; !ok {
		t.Error("huggingface catalog not registered")
	}
}
