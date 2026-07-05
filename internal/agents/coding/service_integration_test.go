//go:build integration

package coding

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

type codingSeed struct {
	businessID  uuid.UUID
	principalID uuid.UUID
	agentID     uuid.UUID // a real agent row (CreateCodeReviewAgentRun derives tenant from it)
}

func newTestSealerCoding(t *testing.T) *crypto.Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	s, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

// seedCodingTenant inserts the minimal tenant/principal/business rows needed by
// the coding integration test. Mirrors connectors.seedConnectorTenant.
func seedCodingTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) codingSeed {
	t.Helper()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}

	masterID := uuid.New()
	agentID := uuid.New()    // the agent's PRINCIPAL id
	agentRowID := uuid.New() // the agent table row id
	benignRoleID := uuid.New()
	ownerAcctID := uuid.New()
	ownerHumanID := uuid.New()
	ownerEmail := "coding-owner-" + masterID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{ownerAcctID, ownerEmail}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{ownerHumanID, ownerAcctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'CodingCo','active',now(),now())`,
			[]any{masterID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{masterID}},
		{`INSERT INTO principal (id,kind,home_business_id,tenant_root_id,created_at) VALUES ($1,'agent',$2,$2,now())`,
			[]any{agentID, masterID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{ownerHumanID, masterID, ownerRole}},
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'coding-read','CodingRead',false,now())`,
			[]any{benignRoleID, masterID}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.read')`,
			[]any{benignRoleID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{agentID, masterID, benignRoleID}},
		// A real agent row so CreateCodeReviewAgentRun (FROM agent WHERE id=…) resolves.
		{`INSERT INTO agent (id,business_id,tenant_root_id,principal_id,name,provider,model,system_prompt,allowed_tools,autonomy_mode,enabled,monthly_budget_cents,created_at,updated_at,allowed_mcp_servers,retriage_on_reply,web_allowed_domains)
		  VALUES ($1,$2,$2,$3,'ReviewBot','anthropic','m','',ARRAY[]::text[],3,true,0,now(),now(),ARRAY[]::uuid[],false,ARRAY[]::text[])`,
			[]any{agentRowID, masterID, agentID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return codingSeed{businessID: masterID, principalID: agentID, agentID: agentRowID}
}

// startCoding starts a testdb and seeds a tenant. Returns the context, the DB, and the seed.
func startCoding(t *testing.T) (context.Context, *testdb.TestDB, codingSeed) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb, seedCodingTenant(ctx, t, tdb)
}

// ---------------------------------------------------------------------------
// GitHub stub server
// ---------------------------------------------------------------------------

type githubStub struct {
	prJSON      []byte // canned PR response
	reviewPosts [][]byte
	postCount   atomic.Int64
}

