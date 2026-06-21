//go:build integration

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func dockerBuild(t *testing.T, dockerfile, tag string) {
	t.Helper()
	c := exec.Command("docker", "build", "-f", dockerfile, "-t", tag, ".")
	c.Dir = repoRoot(t)
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v (%s)", tag, err, b)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// sandboxTempDir creates a temp dir under $HOME so it is reachable via the
// sshfs bind-mount that Colima (or Docker Desktop) uses to share files between
// the macOS host and the Linux VM. Paths under /tmp are NOT on the sshfs mount
// and files written inside the container will not appear on the host.
func sandboxTempDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".cache", "manyforge-sandbox-test")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestSandboxIsolation(t *testing.T) {
	dockerBuild(t, "deploy/egress-proxy/Dockerfile", "manyforge/egress-proxy:test")
	dockerBuild(t, "deploy/sandbox-stub/Dockerfile", "manyforge/sandbox-stub:test")

	ctx := t.Context()
	// allowlist a host that does NOT resolve to anything reachable; we only assert deny behavior here.
	if err := EnsureEgressInfra(ctx, "manyforge/egress-proxy:test", []string{"allowed.invalid"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", ProxyName).Run() })

	// Use home-relative paths so Colima's sshfs mount exposes them on both
	// the macOS host and the Linux VM (Docker's bind-mount source).
	ro := sandboxTempDir(t)
	if err := os.WriteFile(filepath.Join(ro, "code.txt"), []byte("secret-code"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := sandboxTempDir(t)
	r := NewDockerRunner(NetworkName, ProxyDNSAddr)

	// 1. No host env leaks: a sentinel host var must be absent inside the container.
	t.Setenv("MF_SENTINEL_SECRET", "leak-me")
	res, err := r.Run(ctx, SandboxSpec{
		Image:       "manyforge/sandbox-stub:test",
		ReadOnlyDir: ro,
		OutputDir:   out,
		Cmd:         []string{"sh", "-c", "echo START; printenv MF_SENTINEL_SECRET || echo NO_SENTINEL"},
		Env:         map[string]string{"LLM_API_KEY": "only-this"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Stdout), "NO_SENTINEL") {
		t.Fatalf("host env leaked into sandbox: %s", res.Stdout)
	}

	// 2. /work checkout is read-only.
	res, _ = r.Run(ctx, SandboxSpec{
		Image:       "manyforge/sandbox-stub:test",
		ReadOnlyDir: ro,
		OutputDir:   out,
		Cmd:         []string{"sh", "-c", "echo x > /work/code.txt && echo WROTE || echo READONLY"},
	})
	if !strings.Contains(string(res.Stdout), "READONLY") {
		t.Fatalf("checkout was writable: %s", res.Stdout)
	}

	// 3. Direct egress to a non-allowlisted host is refused.
	// The --internal network has no external route; even if wget tries an HTTP_PROXY
	// the proxy denies plain HTTP (CONNECT only) and denies non-allowlisted hosts.
	res, _ = r.Run(ctx, SandboxSpec{
		Image:       "manyforge/sandbox-stub:test",
		ReadOnlyDir: ro,
		OutputDir:   out,
		Cmd:         []string{"sh", "-c", "wget -T 5 -q -O- http://example.com >/dev/null 2>&1 && echo REACHED || echo BLOCKED"},
	})
	if !strings.Contains(string(res.Stdout), "BLOCKED") {
		t.Fatalf("egress was not blocked: stdout=%s", res.Stdout)
	}

	// 4. /out is writable and the file appears on the host.
	res, _ = r.Run(ctx, SandboxSpec{
		Image:       "manyforge/sandbox-stub:test",
		ReadOnlyDir: ro,
		OutputDir:   out,
		Cmd:         []string{"sh", "-c", "echo ok > /out/review.json && echo WROTE_OUT"},
	})
	if !strings.Contains(string(res.Stdout), "WROTE_OUT") {
		t.Fatalf("output dir not writable: stdout=%s stderr=%s", res.Stdout, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "review.json")); err != nil {
		t.Fatalf("expected review.json on host: %v", err)
	}
}
