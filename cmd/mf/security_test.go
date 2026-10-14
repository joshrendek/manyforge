package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A repo that commits .mf/state.json must not get its outbox replayed with
// the operator's token.
func TestSyncRefusesGitTrackedState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e, srv := newSyncedEnv(t)
	e.env["MF_OFFLINE"] = "1"
	e.mustRun("create", "planted op")
	delete(e.env, "MF_OFFLINE")
	for _, args := range [][]string{{"init", "-q"}, {"add", "-f", ".mf/state.json"}} {
		cmd := exec.Command("git", append([]string{"-C", e.dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if code := e.run("sync"); code == 0 || !strings.Contains(e.err.String(), "tracked by git") {
		t.Fatalf("sync exit %d stderr %q", code, e.err.String())
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

func TestInitRefusesSymlinkedDocsOutsideRepo(t *testing.T) {
	e := newTestEnv(t)
	outside := filepath.Join(t.TempDir(), "bashrc")
	if err := os.WriteFile(outside, []byte("export PATH=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if code := e.run("init"); code == 0 {
		t.Fatalf("init should fail on an escaping symlink")
	}
	if got, _ := os.ReadFile(outside); string(got) != "export PATH=x\n" {
		t.Fatalf("file outside repo was modified: %q", got)
	}
}

func TestInitFollowsSymlinkInsideRepo(t *testing.T) {
	e := newTestEnv(t)
	if err := os.WriteFile(filepath.Join(e.dir, "AGENTS.md"), []byte("# agents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("AGENTS.md", filepath.Join(e.dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	e.mustRun("init")
	got, _ := os.ReadFile(filepath.Join(e.dir, "AGENTS.md"))
	if strings.Count(string(got), blockBegin) != 1 {
		t.Fatalf("AGENTS.md should hold exactly one block:\n%s", got)
	}
	if fi, _ := os.Lstat(filepath.Join(e.dir, "CLAUDE.md")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("CLAUDE.md symlink was replaced")
	}
}

func TestSetupClaudeRefusesSymlinkedDirOutsideRepo(t *testing.T) {
	e := newTestEnv(t)
	e.mustRun("init", "--no-docs")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.dir, ".claude")); err != nil {
		t.Fatal(err)
	}
	if code := e.run("setup", "claude"); code == 0 {
		t.Fatalf("setup should refuse a .claude symlink leaving the repo")
	}
	if _, err := os.Stat(filepath.Join(outside, "settings.json")); err == nil {
		t.Fatalf("settings.json written outside the repo")
	}
}

func TestLoginRejectsServerMismatchAndWarnsWhenImplicit(t *testing.T) {
	e := newTestEnv(t)
	e.mustRun("init", "--server", "https://evil.example", "--project", "p", "--no-docs")
	if code := e.run("login", "--token", "mfw_x", "--server", "https://hub.example"); code == 0 {
		t.Fatalf("login with mismatched --server should fail")
	}
	e.mustRun("login", "--token", "mfw_x")
	if !strings.Contains(e.err.String(), "make sure https://evil.example is your ManyForge instance") {
		t.Fatalf("implicit server should be called out, stderr %q", e.err.String())
	}
}
