package security_regression

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// MANYFORGE-BHX-PIN-1: no live provider API token may ever be committed. BYO provider keys
// are sealed at rest (ai_provider_credential.sealed_key_ref) and injected into the sandbox
// at run time via SecretKeyRef — they must never appear in source, fixtures, testdata, or
// checked-in logs. Tests that need a key use an obviously-fake stand-in ("sk-test",
// "hf_test"), which is too short to match any pattern below.
//
// This exists because a HuggingFace token lives at ~/.config/hf during development
// (manyforge-bhx) and it would be trivially easy to paste one into a golden fixture.

// secretPatterns are the live-token shapes for every provider manyforge speaks to. Each
// requires the vendor prefix plus enough entropy that a fake ("hf_test") cannot match.
// NOTE: these regex sources do not match themselves — a character class follows each
// prefix, and `[` is not in any of the trailing character sets.
var secretPatterns = map[string]*regexp.Regexp{
	"huggingface user access token": regexp.MustCompile(`hf_[A-Za-z0-9]{30,}`),
	"anthropic api key":             regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{24,}`),
	"openrouter api key":            regexp.MustCompile(`sk-or-v1-[a-f0-9]{40,}`),
	"openai project key":            regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{24,}`),
	"github personal access token":  regexp.MustCompile(`ghp_[A-Za-z0-9]{30,}`),
}

// skipExts are binary/asset types that cannot meaningfully contain a pasted token and
// would otherwise cost time to scan.
var skipExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".woff": true, ".woff2": true, ".ttf": true, ".pdf": true, ".webp": true,
}

// secretScanExemptions lists files that MUST contain a token-shaped string, mapped to the
// single pattern each is allowed to trip. Scoped per-pattern on purpose: redact_test.go may
// carry a synthetic GitHub token to prove redactSecrets() strips it, but a huggingface or
// anthropic key pasted into that same file still fails the scan. Add an entry only with a
// reason, and only when the string is provably synthetic.
var secretScanExemptions = map[string]string{
	// Synthetic vector proving redactSecrets() strips GitHub tokens from sandbox stderr.
	"internal/agents/coding/redact_test.go": "github personal access token",
	// Design doc that quotes the redact_test.go vector verbatim.
	"docs/superpowers/plans/2026-06-28-review-redaction-provider-generality.md": "github personal access token",
}

const maxScanBytes = 2 << 20 // skip anything larger; no source file is 2MiB

func TestNoProviderSecretsCommitted(t *testing.T) {
	root := filepath.Join("..", "..")
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	self := "manyforge_bhx_provider_secret_scan_test.go"

	var scanned int
	seenExempt := map[string]bool{}
	for rel := range strings.SplitSeq(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" || skipExts[strings.ToLower(filepath.Ext(rel))] || filepath.Base(rel) == self {
			continue
		}
		path := filepath.Join(root, rel)
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() || fi.Size() > maxScanBytes {
			continue // deleted-but-tracked, or too large to be source
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		scanned++
		for name, re := range secretPatterns {
			loc := re.FindIndex(b)
			if loc == nil {
				continue
			}
			if secretScanExemptions[rel] == name {
				seenExempt[rel] = true
				continue // documented synthetic vector for exactly this pattern
			}
			// Report the location, NEVER the matched bytes.
			line := 1 + strings.Count(string(b[:loc[0]]), "\n")
			t.Errorf("MANYFORGE-BHX-PIN-1: %s:%d appears to contain a live %s — "+
				"rotate it immediately and use a fake stand-in in tests", rel, line, name)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned 0 files — the git ls-files walk is broken, so this pin proves nothing")
	}
	// An exemption that no longer matches anything is dead weight that would silently
	// grant cover to a future paste. Make it fail so it gets removed.
	for rel := range secretScanExemptions {
		if !seenExempt[rel] {
			t.Errorf("MANYFORGE-BHX-PIN-1: exemption for %q no longer matches any token-shaped "+
				"string — delete it from secretScanExemptions", rel)
		}
	}
}

// TestProviderSecretPatternsActuallyMatch proves the patterns above are live: a synthetic
// token (assembled at run time so this file never contains one) must match, and the fake
// stand-ins real tests use must not. Without this, a typo'd regex would make
// TestNoProviderSecretsCommitted a silent no-op that passes forever.
func TestProviderSecretPatternsActuallyMatch(t *testing.T) {
	entropy := strings.Repeat("aB3", 16) // 48 alnum chars, long enough for every pattern
	cases := []struct {
		pattern string
		live    string
		fake    string
	}{
		{"huggingface user access token", "hf" + "_" + entropy, "hf_test"},
		{"anthropic api key", "sk-" + "ant-" + entropy, "sk-test"},
		{"openrouter api key", "sk-or-" + "v1-" + strings.Repeat("a1", 24), "sk-or-v1-test"},
		{"openai project key", "sk-" + "proj-" + entropy, "sk-proj-test"},
		{"github personal access token", "ghp" + "_" + entropy, "ghp_test"},
	}
	for _, tc := range cases {
		re, ok := secretPatterns[tc.pattern]
		if !ok {
			t.Fatalf("no pattern registered for %q", tc.pattern)
		}
		if !re.MatchString(tc.live) {
			t.Errorf("%s: pattern does not match a live-shaped token — the scan pin is a no-op", tc.pattern)
		}
		if re.MatchString(tc.fake) {
			t.Errorf("%s: pattern matches the fake stand-in %q — tests cannot use it", tc.pattern, tc.fake)
		}
	}
}
