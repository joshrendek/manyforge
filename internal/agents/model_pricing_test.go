package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

func TestModelRowToAIModel(t *testing.T) {
	row := dbgen.ListModelPricingRow{
		ModelID: "claude-sonnet-4-5", Provider: "anthropic", ContextWindow: 200000,
		InputCentsPerMtok: 300, OutputCentsPerMtok: 1500, SupportsTools: true,
	}
	got := modelRowToAIModel(row)
	want := ai.Model{
		ID: "claude-sonnet-4-5", Provider: "anthropic", ContextWindow: 200000,
		InputCentsPerMTok: 300, OutputCentsPerMTok: 1500, SupportsTools: true,
	}
	if got != want {
		t.Fatalf("modelRowToAIModel = %+v, want %+v", got, want)
	}
	if c := got.CostCents(ai.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); c != 1800 {
		t.Fatalf("CostCents = %d, want 1800", c)
	}
}

func TestRegistryLen(t *testing.T) {
	reg := ai.NewRegistry()
	if got := reg.Len(); got != 0 {
		t.Fatalf("Len of empty registry = %d, want 0", got)
	}
	reg.Register(ai.Model{ID: "claude-sonnet-4-5", Provider: "anthropic"})
	if got := reg.Len(); got != 1 {
		t.Fatalf("Len after one Register = %d, want 1", got)
	}
}

// stubModelPricingDB lets us exercise LoadModelRegistry's error path without a real
// pgx.Tx: WithTx just returns the canned error (it never invokes the callback).
type stubModelPricingDB struct {
	withTxErr error
}

func (s stubModelPricingDB) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.withTxErr
}

func TestLoadModelRegistryWrapsWithTxError(t *testing.T) {
	sentinel := errors.New("boom: connection refused")
	_, err := LoadModelRegistry(context.Background(), stubModelPricingDB{withTxErr: sentinel})
	if err == nil {
		t.Fatal("LoadModelRegistry = nil error, want wrapped WithTx error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("LoadModelRegistry error = %v, want it to wrap %v", err, sentinel)
	}
}

// The empty-catalog branch: WithTx succeeds but the catalog yields zero models (an
// unseeded deploy). The loader must fail loudly, not silently return an empty registry.
// The zero-value stub returns nil from WithTx without registering any row, modelling
// exactly that state (reg.Len()==0).
func TestLoadModelRegistryEmptyCatalogFailsLoudly(t *testing.T) {
	_, err := LoadModelRegistry(context.Background(), stubModelPricingDB{})
	if err == nil {
		t.Fatal("LoadModelRegistry with empty catalog = nil error, want a fail-loud error")
	}
	if !strings.Contains(err.Error(), "empty or unseeded") {
		t.Fatalf("error = %v, want it to name the empty/unseeded catalog", err)
	}
}

func TestRegistryCostFn_UnknownModelIsFree(t *testing.T) {
	reg := ai.NewRegistry()
	ai.RegisterDefaults(reg) // anthropic + openai only — no self-host models
	cost := NewRegistryCostFn(reg, nil)

	// A self-hosted model absent from the catalog costs 0 — never an error/panic.
	if c := cost("llama3.1:70b", ai.Usage{InputTokens: 1000, OutputTokens: 1000}); c != 0 {
		t.Fatalf("unknown model cost = %d, want 0", c)
	}
	// Sanity: a known model still prices > 0 (the fn isn't always-zero).
	if c := cost("gpt-4o", ai.Usage{InputTokens: 1_000_000, OutputTokens: 0}); c <= 0 {
		t.Fatalf("known model cost = %d, want > 0", c)
	}
}
