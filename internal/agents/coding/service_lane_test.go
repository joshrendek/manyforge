//go:build integration

package coding

import (
	"context"
	"encoding/json"
	"errors"
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

// failoverRunner is a fake sandbox runner that FAILS every lane run for a configured provider
// (identified via spec.Env["LLM_PROVIDER"], the same key sandboxEnv sets from the lane's
// credential) and SUCCEEDS with a valid empty review.json for every other provider. It lets a
// test deterministically exercise a runtime local→cloud fallback: the local lane's own sandbox
// run genuinely fails, and the fallback re-run on a different provider genuinely succeeds.
type failoverRunner struct {
	failProvider string
	Last         sandbox.SandboxSpec
}

func (r *failoverRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	r.Last = spec
	if spec.Env["LLM_PROVIDER"] == r.failProvider {
		return sandbox.SandboxResult{ExitCode: 1, Stderr: []byte("local backend unreachable")},
			errors.New("sandbox: local backend unreachable")
	}
	doc, _ := json.Marshal(map[string]any{"summary": "ok", "findings": []map[string]any{}})
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{"review.json": doc}}, nil
}

// TestReviewLaneFallsBackToCloudOnLocalFailure pins manyforge-9er Task 5: when a dimension's
// chosen lane (its LIVE primary, per resolveLaneCred) fails the actual sandbox run, and that
// lane was NOT already the dimension's configured cloud fallback, reviewLane re-runs the lane
// once on the dimension's (FallbackProvider, FallbackModel) before giving up. This is runtime
// fallback (a real run failure), distinct from resolveLaneCred's config-time liveness-probe
// fallback (manyforge-azy) exercised by TestCodeReviewPerDimensionProviderAndFallback.
//
// Before Task 5, reviewLane is a bare `return runSandboxLane(dim, laneCreds[dim.Key], laneOutDir)`
// with no retry on failure — this test fails red against that code (the "security" dimension_run
// is persisted as provider="vllm", status="failed") and passes green once reviewLane adds the
// runtime fallback.
func TestReviewLaneFallsBackToCloudOnLocalFailure(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	// One dimension primarily routed to a local (vllm) reviewbot, with a cloud fallback
	// configured (openrouter). The vllm endpoint is LIVE (passes the config-time liveness
	// probe, so it IS the chosen lane) but its actual sandbox run fails.
	seedDimFull(ctx, t, tdb, seed.businessID, "security", "vllm", "local-model", "openrouter", "deepseek/deepseek-chat-v3.1", 1)

	vllmCred := AICredential{Provider: "vllm", BaseURL: "http://127.0.0.1:19999", Model: "local-model"}
	openrouterCred := AICredential{APIKey: "ork", Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "deepseek/deepseek-chat-v3.1"}
	resolver := &FakeCredResolver{
		Cred: AICredential{APIKey: "k", Provider: "anthropic", BaseURL: "https://api.anthropic.com", Model: "def"},
		ByProvider: map[string]AICredential{
			"vllm":       vllmCred,
			"openrouter": openrouterCred,
		},
	}

	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)

	runner := &failoverRunner{failProvider: "vllm"}

	svc := buildService(t, tdb, env, runner, resolver)
	svc.EgressAllow = netsafe.ParseHostAllowlist("api.anthropic.com,127.0.0.1,openrouter.ai")
	// The vllm primary is live (so resolveLaneCred picks it as the CHOSEN lane, not the
	// config-time fallback) — its sandbox run then fails for real, triggering Task 5's
	// runtime fallback.
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

	got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var runs []struct {
		Dimension string `json:"dimension"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(got.DimensionRuns, &runs); err != nil {
		t.Fatalf("unmarshal dimension_runs %q: %v", got.DimensionRuns, err)
	}
	if len(runs) != 1 {
		t.Fatalf("want exactly 1 dimension run, got %d: %+v", len(runs), runs)
	}
	run := runs[0]
	if run.Status != "succeeded" {
		t.Fatalf("dimension run status = %q, want %q (runtime fallback to cloud should have recovered the lane): %+v", run.Status, "succeeded", run)
	}
	if run.Provider != "openrouter" {
		t.Fatalf("dimension run provider = %q, want %q (must have fallen back off the failed local lane)", run.Provider, "openrouter")
	}
	if run.Model != "deepseek/deepseek-chat-v3.1" {
		t.Fatalf("dimension run model = %q, want %q", run.Model, "deepseek/deepseek-chat-v3.1")
	}

	// The FAILED local attempt must have actually been tried (proves this is a genuine
	// runtime fallback after a real failure, not a config-time reroute that skipped vllm
	// entirely).
	if runner.Last.Env["LLM_PROVIDER"] != "openrouter" {
		t.Fatalf("last sandbox invocation provider = %q, want %q (fallback must run last)", runner.Last.Env["LLM_PROVIDER"], "openrouter")
	}
}
