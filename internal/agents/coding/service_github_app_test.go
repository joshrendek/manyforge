//go:build integration

package coding

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
	"github.com/manyforge/manyforge/internal/connectors/github"
	"github.com/manyforge/manyforge/internal/githubapp"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// fakeAppTokens is the integration-test double for installationTokenSource: it returns
// a fixed ghs_ token and records the (installation id, repo) the mint was scoped to.
type fakeAppTokens struct {
	tok       string
	gotInstID int64
	gotRepo   string
	calls     int
}

func (f *fakeAppTokens) Token(_ context.Context, instID int64, repo string) (string, error) {
	f.calls++
	f.gotInstID, f.gotRepo = instID, repo
	return f.tok, nil
}

// linkInstallation upserts + links a GitHub App installation to the seeded business/
// agent and ingests one pull_request review via the enqueuer, returning the created
// app-backed connector id (repointed at the stub) and the pending review id. This is
// the same principal-less path the webhook handler drives (handlePullRequestEvent →
// ResolveInstallation → IngestPRReview), exercised directly from the coding package.
func linkInstallation(ctx context.Context, t *testing.T, tdb *testdb.TestDB, seed codingSeed, installationID int64, stubURL, headSHA string) (uuid.UUID, uuid.UUID, githubapp.InstallationContext) {
	t.Helper()
	installSvc := &githubapp.InstallationService{DB: tdb.App}
	if err := installSvc.UpsertFromEvent(ctx, installationID, "o", "Organization"); err != nil {
		t.Fatalf("upsert installation: %v", err)
	}
	if err := installSvc.Link(ctx, installationID, seed.businessID, seed.agentID); err != nil {
		t.Fatalf("link installation: %v", err)
	}
	enq := &githubapp.PRReviewEnqueuer{DB: tdb.App}
	ic, found, err := enq.ResolveInstallation(ctx, installationID)
	if err != nil || !found {
		t.Fatalf("resolve installation: found=%v err=%v", found, err)
	}
	reviewID, ok, err := enq.IngestPRReview(ctx, githubapp.PRReviewInput{
		InstallationID:   installationID,
		DeliveryID:       "delivery-e2e",
		Repo:             "o/r",
		PRNumber:         1,
		HeadSHA:          headSHA,
		BusinessID:       ic.BusinessID,
		TenantRootID:     ic.TenantRootID,
		AgentID:          ic.AgentID,
		AgentPrincipalID: ic.AgentPrincipalID,
	})
	if err != nil || !ok {
		t.Fatalf("ingest pr review: ok=%v err=%v", ok, err)
	}
	// The DEFINER creates the connector with base_url=https://api.github.com and
	// allow_private_base_url=false; repoint it at the loopback stub for the test.
	var connID uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM repo_connector WHERE business_id=$1 AND repo='o/r' AND type='github_app'", seed.businessID,
	).Scan(&connID); err != nil {
		t.Fatalf("lookup app connector: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx,
		"UPDATE repo_connector SET base_url=$1, allow_private_base_url=true WHERE id=$2", stubURL, connID); err != nil {
		t.Fatalf("repoint connector: %v", err)
	}
	return connID, reviewID, ic
}