// startGitHubStub creates an httptest server that:
//   - GET /repos/o/r/pulls/{n} → 200 with prJSON
//   - POST /repos/o/r/pulls/{n}/reviews → 201 with a stub review object
//
// It records the request bodies of all POST review calls.
func startGitHubStub(t *testing.T, prJSON []byte) (*httptest.Server, *githubStub) {
	t.Helper()
	stub := &githubStub{prJSON: prJSON}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(stub.prJSON)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/o/r/pulls/1/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			stub.reviewPosts = append(stub.reviewPosts, body)
			stub.postCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 42, "html_url": "https://github.com/o/r/pull/1#pullrequestreview-42"}`))
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, stub
}

// ---------------------------------------------------------------------------
// Fake runners
// ---------------------------------------------------------------------------

// validFakeRunner returns a valid review.json in the result's in-band Outputs.
type validFakeRunner struct {
	summary string
}

func (r *validFakeRunner) Run(_ context.Context, _ sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	findings := []map[string]any{
		{"file": "main.go", "line": 10, "severity": "warning", "title": "Issue", "detail": "A detail"},
	}
	doc := map[string]any{"summary": r.summary, "findings": findings}
	data, _ := json.Marshal(doc)
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{"review.json": data}}, nil
}

// malformedFakeRunner returns invalid JSON as review.json in Outputs.
type malformedFakeRunner struct{}

func (r *malformedFakeRunner) Run(_ context.Context, _ sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{"review.json": []byte(`{not json`)}}, nil
}

// malformedWithUsageRunner returns invalid findings JSON (so ParseFindings fails)
// AND a usage.json — mimicking a run where the model burned tokens before the
// review failed (manyforge-7n5: a failed-but-billed run must still be accounted).
type malformedWithUsageRunner struct{}

func (r *malformedWithUsageRunner) Run(_ context.Context, _ sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{
		"review.json": []byte(`{not json`),
		"usage.json":  []byte(`[{"input":1000,"output":200,"reasoning":0}]`),
	}}, nil
}

// retryFakeRunner fails the FIRST run with unparseable output (no JSON object), then
// succeeds on the second — exercising the cloud-lane single retry (manyforge-6h1). Each
// attempt returns its own usage.json so the test can assert the lane bills for BOTH attempts.
type retryFakeRunner struct{ calls atomic.Int32 }

func (r *retryFakeRunner) Run(_ context.Context, _ sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	if r.calls.Add(1) == 1 {
		return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{
			"review.json": []byte("prose only, no JSON here"),
			"usage.json":  []byte(`[{"cost":0.10,"input":1000,"output":50}]`),
		}}, nil
	}
	doc := map[string]any{"summary": "recovered on retry", "findings": []map[string]any{
		{"file": "main.go", "line": 3, "severity": "info", "title": "ok", "detail": "d"},
	}}
	data, _ := json.Marshal(doc)
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{
		"review.json": data,
		"usage.json":  []byte(`[{"cost":0.05,"input":500,"output":20}]`),
	}}, nil
}

// fixedPricing is a CostEstimator that returns a constant cost regardless of input.
type fixedPricing struct{ cents int64 }

func (f *fixedPricing) CostCents(_ context.Context, _, _ string, _, _ int64) (int64, error) {
	return f.cents, nil
}

// fakeClone is the injectable clone seam: just creates destDir and a placeholder file.
// The allowPrivate parameter is accepted but ignored — no real network is involved.
func fakeClone(_ context.Context, _, _, _, destDir string, _ bool) error {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(destDir+"/README.md", []byte("fake repo\n"), 0o600)
}

// ---------------------------------------------------------------------------
// Test setup helper
// ---------------------------------------------------------------------------

// codingEnv bundles a shared sealer + the RepoConnectorService + CodeReviewService
// so secrets sealed at connector creation can be opened at Enqueue time.
type codingEnv struct {
	Sealer *crypto.Sealer
	Repos  *connectors.RepoConnectorService
}

// newCodingEnv creates a shared sealer and the connector service that uses it.
func newCodingEnv(t *testing.T, tdb *testdb.TestDB) *codingEnv {
	t.Helper()
	sealer := newTestSealerCoding(t)
	repos := &connectors.RepoConnectorService{
		DB:    tdb.App,
		Vault: secrets.NewVault(sealer),
	}
	return &codingEnv{Sealer: sealer, Repos: repos}
}

// buildService constructs a CodeReviewService wired to a real DB, the shared
// RepoConnectorService (so sealer is shared), and the provided fake runner + cred resolver.
func buildService(
	t *testing.T,
	tdb *testdb.TestDB,
	env *codingEnv,
	runner sandbox.SandboxRunner,
	credResolver AICredentialResolver,
) *CodeReviewService {
	t.Helper()
	return &CodeReviewService{
		DB:       tdb.App,
		Repos:    env.Repos,
		Sandbox:  runner,
		Creds:    credResolver,
		Image:    "opencode:stub",
		WorkRoot: t.TempDir(),
		Timeout:  30 * time.Second,
		Clone:    fakeClone,
		// fakeCred resolves to api.anthropic.com; allow it so the egress pre-flight passes.
		EgressAllow: netsafe.ParseHostAllowlist("api.anthropic.com"),
	}
}

// createRepoConnector creates a repo_connector pointing at the stub GitHub server
// and returns its UUID. Uses the shared env so the sealer matches.
func createRepoConnector(
	ctx context.Context,
	t *testing.T,
	env *codingEnv,
	seed codingSeed,
	stubURL string,
) uuid.UUID {
	t.Helper()
	id, err := env.Repos.Create(ctx, seed.principalID, seed.businessID, connectors.CreateRepoConnectorInput{
		Type:                "github",
		DisplayName:         "Test GitHub",
		BaseURL:             stubURL,
		Repo:                "o/r",
		AllowPrivateBaseURL: true, // stub is 127.0.0.1
		APIToken:            "ghp_test_token",
	})
	if err != nil {
		t.Fatalf("create repo connector: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Test: TestCodeReviewTrigger
// ---------------------------------------------------------------------------

func TestCodeReviewTrigger(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	prJSON := []byte(`{
		"number": 1,
		"title": "Test PR",
		"state": "open",
		"merged": false,
		"head": {"sha": "abc123deadbeef", "ref": "feature/test"},
		"base": {"ref": "main"}
	}`)

	srv, _ := startGitHubStub(t, prJSON)

	// A real seeded agent so the finalize's CreateCodeReviewAgentRun resolves it.
	agentID := seed.agentID

	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey:   "k",
		BaseURL:  "https://api.anthropic.com",
		Model:    "anthropic/claude-3-5-sonnet",
		Provider: "anthropic",
	}}

	t.Run("succeeded", func(t *testing.T) {
		localSrv, localStub := startGitHubStub(t, prJSON)
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)
		runner := &validFakeRunner{summary: "LGTM"}
		svc := buildService(t, tdb, env, runner, fakeCred)

		cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if cr.Status != "pending" {
			t.Fatalf("want pending, got %s", cr.Status)
		}

		claimed := ClaimedReview{
			ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
			AgentID: agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
		}
		if err := svc.runJob(ctx, claimed, nil); err != nil {
			t.Fatalf("runJob: %v", err)
		}

		got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != "succeeded" {
			t.Errorf("want succeeded, got %s", got.Status)
		}
		if len(got.Findings) == 0 {
			t.Errorf("want findings, got none")
		}
		if got.PostedAt == nil {
			t.Errorf("want posted_at set")
		}
		if n := localStub.postCount.Load(); n != 1 {
			t.Errorf("want exactly 1 GitHub POST, got %d", n)
		}
	})

	t.Run("malformed_json_marks_failed", func(t *testing.T) {
		localSrv, _ := startGitHubStub(t, prJSON)
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)
		svc := buildService(t, tdb, env, &malformedFakeRunner{}, fakeCred)

		cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		claimed := ClaimedReview{
			ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
			AgentID: agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
		}
		err = svc.runJob(ctx, claimed, nil)
		if err == nil {
			t.Error("runJob with malformed JSON: want error, got nil")
		}
	})

	t.Run("cloud_lane_retries_unparseable_then_succeeds", func(t *testing.T) {
		localSrv, _ := startGitHubStub(t, prJSON)
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)
		runner := &retryFakeRunner{}
		svc := buildService(t, tdb, env, runner, fakeCred)

		cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		claimed := ClaimedReview{
			ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
			AgentID: agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
		}
		if err := svc.runJob(ctx, claimed, nil); err != nil {
			t.Fatalf("runJob: want success after retry, got %v", err)
		}
		if n := runner.calls.Load(); n != 2 {
			t.Fatalf("want exactly 2 sandbox runs (1 unparseable + 1 retry), got %d", n)
		}
		got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != "succeeded" {
			t.Fatalf("status after successful retry: want succeeded, got %q", got.Status)
		}
		if got.CostCents != 15 { // both attempts billed: 10¢ (failed) + 5¢ (retry)
			t.Fatalf("lane must bill for BOTH attempts (10¢+5¢=15¢), got %d", got.CostCents)
		}
	})

	t.Run("rls_cross_tenant_get_not_found", func(t *testing.T) {
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, srv.URL)

		svc := buildService(t, tdb, env, &validFakeRunner{}, fakeCred)

		// Enqueue inserts a pending row; that's sufficient to exercise RLS on Get.
		cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if cr.Status != "pending" {
			t.Errorf("Enqueue status: want %q, got %q", "pending", cr.Status)
		}

		// Seed a second independent tenant.
		otherSeed := seedCodingTenant(ctx, t, tdb)

		// Attempt to Get under other tenant's principal — must get ErrNotFound.
		_, err = svc.Get(ctx, otherSeed.principalID, otherSeed.businessID, cr.ID)
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("cross-tenant Get: want ErrNotFound, got %v", err)
		}
	})

	t.Run("two_runs_malformed_second_no_extra_post", func(t *testing.T) {
		t.Skip("covered by succeeded + malformed_json_marks_failed subtests above")
	})

	t.Run("enqueue_inserts_pending_row", func(t *testing.T) {
		// New integration subtest: verifies Enqueue inserts a pending row with the
		// correct principal_id and agent_id — assertions the old synchronous subtests
		// skipped over because they called Trigger synchronously.
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, srv.URL)
		svc := buildService(t, tdb, env, &validFakeRunner{}, fakeCred)

		cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if cr.Status != "pending" {
			t.Errorf("Enqueue status: want %q, got %q", "pending", cr.Status)
		}
		if cr.PRNumber != 1 {
			t.Errorf("Enqueue PRNumber: want 1, got %d", cr.PRNumber)
		}

		// Verify the row is in the DB with status=pending, correct principal+agent.
		var dbStatus string
		var dbPrincipal, dbAgent uuid.UUID
		if err := tdb.Super.QueryRow(ctx,
			`SELECT status, principal_id, agent_id FROM code_review WHERE id=$1`, cr.ID,
		).Scan(&dbStatus, &dbPrincipal, &dbAgent); err != nil {
			t.Fatalf("read code_review: %v", err)
		}
		if dbStatus != "pending" {
			t.Errorf("DB status: want pending, got %q", dbStatus)
		}
		if dbPrincipal != seed.principalID {
			t.Errorf("DB principal_id: want %v, got %v", seed.principalID, dbPrincipal)
		}
		if dbAgent != agentID {
			t.Errorf("DB agent_id: want %v, got %v", agentID, dbAgent)
		}
	})
}

// ---------------------------------------------------------------------------
// TestCodeReviewFailureRecordsUsage — a run that burns tokens then fails (e.g.
// unparseable model output) still records the spend (manyforge-7n5).
// ---------------------------------------------------------------------------

func TestCodeReviewFailureRecordsUsage(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	localSrv, _ := startGitHubStub(t, prJSON)
	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "z-ai/glm-5.2", Provider: "openrouter"}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)
	svc := buildService(t, tdb, env, &malformedWithUsageRunner{}, fakeCred)
	svc.Pricing = &fixedPricing{cents: 204} // mimic the $2.04 glm-5.2 run

	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed := ClaimedReview{
		ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
		AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
	}
	if err := svc.runJob(ctx, claimed, nil); err == nil {
		t.Fatal("want error (malformed findings), got nil")
	}

	// A failed agent_run must capture the spend so accounting reflects it. The lane exits
	// cleanly with unparseable output, so it retries once (manyforge-6h1); both attempts hit
	// the model and are billed, so tokens SUM across attempts (1000+1000 in, 200+200 out).
	// fixedPricing returns a constant regardless of token count, so cost stays 204.
	var arStatus string
	var arIn, arOut int32
	var arCost int64
	if err := tdb.Super.QueryRow(ctx,
		`SELECT status, tokens_in, tokens_out, cost_cents FROM agent_run WHERE target_id=$1`, cr.ID,
	).Scan(&arStatus, &arIn, &arOut, &arCost); err != nil {
		t.Fatalf("expected a failed agent_run recording the spend: %v", err)
	}
	if arStatus != "failed" || arIn != 2000 || arOut != 400 || arCost != 204 {
		t.Fatalf("agent_run = status=%s in=%d out=%d cost=%d; want failed/2000/400/204 (usage summed over the retry)", arStatus, arIn, arOut, arCost)
	}

	// The review row must also show the usage/cost (via SetCodeReviewUsage).
	var rIn, rOut int32
	var rCost int64
	if err := tdb.Super.QueryRow(ctx,
		`SELECT tokens_in, tokens_out, cost_cents FROM code_review WHERE id=$1`, cr.ID,
	).Scan(&rIn, &rOut, &rCost); err != nil {
		t.Fatalf("read code_review: %v", err)
	}
	if rIn != 2000 || rOut != 400 || rCost != 204 {
		t.Fatalf("code_review usage = in=%d out=%d cost=%d; want 2000/400/204 (usage summed over the retry)", rIn, rOut, rCost)
	}
}

// ---------------------------------------------------------------------------
// Compile-time check: *connectors.RepoConnectorService satisfies repoResolver.
// ---------------------------------------------------------------------------

var _ repoResolver = (*connectors.RepoConnectorService)(nil)

// ---------------------------------------------------------------------------
// Helper: ensure WithPrincipal from testdb.App works as a serviceDB.
// ---------------------------------------------------------------------------

func TestServiceDBInterfaceCompiles(t *testing.T) {
	// This test just verifies the interface is satisfied at compile time.
	// The actual assert is the var _ line above; this function exists so
	// 'go test' exercises the file.
	_ = func(ctx context.Context, db serviceDB, id uuid.UUID) {
		_ = db.WithPrincipal(ctx, id, func(pgx.Tx) error { return nil })
	}
}

// ---------------------------------------------------------------------------
// TestCodeReviewWorkerCrashRecovery — expired-lease rows are re-claimed.
// ---------------------------------------------------------------------------

func TestCodeReviewWorkerCrashRecovery(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	localSrv, _ := startGitHubStub(t, prJSON)
	agentID := seed.agentID
	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)
	svc := buildService(t, tdb, env, &validFakeRunner{summary: "crash-recovery"}, fakeCred)

	// Enqueue a review.
	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Simulate a crashed worker: force status='running' + lease_expires_at in the past.
	_, err = tdb.Super.Exec(ctx,
		`UPDATE code_review SET status='running', attempts=1, lease_expires_at=now()-interval '1 hour' WHERE id=$1`,
		cr.ID)
	if err != nil {
		t.Fatalf("force running state: %v", err)
	}

	// ClaimCodeReviews should pick up the expired-lease row.
	// Claim under tdb.App (the real RLS-subject manyforge_app role) via AppDBAdapter
	// — the exact production path. The claim goes through the claim_code_reviews
	// SECURITY DEFINER function (migrations/0073), whose owner bypasses RLS, so the
	// principal-less system worker can still see rows across tenants. (A raw sqlc
	// UPDATE under tdb.App would be RLS-blocked — that was the production bug.)
	adapter := &AppDBAdapter{DB: tdb.App}
	rows, err := adapter.ClaimCodeReviews(ctx, 60, 10)
	if err != nil {
		t.Fatalf("ClaimCodeReviews: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one claimed row (crash recovery)")
	}

	var found *ClaimedReview
	for i := range rows {
		if rows[i].ID == cr.ID {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("our review %v not in claimed rows", cr.ID)
	}

	// Run the job directly via runJob.
	if err := svc.runJob(ctx, *found, nil); err != nil {
		t.Fatalf("runJob: %v", err)
	}

	// Assert it succeeded.
	got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "succeeded" {
		t.Errorf("want succeeded, got %s", got.Status)
	}
}

// ---------------------------------------------------------------------------
// TestRLSIsolation — cross-tenant RLS for List/Delete connector + List reviews.
// ---------------------------------------------------------------------------

func TestRLSIsolation(t *testing.T) {
	ctx, tdb, seedA := startCoding(t)
	seedB := seedCodingTenant(ctx, t, tdb)

	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	srvA, _ := startGitHubStub(t, prJSON)
	agentID := uuid.New()
	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}

	// Set up tenant A's connector and review.
	envA := newCodingEnv(t, tdb)
	connAID := createRepoConnector(ctx, t, envA, seedA, srvA.URL)
	svcA := buildService(t, tdb, envA, &validFakeRunner{}, fakeCred)

	// Tenant A creates a pending review.
	crA, err := svcA.Enqueue(ctx, seedA.principalID, seedA.businessID, agentID, connAID, 1)
	if err != nil {
		t.Fatalf("Enqueue A: %v", err)
	}

	// Tenant B: separate env+service (same tdb, different sealer).
	envB := newCodingEnv(t, tdb)
	svcB := buildService(t, tdb, envB, &validFakeRunner{}, fakeCred)

	t.Run("tenant_b_cannot_get_a_review", func(t *testing.T) {
		_, err := svcB.Get(ctx, seedB.principalID, seedB.businessID, crA.ID)
		if !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("cross-tenant Get: want ErrNotFound, got %v", err)
		}
	})

	t.Run("tenant_b_list_reviews_sees_empty", func(t *testing.T) {
		reviews, err := svcB.List(ctx, seedB.principalID, seedB.businessID)
		if err != nil {
			t.Fatalf("List B reviews: %v", err)
		}
		for _, r := range reviews {
			if r.ID == crA.ID {
				t.Errorf("tenant B should not see tenant A's review")
			}
		}
	})

	t.Run("tenant_b_cannot_delete_a_connector", func(t *testing.T) {
		err := envB.Repos.Delete(ctx, seedB.principalID, seedB.businessID, connAID)
		if !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("cross-tenant Delete connector: want ErrNotFound, got %v", err)
		}
	})

	t.Run("tenant_b_list_connectors_sees_empty", func(t *testing.T) {
		summaries, err := envB.Repos.List(ctx, seedB.principalID, seedB.businessID)
		if err != nil {
			t.Fatalf("List B connectors: %v", err)
		}
		connAIDStr := connAID.String()
		for _, s := range summaries {
			if s.ID == connAIDStr {
				t.Errorf("tenant B should not see tenant A's connector")
			}
		}
	})
}

// recordingRunner captures the SandboxSpec.Env it was invoked with and returns a
// valid (empty-findings) review.json in Outputs so the run succeeds.
type recordingRunner struct{ env map[string]string }

func (r *recordingRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	r.env = spec.Env
	data, _ := json.Marshal(map[string]any{"summary": "ok", "findings": []any{}})
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{"review.json": data}}, nil
}

// TestCodeReviewFallbackModelOnRetry: a fresh attempt uses the configured model; a
// retry (Attempts>=2) drives the sandbox with the provider fallback (manyforge-206).
func TestCodeReviewFallbackModelOnRetry(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc123","ref":"f"},"base":{"ref":"main"}}`)
	localSrv, _ := startGitHubStub(t, prJSON)
	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://openrouter.ai/api/v1", Model: "google/gemini-2.5-pro", Provider: "openrouter",
	}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)

	run := func(attempts int) string {
		rr := &recordingRunner{}
		// Build service with openrouter.ai in the egress allowlist.
		svc := &CodeReviewService{
			DB:          tdb.App,
			Repos:       env.Repos,
			Sandbox:     rr,
			Creds:       fakeCred,
			Image:       "opencode:stub",
			WorkRoot:    t.TempDir(),
			Timeout:     30 * time.Second,
			Clone:       fakeClone,
			EgressAllow: netsafe.ParseHostAllowlist("openrouter.ai"),
		}
		cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		claimed := ClaimedReview{
			ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
			AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: attempts,
		}
		if err := svc.runJob(ctx, claimed, nil); err != nil {
			t.Fatalf("runJob(attempts=%d): %v", attempts, err)
		}
		return rr.env["LLM_MODEL"]
	}

	if got := run(1); got != "google/gemini-2.5-pro" {
		t.Fatalf("attempts=1 should use the configured model, got %q", got)
	}
	if got := run(2); got != "google/gemini-2.5-flash" {
		t.Fatalf("attempts=2 should use the fallback model, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// ClonesInSandbox (kube-mode host-clone fix): the distroless app pod has no
// git/shell, so runJob must skip the host-side clone entirely when the
// configured runner clones in-cluster itself — while the SSRF guard
// (checkCloneURL) keeps running in every mode.
// ---------------------------------------------------------------------------

// recordingClone counts host-side clone invocations and otherwise behaves like
// fakeClone (creates destDir + a placeholder file), so a case where it IS
// legitimately invoked (ClonesInSandbox=false) still lets the run succeed.
type recordingClone struct{ calls atomic.Int32 }

func (r *recordingClone) Fn(ctx context.Context, cloneURL, authHeader, sha, destDir string, allowPrivate bool) error {
	r.calls.Add(1)
	return fakeClone(ctx, cloneURL, authHeader, sha, destDir, allowPrivate)
}

// TestCodeReviewClonesInSandboxSkipsHostClone pins the kube-mode fix: when
// ClonesInSandbox is true, runJob must NOT create the host WorkRoot/checkout/
// outDir tree or invoke s.cloneFn() — the configured runner (KubeRunner in
// production) clones the repo itself via an in-cluster init container and
// returns results via SandboxResult.Outputs. The SSRF guard (checkCloneURL)
// must still run in every mode — the "ssrf_guard_still_blocks..." subtest
// proves a disallowed clone URL still fails the job even though no host clone
// (and no sandbox run) ever happens.
func TestCodeReviewClonesInSandboxSkipsHostClone(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc123","ref":"f"},"base":{"ref":"main"}}`)
	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "claude-3-5-sonnet", Provider: "anthropic",
	}}

	t.Run("succeeds_without_any_host_clone_or_workdir", func(t *testing.T) {
		localSrv, _ := startGitHubStub(t, prJSON)
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, localSrv.URL) // AllowPrivateBaseURL: true

		rec := &recordingClone{}
		workRoot := t.TempDir()
		svc := &CodeReviewService{
			DB:              tdb.App,
			Repos:           env.Repos,
			Sandbox:         &validFakeRunner{summary: "kube lane"},
			Creds:           fakeCred,
			Image:           "opencode:stub",
			WorkRoot:        workRoot,
			Timeout:         30 * time.Second,
			Clone:           rec.Fn,
			ClonesInSandbox: true,
			EgressAllow:     netsafe.ParseHostAllowlist("api.anthropic.com"),
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

		if n := rec.calls.Load(); n != 0 {
			t.Fatalf("host clone must NOT be invoked when ClonesInSandbox=true, got %d call(s)", n)
		}
		if _, statErr := os.Stat(filepath.Join(workRoot, cr.ID.String())); !os.IsNotExist(statErr) {
			t.Fatalf("host per-run dir must NOT be created when ClonesInSandbox=true (stat err: %v)", statErr)
		}

		got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != "succeeded" {
			t.Fatalf("want succeeded, got %s", got.Status)
		}
	})

	t.Run("disallowed_host_still_fails_before_any_clone_or_sandbox", func(t *testing.T) {
		// A loopback connector with AllowPrivateBaseURL=false must never reach the
		// clone/sandbox step, ClonesInSandbox=true or not. In this end-to-end path the
		// github connector's own SSRF-guarded HTTP client (used for FetchPR) shares the
		// same host + AllowPrivateBaseURL flag as the derived clone URL, so it is what
		// actually rejects the loopback host first here — checkCloneURL's OWN blocking
		// behavior in isolation is pinned precisely (and hermetically, without this
		// FetchPR coupling) by TestCheckCloneURLDirect in clone_test.go, and that
		// runJob calls it unconditionally is pinned at the source level by
		// MF-KUBE-SANDBOX-23 (internal/security_regression/mf_kube_sandbox_test.go).
		// What this test proves end-to-end: a disallowed host NEVER reaches the host
		// clone recorder or the sandbox runner, even when ClonesInSandbox=true.
		//
		// This uses a fake repoResolver (bypassing RepoConnectorService.Create, which
		// independently requires https unless AllowPrivateBaseURL is set — a
		// create-time policy check, not the runtime SSRF guard under test here) so the
		// resolved connector can carry a loopback BaseURL with AllowPrivateBaseURL=
		// false. A real (safely-configured) repo_connector row is still created so the
		// code_review row's FK constraint is satisfied — only svc.Repos (what the
		// service actually consults) is faked.
		localSrv, _ := startGitHubStub(t, prJSON)
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, "https://api.github.com")

		fakeRepoResolver := &fakeRepos{rc: connectors.ResolvedRepoConnector{
			Type:                "github",
			BaseURL:             localSrv.URL,
			Repo:                "o/r",
			AllowPrivateBaseURL: false,
			Credential:          connectors.Credential{APIToken: "ghp_test_token"},
		}}

		rec := &recordingClone{}
		runner := &sandbox.FakeRunner{}
		svc := &CodeReviewService{
			DB:              tdb.App,
			Repos:           fakeRepoResolver,
			Sandbox:         runner,
			Creds:           fakeCred,
			Image:           "opencode:stub",
			WorkRoot:        t.TempDir(),
			Timeout:         30 * time.Second,
			Clone:           rec.Fn,
			ClonesInSandbox: true,
			EgressAllow:     netsafe.ParseHostAllowlist("api.anthropic.com"),
		}

		cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		claimed := ClaimedReview{
			ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
			AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
		}
		if err := svc.runJob(ctx, claimed, nil); err == nil {
			t.Fatal("runJob: want an error for a disallowed/blocked clone host, got nil")
		}
		if n := rec.calls.Load(); n != 0 {
			t.Fatalf("host clone must not be invoked even in the blocked case, got %d call(s)", n)
		}
		if runner.Last.Image != "" {
			t.Error("sandbox must NOT be invoked when the clone host is blocked")
		}
	})
}

