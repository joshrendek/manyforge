//go:build integration

package coding

import (
	"testing"
)

// TestEstimateConfigUsesRecentReviewHistory runs a real review to seed dimension_runs, then checks
// the cost estimate reflects that history: lane count = the enabled dimensions, priced by the
// observed per-lane cost (manyforge-8qs.3).
func TestEstimateConfigUsesRecentReviewHistory(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	seedReviewDimension(ctx, t, tdb, seed.businessID, "security", "DIMPROMPT:security", "info", nil, 1)
	seedReviewDimension(ctx, t, tdb, seed.businessID, "correctness", "DIMPROMPT:correctness", "info", nil, 2)

	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)
	svc := buildService(t, tdb, env, &perDimRunner{}, fakeCred)

	// Before any review: no history ⇒ fallback per-lane cost, based_on_reviews = 0.
	cfgSvc := &ReviewDimensionService{DB: tdb.App}
	pre, err := cfgSvc.EstimateConfig(ctx, seed.principalID, seed.businessID)
	if err != nil {
		t.Fatalf("EstimateConfig(pre): %v", err)
	}
	if pre.BasedOnReviews != 0 || pre.PerLaneCostMicroCents != fallbackPerLaneMicroCents {
		t.Fatalf("with no history: want fallback per-lane + based_on_reviews 0; got %+v", pre)
	}
	if pre.LaneCount != 2 {
		t.Fatalf("lane_count = %d, want 2 enabled dims", pre.LaneCount)
	}

	// Run one review to seed dimension_runs (each perDimRunner lane bills 0.02 USD = 2¢ = 2e6 µ¢).
	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed := ClaimedReview{
		ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
		AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
	}
	if err := svc.runJob(ctx, claimed, nil); err != nil {
		t.Fatalf("runJob: %v", err)
	}

	post, err := cfgSvc.EstimateConfig(ctx, seed.principalID, seed.businessID)
	if err != nil {
		t.Fatalf("EstimateConfig(post): %v", err)
	}
	if post.BasedOnReviews != 1 {
		t.Fatalf("based_on_reviews = %d, want 1 (seeded by the review we ran)", post.BasedOnReviews)
	}
	if post.PerLaneCostMicroCents != 2_000_000 {
		t.Fatalf("per-lane cost = %d, want observed 2_000_000 (0.02 USD/lane)", post.PerLaneCostMicroCents)
	}
	if post.LaneCount != 2 || post.EstCostMicroCents != post.PerLaneCostMicroCents*2 {
		t.Fatalf("est must be per-lane × 2 lanes; got %+v", post)
	}
	if post.EstCostCents != 4 {
		t.Fatalf("est_cost_cents = %d, want 4 (2 lanes × 2¢)", post.EstCostCents)
	}
}
