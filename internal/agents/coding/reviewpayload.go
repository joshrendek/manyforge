package coding

// reviewpayload.go holds the review-payload assembly, prompt constants, and
// per-provider diff-budget helpers shared by every review path. Local (Ollama/vLLM)
// and cloud providers alike now run through the opencode sandbox (manyforge-9er
// Tasks 4-5); this file used to also hold the direct-POST host-side local-review
// call (localReview and its streaming/response-format helpers) for on-host models,
// but that path had no remaining caller once the sandbox routing landed and was
// deleted in Task 6. What's left here is what both paths still share.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/connectors/github"
)

// reviewInstructions is the balanced review prompt. KEEP IN SYNC with the cloud
// path in deploy/sandbox/entrypoint.sh and the eval harness in
// tools/local-model-eval/run.sh.
const reviewInstructions = `You are a senior software engineer reviewing a pull request. Surface every plausible correctness, security, or robustness concern — including ones you are only moderately confident about — and express your confidence through the severity field rather than by staying silent. Do not withhold a real risk because it seems minor or uncertain. Still skip pure style/formatting preferences, and do not fabricate issues with no basis in the code.

Prioritize in this order: (1) bugs and correctness errors (crashes, nil/undefined access, logic errors, race conditions, incorrect results); (2) security vulnerabilities (injection, auth/authorization gaps, secret exposure, unsafe or unbounded input); (3) robustness and maintainability problems (unhandled errors, resource leaks, missing validation, silent failures).

Set the severity of each finding to exactly one of:
- "error": a real bug or security vulnerability causing incorrect behavior, a crash, data loss, or an exploitable condition.
- "warning": a likely problem or risky pattern that should be fixed (e.g. an unhandled error, a missing bound/validation, a resource leak).
- "info": a plausible concern or worthwhile improvement worth surfacing to the reviewer — when unsure whether something is a real issue, prefer flagging it here rather than omitting it (but never pure style).

You are given the changed code as unified-diff hunks: each block is headed by "=== <path> ===", and every changed line shows its current-file line number in the left gutter with a +/space marker. Use that real file path and gutter line number in each finding. Report each distinct issue once. Only return an empty findings array if the diff genuinely contains nothing worth surfacing.`

// isConstrainedProvider reports whether a provider serves a small/slow model that cannot
// prompt-eval a large diff quickly, and so must be sent a tighter one. This is a statement
// about MODEL CAPABILITY, not about network locality: ollama/vllm are on-host, while
// huggingface is a public ZeroGPU Space, yet all three run modest models on unbatched
// inference. (Network trust lives on the credential's AllowPrivateBaseURL flag; the opencode
// provider mechanism lives in deploy/sandbox/entrypoint.sh's LLM_OPENCODE_MODE. Three
// separate axes — see manyforge-bhx.)
//
// Every provider runs through the sandbox+opencode path (manyforge-9er Tasks 4-5); this is
// consulted only to pick the constrainedProviderMaxTotalBytes diff budget.
func isConstrainedProvider(provider string) bool {
	return provider == "ollama" || provider == "vllm" || provider == "huggingface"
}

const (
	reviewMaxFileBytes  = 48 << 10 // skip any single file whose rendered hunks exceed this
	reviewMaxTotalBytes = 96 << 10 // default total-diff budget (capable cloud models)
	// constrainedProviderMaxTotalBytes is a TIGHTER total-diff budget for providers serving
	// small models on unbatched inference (Ollama/vLLM on-host; a HuggingFace ZeroGPU Space
	// remotely). Small models can't prompt-eval a large diff quickly — a ~28K-token diff wedged
	// ornith:9b for minutes at every context size we tried — so these lanes send far less. On
	// ZeroGPU the pressure is sharper still: the GPU is released between calls, so every opencode
	// turn re-prefills the whole conversation against a hard per-call duration cap. Combined with
	// the isNonReviewableDoc filter (which strips prose/plan files that both waste this budget and
	// derail weak models), this keeps constrained reviews fast and code-focused.
	constrainedProviderMaxTotalBytes = 32 << 10
)

// codeExt is the set of source extensions the local reviewer prioritizes — the
// inline budget is small, so spend it on code, not data/docs (e.g. .jsonl, .md).
var codeExt = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".py": true,
	".rs": true, ".java": true, ".rb": true, ".c": true, ".h": true, ".cc": true,
	".cpp": true, ".cs": true, ".kt": true, ".swift": true, ".php": true, ".scala": true,
	".sql": true, ".sh": true, ".tf": true, ".proto": true,
}

// isNonReviewableDoc reports whether a changed file is prose / planning / tracking content
// rather than reviewable code. A code review targets code — feeding it plan/spec/doc markdown
// or tracker data wastes the token budget and derails weaker local models into acting on the
// plan instead of reviewing the diff (manyforge-206 dogfood: a 447-line plan doc made
// ornith:9b hallucinate findings about non-existent files). Non-prose config/data (e.g.
// .yaml, Dockerfile, .json) is NOT excluded — only prose docs and known tracker paths.
func isNonReviewableDoc(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".mdx", ".rst", ".adoc":
		return true
	}
	lower := strings.ToLower(path)
	for _, seg := range []string{"docs/", ".beads/"} {
		if strings.HasPrefix(lower, seg) || strings.Contains(lower, "/"+seg) {
			return true
		}
	}
	return false
}

// assembleDiffPayload renders the changed files' hunks into the review payload. Prose/
// planning docs are dropped first (isNonReviewableDoc → filtered); the remaining reviewable
// files are ordered source-first and rendered up to maxTotalBytes (callers pass a tighter
// budget for on-host local providers). Returns the payload plus the paths it could not
// include — skipped (no usable patch: binary or omitted by GitHub), omitted (budget filled),
// and filtered (non-reviewable docs) — so callers can surface them (no silent caps).
func assembleDiffPayload(files []connectors.ChangedFile, maxTotalBytes int) (payload string, skipped, omitted, filtered []string) {
	reviewable := make([]connectors.ChangedFile, 0, len(files))
	for _, f := range files {
		if isNonReviewableDoc(f.Path) {
			filtered = append(filtered, f.Path)
			continue
		}
		reviewable = append(reviewable, f)
	}
	ordered := append([]connectors.ChangedFile(nil), reviewable...)
	sort.SliceStable(ordered, func(i, j int) bool {
		ci := codeExt[strings.ToLower(filepath.Ext(ordered[i].Path))]
		cj := codeExt[strings.ToLower(filepath.Ext(ordered[j].Path))]
		if ci != cj {
			return ci // code before non-code
		}
		return ordered[i].Path < ordered[j].Path
	})
	var b strings.Builder
	total := 0
	for _, f := range ordered {
		rendered := github.RenderAnnotatedHunks(f.Patch)
		if rendered == "" {
			skipped = append(skipped, f.Path)
			continue
		}
		block := fmt.Sprintf("\n=== %s ===\n%s", f.Path, rendered)
		if len(block) > reviewMaxFileBytes || total+len(block) > maxTotalBytes {
			omitted = append(omitted, f.Path)
			continue
		}
		b.WriteString(block)
		total += len(block)
	}
	return b.String(), skipped, omitted, filtered
}

// commentableMap reduces ChangedFiles to the file→commentable-lines map buildReview
// needs to place inline comments.
func commentableMap(files []connectors.ChangedFile) map[string]map[int]bool {
	out := make(map[string]map[int]bool, len(files))
	for _, f := range files {
		out[f.Path] = f.Commentable
	}
	return out
}
