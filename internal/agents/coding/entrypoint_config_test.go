//go:build !integration

package coding

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestEntrypointLocalProviderConfig runs entrypoint.sh up to config generation with
// opencode stubbed, and asserts a local provider maps to the bundled openai-compatible
// provider (Chat Completions), NOT the built-in openai provider (Responses API).
func TestEntrypointLocalProviderConfig(t *testing.T) {
	script, err := os.ReadFile("../../../deploy/sandbox/entrypoint.sh")
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}

	// entrypoint.sh assumes the production sandbox mounts: a real /work checkout to
	// copy from, and a writable /out. Neither exists on a host test runner (and on a
	// sealed macOS system volume even `mkdir -p /out` fails with EROFS), so with
	// `set -e` the script would abort before ever generating the config. Neutralize
	// those mounts without touching the config-generation logic under test:
	//   - `cp` no-ops (nothing to copy) and we pre-create /tmp/src (the `cd` target).
	//   - `mkdir` no-ops ONLY for the single `-p /out` call; every other mkdir the
	//     script makes (the XDG_* dirs, all under /tmp) still runs for real.
	//   - `opencode` is stubbed to exit immediately, short-circuiting before the
	//     (unavailable) real binary and the later sqlite3 usage-capture step.
	// The config/auth files under test are written to their hardcoded /tmp paths by
	// the script BEFORE the `opencode run` line, so we read them directly off disk
	// afterward rather than relying on that line's stdout (which the real script
	// redirects to /out/review.json, not to the process's own stdout).
	const opencodeConfigPath = "/tmp/opencode.json"
	const authPath = "/tmp/.local/share/opencode/auth.json"
	t.Cleanup(func() {
		_ = os.Remove(opencodeConfigPath)
		_ = os.Remove(authPath)
	})

	harness := `cp() { :; }
mkdir() { [ "$*" = "-p /out" ] && return 0; command mkdir "$@"; }
opencode() { exit 0; }
export -f cp mkdir opencode 2>/dev/null || true
mkdir -p /tmp/src
`
	cmd := exec.Command("bash", "-c", harness+string(script))
	cmd.Env = append(os.Environ(),
		"LLM_PROVIDER=vllm", "LLM_MODEL=ornith-1.0-9b",
		"LLM_BASE_URL=http://192.168.2.241:1234/v1", "LLM_API_KEY=k",
		"MF_MARKER_NONCE=")
	out, _ := cmd.CombinedOutput()

	config, err := os.ReadFile(opencodeConfigPath)
	if err != nil {
		t.Fatalf("read generated opencode.json: %v\nscript output:\n%s", err, out)
	}
	auth, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read generated auth.json: %v\nscript output:\n%s", err, out)
	}
	s := string(config) + string(auth)

	for _, want := range []string{
		`"npm": "@ai-sdk/openai-compatible"`,
		`"baseURL": "http://192.168.2.241:1234/v1"`,
		`"model": "local/ornith-1.0-9b"`,
		`"local":{"type":"api","key":"k"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("entrypoint config missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, `"model": "openai/`) || strings.Contains(s, "/v1/responses") {
		t.Errorf("local provider must NOT use the built-in openai/Responses path\n%s", s)
	}
}
