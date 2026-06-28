//go:build integration

package coding

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestCodeReviewClaimUnderAppRole is the regression test for the production
// correctness bug fixed by migrations/0073: the CodeReviewWorker claims pending
// rows principal-less (no manyforge.principal_id GUC), but code_review has RLS
// ENABLEd (0071) and the app connects as manyforge_app (NOSUPERUSER NOBYPASSRLS).
// A raw sqlc UPDATE under that role is RLS-blocked because authorized_businesses(NULL)
// returns EMPTY (0007_rls) → the worker claimed ZERO rows in production.
//
// This test claims via *AppDBAdapter wired to tdb.App — the *real* RLS-subject
// manyforge_app role, exactly as production runs — and asserts the claim RETURNS
// the seeded row and that runJob then drives it to 'succeeded'.
//
// BEFORE the fix (raw sqlc claim under tdb.App): the claim returns 0 rows and this
// test fails at "claim returned no rows". AFTER the fix (claim_code_reviews
// SECURITY DEFINER, owner bypasses RLS): the claim returns the row and runJob
// reaches 'succeeded'.
func TestCodeReviewClaimUnderAppRole(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc123","ref":"f"},"base":{"ref":"main"}}`)
	localSrv, _ := startGitHubStub(t, prJSON)
	// Use the REAL seeded agent row (seed.agentID). The success-path finalize records
	// an agent_run via CreateCodeReviewAgentRun, whose INSERT/SELECT FROM agent yields
	// no row for a foreign/non-existent agent — a fabricated uuid.New() here made
	// runJob fail with ErrNoRows on the success path (pre-existing fixture bug).
	agentID := seed.agentID
	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic",
	}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)
	svc := buildService(t, tdb, env, &validFakeRunner{summary: "claimed-under-app-role"}, fakeCred)

	// Seed a pending review under the tenant (status='pending', run_after<=now()).
	cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if cr.Status != "pending" {
		t.Fatalf("want pending, got %s", cr.Status)
	}

	// Claim via the production path: *AppDBAdapter over tdb.App (manyforge_app,
	// RLS-subject). The claim must RETURN the row — this is exactly what was broken.
	adapter := &AppDBAdapter{DB: tdb.App}
	rows, err := adapter.ClaimCodeReviews(ctx, 900, 10)
	if err != nil {
		t.Fatalf("ClaimCodeReviews under manyforge_app: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("claim returned no rows — principal-less claim under manyforge_app is RLS-blocked (the production bug)")
	}

	var found *ClaimedReview
	for i := range rows {
		if rows[i].ID == cr.ID {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("seeded review %v not in claimed rows", cr.ID)
	}
	if found.Attempts != 1 {
		t.Errorf("claimed attempts: want 1 (incremented), got %d", found.Attempts)
	}

	// runJob must drive the claimed row to 'succeeded'.
	if err := svc.runJob(ctx, *found); err != nil {
		t.Fatalf("runJob: %v", err)
	}
	got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "succeeded" {
		t.Errorf("want succeeded after claim+runJob, got %s", got.Status)
	}
}

// TestCodeReviewRequeueAndFailUnderAppRole verifies requeue/fail also work under
// the real manyforge_app role via the SECURITY DEFINER functions — the other two
// principal-less queue mutations that would otherwise be RLS-blocked.
func TestCodeReviewRequeueAndFailUnderAppRole(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc123","ref":"f"},"base":{"ref":"main"}}`)
	localSrv, _ := startGitHubStub(t, prJSON)
	agentID := uuid.New()
	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic",
	}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)
	svc := buildService(t, tdb, env, &validFakeRunner{}, fakeCred)
	adapter := &AppDBAdapter{DB: tdb.App}

	// Requeue path: claim then requeue back to pending.
	crReq, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue (requeue): %v", err)
	}
	if _, err := adapter.ClaimCodeReviews(ctx, 900, 10); err != nil {
		t.Fatalf("claim (requeue): %v", err)
	}
	if err := adapter.RequeueCodeReview(ctx, crReq.ID, 30, "transient"); err != nil {
		t.Fatalf("RequeueCodeReview under manyforge_app: %v", err)
	}
	if st := readStatus(ctx, t, tdb, crReq.ID); st != "pending" {
		t.Errorf("after requeue: want pending, got %s", st)
	}

	// Fail path: claim then fail terminally.
	crFail, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
	if err != nil {
		t.Fatalf("Enqueue (fail): %v", err)
	}
	if _, err := adapter.ClaimCodeReviews(ctx, 900, 10); err != nil {
		t.Fatalf("claim (fail): %v", err)
	}
	if err := adapter.FailCodeReview(ctx, crFail.ID, "terminal"); err != nil {
		t.Fatalf("FailCodeReview under manyforge_app: %v", err)
	}
	if st := readStatus(ctx, t, tdb, crFail.ID); st != "failed" {
		t.Errorf("after fail: want failed, got %s", st)
	}
}

// readStatus reads a code_review status via the superuser pool (read-only assert
// helper; bypasses RLS so the test can inspect any tenant's row directly).
func readStatus(ctx context.Context, t *testing.T, tdb *testdb.TestDB, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := tdb.Super.QueryRow(ctx, `SELECT status FROM code_review WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}
