//go:build integration

package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// TestReviewLaneFallsBackToCloudOnLocalFailure pins manyforge-9er Task 5 (as generalized by
// manyforge-7lx): when a dimension's chosen lane (its LIVE primary, per resolveLaneCred) fails
// the actual sandbox run, reviewLane walks the untried fallback tail (laneRest) in order,
// re-running the lane on each entry until one succeeds. This test's dimension has a single
// fallback, so the tail is one entry (the configured cloud fallback). This is runtime fallback
// (a real run failure), distinct from resolveLaneCred's config-time liveness-probe fallback
// (manyforge-azy) exercised by TestCodeReviewPerDimensionProviderAndFallback.
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

// callCountingRunner is a fake sandbox runner that fails every lane run for a configured set of
// providers (identified via spec.Env["LLM_PROVIDER"], the same key sandboxEnv sets from the
// lane's credential) and records the provider of EVERY invocation in order. Unlike failoverRunner
// (which only tracks the LAST spec), this lets a test assert exactly how many times — and on
// which providers — a lane was actually run, distinguishing a genuine single/double run from one
// masked by a short-circuit.
type callCountingRunner struct {
	failProviders map[string]bool // providers whose lane run fails
	calls         []string        // LLM_PROVIDER of each Run() call, in invocation order
}

func (r *callCountingRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	provider := spec.Env["LLM_PROVIDER"]
	r.calls = append(r.calls, provider)
	if r.failProviders[provider] {
		return sandbox.SandboxResult{ExitCode: 1, Stderr: []byte(provider + " backend unreachable")},
			fmt.Errorf("sandbox: %s backend unreachable", provider)
	}
	doc, _ := json.Marshal(map[string]any{"summary": "ok", "findings": []map[string]any{}})
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{"review.json": doc}}, nil
}

// TestReviewLaneNoDoubleRunWhenChosenIsAlreadyFallback pins manyforge-9er (review MINOR 2): when
// a dimension's chosen lane's provider IS ALREADY its own configured fallback provider (a no-op
// fallback config), reviewLane's per-entry dedup guard — `strings.EqualFold(fb.Provider,
// chosen.Provider) && fb.Model == chosen.Model`, checked against each entry in laneRest (the
// not-yet-tried tail of the fallback chain, manyforge-7lx T3) — must skip that entry's
// runtime-fallback re-run entirely. Re-running the exact same (down) provider+model a second time
// burns another full sandbox invocation for no chance of a different outcome. This test fails red
// if that guard is removed/broken (the fake runner would be invoked twice) and passes green with
// exactly one invocation.
func TestReviewLaneNoDoubleRunWhenChosenIsAlreadyFallback(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	// The dimension's fallback provider is the SAME as its primary — a no-op fallback config.
	seedDimFull(ctx, t, tdb, seed.businessID, "security", "vllm", "local-model", "vllm", "local-model", 1)

	vllmCred := AICredential{Provider: "vllm", BaseURL: "http://127.0.0.1:19999", Model: "local-model"}
	resolver := &FakeCredResolver{
		Cred:       AICredential{APIKey: "k", Provider: "anthropic", BaseURL: "https://api.anthropic.com", Model: "def"},
		ByProvider: map[string]AICredential{"vllm": vllmCred},
	}

	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)

	runner := &callCountingRunner{failProviders: map[string]bool{"vllm": true}}

	svc := buildService(t, tdb, env, runner, resolver)
	svc.EgressAllow = netsafe.ParseHostAllowlist("api.anthropic.com,127.0.0.1")
	svc.Prober = stubProbe{vllmCred.BaseURL: true}

	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed := ClaimedReview{
		ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
		AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
	}
	// The only dimension's only lane fails and has nowhere else to go (fallback == chosen
	// provider), so the whole review fails — that is the expected outcome here, not a test bug.
	if err := svc.runJob(ctx, claimed, nil); err == nil {
		t.Fatal("runJob: want error (the lane's only provider failed), got nil")
	}

	if len(runner.calls) != 1 {
		t.Fatalf("sandbox runner invoked %d times, want exactly 1 (no double-run when chosen == fallback provider): calls=%v", len(runner.calls), runner.calls)
	}
	if runner.calls[0] != "vllm" {
		t.Fatalf("sandbox runner's only call was for provider %q, want %q", runner.calls[0], "vllm")
	}
}

