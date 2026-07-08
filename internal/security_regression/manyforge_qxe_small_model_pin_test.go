// manyforge-qxe (cost-leak regression, not a security invariant — kept here so
// `make sec-test` guards it alongside the other entrypoint source-pins):
//
// The opencode sandbox runner (deploy/sandbox/entrypoint.sh) must pin opencode's
// "small_model" to the same review model. opencode auto-generates a session title
// (and other auxiliary summaries) on every `opencode run`; if "small_model" is
// unset it DEFAULTS to Claude Haiku and bills a throwaway title call to whatever
// provider/key the lane was handed — on the hub OpenRouter key for cloud reviews.
// manyforge discards opencode's title (the check-run title is hardcoded), so this
// is pure waste: one extra Haiku call per dimension-lane per review.
//
// The fix pins small_model == model in BOTH config branches (built-in provider and
// the local @ai-sdk/openai-compatible provider). A refactor that drops either would
// silently reintroduce the leak, so pin the literal in each branch.
package security_regression

import (
	"strings"
	"testing"
)

func TestOpencodeSmallModelPinnedToReviewModel(t *testing.T) {
	entry := mustRead(t, "../../deploy/sandbox/entrypoint.sh")

	// The config's model line and the small_model line both interpolate ${MODEL},
	// so the pinned literal is identical; it must appear in BOTH printf blocks
	// (local branch + built-in branch).
	const smallModelLine = `"small_model": "'"${MODEL}"'",`
	if got := strings.Count(entry, smallModelLine); got < 2 {
		t.Errorf("manyforge-qxe: entrypoint must pin opencode's small_model to the review model in BOTH config branches (local + built-in) so title/summary side-calls don't default to Claude Haiku on the provider key; found %d of 2 occurrences of %q — pin broken, update this pin in the same change if the refactor is intentional", got, smallModelLine)
	}
}
