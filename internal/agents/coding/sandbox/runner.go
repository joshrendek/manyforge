package sandbox

import (
	"context"
	"io"
	"time"
)

type SandboxSpec struct {
	Image       string            // container image
	ReadOnlyDir string            // host path mounted read-only at /work
	OutputDir   string            // host path mounted read-write at /out
	Cmd         []string          // command run inside the container
	Env         map[string]string // ONLY allowlisted run-scoped secrets/config
	EgressAllow []string          // allowlisted egress hosts (informational; enforced by proxy/network)
	Timeout     time.Duration     // wall-clock cap
	// StreamStderr, when non-nil, receives the container's stderr LIVE (in addition to the
	// buffered SandboxResult.Stderr) so a caller can stream progress as the run proceeds. The
	// entrypoint routes the tool's stderr to the container's stderr for exactly this.
	StreamStderr io.Writer
}

type SandboxResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
}

type SandboxRunner interface {
	Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error)
}

// FakeRunner is for service-layer tests. It records the last spec and returns Result/Err.
type FakeRunner struct {
	Last   SandboxSpec
	Result SandboxResult
	Err    error
}

func (f *FakeRunner) Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error) {
	f.Last = spec
	return f.Result, f.Err
}
