//go:build integration

package coding

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
)

// verifyDropRunner emits one finding per dimension lane, and for the verify lane confirms ONLY the
// security finding — so the correctness finding must be dropped by the verify pass. The verify lane
// is discriminated by the "VERIFICATION pass" marker its prompt carries (buildVerifyPrompt).
type verifyDropRunner struct{}

func (r *verifyDropRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	instr := string(spec.Inputs["review_instructions.txt"])
	usage := `[{"cost":0.02,"input":1000,"output":50,"reasoning":10,"cache_read":40000,"cache_write":0}]`
	mk := func(findings []map[string]any) (sandbox.SandboxResult, error) {
		data, _ := json.Marshal(map[string]any{"summary": "s", "findings": findings})
		return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{
			"review.json": data, "usage.json": []byte(usage),
		}}, nil
	}
	if strings.Contains(instr, "VERIFICATION pass") {
		// Confirm only the security finding — echo file/line/severity/title verbatim.
		return mk([]map[string]any{
			{"file": "main.go", "line": 1, "severity": "error", "title": "security-finding", "detail": "confirmed"},
		})
	}
	title := "general-finding"
	switch {
	case strings.Contains(instr, "DIMPROMPT:security"):
		title = "security-finding"
	case strings.Contains(instr, "DIMPROMPT:correctness"):
		title = "correctness-finding"
	}
	return mk([]map[string]any{
		{"file": "main.go", "line": 1, "severity": "error", "title": title, "detail": "d"},
	})
}

// TestCodeReviewVerifyPassDropsFalsePositives exercises the whole verify wiring (manyforge-8qs.1)
// end-to-end: two dimensions each emit a finding; the verify pass confirms only one; the posted
// review + stored row drop the unconfirmed finding, and the verify lane is recorded as its own
// dimension_run. This pins the wiring the pure verify_test.go unit tests can't reach (config load,
// lane invocation, drop application, accounting).
func TestCodeReviewVerifyPassDropsFalsePositives(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	seedReviewDimension(ctx, t, tdb, seed.businessID, "security", "DIMPROMPT:security", "info", nil, 1)
	seedReviewDimension(ctx, t, tdb, seed.businessID, "correctness", "DIMPROMPT:correctness", "info", nil, 2)

	// Enable the verify pass. Blank verify_provider ⇒ the verifier runs on the review's default
	// credential with VerifyModel overriding — so no separate BYO credential is needed.
	cfgSvc := &ReviewDimensionService{DB: tdb.App}
	if _, err := cfgSvc.UpsertConfig(ctx, seed.principalID, seed.businessID, ReviewConfigInput{
		Dedupe: true, VerifyEnabled: true, VerifyModel: "verifier", PostMode: "single",
	}); err != nil {
		t.Fatalf("UpsertConfig: %v", err)
	}

	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)
	svc := buildService(t, tdb, env, &verifyDropRunner{}, fakeCred)

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
	if got.Status != "succeeded" {
		t.Fatalf("want succeeded, got %s", got.Status)
	}

	// The verify pass confirmed only the security finding; the correctness finding is dropped.
	if len(got.Findings) != 1 || got.Findings[0].Title != "security-finding" {
		t.Fatalf("verify pass must keep only the confirmed security finding; got %+v", got.Findings)
	}

	// The verify lane is recorded as its own dimension_run (its usage/status is surfaced).
	var runs []struct {
		Dimension string `json:"dimension"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(got.DimensionRuns, &runs); err != nil {
		t.Fatalf("unmarshal DimensionRuns %q: %v", got.DimensionRuns, err)
	}
	var haveVerify bool
	for _, r := range runs {
		if r.Dimension == verifyDimensionKey {
			haveVerify = true
		}
	}
	if !haveVerify {
		t.Fatalf("verify lane must be recorded as a dimension_run; got %s", got.DimensionRuns)
	}
}
