//go:build integration

package coding

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// perDimRunner is a fake sandbox runner for the multi-dimension fan-out: it reads the per-lane
// review_instructions.txt (which the cloud lane carries in-band via spec.Inputs, keyed by the
// dimension's prompt) to learn WHICH dimension it is reviewing, then returns a dimension-specific
// finding in that lane's review.json (via the result's Outputs). This lets one runner drive N
// lanes with distinct findings, exercising the real per-lane spec/result plumbing + aggregation +
// tagging.
type perDimRunner struct{}

func (r *perDimRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	instr := spec.Inputs["review_instructions.txt"]
	dim := "general"
	switch {
	case strings.Contains(string(instr), "DIMPROMPT:security"):
		dim = "security"
	case strings.Contains(string(instr), "DIMPROMPT:correctness"):
		dim = "correctness"
	}
	doc := map[string]any{
		"summary": dim + " summary",
		"findings": []map[string]any{
			{"file": "main.go", "line": 1, "severity": "error", "title": dim + "-finding", "detail": "d"},
		},
	}
	data, _ := json.Marshal(doc)
	// Emit usage.json the way the real entrypoint does — including opencode's OWN cost and
	// the dominant cache-read tokens — so the fan-out's cost accounting is exercised: the
	// host must bill from `cost` (0.02 ⇒ 2¢), not re-price from the token subset.
	usage := `[{"cost":0.02,"input":1000,"output":50,"reasoning":10,"cache_read":40000,"cache_write":0}]`
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{
		"review.json": data,
		"usage.json":  []byte(usage),
	}}, nil
}

// overlapRunner records the maximum number of Run calls in flight at once, to prove
// dimension lanes execute concurrently and never exceed maxConcurrentLanes
// (manyforge-w54). It is stateless apart from the two atomics, so it is safe to
// invoke from many goroutines.
type overlapRunner struct {
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func (r *overlapRunner) Run(_ context.Context, _ sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	n := r.inFlight.Add(1)
	for { // publish the running max
		m := r.maxSeen.Load()
		if n <= m || r.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(60 * time.Millisecond) // hold the slot so sibling lanes overlap
	r.inFlight.Add(-1)
	doc, _ := json.Marshal(map[string]any{
		"summary":  "s",
		"findings": []map[string]any{{"file": "main.go", "line": 1, "severity": "error", "title": "t", "detail": "d"}},
	})
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{"review.json": doc}}, nil
}

// TestCodeReviewLanesRunInParallel pins that a multi-dimension review fans out
// concurrently (manyforge-w54): with 5 dimensions all reviewing everything, at
// least 2 lanes overlap, and the fan-out never exceeds defaultConcurrentLanes. Run
// under -race, it also guards the indexed-write result collection against races.
func TestCodeReviewLanesRunInParallel(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	// 5 dimensions, all with nil scope globs so every lane actually runs (nothing skipped).
	for i, d := range []string{"security", "correctness", "performance", "tests", "docs"} {
		seedReviewDimension(ctx, t, tdb, seed.businessID, d, "DIMPROMPT:"+d, "info", nil, i+1)
	}

	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)
	runner := &overlapRunner{}
	svc := buildService(t, tdb, env, runner, fakeCred)

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

	if maxN := runner.maxSeen.Load(); maxN < 2 {
		t.Fatalf("lanes must run in parallel: max concurrent Run = %d, want >= 2", maxN)
	} else if maxN > defaultConcurrentLanes {
		t.Fatalf("max concurrent Run = %d exceeds the default cap defaultConcurrentLanes=%d", maxN, defaultConcurrentLanes)
	}

	// Determinism (manyforge-w54, PR #23 review): despite lanes completing in
	// arbitrary order, indexed writes keep dimension_runs in the configured sort
	// order — so aggregation output is identical to the old sequential path.
	got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var runs []struct {
		Dimension string `json:"dimension"`
	}
	if err := json.Unmarshal(got.DimensionRuns, &runs); err != nil {
		t.Fatalf("unmarshal dimension_runs %q: %v", got.DimensionRuns, err)
	}
	order := make([]string, len(runs))
	for i, r := range runs {
		order[i] = r.Dimension
	}
	if want := "security,correctness,performance,tests,docs"; strings.Join(order, ",") != want {
		t.Errorf("dimension_runs order = %q, want %q (indexed writes must preserve sort order)", strings.Join(order, ","), want)
	}
}

