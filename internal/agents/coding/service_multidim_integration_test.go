//go:build integration

package coding

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// perDimRunner is a fake sandbox runner for the multi-dimension fan-out: it reads the per-lane
// review_instructions.txt (which the cloud lane writes with the dimension's prompt) to learn
// WHICH dimension it is reviewing, then writes a dimension-specific finding into that lane's
// review.json. This lets one runner drive N lanes with distinct findings, exercising the real
// per-lane sandbox output dirs + aggregation + tagging.
type perDimRunner struct{}

func (r *perDimRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	instr, _ := os.ReadFile(filepath.Join(spec.OutputDir, "review_instructions.txt"))
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
	if err := os.WriteFile(filepath.Join(spec.OutputDir, "review.json"), data, 0o644); err != nil {
		return sandbox.SandboxResult{}, err
	}
	return sandbox.SandboxResult{ExitCode: 0}, nil
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

	// Findings from the two ran lanes are present and tagged by dimension.
	tags := map[string]bool{}
	for _, f := range got.Findings {
		tags[f.Dimension] = true
	}
	if !tags["security"] || !tags["correctness"] {
		t.Fatalf("findings must be tagged by their dimension; got tags %v (findings=%+v)", tags, got.Findings)
	}

	// dimension_runs records all three lanes: two succeeded, ui skipped (scope: no files).
	var raw []byte
	if err := tdb.Super.QueryRow(ctx, `SELECT dimension_runs FROM code_review WHERE id=$1`, cr.ID).Scan(&raw); err != nil {
		t.Fatalf("read dimension_runs: %v", err)
	}
	var runs []struct {
		Dimension    string `json:"dimension"`
		Status       string `json:"status"`
		FindingCount int    `json:"finding_count"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil {
		t.Fatalf("unmarshal dimension_runs %q: %v", raw, err)
	}
	byDim := map[string]string{}
	for _, r := range runs {
		byDim[r.Dimension] = r.Status
	}
	if byDim["security"] != "succeeded" || byDim["correctness"] != "succeeded" {
		t.Fatalf("ran lanes must be recorded succeeded; got %v", byDim)
	}
	if byDim["ui"] != "skipped" {
		t.Fatalf("the scoped-out ui dimension must be recorded as skipped, not silently dropped; got %v", byDim)
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
