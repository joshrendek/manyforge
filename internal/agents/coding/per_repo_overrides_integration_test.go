//go:build integration

package coding

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPerRepoOverrideDisablesDimension pins that a per-repo override (manyforge-e54.2) actually
// changes the panel a review runs: disabling "correctness" for this repo skips that lane while
// "security" still runs.
func TestPerRepoOverrideDisablesDimension(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)
	seedReviewDimension(ctx, t, tdb, seed.businessID, "security", "DIMPROMPT:security", "info", nil, 1)
	seedReviewDimension(ctx, t, tdb, seed.businessID, "correctness", "DIMPROMPT:correctness", "info", nil, 2)

	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)

	// Per-repo override: disable "correctness" for THIS connector only.
	cfgSvc := &ReviewDimensionService{DB: tdb.App}
	if _, err := cfgSvc.UpsertRepoOverride(ctx, seed.principalID, connID, RepoDimensionOverrideInput{
		DimensionKey: "correctness", Enabled: false,
	}); err != nil {
		t.Fatalf("UpsertRepoOverride: %v", err)
	}
	// Sanity: the override reads back.
	ovs, err := cfgSvc.ListRepoOverrides(ctx, seed.principalID, connID)
	if err != nil || len(ovs) != 1 || ovs[0].DimensionKey != "correctness" || ovs[0].Enabled {
		t.Fatalf("ListRepoOverrides = %+v, err %v; want one disabled 'correctness'", ovs, err)
	}

	svc := buildService(t, tdb, env, &perDimRunner{}, fakeCred)
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
	got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	var runs []struct {
		Dimension string `json:"dimension"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(got.DimensionRuns, &runs); err != nil {
		t.Fatalf("unmarshal DimensionRuns: %v", err)
	}
	byDim := map[string]string{}
	for _, r := range runs {
		byDim[r.Dimension] = r.Status
	}
	if byDim["security"] != "succeeded" {
		t.Fatalf("security should still run; dimension_runs=%v", byDim)
	}
	if byDim["correctness"] != "skipped" {
		t.Fatalf("correctness must be skipped by the per-repo override; dimension_runs=%v", byDim)
	}
	for _, f := range got.Findings {
		if strings.Contains(f.Title, "correctness") {
			t.Errorf("a disabled dimension must produce no findings; got %+v", f)
		}
	}
}