// TestRunJobAppBackedEndToEnd drives the full app-triggered path against real Postgres:
// a linked installation + ingested pending review, then runJob with a FakeRunner + a
// fake token source. It asserts the minted ghs_ token reached the clone auth header AND
// the sandbox spec, that PostReview fired, and that the resolved model was stamped
// (fable M2 — app reviews enqueue with model=”).
func TestRunJobAppBackedEndToEnd(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	const headSHA = "abc123deadbeef"
	prJSON := []byte(`{"number":1,"title":"PR","state":"open","merged":false,"head":{"sha":"` + headSHA + `","ref":"feature"},"base":{"ref":"main"}}`)
	srv, stub := startGitHubStub(t, prJSON)
	env := newCodingEnv(t, tdb)

	const installationID int64 = 55220
	connID, reviewID, ic := linkInstallation(ctx, t, tdb, seed, installationID, srv.URL, headSHA)

	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "anthropic/claude-3-5-sonnet", Provider: "anthropic",
	}}
	validReview, _ := json.Marshal(map[string]any{
		"summary": "LGTM", "findings": []map[string]any{{"file": "main.go", "line": 10, "severity": "warning", "title": "t", "detail": "d"}},
	})
	// sandbox.FakeRunner returns the canned review AND records the last spec (so we can
	// assert the minted token reached CloneAuthHeader).
	runner := &sandbox.FakeRunner{Result: sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{"review.json": validReview}}}
	svc := buildService(t, tdb, env, runner, fakeCred)
	tokens := &fakeAppTokens{tok: "ghs_test"}
	svc.Tokens = tokens

	job := ClaimedReview{
		ID: reviewID, BusinessID: ic.BusinessID, PrincipalID: ic.AgentPrincipalID,
		AgentID: ic.AgentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
	}
	if err := svc.runJob(ctx, job, nil); err != nil {
		t.Fatalf("runJob: %v", err)
	}

	// The mint ran scoped to this installation + repo.
	if tokens.calls != 1 || tokens.gotInstID != installationID || tokens.gotRepo != "o/r" {
		t.Fatalf("mint: calls=%d instID=%d repo=%q, want 1/%d/o/r", tokens.calls, tokens.gotInstID, tokens.gotRepo, installationID)
	}
	// The minted token reached the clone auth on the sandbox spec.
	wantAuth := github.BasicAuthHeader("ghs_test")
	if runner.Last.CloneAuthHeader != wantAuth {
		t.Fatalf("sandbox CloneAuthHeader = %q, want the minted-token header %q", runner.Last.CloneAuthHeader, wantAuth)
	}
	// PostReview fired exactly once.
	if n := stub.postCount.Load(); n != 1 {
		t.Fatalf("want exactly 1 GitHub review POST, got %d", n)
	}
	// The review succeeded and the resolved model was stamped (fable M2).
	got, err := svc.Get(ctx, ic.AgentPrincipalID, ic.BusinessID, reviewID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded", got.Status)
	}
	if got.Model != "anthropic/claude-3-5-sonnet" {
		t.Errorf("model = %q, want the resolved review model (fable M2 stamp)", got.Model)
	}
	// manyforge-nh6: an app-backed review opens exactly one in-progress check run on
	// the PR head and resolves it to success on completion.
	if n := stub.checkCreateN.Load(); n != 1 {
		t.Fatalf("want 1 check-run create, got %d", n)
	}
	var created map[string]any
	if err := json.Unmarshal(stub.checkRunCreates[0], &created); err != nil {
		t.Fatalf("unmarshal check-run create: %v", err)
	}
	if created["status"] != "in_progress" || created["head_sha"] != headSHA {
		t.Errorf("check-run create body = %+v, want status=in_progress head_sha=%s", created, headSHA)
	}
	if n := stub.checkUpdateN.Load(); n != 1 {
		t.Fatalf("want 1 check-run update, got %d", n)
	}
	var updated map[string]any
	if err := json.Unmarshal(stub.checkRunUpdates[0], &updated); err != nil {
		t.Fatalf("unmarshal check-run update: %v", err)
	}
	if updated["status"] != "completed" || updated["conclusion"] != "success" {
		t.Errorf("check-run update body = %+v, want status=completed conclusion=success", updated)
	}
	// The success summary reports the finding count (PR #20 review: exercise the
	// success-summary branch, not just the conclusion).
	out, _ := updated["output"].(map[string]any)
	if summ, _ := out["summary"].(string); !strings.Contains(summ, "finding") {
		t.Errorf("check-run success summary = %q, want it to mention the finding count", summ)
	}
}

// panicRunner panics inside the sandbox Run — since lanes run synchronously on
// runJob's stack, this exercises the deferred resolver's recover() path (PR #20
// review: panic-specific failure handling was untested).
type panicRunner struct{}

func (panicRunner) Run(context.Context, sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	panic("boom in sandbox")
}

// TestRunJobAppBacked_CheckRunResolvesOnPanic pins that a panic on runJob's stack
// resolves the in-progress check run to failure (not a misleading success) and is
// re-raised so the worker still observes the crash (manyforge-nh6, PR #20 review).
func TestRunJobAppBacked_CheckRunResolvesOnPanic(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	const headSHA = "abc123deadbeef"
	prJSON := []byte(`{"number":1,"title":"PR","state":"open","merged":false,"head":{"sha":"` + headSHA + `","ref":"feature"},"base":{"ref":"main"}}`)
	srv, stub := startGitHubStub(t, prJSON)
	env := newCodingEnv(t, tdb)

	const installationID int64 = 55222
	connID, reviewID, ic := linkInstallation(ctx, t, tdb, seed, installationID, srv.URL, headSHA)

	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "anthropic/claude-3-5-sonnet", Provider: "anthropic",
	}}
	svc := buildService(t, tdb, env, panicRunner{}, fakeCred)
	svc.Tokens = &fakeAppTokens{tok: "ghs_test"}

	job := ClaimedReview{
		ID: reviewID, BusinessID: ic.BusinessID, PrincipalID: ic.AgentPrincipalID,
		AgentID: ic.AgentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("runJob must re-raise the panic after resolving the check run")
			}
		}()
		_ = svc.runJob(ctx, job, nil)
	}()

	if n := stub.checkCreateN.Load(); n != 1 {
		t.Fatalf("want 1 check-run create, got %d", n)
	}
	if n := stub.checkUpdateN.Load(); n != 1 {
		t.Fatalf("want 1 check-run update (resolve on panic), got %d", n)
	}
	var updated map[string]any
	if err := json.Unmarshal(stub.checkRunUpdates[0], &updated); err != nil {
		t.Fatalf("unmarshal check-run update: %v", err)
	}
	if updated["conclusion"] != "failure" {
		t.Errorf("check-run conclusion = %v, want failure", updated["conclusion"])
	}
}

