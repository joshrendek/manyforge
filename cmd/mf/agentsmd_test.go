package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertBlock(t *testing.T) {
	block := blockBegin + "\nnew\n" + blockEnd
	cases := []struct{ name, in, want string }{
		{"empty file", "", block + "\n"},
		{"append with blank line", "# Repo\n\nrules\n\n\n", "# Repo\n\nrules\n\n" + block + "\n"},
		{"replace in place", "top\n" + blockBegin + "\nold\nstuff\n" + blockEnd + "\nbottom\n", "top\n" + block + "\nbottom\n"},
	}
	for _, c := range cases {
		got, err := upsertBlock(c.in, block)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
		again, _ := upsertBlock(got, block)
		if again != got {
			t.Errorf("%s: not idempotent: %q", c.name, again)
		}
	}
	if _, err := upsertBlock("x\n"+blockBegin+"\nno end\n", block); err == nil {
		t.Error("begin without end must be an error, not a duplicate block")
	}
}

func TestUpsertAgentDocsCreatesAgentsMD(t *testing.T) {
	root := t.TempDir()
	changed, err := upsertAgentDocs(root)
	if err != nil || len(changed) != 1 || filepath.Base(changed[0]) != "AGENTS.md" {
		t.Fatalf("changed = %v, %v", changed, err)
	}
	if got := readFile(t, filepath.Join(root, "AGENTS.md")); got != agentBlock+"\n" {
		t.Fatalf("AGENTS.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatal("CLAUDE.md must not be created when AGENTS.md is")
	}
}

func TestUpsertAgentDocsBothFilesIdempotent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "# Agents\n")
	stale := "# Claude\n\n" + blockBegin + "\nstale text\n" + blockEnd + "\n\n## Later section\n"
	writeFile(t, filepath.Join(root, "CLAUDE.md"), stale)

	changed, err := upsertAgentDocs(root)
	if err != nil || len(changed) != 2 {
		t.Fatalf("changed = %v, %v", changed, err)
	}
	agents := readFile(t, filepath.Join(root, "AGENTS.md"))
	if agents != "# Agents\n\n"+agentBlock+"\n" {
		t.Fatalf("AGENTS.md = %q", agents)
	}
	claude := readFile(t, filepath.Join(root, "CLAUDE.md"))
	if claude != "# Claude\n\n"+agentBlock+"\n\n## Later section\n" || strings.Contains(claude, "stale") {
		t.Fatalf("CLAUDE.md = %q", claude)
	}
	if changed, err := upsertAgentDocs(root); err != nil || len(changed) != 0 {
		t.Fatalf("second run changed %v, %v", changed, err)
	}
}

func TestUpsertAgentDocsFollowsSymlink(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "# Agents\n")
	if err := os.Symlink("AGENTS.md", filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	changed, err := upsertAgentDocs(root)
	if err != nil || len(changed) != 1 {
		t.Fatalf("changed = %v, %v", changed, err)
	}
	if fi, _ := os.Lstat(filepath.Join(root, "CLAUDE.md")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("CLAUDE.md symlink was replaced by a regular file")
	}
	if strings.Count(readFile(t, filepath.Join(root, "CLAUDE.md")), blockBegin) != 1 {
		t.Fatal("block should appear exactly once")
	}
}

func TestMergeClaudeHooks(t *testing.T) {
	in := `{
  "permissions": {"allow": ["Bash(go test:*)"]},
  "hooks": {
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "bd prime"}]}],
    "PostToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "fmt.sh"}]}]
  }
}`
	out, changed, err := mergeClaudeHooks([]byte(in))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var got struct {
		Permissions map[string][]string `json:"permissions"`
		Hooks       map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct{ Type, Command string }
		} `json:"hooks"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Permissions["allow"][0] != "Bash(go test:*)" {
		t.Fatal("permissions lost")
	}
	if pt := got.Hooks["PostToolUse"]; len(pt) != 1 || pt[0].Hooks[0].Command != "fmt.sh" {
		t.Fatalf("PostToolUse lost: %+v", pt)
	}
	ss := got.Hooks["SessionStart"]
	if len(ss) != 2 || ss[0].Hooks[0].Command != "bd prime" || ss[1].Hooks[0].Command != "mf prime" {
		t.Fatalf("SessionStart = %+v", ss)
	}
	if pc := got.Hooks["PreCompact"]; len(pc) != 1 || pc[0].Hooks[0].Command != "mf prime" || pc[0].Hooks[0].Type != "command" {
		t.Fatalf("PreCompact = %+v", pc)
	}

	again, changed, err := mergeClaudeHooks(out)
	if err != nil || changed || string(again) != string(out) {
		t.Fatalf("second merge changed=%v err=%v", changed, err)
	}
}

func TestMergeClaudeHooksPartialAndEmpty(t *testing.T) {
	// PreCompact already has mf prime: only SessionStart is added.
	in := `{"hooks":{"PreCompact":[{"matcher":"","hooks":[{"type":"command","command":"mf prime"}]}]}}`
	out, changed, err := mergeClaudeHooks([]byte(in))
	if err != nil || !changed || strings.Count(string(out), `"mf prime"`) != 2 {
		t.Fatalf("partial: changed=%v err=%v out=%s", changed, err, out)
	}
	out, changed, err = mergeClaudeHooks(nil)
	if err != nil || !changed || strings.Count(string(out), `"mf prime"`) != 2 {
		t.Fatalf("empty: changed=%v err=%v out=%s", changed, err, out)
	}
	if _, _, err := mergeClaudeHooks([]byte(`{"hooks": []}`)); err == nil {
		t.Fatal("non-object hooks must be rejected, not clobbered")
	}
}

func TestSetupClaudeCommand(t *testing.T) {
	e := newTestEnv(t)
	e.mustRun("init", "--no-docs")
	e.mustRun("setup", "claude")
	path := filepath.Join(e.dir, ".claude", "settings.json")
	first := readFile(t, path)
	if out := e.mustRun("setup", "claude"); !strings.Contains(out, "already") {
		t.Fatalf("second setup: %q", out)
	}
	if readFile(t, path) != first {
		t.Fatal("setup claude is not idempotent")
	}
}
