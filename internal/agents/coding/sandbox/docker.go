package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

var envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func validEnvKey(k string) bool { return envKeyRe.MatchString(k) }

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

	args := []string{
		"run", "--rm",
		"--network", d.Network,
		"--read-only",                        // read-only root fs
		"--cap-drop", "ALL",                  // drop all Linux capabilities
		"--security-opt", "no-new-privileges", // prevent privilege escalation
		"--pids-limit", "256",                // cap process count
		"--memory", "2g",                     // memory cap
		"-v", spec.ReadOnlyDir + ":/work:ro", // checkout read-only
		"-v", spec.OutputDir + ":/out:rw",    // findings output writable
		"--tmpfs", "/tmp:rw,size=256m",       // writable /tmp for tools
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
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := SandboxResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}

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