// TestCodeReviewLanesRespectPerAgentCap pins that the resolved reviewbot's
// max_concurrent_lanes serializes the fan-out (manyforge-k8e.2): a bot capped at 1 (a
// single-GPU self-host) runs the whole 5-dimension panel one lane at a time, even though
// the lanes would otherwise overlap. This exercises the real runJob → laneLimit →
// g.SetLimit path end-to-end, not just the pure laneLimit unit.
func TestCodeReviewLanesRespectPerAgentCap(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	// 5 dimensions, all unscoped so every lane runs (nothing skipped) — the same panel
	// the parallel test uses, so the ONLY behavioral difference is the per-agent cap.
	for i, d := range []string{"security", "correctness", "performance", "tests", "docs"} {
		seedReviewDimension(ctx, t, tdb, seed.businessID, d, "DIMPROMPT:"+d, "info", nil, i+1)
	}

	// A single-GPU self-host reviewbot: cap the fan-out at one lane.
	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic", MaxConcurrentLanes: 1}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)
	runner := &overlapRunner{}
	svc := buildService(t, tdb, env, runner, fakeCred)

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

	if maxN := runner.maxSeen.Load(); maxN != 1 {
		t.Fatalf("per-agent cap=1 must serialize lanes: max concurrent Run = %d, want 1", maxN)
	}
}

// mapCredResolver resolves per-agent credentials from a map — lets a fallback-chain test
// give the primary and secondary bots DIFFERENT creds (a single FakeCredResolver can't).
type mapCredResolver map[uuid.UUID]AICredential

func (m mapCredResolver) Resolve(_ context.Context, _, _, agentID uuid.UUID) (AICredential, error) {
	c, ok := m[agentID]
	if !ok {
		return AICredential{}, errs.ErrNotFound
	}
	return c, nil
}

// ResolveProvider satisfies the interface; the chain test's dims all have blank providers
// (they inherit the review default), so per-lane provider resolution is never invoked here.
func (m mapCredResolver) ResolveProvider(_ context.Context, _, _ uuid.UUID, _, _ string) (AICredential, error) {
	return AICredential{}, errs.ErrNotFound
}

