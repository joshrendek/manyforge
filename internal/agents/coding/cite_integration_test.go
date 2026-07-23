//go:build integration

package coding

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
)

// citeRunner records whether the sandbox spec carried CITE_RULES (proving the host wired the flag
// through from config) and returns a finding carrying a rule_id (proving rule_id round-trips
// through parse → aggregate → store → Get).
type citeRunner struct{ sawCiteRules atomic.Bool }

func (r *citeRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	if spec.Env["CITE_RULES"] == "1" {
		r.sawCiteRules.Store(true)
	}
	doc, _ := json.Marshal(map[string]any{"summary": "s", "findings": []map[string]any{
		{"file": "main.go", "line": 1, "severity": "error", "title": "raw sql", "detail": "d", "rule_id": "no-raw-sql"},
	}})
	usage := `[{"cost":0.02,"input":1000,"output":50,"reasoning":10,"cache_read":40000,"cache_write":0}]`
	return sandbox.SandboxResult{ExitCode: 0, Outputs: map[string][]byte{
		"review.json": doc, "usage.json": []byte(usage),
	}}, nil
}

// TestCodeReviewCiteRulesWiresEnvAndRoundTripsRuleID pins the Go-side rule-citation wiring
// (manyforge-8qs.2): enabling CiteRules sets CITE_RULES=1 on the sandbox spec, and a finding's
// rule_id survives the whole pipeline into the stored/returned review. (The entrypoint's actual
// doc-reading is validated separately by the bash test in rules_test.go.)
func TestCodeReviewCiteRulesWiresEnvAndRoundTripsRuleID(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc","ref":"f"},"base":{"ref":"main"}}`)
	ghSrv, _ := startGitHubStub(t, prJSON)

	seedReviewDimension(ctx, t, tdb, seed.businessID, "security", "DIMPROMPT:security", "info", nil, 1)

	cfgSvc := &ReviewDimensionService{DB: tdb.App}
	if _, err := cfgSvc.UpsertConfig(ctx, seed.principalID, seed.businessID, ReviewConfigInput{
		Dedupe: true, CiteRules: true, PostMode: "single",
	}); err != nil {
		t.Fatalf("UpsertConfig: %v", err)
	}

	fakeCred := &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, ghSrv.URL)
	runner := &citeRunner{}
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

	if !runner.sawCiteRules.Load() {
		t.Fatal("CiteRules enabled must set CITE_RULES=1 on the sandbox spec env")
	}

	got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Findings) != 1 || got.Findings[0].RuleID != "no-raw-sql" {
		t.Fatalf("rule_id must round-trip into the stored review; got %+v", got.Findings)
	}
}
