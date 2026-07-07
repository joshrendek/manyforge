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

// TestEntrypointRejectsJSONInjection is the regression test for manyforge-9er:
// entrypoint.sh interpolates LLM_BASE_URL/LLM_MODEL/LLM_API_KEY into JSON string
// literals (and, for LLM_MODEL, a JSON object key) with no escaping when it
// generates opencode.json/auth.json. Upstream validation (credential.go
// validateBaseURL) only checks URL parse + scheme + host — it does not reject `"`
// or `\`. A crafted LLM_BASE_URL can therefore break out of its JSON string and
// inject arbitrary config keys, including overriding the read-only "permission"
// block that denies bash/webfetch/websearch (pins MF-KUBE-SANDBOX-19/20/21). The
// fix is a guard in entrypoint.sh, right after the provider case block and before
// any config/auth.json generation, that rejects any LLM_* value containing a JSON
// metacharacter.
func TestEntrypointRejectsJSONInjection(t *testing.T) {
	script, err := os.ReadFile("../../../deploy/sandbox/entrypoint.sh")
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}

	const opencodeConfigPath = "/tmp/opencode.json"
	const authPath = "/tmp/.local/share/opencode/auth.json"

	// Same host-mount neutralization as TestEntrypointLocalProviderConfig, plus an
	// `opencode` stub that echoes a sentinel so a passing test can prove opencode
	// was never invoked when the guard rejects the input.
	harness := `cp() { :; }
mkdir() { [ "$*" = "-p /out" ] && return 0; command mkdir "$@"; }
opencode() { echo OPENCODE_WAS_REACHED; exit 0; }
export -f cp mkdir opencode 2>/dev/null || true
mkdir -p /tmp/src
`

	run := func(t *testing.T, baseURL string) (output string, exitCode int) {
		t.Helper()
		_ = os.Remove(opencodeConfigPath)
		_ = os.Remove(authPath)
		t.Cleanup(func() {
			_ = os.Remove(opencodeConfigPath)
			_ = os.Remove(authPath)
		})

		cmd := exec.Command("bash", "-c", harness+string(script))
		cmd.Env = append(os.Environ(),
			"LLM_PROVIDER=vllm", "LLM_MODEL=m", "LLM_API_KEY=k",
			"LLM_BASE_URL="+baseURL,
			"MF_MARKER_NONCE=")
		out, runErr := cmd.CombinedOutput()
		switch e := runErr.(type) {
		case nil:
			exitCode = 0
		case *exec.ExitError:
			exitCode = e.ExitCode()
		default:
			t.Fatalf("run entrypoint: %v\noutput:\n%s", runErr, out)
		}
		return string(out), exitCode
	}

	t.Run("injecting base URL is rejected before config generation", func(t *testing.T) {
		injecting := `http://x/v1", "permission": {"bash": "allow"}, "z": "`
		out, code := run(t, injecting)

		if code == 0 {
			t.Errorf("expected entrypoint to exit non-zero for an injecting LLM_BASE_URL, got 0\noutput:\n%s", out)
		}
		if !strings.Contains(out, "JSON metacharacter") {
			t.Errorf("expected rejection message to mention 'JSON metacharacter'\noutput:\n%s", out)
		}
		if strings.Contains(out, "OPENCODE_WAS_REACHED") {
			t.Errorf("opencode must never run when LLM_BASE_URL carries a JSON metacharacter\noutput:\n%s", out)
		}
		if _, err := os.Stat(opencodeConfigPath); !os.IsNotExist(err) {
			t.Errorf("opencode.json must not be written when LLM_BASE_URL carries a JSON metacharacter (stat err: %v)", err)
		}
		if _, err := os.Stat(authPath); !os.IsNotExist(err) {
			t.Errorf("auth.json must not be written when LLM_BASE_URL carries a JSON metacharacter (stat err: %v)", err)
		}
	})

	t.Run("legitimate base URL is not rejected by the guard", func(t *testing.T) {
		out, _ := run(t, "http://192.168.2.241:1234/v1")

		if strings.Contains(out, "JSON metacharacter") {
			t.Errorf("guard must not reject a legitimate LLM_BASE_URL\noutput:\n%s", out)
		}
		config, err := os.ReadFile(opencodeConfigPath)
		if err != nil {
			t.Fatalf("guard blocked config generation for a legitimate LLM_BASE_URL: %v\noutput:\n%s", err, out)
		}
		if !strings.Contains(string(config), `"baseURL": "http://192.168.2.241:1234/v1"`) {
			t.Errorf("expected generated config to carry the legitimate base URL\n%s", config)
		}
	})
}
