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

	// Inputs are files the entrypoint reads back out of /out (review_diff.txt,
	// review_instructions.txt, review_files.txt) keyed by filename. Carrying them in-band
	// on the spec — rather than the caller pre-writing them to a shared host OutputDir —
	// means a runner with no shared host filesystem (e.g. a future KubeRunner) can
	// materialize them however it wires up /out.
	Inputs map[string][]byte

	// Clone* let a runner clone the reviewed repo itself instead of depending on a
	// pre-populated ReadOnlyDir (which only DockerRunner uses today — service.go still
	// clones host-side for it). CloneAuthHeader is a secret credential — NEVER log it.
	CloneURL          string
	CloneAuthHeader   string
	CloneSHA          string
	CloneAllowPrivate bool
}

type SandboxResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
	// Outputs holds files read back from /out after the run (review.json, usage.json),
	// keyed by filename. A file that was never written is simply absent from the map —
	// callers must not assume presence.
	Outputs map[string][]byte
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
