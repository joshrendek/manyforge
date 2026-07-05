//go:build integration

package githubapp_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/githubapp"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestPRReviewIngest exercises the migrations/0084 principal-less path end to
// end: installation resolution, then the atomic github_pr_review_ingest
// DEFINER's six steps (delivery dedup, hourly rate cap, ensure-connector,
// same-head skip, pending-supersede, insert) against a real Postgres.
func TestPRReviewIngest(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb.Start: %v", err)
	}
	defer tdb.Close(ctx)

	bizA, agentA, _, _, _, _ := seedTwoBusinesses(t, ctx, tdb)

	var agentPrincipal uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT principal_id FROM agent WHERE id=$1", agentA).Scan(&agentPrincipal); err != nil {
		t.Fatalf("lookup agent principal: %v", err)
	}

	const installationID int64 = 991122
	installSvc := &githubapp.InstallationService{DB: tdb.App}
	if err := installSvc.UpsertFromEvent(ctx, installationID, "prreview-org", "Organization"); err != nil {
		t.Fatalf("upsert installation: %v", err)
	}
	if err := installSvc.Link(ctx, installationID, bizA, agentA); err != nil {
		t.Fatalf("link installation: %v", err)
	}

	enqueuer := &githubapp.PRReviewEnqueuer{DB: tdb.App}

	// --- ResolveInstallation: linked installation resolves business/agent/principal. ---
	rctx, found, err := enqueuer.ResolveInstallation(ctx, installationID)
	if err != nil {
		t.Fatalf("resolve installation: %v", err)
	}
	if !found {
		t.Fatalf("resolve installation: found=false, want true")
	}
	if rctx.BusinessID != bizA {
		t.Fatalf("business_id = %v, want %v", rctx.BusinessID, bizA)
	}
	if rctx.TenantRootID != bizA {
		t.Fatalf("tenant_root_id = %v, want %v (master business is its own tenant root)", rctx.TenantRootID, bizA)
	}
	if rctx.AgentID != agentA {
		t.Fatalf("agent_id = %v, want %v", rctx.AgentID, agentA)
	}
	if rctx.AgentPrincipalID != agentPrincipal {
		t.Fatalf("agent_principal_id = %v, want %v", rctx.AgentPrincipalID, agentPrincipal)
	}
	if !rctx.AgentEnabled || !rctx.Enabled || rctx.Suspended {
		t.Fatalf("agent_enabled/enabled/suspended = %v/%v/%v, want true/true/false", rctx.AgentEnabled, rctx.Enabled, rctx.Suspended)
	}

	repo := "prreview-org/widgets"
	baseIn := func() githubapp.PRReviewInput {
		return githubapp.PRReviewInput{
			InstallationID:   installationID,
			Repo:             repo,
			PRNumber:         1,
			BusinessID:       rctx.BusinessID,
			TenantRootID:     rctx.TenantRootID,
			AgentID:          rctx.AgentID,
			AgentPrincipalID: rctx.AgentPrincipalID,
		}
	}

	// --- First ingest: creates the app-backed connector + a pending review. ---
	in1 := baseIn()
	in1.DeliveryID = "delivery-1"
	in1.HeadSHA = "sha-1"
	review1, ok, err := enqueuer.IngestPRReview(ctx, in1)
	if err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	if !ok || review1 == uuid.Nil {
		t.Fatalf("ingest 1: ok=%v review=%v, want ok=true and a real id", ok, review1)
	}

	var connID uuid.UUID
	var connType, connStatus string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id, type, status FROM repo_connector WHERE business_id=$1 AND repo=$2", bizA, repo,
	).Scan(&connID, &connType, &connStatus); err != nil {
		t.Fatalf("lookup connector: %v", err)
	}
	if connType != "github_app" || connStatus != "enabled" {
		t.Fatalf("connector type/status = %s/%s, want github_app/enabled", connType, connStatus)
	}
	assertReviewStatus(t, ctx, tdb, review1, "pending")

	// --- Replay: same delivery id -> ok=false, no new review. ---
	if _, ok, err := enqueuer.IngestPRReview(ctx, in1); err != nil || ok {
		t.Fatalf("replay ingest: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	assertReviewCount(t, ctx, tdb, connID, 1)

	// --- Dup: new delivery id, same (repo, pr, head_sha) still pending -> ok=false. ---
	in2 := baseIn()
	in2.DeliveryID = "delivery-2"
	in2.HeadSHA = "sha-1"
	if _, ok, err := enqueuer.IngestPRReview(ctx, in2); err != nil || ok {
		t.Fatalf("dup ingest: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	assertReviewCount(t, ctx, tdb, connID, 1)

	// --- New head with a still-pending prior review -> prior superseded, new pending. ---
	in3 := baseIn()
	in3.DeliveryID = "delivery-3"
	in3.HeadSHA = "sha-2"
	review3, ok, err := enqueuer.IngestPRReview(ctx, in3)
	if err != nil {
		t.Fatalf("ingest 3 (new head): %v", err)
	}
	if !ok || review3 == uuid.Nil || review3 == review1 {
		t.Fatalf("ingest 3: ok=%v review=%v, want ok=true and a NEW id", ok, review3)
	}
	assertReviewStatus(t, ctx, tdb, review1, "superseded")
	assertReviewStatus(t, ctx, tdb, review3, "pending")

	// --- Rate cap: pad to 30 reviews in the last hour for this installation, then
	// the 31st (a wholly novel repo/pr/head/delivery — no dedup/same-head reason to
	// block) must still be rejected on the rate cap alone. ---
	// 2 reviews already exist (review1, superseded, and review3, pending) — 28 more
	// brings the installation's rolling-hour count to exactly the 30 cap.
	for i := range 28 {
		in := baseIn()
		in.DeliveryID = fmt.Sprintf("delivery-pad-%d", i)
		in.PRNumber = 1000 + i
		in.HeadSHA = fmt.Sprintf("sha-pad-%d", i)
		if _, ok, err := enqueuer.IngestPRReview(ctx, in); err != nil || !ok {
			t.Fatalf("pad ingest %d: ok=%v err=%v, want ok=true", i, ok, err)
		}
	}
	assertReviewCount(t, ctx, tdb, connID, 30)

	over := baseIn()
	over.DeliveryID = "delivery-over-cap"
	over.PRNumber = 9999
	over.HeadSHA = "sha-over-cap"
	if _, ok, err := enqueuer.IngestPRReview(ctx, over); err != nil || ok {
		t.Fatalf("over-cap ingest: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

// assertReviewStatus asserts the code_review row id has the given status.
func assertReviewStatus(t *testing.T, ctx context.Context, tdb *testdb.TestDB, id uuid.UUID, want string) {
	t.Helper()
	var got string
	if err := tdb.Super.QueryRow(ctx, "SELECT status FROM code_review WHERE id=$1", id).Scan(&got); err != nil {
		t.Fatalf("lookup review %s status: %v", id, err)
	}
	if got != want {
		t.Fatalf("review %s status = %s, want %s", id, got, want)
	}
}

// assertReviewCount asserts the total number of code_review rows attached to
// connID (any status — dedup/supersede/rate all operate over the full set).
func assertReviewCount(t *testing.T, ctx context.Context, tdb *testdb.TestDB, connID uuid.UUID, want int) {
	t.Helper()
	var got int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM code_review WHERE repo_connector_id=$1", connID).Scan(&got); err != nil {
		t.Fatalf("count reviews: %v", err)
	}
	if got != want {
		t.Fatalf("review count = %d, want %d", got, want)
	}
}
