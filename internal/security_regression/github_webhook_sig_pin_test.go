// No build tag: source-level pins run in `make test` and `make sec-test` with NO
// infrastructure, complementing the behavioral tests in internal/githubapp/.

package security_regression

import (
	"os"
	"strings"
	"testing"
)

const FindingGithubWebhookSigPin = "MF-009-GH-WEBHOOK-SIG"

// TestGithubWebhookSignaturePinned pins the GitHub webhook HMAC verification
// in place: constant-time compare via hmac.Equal, SHA-256, fail-closed on an
// empty secret, and the X-Hub-Signature-256 header name — so a refactor that
// silently weakens (or removes) constant-time verification fails CI even if
// the behavioral webhook tests are also weakened or removed.
func TestGithubWebhookSignaturePinned(t *testing.T) {
	b, err := os.ReadFile("../githubapp/webhook.go")
	if err != nil {
		t.Fatalf("read webhook.go: %v", err)
	}
	src := string(b)
	for _, want := range []string{"hmac.Equal", "sha256.New", `secret == ""`, "X-Hub-Signature-256"} {
		if !strings.Contains(src, want) {
			t.Fatalf("%s: webhook.go missing %q", FindingGithubWebhookSigPin, want)
		}
	}
}