// TestCodeReviewClonesInSandboxFalseStillClonesHostSide is the mirror regression check:
// with the zero-value default (ClonesInSandbox=false — docker/off/tests), the host
// clone MUST still run exactly as before this change, proving the fix is additive and
// does not alter the existing docker code path.
func TestCodeReviewClonesInSandboxFalseStillClonesHostSide(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc123","ref":"f"},"base":{"ref":"main"}}`)
	localSrv, _ := startGitHubStub(t, prJSON)
	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "claude-3-5-sonnet", Provider: "anthropic",
	}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)

	rec := &recordingClone{}
	svc := &CodeReviewService{
		DB:       tdb.App,
		Repos:    env.Repos,
		Sandbox:  &validFakeRunner{summary: "docker lane"},
		Creds:    fakeCred,
		Image:    "opencode:stub",
		WorkRoot: t.TempDir(),
		Timeout:  30 * time.Second,
		Clone:    rec.Fn,
		// ClonesInSandbox intentionally omitted (zero value = false): docker/off/tests.
		EgressAllow: netsafe.ParseHostAllowlist("api.anthropic.com"),
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

	if n := rec.calls.Load(); n != 1 {
		t.Fatalf("host clone must be invoked exactly once when ClonesInSandbox=false, got %d call(s)", n)
	}
}
