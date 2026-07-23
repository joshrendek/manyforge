package coding

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

func TestLaneEnv_SetsCiteRulesFlagOnlyWhenEnabled(t *testing.T) {
	on := laneEnv(map[string]string{"LLM_PROVIDER": "x"}, true)
	if on["CITE_RULES"] != "1" {
		t.Errorf("cite-rules on must set CITE_RULES=1; got %q", on["CITE_RULES"])
	}
	off := laneEnv(map[string]string{"LLM_PROVIDER": "x"}, false)
	if _, present := off["CITE_RULES"]; present {
		t.Errorf("cite-rules off must NOT set CITE_RULES; got %q", off["CITE_RULES"])
	}
}

func TestParseFindings_DecodesRuleID(t *testing.T) {
	raw := `{"summary":"s","findings":[{"file":"a.go","line":3,"severity":"error","title":"t","detail":"d","rule_id":"no-raw-sql"}]}`
	doc, err := ParseFindings([]byte(raw))
	if err != nil {
		t.Fatalf("ParseFindings: %v", err)
	}
	if len(doc.Findings) != 1 || doc.Findings[0].RuleID != "no-raw-sql" {
		t.Fatalf("rule_id must round-trip through ParseFindings; got %+v", doc.Findings)
	}
}

func TestRuleSuffix_RendersOnlyWhenCited(t *testing.T) {
	if s := ruleSuffix(connectors.Finding{Title: "x"}); s != "" {
		t.Errorf("no rule_id ⇒ empty suffix; got %q", s)
	}
	if s := ruleSuffix(connectors.Finding{Title: "x", RuleID: "R-1"}); !strings.Contains(s, "R-1") {
		t.Errorf("cited rule must appear in the suffix; got %q", s)
	}
	// It reaches the posted comment body.
	body := renderInlineComment(connectors.Finding{Severity: "error", Title: "SQLi", RuleID: "no-raw-sql"})
	if !strings.Contains(body, "no-raw-sql") {
		t.Errorf("inline comment must surface the cited rule; got %q", body)
	}
}

// rulesScriptPath locates deploy/sandbox/rules.sh relative to this test file's package dir
// (internal/agents/coding → repo root is ../../..).
func rulesScriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../../deploy/sandbox/rules.sh")
	if err != nil {
		t.Fatalf("abs rules.sh: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("rules.sh not found at %s: %v", p, err)
	}
	return p
}

func runEmitProjectRules(t *testing.T, work string, env ...string) string {
	t.Helper()
	// Pass the script path + workdir as positional args ($1/$2), never interpolated into the
	// script string — no shell-metacharacter surface even though both are test-controlled.
	cmd := exec.Command("bash", "-c", `. "$1"; emit_project_rules "$2"`, "rules-test", rulesScriptPath(t), work)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("emit_project_rules: %v\n%s", err, out)
	}
	return string(out)
}

// TestEmitProjectRules_ReadsFixedRepoDocs validates the bash rule-extractor without Docker/opencode:
// it reads the reviewed repo's own rule docs from /work, only from fixed paths, byte-capped.
func TestEmitProjectRules_ReadsFixedRepoDocs(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "CLAUDE.md"), []byte("# House rules\nNo raw SQL; use sqlc."), 0o644); err != nil {
		t.Fatal(err)
	}
	// A secret-looking file that must NOT be read (only the fixed allowlist is).
	if err := os.WriteFile(filepath.Join(work, ".env"), []byte("SECRET=hunter2"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runEmitProjectRules(t, work)
	if !strings.Contains(out, "PROJECT RULES") || !strings.Contains(out, "rule_id") {
		t.Errorf("rules block must include the header + rule_id instruction; got:\n%s", out)
	}
	if !strings.Contains(out, "No raw SQL; use sqlc.") || !strings.Contains(out, "=== CLAUDE.md ===") {
		t.Errorf("rules block must include the CLAUDE.md content under its heading; got:\n%s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Fatalf("SECURITY: the extractor must NOT read non-allowlisted files like .env; got:\n%s", out)
	}
}

func TestEmitProjectRules_EmptyWhenNoDocs(t *testing.T) {
	out := runEmitProjectRules(t, t.TempDir())
	if strings.TrimSpace(out) != "" {
		t.Errorf("no rule docs ⇒ empty output (caller skips seeding); got %q", out)
	}
}

func TestEmitProjectRules_ByteCapped(t *testing.T) {
	work := t.TempDir()
	big := strings.Repeat("x", 40000) // > cap
	if err := os.WriteFile(filepath.Join(work, "CLAUDE.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runEmitProjectRules(t, work, "RULES_MAX_BYTES=1024")
	// Header line + at most the cap of body bytes + small framing; must be far below the 40k input.
	if len(out) > 4096 {
		t.Errorf("rules block must be byte-capped (cap=1024); got %d bytes", len(out))
	}
}
