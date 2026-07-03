//go:build integration

package sandbox

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunStreamsStderr pins the cloud-streaming wiring: when SandboxSpec.StreamStderr is set, the
// container's stderr is tee'd to it live (in addition to SandboxResult.Stderr), and stdout does
// NOT leak into it.
func TestRunStreamsStderr(t *testing.T) {
	dockerBuild(t, "deploy/sandbox-stub/Dockerfile", "manyforge/sandbox-stub:test")

	ctx := t.Context()
	ro := sandboxTempDir(t)
	out := sandboxTempDir(t)
	r := NewDockerRunner("bridge", "")

	var streamed bytes.Buffer
	res, err := r.Run(ctx, SandboxSpec{
		Image:        "manyforge/sandbox-stub:test",
		ReadOnlyDir:  ro,
		OutputDir:    out,
		Cmd:          []string{"sh", "-c", "echo TO_STDERR 1>&2; echo TO_STDOUT"},
		StreamStderr: &streamed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(streamed.String(), "TO_STDERR") {
		t.Fatalf("StreamStderr did not receive live stderr: %q", streamed.String())
	}
	if !strings.Contains(string(res.Stderr), "TO_STDERR") {
		t.Fatalf("res.Stderr must still buffer stderr: %q", res.Stderr)
	}
	if strings.Contains(streamed.String(), "TO_STDOUT") {
		t.Fatalf("stdout leaked into StreamStderr: %q", streamed.String())
	}
}

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
	// The sandbox runs with --cap-drop ALL (no CAP_DAC_OVERRIDE), so even
	// container-root obeys DAC bits. os.MkdirTemp creates 0700; on native Linux the
	// container uid differs from the host owner and could neither read /work nor
	// write /out. Make per-run dirs world-accessible — mirrors the perms
	// CodeReviewService applies to real run dirs. (Colima remaps ownership and masks
	// this; native Linux CI does not.)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestRunReapsContainerOnTimeout pins the manyforge-2s1 fix: on timeout, exec.CommandContext
// SIGKILLs the `docker` CLI but the daemon-run container keeps running (an orphan that keeps
// burning LLM spend). Run must reap it by name. Before the fix the sleep container stays "Up"
// long after the 2s cap; after it, the container is gone within the poll window.
func TestRunReapsContainerOnTimeout(t *testing.T) {
	dockerBuild(t, "deploy/sandbox-stub/Dockerfile", "manyforge/sandbox-stub:test")

	ctx := t.Context()
	ro := sandboxTempDir(t)
	out := sandboxTempDir(t)
	r := NewDockerRunner("bridge", "") // default network; a sleep needs no egress proxy

	res, err := r.Run(ctx, SandboxSpec{
		Image:       "manyforge/sandbox-stub:test",
		ReadOnlyDir: ro,
		OutputDir:   out,
		Cmd:         []string{"sh", "-c", "sleep 120"},
		Timeout:     2 * time.Second,
	})
	if err == nil || !res.TimedOut {
		t.Fatalf("want a timeout (TimedOut=true, non-nil err), got res=%+v err=%v", res, err)
	}

	// docker kill + --rm removal is near-immediate but not synchronous; poll briefly.
	running := func() string {
		b, _ := exec.Command("docker", "ps", "-q",
			"--filter", "ancestor=manyforge/sandbox-stub:test", "--filter", "status=running").Output()
		return strings.TrimSpace(string(b))
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if running() == "" {
			return // reaped ✓
		}
		if time.Now().After(deadline) {
			for _, id := range strings.Fields(running()) { // don't poison sibling tests
				_ = exec.Command("docker", "kill", id).Run()
			}
			t.Fatal("timed-out container was NOT reaped — still running (orphan leak)")
		}
		time.Sleep(200 * time.Millisecond)
	}
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

	// 2b. /work IS readable — opencode must read the checkout. On native Linux a
	// 0700 host dir owned by a different uid would be unreadable by the capless
	// container (--cap-drop ALL); this pins that /work is world-readable/traversable.
	res, _ = r.Run(ctx, SandboxSpec{
		Image:       "manyforge/sandbox-stub:test",
		ReadOnlyDir: ro,
		OutputDir:   out,
		Cmd:         []string{"sh", "-c", "cat /work/code.txt"},
	})
	if !strings.Contains(string(res.Stdout), "secret-code") {
		t.Fatalf("/work not readable by sandbox: stdout=%s stderr=%s", res.Stdout, res.Stderr)
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

	// 5. HTTPS CONNECT-path egress to a non-allowlisted host is refused.
	// wget uses HTTP CONNECT to tunnel TLS through the proxy; the proxy must deny
	// the tunnel for non-allowlisted hosts just as it denies plain HTTP.
	res, _ = r.Run(ctx, SandboxSpec{
		Image:       "manyforge/sandbox-stub:test",
		ReadOnlyDir: ro,
		OutputDir:   out,
		Cmd:         []string{"sh", "-c", "wget -T 5 -q -O- https://example.com >/dev/null 2>&1 && echo REACHED || echo BLOCKED"},
	})
	if !strings.Contains(string(res.Stdout), "BLOCKED") {
		t.Fatalf("HTTPS egress was not blocked: stdout=%s", res.Stdout)
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
