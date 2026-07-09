package coding

import (
	"testing"

	"github.com/manyforge/manyforge/internal/agents"
)

// cmd/manyforge/main.go wires *agents.ProviderCatalogs into CodeReviewService.Pricing. That
// assignment only type-checks in main, which no unit test exercises — so pin the seam here.
// If CostEstimator's signature or ProviderCatalogs' method drifts, this fails at compile time
// instead of at startup, where a lane would silently price every run at 0.
var _ CostEstimator = (*agents.ProviderCatalogs)(nil)

func TestProviderCatalogsSatisfiesCostEstimator(t *testing.T) {
	// The compile-time assertion above is the test; this exists so the pin is discoverable
	// by name and shows up in `go test -v` output alongside the other seam pins.
	var e CostEstimator = agents.NewProviderCatalogs(nil)
	cents, err := e.CostCents(t.Context(), "anthropic", "claude-opus-4-8", 1_000_000, 1_000_000)
	if err != nil || cents != 0 {
		t.Fatalf("CostCents for a provider with no live catalog = (%d, %v), want (0, nil)", cents, err)
	}
}