// TestRunJobAppBacked_CheckRunResolvesFailure pins that the in-progress check run
// opened at the start of an app-backed review still resolves — to conclusion
// "failure" — when the run fails partway through (manyforge-nh6). The deferred
// update must fire on the failJob* exit path, not only on success.
func TestRunJobAppBacked_CheckRunResolvesFailure(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	const headSHA = "abc123deadbeef"
	prJSON := []byte(`{"number":1,"title":"PR","state":"open","merged":false,"head":{"sha":"` + headSHA + `","ref":"feature"},"base":{"ref":"main"}}`)
	srv, stub := startGitHubStub(t, prJSON)
	env := newCodingEnv(t, tdb)

	const installationID int64 = 55221
	connID, reviewID, ic := linkInstallation(ctx, t, tdb, seed, installationID, srv.URL, headSHA)

	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "anthropic/claude-3-5-sonnet", Provider: "anthropic",
	}}
	// malformedFakeRunner returns unparseable review.json → runJob fails after the
	// check run was already opened.
	svc := buildService(t, tdb, env, &malformedFakeRunner{}, fakeCred)
	svc.Tokens = &fakeAppTokens{tok: "ghs_test"}

	job := ClaimedReview{
		ID: reviewID, BusinessID: ic.BusinessID, PrincipalID: ic.AgentPrincipalID,
		AgentID: ic.AgentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
	}
	if err := svc.runJob(ctx, job, nil); err == nil {
		t.Fatal("runJob: want error from malformed review output")
	}

	if n := stub.checkCreateN.Load(); n != 1 {
		t.Fatalf("want 1 check-run create, got %d", n)
	}
	if n := stub.checkUpdateN.Load(); n != 1 {
		t.Fatalf("want 1 check-run update (resolve on failure), got %d", n)
	}
	var updated map[string]any
	if err := json.Unmarshal(stub.checkRunUpdates[0], &updated); err != nil {
		t.Fatalf("unmarshal check-run update: %v", err)
	}
	if updated["conclusion"] != "failure" {
		t.Errorf("check-run conclusion = %v, want failure", updated["conclusion"])
	}
}

// TestRunJobFailLifecycle pins the fable C1 fix: a runJob failure must land the row at
// 'failed' (proving fail()'s dimension_runs fix — a nil jsonb would 23502-abort the
// UPDATE and leave the row stuck 'running'), the requeue DEFINER must then flip
// 'failed'→'pending' (the status IN ('running','failed') guard), and a terminal
// fail_code_review must persist last_error.
func TestRunJobFailLifecycle(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	prJSON := []byte(`{"number":1,"title":"PR","state":"open","merged":false,"head":{"sha":"deadbeef","ref":"f"},"base":{"ref":"main"}}`)
	srv, _ := startGitHubStub(t, prJSON)
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, srv.URL)
	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	svc := buildService(t, tdb, env, &malformedFakeRunner{}, fakeCred)

	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Claim (pending → running, attempts=1) via the DEFINER, mirroring the worker.
	if _, err := tdb.Super.Exec(ctx, "SELECT claim_code_reviews($1,$2)", 300, 100); err != nil {
		t.Fatalf("claim: %v", err)
	}

	job := ClaimedReview{
		ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
		AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
	}
	if err := svc.runJob(ctx, job, nil); err == nil {
		t.Fatal("runJob with malformed output: want error, got nil")
	}
	// fable C1: without the dimension_runs fix the fail() UPDATE 23502-aborts and the
	// row stays 'running'. It must be 'failed'.
	if got := reviewStatus(ctx, t, tdb, cr.ID); got != "failed" {
		t.Fatalf("after runJob failure: status = %q, want failed (fail() must reach 'failed', not stay 'running')", got)
	}
	// requeue guard allows failed → pending.
	if _, err := tdb.Super.Exec(ctx, "SELECT requeue_code_review($1,$2,$3)", cr.ID, 0, "retriable"); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if got := reviewStatus(ctx, t, tdb, cr.ID); got != "pending" {
		t.Fatalf("after requeue: status = %q, want pending", got)
	}
	// Re-claim then terminally fail (attempts exhausted) — last_error must persist.
	if _, err := tdb.Super.Exec(ctx, "SELECT claim_code_reviews($1,$2)", 300, 100); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx, "SELECT fail_code_review($1,$2)", cr.ID, "terminal boom"); err != nil {
		t.Fatalf("fail terminal: %v", err)
	}
	var status, lastErr string
	if err := tdb.Super.QueryRow(ctx, "SELECT status, coalesce(last_error,'') FROM code_review WHERE id=$1", cr.ID).Scan(&status, &lastErr); err != nil {
		t.Fatalf("lookup terminal: %v", err)
	}
	if status != "failed" || lastErr != "terminal boom" {
		t.Fatalf("terminal: status=%q last_error=%q, want failed/'terminal boom'", status, lastErr)
	}
}

