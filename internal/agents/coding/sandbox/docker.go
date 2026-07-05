package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

var envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func validEnvKey(k string) bool { return envKeyRe.MatchString(k) }

// containerName returns a unique, docker-safe name for one sandbox run. We name the
// container so a timed-out/cancelled run can be reaped by name: exec.CommandContext
// SIGKILLs the `docker` CLI on ctx expiry, but the daemon-run container keeps going
// (an orphan that continues to burn LLM API spend with no accounting). Killing it by
// name closes that leak. Fixed prefix + random suffix; crypto/rand can't fail here.
func containerName() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "mf-sbx-" + hex.EncodeToString(b[:])
}

// DockerRunner implements SandboxRunner via the docker CLI.
type DockerRunner struct {
	Network   string // internal docker network the sandbox joins
	ProxyAddr string // e.g. http://mf-egress-proxy:8080
}

// NewDockerRunner returns a DockerRunner that places containers on network and
// forces all egress through proxyAddr.
func NewDockerRunner(network, proxyAddr string) *DockerRunner {
	return &DockerRunner{Network: network, ProxyAddr: proxyAddr}
}

// Run executes spec.Cmd inside a hardened container and returns the result.
// A non-zero container exit is a result (ExitCode), not a Go error; only
// docker-invocation failure or timeout is returned as an error.
func (d *DockerRunner) Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error) {
	if spec.Timeout <= 0 {
		spec.Timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	// Materialize in-band inputs (review_diff.txt, review_instructions.txt,
	// review_files.txt) into the host OutputDir before the container starts — the
	// entrypoint reads them from /out. Best-effort, mirroring the best-effort
	// os.WriteFile calls in service.go this replaces: a write failure here must not
	// abort the run (the entrypoint degrades to its baked-in fallback instructions).
	for fname, data := range spec.Inputs {
		_ = os.WriteFile(filepath.Join(spec.OutputDir, fname), data, 0o644)
	}

	name := containerName()
	args := []string{
		"run", "--rm",
		"--name", name, // named so a timed-out run's orphaned container can be reaped (below)
		"--network", d.Network,
		"--read-only",                        // read-only root fs
		"--cap-drop", "ALL",                  // drop all Linux capabilities
		"--security-opt", "no-new-privileges", // prevent privilege escalation
		"--pids-limit", "256",                // cap process count
		"--memory", "2g",                     // memory cap
		"-v", spec.ReadOnlyDir + ":/work:ro", // checkout read-only
		"-v", spec.OutputDir + ":/out:rw",    // findings output writable
		"--tmpfs", "/tmp:rw,size=1g",         // writable /tmp: opencode copies the checkout here + its .opencode data dir
		"-w", "/work",
		// force ALL egress through the allowlisting proxy:
		"-e", "HTTPS_PROXY=" + d.ProxyAddr,
		"-e", "HTTP_PROXY=" + d.ProxyAddr,
		"-e", "https_proxy=" + d.ProxyAddr,
		"-e", "http_proxy=" + d.ProxyAddr,
	}
	// ONLY allowlisted run-scoped secrets/config — no host env passthrough.
	for k, v := range spec.Env {
		if !validEnvKey(k) {
			return SandboxResult{}, fmt.Errorf("sandbox: invalid env key %q", k)
		}
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Cmd...)

	cmd := exec.CommandContext(runCtx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	// Always buffer stderr into res.Stderr; when a live stream is requested, also tee it there
	// so the caller can surface progress as the container runs (manyforge cloud streaming).
	if spec.StreamStderr != nil {
		cmd.Stderr = io.MultiWriter(&stderr, spec.StreamStderr)
	} else {
		cmd.Stderr = &stderr
	}

	err := cmd.Run()
	// Read back whatever the entrypoint wrote to /out, regardless of exit code — a run
	// that ultimately failed to parse may still have burned tokens and written usage.json.
	// Best-effort: a missing file is simply absent from Outputs.
	res := SandboxResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Outputs: readSandboxOutputs(spec.OutputDir)}

	// On timeout/cancel, exec killed the `docker` CLI but not the daemon-run container —
	// it orphans and keeps running (observed: still "Up" 5+ min past a 5-min cap, continuing
	// to call the LLM). Reap it by name so it stops spending. Best-effort; a container that
	// already exited (normal completion) yields "no such container", which we ignore.
	if runCtx.Err() != nil {
		killCtx, killCancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = exec.CommandContext(killCtx, "docker", "kill", name).Run()
		killCancel()
	}

	if runCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, fmt.Errorf("sandbox: timed out after %s", spec.Timeout)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil // non-zero exit is a result, not a Go error
		}
		return res, fmt.Errorf("sandbox: docker run: %w", err)
	}
	return res, nil
}

// sandboxOutputFiles are the fixed set of files the opencode entrypoint writes to /out
// that callers read back in-band via SandboxResult.Outputs.
var sandboxOutputFiles = []string{"review.json", "usage.json"}

// readSandboxOutputs reads sandboxOutputFiles back from the host OutputDir into a map so
// SandboxResult carries them in-band rather than requiring the caller to know the runner
// used a shared host filesystem. Best-effort: a missing file is simply omitted.
func readSandboxOutputs(outputDir string) map[string][]byte {
	outputs := map[string][]byte{}
	for _, fname := range sandboxOutputFiles {
		if b, err := os.ReadFile(filepath.Join(outputDir, fname)); err == nil {
			outputs[fname] = b
		}
	}
	return outputs
}