// TestReviewLaneBothChosenAndFallbackFailReturnsOriginalError pins manyforge-9er (review MINOR
// 2): when the chosen (local) lane fails AND the runtime fallback re-run ALSO fails, reviewLane
// must surface the ORIGINAL local failure — not a masked/empty result, and not the fallback's own
// error. An operator debugging a failed review needs to see what actually broke FIRST (the local
// endpoint), not a confusing cloud-provider error that obscures the real root cause.
func TestReviewLaneBothChosenAndFallbackFailReturnsOriginalError(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

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

	runner := &callCountingRunner{failProviders: map[string]bool{"vllm": true, "openrouter": true}}

	svc := buildService(t, tdb, env, runner, resolver)
	svc.EgressAllow = netsafe.ParseHostAllowlist("api.anthropic.com,127.0.0.1,openrouter.ai")
	svc.Prober = stubProbe{vllmCred.BaseURL: true}

	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed := ClaimedReview{
		ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
		AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
	}
	runErr := svc.runJob(ctx, claimed, nil)
	if runErr == nil {
		t.Fatal("runJob: want error (both the chosen and fallback lanes failed), got nil")
	}
	// The surfaced error must reference the ORIGINAL local (vllm) failure, not the fallback's —
	// a masked/generic error here would hide which endpoint actually broke first.
	if !strings.Contains(runErr.Error(), "vllm") {
		t.Fatalf("runJob error = %q, want it to reference the ORIGINAL local (vllm) failure", runErr.Error())
	}
	if strings.Contains(runErr.Error(), "openrouter") {
		t.Fatalf("runJob error = %q, must not be masked by the fallback's own failure", runErr.Error())
	}

	// Both the chosen lane AND the fallback must have genuinely been attempted (a masked/
	// short-circuited result would show only 1 call, or the wrong providers/order).
	if len(runner.calls) != 2 {
		t.Fatalf("sandbox runner invoked %d times, want exactly 2 (chosen then fallback): calls=%v", len(runner.calls), runner.calls)
	}
	if runner.calls[0] != "vllm" || runner.calls[1] != "openrouter" {
		t.Fatalf("sandbox runner call order = %v, want [vllm openrouter]", runner.calls)
	}
}

// chainRunner is a fake sandbox runner for the N-fallback-chain runtime walk (manyforge-7lx T3):
// it fails every lane run for a configured set of MODELS (identified via spec.Env["LLM_MODEL"],
// which distinguishes two chain entries that share a provider — LLM_PROVIDER alone cannot) and
// records the model of EVERY invocation, in order, so a test can assert the exact sequence the
// whole chain was walked in, not just its first hop.
type chainRunner struct {
	failModels map[string]bool
	calls      []string // LLM_MODEL of each Run() call, in invocation order
}

func (r *chainRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	model := spec.Env["LLM_MODEL"]
	r.calls = append(r.calls, model)
	if r.failModels[model] {
		return sandbox.SandboxResult{ExitCode: 1, Stderr: []byte(model + " backend unreachable")},
			fmt.Errorf("sandbox: %s backend unreachable", model)
	}
	doc, _ := json.Marshal(map[string]any{"summary": "ok", "findings": []map[string]any{}})
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{"review.json": doc}}, nil
}