// TestCodeReviewFallbackChainPicksLiveSecondary is the end-to-end wiring test for the
// fallback chain (manyforge-k8e): with a configured chain whose primary (a self-hosted
// vLLM bot at a dead port) fails the REAL liveness probe, runJob transparently runs the
// whole review on the live secondary (cloud) bot — its provider shows up in the persisted
// dimension_runs, and the review completes rather than failing on the down primary.
func TestCodeReviewFallbackChainPicksLiveSecondary(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)
	seedReviewDimension(ctx, t, tdb, seed.businessID, "security", "P:security", "info", nil, 1)

	primary := uuid.New()     // self-hosted vLLM at a dead port → probe fails
	secondary := seed.agentID // cloud (anthropic), reachable via the sandbox runner
	// Store the chain directly (bypass UpsertConfig validation: primary isn't a real agent
	// row; the map resolver supplies its cred). tenant_root_id == business_id for a root.
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO review_config (business_id, tenant_root_id, dedupe, verify_enabled, verify_model, cite_rules, post_mode, review_agent_chain, updated_at)
		 VALUES ($1,$1,true,false,'',false,'single',$2,now())
		 ON CONFLICT (business_id) DO UPDATE SET review_agent_chain = EXCLUDED.review_agent_chain`,
		seed.businessID, []uuid.UUID{primary, secondary}); err != nil {
		t.Fatalf("seed chain: %v", err)
	}

	resolver := mapCredResolver{
		primary:   {Provider: "vllm", BaseURL: "http://127.0.0.1:9/v1", Model: "ornith-1.0-9b", AllowPrivateBaseURL: true},
		secondary: {APIKey: "k", Provider: "anthropic", BaseURL: "https://api.anthropic.com", Model: "m"},
	}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)
	runner := &overlapRunner{}
	svc := buildService(t, tdb, env, runner, resolver) // Prober nil ⇒ REAL httpProber

	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, secondary, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed := ClaimedReview{
		ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
		AgentID: secondary, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
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
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(got.DimensionRuns, &runs); err != nil {
		t.Fatalf("unmarshal dimension_runs %q: %v", got.DimensionRuns, err)
	}
	if len(runs) != 1 || runs[0].Provider != "anthropic" || runs[0].Status != "succeeded" {
		t.Fatalf("review must run on the live secondary (anthropic), got %+v", runs)
	}
}

// TestCodeReviewRetryBypassesDedup pins the manual retry (manyforge): a normal re-run of an
// already-reviewed head is SKIPPED by the same-head dedup, but Retry enqueues a FORCED review
// that runs anyway (lanes execute) — the exact "re-run after a failure/config change" path.
func TestCodeReviewRetryBypassesDedup(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)
	seedReviewDimension(ctx, t, tdb, seed.businessID, "security", "P:security", "info", nil, 1)

	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)
	svc := buildService(t, tdb, env, &overlapRunner{}, fakeCred)

	run := func(id uuid.UUID) {
		if err := svc.runJob(ctx, ClaimedReview{
			ID: id, BusinessID: seed.businessID, PrincipalID: seed.principalID,
			AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
		}, nil); err != nil {
			t.Fatalf("runJob %s: %v", id, err)
		}
	}
	lanes := func(id uuid.UUID) int {
		got, err := svc.Get(ctx, seed.principalID, seed.businessID, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		var runs []struct{}
		_ = json.Unmarshal(got.DimensionRuns, &runs)
		return len(runs)
	}

	// 1. First review runs + succeeds → head "abc" is now recorded reviewed.
	cr1, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue 1: %v", err)
	}
	run(cr1.ID)
	if lanes(cr1.ID) == 0 {
		t.Fatal("first review should have run its lane")
	}

	// 2. A NORMAL new review of the SAME head is skipped by the dedup → no lanes.
	cr2, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue 2: %v", err)
	}
	run(cr2.ID)
	if n := lanes(cr2.ID); n != 0 {
		t.Fatalf("a normal re-run of an already-reviewed head must be deduped (0 lanes), got %d", n)
	}

	// 3. Retry the first review → a FORCED review that runs despite the same head.
	cr3, err := svc.Retry(ctx, seed.principalID, seed.businessID, cr1.ID)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	run(cr3.ID)
	if n := lanes(cr3.ID); n == 0 {
		t.Fatal("a forced retry must bypass the dedup and run its lane, got 0")
	}
}

// seedDimFull inserts a review_dimension with an explicit primary + a single-entry fallback
// chain (provider, model), via the superuser connection (bypasses RLS for setup). Empty
// provider ⇒ NULL; empty fbProvider ⇒ an empty fallback_chain ('[]', no fallback).
func seedDimFull(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID, dim, provider, model, fbProvider, fbModel string, order int) {
	t.Helper()
	var chain []FallbackEntry
	if fbProvider != "" {
		chain = []FallbackEntry{{Provider: fbProvider, Model: fbModel}}
	}
	seedDimChain(ctx, t, tdb, businessID, dim, provider, model, chain, order)
}

// seedDimChain inserts a review_dimension with an explicit primary + a full ORDERED fallback
// chain (manyforge-7lx T3) — generalizes seedDimFull's single-entry chain to N entries so a
// test can exercise the whole-chain runtime walk (reviewLane), not just one fallback hop. Empty
// provider ⇒ NULL; empty/nil chain ⇒ an empty fallback_chain ('[]', no fallback).
func seedDimChain(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID, dim, provider, model string, chain []FallbackEntry, order int) {
	t.Helper()
	var prov any
	if provider != "" {
		prov = provider
	}
	chainJSON := "[]"
	if len(chain) > 0 {
		b, err := json.Marshal(chain)
		if err != nil {
			t.Fatalf("marshal fallback chain: %v", err)
		}
		chainJSON = string(b)
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO review_dimension
		   (id, business_id, tenant_root_id, dimension, provider, model, fallback_chain,
		    prompt, scope_globs, min_severity, enabled, sort_order, created_at, updated_at)
		 VALUES ($1,$2,$2,$3,$4::ai_provider,$5,$6::jsonb,$7,'{}','info',true,$8,now(),now())`,
		uuid.New(), businessID, dim, prov, model, chainJSON, "DIMPROMPT:"+dim, order); err != nil {
		t.Fatalf("seed dim %q: %v", dim, err)
	}
}

