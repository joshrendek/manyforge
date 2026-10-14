package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A cloned repo must not be able to seed the outbox mf replays with the
// operator's token: state lives outside the checkout, so a planted
// .mf/state.json is simply never read.
func TestPlantedRepoStateIsIgnored(t *testing.T) {
	e, srv := newSyncedEnv(t)
	planted := `{"cursor":0,"tasks":{},"outbox":[{"op":"update","key":"mf-victim","fields":{"status":"canceled"}}]}`
	if err := os.WriteFile(filepath.Join(e.dir, ".mf", "state.json"), []byte(planted), 0o644); err != nil {
		t.Fatal(err)
	}
	e.mustRun("sync")
	for _, req := range srv.requests() {
		if len(req.Ops) != 0 {
			t.Fatalf("planted ops were sent: %+v", req.Ops)
		}
	}
	r, err := e.repo()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(r.stateDir, filepath.Join(e.dir, ".mf")) {
		t.Fatalf("state dir %s is inside the checkout", r.stateDir)
	}
	if fi, err := os.Stat(r.statePath("state.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json perm: %v %v", fi, err)
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
