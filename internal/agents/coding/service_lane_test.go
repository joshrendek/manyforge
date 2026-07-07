//go:build integration

package coding

import (
	"testing"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// TestReviewLaneRoutesLocalProviderThroughSandbox pins manyforge-9er Task 4: a dimension
// configured with a LOCAL provider (vllm) must run through the SAME opencode-sandbox path as a
// cloud provider — not the host-side localReview direct-API path (removed from reviewLane by
// this task; Task 6 deletes localReview itself). Before this task, isLocalProvider(laneCred.
// Provider) short-circuited reviewLane straight into localReview and the fake Sandbox runner
// was never invoked for a local cred — this test fails red against that code (runner.Last is
// the zero value) and passes green once reviewLane routes every provider through
// runSandboxLane.
func TestReviewLaneRoutesLocalProviderThroughSandbox(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	// One dimension routed to a local (vllm) reviewbot — no fallback configured.
	seedDimFull(ctx, t, tdb, seed.businessID, "security", "vllm", "local-model", "", "", 1)

	vllmCred := AICredential{Provider: "vllm", BaseURL: "http://127.0.0.1:19999", Model: "local-model"}
	resolver := &FakeCredResolver{
		Cred:       AICredential{APIKey: "k", Provider: "anthropic", BaseURL: "https://api.anthropic.com", Model: "def"},
		ByProvider: map[string]AICredential{"vllm": vllmCred},
	}

	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)

	doc := []byte(`{"summary":"ok","findings":[]}`)
	runner := &sandbox.FakeRunner{Result: sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{"review.json": doc}}}

	svc := buildService(t, tdb, env, runner, resolver)
	svc.EgressAllow = netsafe.ParseHostAllowlist("api.anthropic.com,127.0.0.1")
	// Deterministic liveness — avoids a real network probe against the fake local endpoint.
	svc.Prober = stubProbe{vllmCred.BaseURL: true}

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

	// The fake SANDBOX runner must have been invoked for the local cred (i.e. local no longer
	// takes the host-side localReview path).
	if runner.Last.Env == nil {
		t.Fatal("sandbox runner was never invoked — local cred took the host-side localReview path")
	}
	if got := runner.Last.Env["LLM_PROVIDER"]; got != "vllm" {
		t.Fatalf("spec.Env[LLM_PROVIDER] = %q, want %q", got, "vllm")
	}
	wantAllow := []string{vllmCred.Host()}
	if len(runner.Last.EgressAllow) != len(wantAllow) || runner.Last.EgressAllow[0] != wantAllow[0] {
		t.Fatalf("spec.EgressAllow = %v, want %v", runner.Last.EgressAllow, wantAllow)
	}
}