// TestCodeReviewPerDimensionProviderAndFallback is the core manyforge-azy exercise: each
// dimension routes to its OWN (provider, model), and a dimension whose primary endpoint fails
// the liveness probe transparently runs on its fallback (provider, model). All three lanes are
// cloud (sandbox), so a stub prober controls liveness deterministically.
func TestCodeReviewPerDimensionProviderAndFallback(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	// security → openrouter (up); docs → anthropic (up); tests → openai (DOWN) ⇒ fallback anthropic.
	seedDimFull(ctx, t, tdb, seed.businessID, "security", "openrouter", "grok", "", "", 1)
	seedDimFull(ctx, t, tdb, seed.businessID, "docs", "anthropic", "claude-docs", "", "", 2)
	seedDimFull(ctx, t, tdb, seed.businessID, "tests", "openai", "gpt", "anthropic", "claude-fb", 3)

	resolver := &FakeCredResolver{
		Cred: AICredential{APIKey: "k", Provider: "anthropic", BaseURL: "https://api.anthropic.com", Model: "def"},
		ByProvider: map[string]AICredential{
			"openrouter": {APIKey: "k", Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "grok"},
			"anthropic":  {APIKey: "k", Provider: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude"},
			"openai":     {APIKey: "k", Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt"},
		},
	}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)
	runner := &perDimRunner{}
	svc := buildService(t, tdb, env, runner, resolver)
	svc.EgressAllow = netsafe.ParseHostAllowlist("api.anthropic.com,openrouter.ai,api.openai.com")
	svc.Prober = stubProbe{
		"https://openrouter.ai/api/v1": true,
		"https://api.anthropic.com":    true,
		"https://api.openai.com/v1":    false, // down ⇒ the tests lane falls back to anthropic
	}

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
	type pm struct{ provider, model string }
	byDim := map[string]pm{}
	for _, r := range runs {
		byDim[r.Dimension] = pm{r.Provider, r.Model}
	}
	if byDim["security"] != (pm{"openrouter", "grok"}) {
		t.Fatalf("security must route to openrouter/grok, got %+v", byDim["security"])
	}
	if byDim["docs"] != (pm{"anthropic", "claude-docs"}) {
		t.Fatalf("docs must route to anthropic/claude-docs, got %+v", byDim["docs"])
	}
	if byDim["tests"] != (pm{"anthropic", "claude-fb"}) {
		t.Fatalf("tests must FALL BACK to anthropic/claude-fb, got %+v", byDim["tests"])
	}
}

// seedReviewDimension inserts a configured review_dimension row for a business via the superuser
// connection (bypasses RLS for setup). tenant_root_id == business_id for a root business.
func seedReviewDimension(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID, dimension, prompt, minSeverity string, globs []string, order int) {
	t.Helper()
	if globs == nil {
		globs = []string{}
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO review_dimension
		   (id, business_id, tenant_root_id, dimension, model, prompt, scope_globs, min_severity, enabled, sort_order, created_at, updated_at)
		 VALUES ($1,$2,$2,$3,'',$4,$5,$6,true,$7,now(),now())`,
		uuid.New(), businessID, dimension, prompt, globs, minSeverity, order); err != nil {
		t.Fatalf("seed review_dimension %q: %v", dimension, err)
	}
}

// TestCodeReviewMultiDimensionFanout is the first end-to-end exercise of the >1-lane path
// (spec 008): a business configures three dimensions; a review fans out across them, tags each
// lane's findings, aggregates into ONE posted review, records per-dimension accounting, and
// records a scoped-out dimension as skipped (never silently dropped).
func TestCodeReviewMultiDimensionFanout(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, ghStub := startGitHubStub(t, prJSON)

	// Configure a 3-dimension panel: security + correctness review everything; ui is scoped to
	// frontend paths so — with no changed files surfaced by the stub — it is skipped, not run.
	seedReviewDimension(ctx, t, tdb, seed.businessID, "security", "DIMPROMPT:security", "info", nil, 1)
	seedReviewDimension(ctx, t, tdb, seed.businessID, "correctness", "DIMPROMPT:correctness", "info", nil, 2)
	seedReviewDimension(ctx, t, tdb, seed.businessID, "ui", "DIMPROMPT:ui", "info", []string{"frontend/**"}, 3)

	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)
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

	// Exactly ONE aggregated review is posted (not one per dimension).
	if n := ghStub.postCount.Load(); n != 1 {
		t.Fatalf("want exactly 1 aggregated GitHub post across all lanes, got %d", n)
	}

	got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("want succeeded, got %s", got.Status)
	}

	// vv6: a multi-dimension review stamps the "panel" sentinel on the top-level
	// model, not the agent's single default model (which no lane necessarily ran) —
	// the per-lane models live in dimension_runs.
	if got.Model != "panel" {
		t.Errorf("multi-dim review model = %q, want the \"panel\" sentinel (vv6)", got.Model)
	}

	// Findings from the two ran lanes are present and tagged by dimension.
	tags := map[string]bool{}
	for _, f := range got.Findings {
		tags[f.Dimension] = true
	}
	if !tags["security"] || !tags["correctness"] {
		t.Fatalf("findings must be tagged by their dimension; got tags %v (findings=%+v)", tags, got.Findings)
	}

	// dimension_runs records all three lanes: two succeeded, ui skipped (scope: no files).
	// Read them off the Get DTO (not the raw DB column) so this pins that Get plumbs the
	// dimension_runs jsonb into CodeReview.DimensionRuns for the detail UI (spec 008 Slice 2).
	if len(got.DimensionRuns) == 0 {
		t.Fatalf("Get must surface dimension_runs on the DTO; got empty DimensionRuns")
	}
	var runs []struct {
		Dimension    string `json:"dimension"`
		Status       string `json:"status"`
		Model        string `json:"model"`
		FindingCount int    `json:"finding_count"`
		CostCents    int64  `json:"cost_cents"`
		TokensIn     int64  `json:"tokens_in"`
	}
	if err := json.Unmarshal(got.DimensionRuns, &runs); err != nil {
		t.Fatalf("unmarshal DimensionRuns %q: %v", got.DimensionRuns, err)
	}
	byDim := map[string]string{}
	byModel := map[string]string{}
	var byCost map[string]int64 = map[string]int64{}
	var byTokensIn map[string]int64 = map[string]int64{}
	for _, r := range runs {
		byDim[r.Dimension] = r.Status
		byModel[r.Dimension] = r.Model
		byCost[r.Dimension] = r.CostCents
		byTokensIn[r.Dimension] = r.TokensIn
	}
	if byDim["security"] != "succeeded" || byDim["correctness"] != "succeeded" {
		t.Fatalf("ran lanes must be recorded succeeded; got %v", byDim)
	}
	// vv6: the top-level model is the "panel" sentinel, but the REAL per-lane models
	// are preserved in dimension_runs (here the resolved "m", since these dims carry
	// no own model). That's the split the panel sentinel relies on.
	if byModel["security"] != "m" || byModel["correctness"] != "m" {
		t.Errorf("per-lane models must be preserved in dimension_runs; got %v (top-level model=%q)", byModel, got.Model)
	}
	if byDim["ui"] != "skipped" {
		t.Fatalf("the scoped-out ui dimension must be recorded as skipped, not silently dropped; got %v", byDim)
	}
	// A ran lane must bill from opencode's OWN cost (0.02 ⇒ 2¢), not re-price the tokens,
	// and TokensIn must include the cache-read tokens (1000 fresh + 40000 cached = 41000).
	if byCost["security"] != 2 {
		t.Fatalf("lane cost must come from opencode cost (2¢); got %d", byCost["security"])
	}
	if byTokensIn["security"] != 41000 {
		t.Fatalf("TokensIn must include cache-read tokens (1000+40000); got %d", byTokensIn["security"])
	}
	// Aggregated review cost = sum of the two ran lanes (2¢ + 2¢).
	if got.CostCents != 4 {
		t.Fatalf("aggregated cost must sum lane costs (4¢); got %d", got.CostCents)
	}
}

// TestReviewDimensionServiceCRUD exercises the Slice 2 config service against a real DB: the
// upsert insert+update paths (ON CONFLICT, no duplicate), list, config default-then-upsert,
// delete, and cross-tenant ownership (a foreign business yields ErrNotFound, not a forged row).
func TestReviewDimensionServiceCRUD(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	svc := &ReviewDimensionService{DB: tdb.App}

	// Insert.
	if _, err := svc.UpsertDimension(ctx, seed.principalID, seed.businessID, ReviewDimensionInput{
		Dimension: "security", MinSeverity: "warning", Provider: "openrouter", Model: "x-ai/grok", Enabled: true, SortOrder: 1,
	}); err != nil {
		t.Fatalf("insert dimension: %v", err)
	}
	panel, err := svc.ListPanel(ctx, seed.principalID, seed.businessID)
	if err != nil || len(panel) != 1 || panel[0].Dimension != "security" || !panel[0].Enabled {
		t.Fatalf("list after insert: %+v err=%v", panel, err)
	}

	// Update via upsert (same business+dimension) — must NOT create a duplicate row.
	if _, err := svc.UpsertDimension(ctx, seed.principalID, seed.businessID, ReviewDimensionInput{
		Dimension: "security", MinSeverity: "error", Enabled: false, SortOrder: 1,
	}); err != nil {
		t.Fatalf("update dimension: %v", err)
	}
	panel, _ = svc.ListPanel(ctx, seed.principalID, seed.businessID)
	if len(panel) != 1 || panel[0].Enabled || panel[0].MinSeverity != "error" || panel[0].Provider != "" {
		t.Fatalf("upsert must update in place (enabled=false, sev=error, provider cleared): %+v", panel)
	}

	// Config: default when absent, then upsert.
	cfg, err := svc.GetConfig(ctx, seed.principalID, seed.businessID)
	if err != nil || !cfg.Dedupe || cfg.PostMode != "single" {
		t.Fatalf("default config wrong: %+v err=%v", cfg, err)
	}
	if _, err := svc.UpsertConfig(ctx, seed.principalID, seed.businessID, ReviewConfigInput{
		Dedupe: false, VerifyEnabled: true, VerifyProvider: "anthropic", VerifyModel: "m", PostMode: "per_dimension",
	}); err != nil {
		t.Fatalf("upsert config: %v", err)
	}
	cfg, _ = svc.GetConfig(ctx, seed.principalID, seed.businessID)
	if cfg.Dedupe || !cfg.VerifyEnabled || cfg.PostMode != "per_dimension" {
		t.Fatalf("config not persisted: %+v", cfg)
	}

	// Fallback chain: a known agent round-trips (order preserved via uuid[]), an unknown
	// agent id is rejected (no forged chain), and clearing persists an empty list.
	if _, err := svc.UpsertConfig(ctx, seed.principalID, seed.businessID, ReviewConfigInput{
		PostMode: "single", ReviewAgentChain: []string{seed.agentID.String()},
	}); err != nil {
		t.Fatalf("upsert config with chain: %v", err)
	}
	cfg, _ = svc.GetConfig(ctx, seed.principalID, seed.businessID)
	if len(cfg.ReviewAgentChain) != 1 || cfg.ReviewAgentChain[0] != seed.agentID.String() {
		t.Fatalf("chain not persisted: %+v", cfg.ReviewAgentChain)
	}
	if _, err := svc.UpsertConfig(ctx, seed.principalID, seed.businessID, ReviewConfigInput{
		PostMode: "single", ReviewAgentChain: []string{uuid.NewString()},
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("unknown agent in chain must be ErrValidation, got %v", err)
	}
	if _, err := svc.UpsertConfig(ctx, seed.principalID, seed.businessID, ReviewConfigInput{PostMode: "single"}); err != nil {
		t.Fatalf("clear chain: %v", err)
	}
	cfg, _ = svc.GetConfig(ctx, seed.principalID, seed.businessID)
	if len(cfg.ReviewAgentChain) != 0 {
		t.Fatalf("chain should be cleared, got %+v", cfg.ReviewAgentChain)
	}

	// Cross-tenant ownership: tenant B upserting for tenant A's business is rejected (no row).
	seedB := seedCodingTenant(ctx, t, tdb)
	if _, err := svc.UpsertDimension(ctx, seedB.principalID, seed.businessID, ReviewDimensionInput{
		Dimension: "docs", MinSeverity: "info",
	}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant upsert must be ErrNotFound (ownership), got %v", err)
	}

	// Delete.
	dimID := uuid.MustParse(panel[0].ID)
	if err := svc.DeleteDimension(ctx, seed.principalID, seed.businessID, dimID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	panel, _ = svc.ListPanel(ctx, seed.principalID, seed.businessID)
	if len(panel) != 0 {
		t.Fatalf("panel must be empty after delete: %+v", panel)
	}
	if err := svc.DeleteDimension(ctx, seed.principalID, seed.businessID, dimID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("re-delete must be ErrNotFound, got %v", err)
	}
}

// TestReviewDimensionFallbackChainRoundTrips pins manyforge-7lx Task 1: an ordered N-entry
// fallback chain persists through UpsertDimension and comes back byte-for-byte (same providers,
// same models, same order) via ListPanel — the jsonb column round-trip, not just the in-memory
// shape. A blank-provider entry is rejected before any DB write (mirrored, faster, in
// TestValidateDimensionInput; asserted again here end-to-end through the real service).
func TestReviewDimensionFallbackChainRoundTrips(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	svc := &ReviewDimensionService{DB: tdb.App}

	chain := []FallbackEntry{
		{Provider: "openrouter", Model: "grok"},
		{Provider: "vllm", Model: "local-model"},
		{Provider: "anthropic", Model: "claude"},
	}
	if _, err := svc.UpsertDimension(ctx, seed.principalID, seed.businessID, ReviewDimensionInput{
		Dimension: "security", MinSeverity: "warning", FallbackChain: chain, Enabled: true, SortOrder: 1,
	}); err != nil {
		t.Fatalf("upsert with 3-entry chain: %v", err)
	}
	panel, err := svc.ListPanel(ctx, seed.principalID, seed.businessID)
	if err != nil || len(panel) != 1 {
		t.Fatalf("list after insert: %+v err=%v", panel, err)
	}
	if got := panel[0].FallbackChain; !reflect.DeepEqual(got, chain) {
		t.Fatalf("fallback chain round-trip: got %+v, want %+v", got, chain)
	}

	// A blank-provider entry is rejected — never persisted as dead/unusable config.
	if _, err := svc.UpsertDimension(ctx, seed.principalID, seed.businessID, ReviewDimensionInput{
		Dimension: "docs", MinSeverity: "info", FallbackChain: []FallbackEntry{{Provider: "", Model: "m"}},
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("blank-provider fallback entry must be ErrValidation, got %v", err)
	}
}

// TestReviewDimensionCrossTenantRLS pins MF008-PIN-1 behaviorally: one tenant's configured
// review dimensions are invisible to another tenant — resolvePanel under tenant B, even asking
// for tenant A's business id, sees no rows (RLS) and falls back to the default single lane.
func TestReviewDimensionCrossTenantRLS(t *testing.T) {
	ctx, tdb, seedA := startCoding(t)
	seedB := seedCodingTenant(ctx, t, tdb)

	seedReviewDimension(ctx, t, tdb, seedA.businessID, "security", "DIMPROMPT:security", "info", nil, 1)

	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	env := newCodingEnv(t, tdb)
	svc := buildService(t, tdb, env, &validFakeRunner{}, fakeCred)

	// Tenant A sees its configured panel (the one security dimension).
	panelA := svc.resolvePanel(ctx, seedA.principalID, seedA.businessID)
	if len(panelA) != 1 || panelA[0].Key != "security" {
		t.Fatalf("tenant A must see its configured dimension; got %+v", panelA)
	}

	// Tenant B — even querying tenant A's business id — is RLS-blocked from A's rows and falls
	// back to the default single general lane.
	panelB := svc.resolvePanel(ctx, seedB.principalID, seedA.businessID)
	if len(panelB) != 1 || panelB[0].Key != generalDimensionKey {
		t.Fatalf("tenant B must NOT see tenant A's dimensions (RLS); want the default general lane, got %+v", panelB)
	}
}
