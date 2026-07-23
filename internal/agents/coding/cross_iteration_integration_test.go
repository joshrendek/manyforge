//go:build integration

package coding

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
)

// iterRunner emits a DIFFERENT finding on each successive review so a re-review of the same PR
// exercises NEW/RESOLVED classification: review 1 → "alpha", review 2 → "beta".
type iterRunner struct{ calls atomic.Int64 }

func (r *iterRunner) Run(_ context.Context, _ sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	title := "alpha"
	if r.calls.Add(1) > 1 {
		title = "beta"
	}
	doc, _ := json.Marshal(map[string]any{"summary": "s", "findings": []map[string]any{
		{"file": "main.go", "line": 1, "severity": "error", "title": title, "detail": "d"},
	}})
	usage := `[{"cost":0.02,"input":1000,"output":50,"reasoning":10,"cache_read":40000,"cache_write":0}]`
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{
		"review.json": doc, "usage.json": []byte(usage),
	}}, nil
}

// TestCrossIterationDeltaAcrossReviews runs two reviews on the same PR and asserts the summary
// gains a NEW/CARRYOVER/RESOLVED delta on the second (manyforge-e54.1): the first review has no
// prior history (no delta); the second, whose finding changed, reports 1 new + 1 resolved.
func TestCrossIterationDeltaAcrossReviews(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"sha1","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, stub := startGitHubStub(t, prJSON)
	seedReviewDimension(ctx, t, tdb, seed.businessID, "security", "DIMPROMPT:security", "info", nil, 1)

	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)
	svc := buildService(t, tdb, env, &iterRunner{}, fakeCred)

	runOnce := func() CodeReview {
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
		return got
	}

	// Review 1: no prior history ⇒ no cross-iteration delta line.
	first := runOnce()
	if strings.Contains(first.Summary, "Since the last review") {
		t.Fatalf("first review must not show a cross-iteration delta; summary=%q", first.Summary)
	}

	// Model a new commit on the PR (a new head SHA) so the second review isn't skipped as an
	// already-reviewed head (reviewedHead dedup); the finding also changes (alpha → beta).
	stub.prJSON = []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"sha2","ref":"f"},"base":{"ref":"main"}}`)

	// Review 2: 'alpha' is gone, 'beta' is new ⇒ 1 new · 0 carried over · 1 resolved.
	second := runOnce()
	if !strings.Contains(second.Summary, "Since the last review") {
		t.Fatalf("second review must show a cross-iteration delta; summary=%q", second.Summary)
	}
	for _, want := range []string{"1 new", "0 carried over", "1 resolved"} {
		if !strings.Contains(second.Summary, want) {
			t.Errorf("delta must report %q; summary=%q", want, second.Summary)
		}
	}
}
