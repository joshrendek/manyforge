package coding

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneAtSHA(t *testing.T) {
	src := t.TempDir()
	run := func(args ...string) string {
		c := exec.Command("git", append([]string{"-C", src}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t",
			"GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t",
			"GIT_COMMITTER_EMAIL=t@t",
		)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "x")
	sha := run("rev-parse", "HEAD")

	dest := filepath.Join(t.TempDir(), "checkout")
	if err := CloneAtSHA(t.Context(), "file://"+src, "", sha, dest); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "a.txt")); err != nil {
		t.Fatalf("expected a.txt in checkout: %v", err)
	}
}