// TestReviewLaneWalksWholeFallbackChain pins manyforge-7lx T3: reviewLane's runtime fallback
// (Task 1/5) only ever tried FallbackChain[0] — a lane failure past that single hop had nowhere
// left to go. This test seeds a dimension with a TWO-entry chain (primary vllm@local-model-a,
// then vllm@local-model-b, then openrouter) where the primary AND the first chain entry both
// genuinely fail their sandbox run and only the second chain entry (openrouter) succeeds. Before
// T3, reviewLane gives up after the first (and only) fallback attempt and this test fails red —
// the dimension_run is persisted as provider="vllm", status="failed", and the runner is invoked
// only twice (primary, then chain[0]). Green requires the runner invoked THREE times, in order
// (primary, chain[0], chain[1]), and the surviving result's provider is openrouter.
func TestReviewLaneWalksWholeFallbackChain(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	seedDimChain(ctx, t, tdb, seed.businessID, "security", "vllm", "local-model-a",
		[]FallbackEntry{{Provider: "vllm", Model: "local-model-b"}, {Provider: "openrouter", Model: "deepseek/deepseek-chat-v3.1"}}, 1)

	vllmCred := AICredential{Provider: "vllm", BaseURL: "http://127.0.0.1:19999", Model: "local-model-a"}
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

	runner := &chainRunner{failModels: map[string]bool{"local-model-a": true, "local-model-b": true}}

	svc := buildService(t, tdb, env, runner, resolver)
	svc.EgressAllow = netsafe.ParseHostAllowlist("api.anthropic.com,127.0.0.1,openrouter.ai")
	// vllm is live (so resolveLaneCred picks the primary as the CHOSEN lane, not a config-time
	// reroute) — its sandbox run then fails for real, as does the first chain entry's, triggering
	// T3's tail walk down to the second chain entry.
	svc.Prober = stubProbe{vllmCred.BaseURL: true, openrouterCred.BaseURL: true}

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
		t.Fatalf("dimension run status = %q, want %q (the whole chain must be walked to its surviving entry): %+v", run.Status, "succeeded", run)
	}
	if run.Provider != "openrouter" {
		t.Fatalf("dimension run provider = %q, want %q (must have fallen through BOTH failed entries)", run.Provider, "openrouter")
	}
	if run.Model != "deepseek/deepseek-chat-v3.1" {
		t.Fatalf("dimension run model = %q, want %q", run.Model, "deepseek/deepseek-chat-v3.1")
	}

	// The whole chain must have genuinely been walked in order — a short-circuit that skips the
	// failed middle entry, or stops after one fallback, would show a different call sequence.
	wantCalls := []string{"local-model-a", "local-model-b", "deepseek/deepseek-chat-v3.1"}
	if len(runner.calls) != len(wantCalls) {
		t.Fatalf("sandbox runner invoked %d times, want %d: calls=%v", len(runner.calls), len(wantCalls), runner.calls)
	}
	for i, want := range wantCalls {
		if runner.calls[i] != want {
			t.Fatalf("sandbox runner call order = %v, want %v", runner.calls, wantCalls)
		}
	}
}

// TestReviewLaneWholeChainExhaustedReturnsOriginalError pins manyforge-7lx T3's exhaustion path:
// when EVERY entry in the chain fails (primary + every fallback), reviewLane must surface the
// ORIGINAL primary failure — not the last-tried fallback's error — so an operator debugging a
// fully-exhausted chain sees what broke FIRST. This mirrors
// TestReviewLaneBothChosenAndFallbackFailReturnsOriginalError but over a THREE-entry chain, to
// pin that the "return the original error" behavior holds no matter how many fallbacks were
// walked before giving up.
func TestReviewLaneWholeChainExhaustedReturnsOriginalError(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	seedDimChain(ctx, t, tdb, seed.businessID, "security", "vllm", "local-model-a",
		[]FallbackEntry{{Provider: "vllm", Model: "local-model-b"}, {Provider: "openrouter", Model: "deepseek/deepseek-chat-v3.1"}}, 1)

	vllmCred := AICredential{Provider: "vllm", BaseURL: "http://127.0.0.1:19999", Model: "local-model-a"}
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

	runner := &chainRunner{failModels: map[string]bool{
		"local-model-a":               true,
		"local-model-b":               true,
		"deepseek/deepseek-chat-v3.1": true,
	}}

	svc := buildService(t, tdb, env, runner, resolver)
	svc.EgressAllow = netsafe.ParseHostAllowlist("api.anthropic.com,127.0.0.1,openrouter.ai")
	svc.Prober = stubProbe{vllmCred.BaseURL: true, openrouterCred.BaseURL: true}

	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed := ClaimedReview{
		ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
		AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
	}
	runErr := svc.runJob(ctx, claimed, nil)
	if runErr == nil {
		t.Fatal("runJob: want error (every entry in the chain failed), got nil")
	}
	// The surfaced error must reference the ORIGINAL primary (local-model-a) failure — not the
	// middle or final fallback's — so a masked error doesn't hide which endpoint broke first.
	if !strings.Contains(runErr.Error(), "local-model-a") {
		t.Fatalf("runJob error = %q, want it to reference the ORIGINAL primary (local-model-a) failure", runErr.Error())
	}
	if strings.Contains(runErr.Error(), "local-model-b") || strings.Contains(runErr.Error(), "deepseek") {
		t.Fatalf("runJob error = %q, must not be masked by a later fallback's own failure", runErr.Error())
	}

	// Every entry in the chain must have genuinely been attempted, in order, before giving up.
	wantCalls := []string{"local-model-a", "local-model-b", "deepseek/deepseek-chat-v3.1"}
	if len(runner.calls) != len(wantCalls) {
		t.Fatalf("sandbox runner invoked %d times, want %d: calls=%v", len(runner.calls), len(wantCalls), runner.calls)
	}
	for i, want := range wantCalls {
		if runner.calls[i] != want {
			t.Fatalf("sandbox runner call order = %v, want %v", runner.calls, wantCalls)
		}
	}
}