// TestRunJobClaimTimeReCheckSkips pins fable C2: when a sibling already reviewed this
// exact (connector, pr, head), the claim-time re-check finalizes the row 'succeeded'
// WITHOUT posting — and finalizeSkipped nulls lease_expires_at so claim_code_reviews
// can never re-claim it (no infinite loop).
func TestRunJobClaimTimeReCheckSkips(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	const headSHA = "sharedhead999"
	prJSON := []byte(`{"number":1,"title":"PR","state":"open","merged":false,"head":{"sha":"` + headSHA + `","ref":"f"},"base":{"ref":"main"}}`)
	srv, stub := startGitHubStub(t, prJSON)
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, srv.URL)

	// Seed a SUCCEEDED sibling for the same (connector, pr, head) — the row a rapid-push
	// race would have produced. jsonb columns are non-null ('[]').
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO code_review (id, business_id, tenant_root_id, repo_connector_id, pr_number, head_sha,
		    status, principal_id, agent_id, model, findings, dimension_runs, created_at, updated_at)
		 VALUES (gen_random_uuid(), $1, $1, $2, 1, $3, 'succeeded', $4, $5, '', '[]', '[]', now(), now())`,
		seed.businessID, connID, headSHA, seed.principalID, seed.agentID); err != nil {
		t.Fatalf("seed succeeded sibling: %v", err)
	}

	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	// This runner would produce a normal review if reached — the point is it must NOT be.
	svc := buildService(t, tdb, env, &validFakeRunner{summary: "should-not-post"}, fakeCred)

	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Claim the under-test row so lease_expires_at is set — finalizeSkipped must null it.
	if _, err := tdb.Super.Exec(ctx, "SELECT claim_code_reviews($1,$2)", 300, 100); err != nil {
		t.Fatalf("claim: %v", err)
	}

	job := ClaimedReview{
		ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
		AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: 1, Attempts: 1,
	}
	if err := svc.runJob(ctx, job, nil); err != nil {
		t.Fatalf("runJob (expected skip): %v", err)
	}

	var status string
	var leaseNull bool
	if err := tdb.Super.QueryRow(ctx,
		"SELECT status, lease_expires_at IS NULL FROM code_review WHERE id=$1", cr.ID).Scan(&status, &leaseNull); err != nil {
		t.Fatalf("lookup under-test row: %v", err)
	}
	if status != "succeeded" {
		t.Errorf("skipped review status = %q, want succeeded", status)
	}
	if !leaseNull {
		t.Error("finalizeSkipped must NULL lease_expires_at (else claim_code_reviews re-claims the expired-running row forever)")
	}
	// The claim-time skip must NOT post a review (the sibling already did).
	if n := stub.postCount.Load(); n != 0 {
		t.Fatalf("skipped review must not PostReview; got %d posts", n)
	}
}

// reviewStatus reads a code_review's status (superuser, RLS-bypassed).
func reviewStatus(ctx context.Context, t *testing.T, tdb *testdb.TestDB, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := tdb.Super.QueryRow(ctx, "SELECT status FROM code_review WHERE id=$1", id).Scan(&s); err != nil {
		t.Fatalf("lookup status: %v", err)
	}
	return s
}
