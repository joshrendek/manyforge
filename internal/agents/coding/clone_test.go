package coding

import (
	"net"
	"net/http"
	"net/http/httptest"
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
	// file:// scheme skips SSRF check; allowPrivate=false is fine here.
	if err := CloneAtSHA(t.Context(), "file://"+src, "", sha, dest, false); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "a.txt")); err != nil {
		t.Fatalf("expected a.txt in checkout: %v", err)
	}
}

// TestCloneAtSHASSRF verifies that an http:// URL resolving to a loopback/private
// address is rejected when allowPrivate=false and accepted when allowPrivate=true.
func TestCloneAtSHASSRF(t *testing.T) {
	// Start a minimal HTTP server on loopback so we have a real port to resolve.
	// The server doesn't need to speak git — we just need the URL to pass host resolution.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// srv.URL is http://127.0.0.1:<port>
	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("parse srv.URL: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("expected loopback host, got %q", host)
	}

	dest := filepath.Join(t.TempDir(), "checkout")

	t.Run("loopback_rejected_when_allowPrivate_false", func(t *testing.T) {
		err := CloneAtSHA(t.Context(), srv.URL+"/repo.git", "", "abc123", dest, false)
		if err == nil {
			t.Fatal("expected error for loopback URL with allowPrivate=false, got nil")
		}
		// Error must not mention the token/header; it just says "blocked address".
		if strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), "Authorization") {
			t.Errorf("error leaks token/header: %v", err)
		}
	})

	t.Run("loopback_allowed_when_allowPrivate_true", func(t *testing.T) {
		dest2 := filepath.Join(t.TempDir(), "checkout2")
		err := CloneAtSHA(t.Context(), srv.URL+"/repo.git", "", "abc123", dest2, true)
		// allowPrivate=true passes the SSRF guard; git clone will then fail because
		// the server is not a git remote — that's expected and acceptable here.
		if err == nil {
			t.Fatal("expected git clone to fail against a non-git HTTP server")
		}
		// The error must be a git failure, not a blocked-address rejection.
		if strings.Contains(err.Error(), "blocked address") {
			t.Errorf("SSRF guard incorrectly blocked loopback with allowPrivate=true: %v", err)
		}
	})
}
