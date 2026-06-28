package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/connectors/github"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// reviewInstructions is the balanced review prompt. KEEP IN SYNC with the cloud
// path in deploy/sandbox/entrypoint.sh and the eval harness in
// tools/local-model-eval/run.sh.
const reviewInstructions = `You are a senior software engineer reviewing a pull request. Report only genuine problems you are confident about — do NOT invent issues, speculate, or flag pure style/formatting preferences.

Prioritize in this order: (1) bugs and correctness errors (crashes, nil/undefined access, logic errors, race conditions, incorrect results); (2) security vulnerabilities (injection, auth/authorization gaps, secret exposure, unsafe or unbounded input); (3) notable maintainability problems (unhandled errors, resource leaks, missing validation). Skip cosmetic style and formatting.

Set each finding's severity to exactly one of:
- "error": a real bug or security vulnerability causing incorrect behavior, a crash, data loss, or an exploitable condition.
- "warning": a likely problem or risky pattern that should be fixed (e.g. an unhandled error, a missing bound/validation, a resource leak).
- "info": a minor but worthwhile maintainability suggestion (never pure style).

You are given the changed code as unified-diff hunks: each block is headed by "=== <path> ===", and every changed line shows its current-file line number in the left gutter with a +/space marker. Use that real file path and gutter line number in each finding. Report each distinct issue once. If there are no genuine problems, return an empty findings array.`

// reviewSchemaLine instructs the model to emit only the findings JSON.
const reviewSchemaLine = `Review the provided diff hunks and output ONLY a single JSON object — no prose, no markdown fences — matching exactly this schema: {"summary": string, "findings": [{"file": string, "line": number|null, "severity": "info"|"warning"|"error", "title": string, "detail": string}]}`

// isLocalProvider reports whether a provider runs on-host (model never leaves the
// machine), in which case reviews go through the host-side direct-API path instead
// of the sandbox+opencode path.
func isLocalProvider(provider string) bool {
	return provider == "ollama" || provider == "vllm"
}

const (
	localReviewMaxFileBytes  = 32 << 10 // skip any single file whose rendered hunks exceed this
	localReviewMaxTotalBytes = 64 << 10 // cap total rendered diff to fit localReviewNumCtx
	localReviewNumCtx        = 16384    // Ollama context window; ~64KB diff + prompt + output fits
)

// codeExt is the set of source extensions the local reviewer prioritizes — the
// inline budget is small, so spend it on code, not data/docs (e.g. .jsonl, .md).
var codeExt = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".py": true,
	".rs": true, ".java": true, ".rb": true, ".c": true, ".h": true, ".cc": true,
	".cpp": true, ".cs": true, ".kt": true, ".swift": true, ".php": true, ".scala": true,
	".sql": true, ".sh": true, ".tf": true, ".proto": true,
}

// assembleDiffPayload renders the changed files' hunks into the local-review
// payload: source files first (the budget is small; spend it on code), then
// path-sorted, stopping at the total budget. It returns the payload plus the paths
// it could not include — skipped (no usable patch: binary or omitted by GitHub) and
// omitted (dropped because the budget filled) — so callers can surface them.
func assembleDiffPayload(files []connectors.ChangedFile) (payload string, skipped, omitted []string) {
	ordered := append([]connectors.ChangedFile(nil), files...)
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
		if len(block) > localReviewMaxFileBytes || total+len(block) > localReviewMaxTotalBytes {
			omitted = append(omitted, f.Path)
			continue
		}
		b.WriteString(block)
		total += len(block)
	}
	return b.String(), skipped, omitted
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

// isLoopbackHost reports whether h is the local machine — the only host a local
// provider's base URL may target (a deliberate, narrow exception to the
// egress-isolation policy: the model is on-host, so the code never leaves it).
func isLoopbackHost(h string) bool {
	switch h {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// localReview POSTs the rendered diff payload to a local OpenAI-compatible chat
// endpoint (Ollama/vLLM) and parses the findings with ParseFindings. No
// sandbox/opencode: small local models can't drive opencode's agent loop, and the
// model is on-host so there is nothing to isolate. The model gets NO tools
// (chat→JSON only), so prompt injection can at worst yield bogus advisory findings.
// Returns (doc, promptTokens, completionTokens, err).
func localReview(ctx context.Context, client *http.Client, cred AICredential, payload string) (FindingsDoc, int64, int64, error) {
	if !isLoopbackHost(cred.Host()) {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: local review base URL must be loopback, got %q: %w", cred.Host(), errs.ErrValidation)
	}
	if strings.TrimSpace(payload) == "" {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: no reviewable changes for local review: %w", errs.ErrValidation)
	}

	reqBody, _ := json.Marshal(map[string]any{
		"model": cred.Model,
		"messages": []map[string]string{
			{"role": "system", "content": reviewInstructions + "\n\n" + reviewSchemaLine},
			{"role": "user", "content": "Diff hunks to review:\n" + payload},
		},
		"stream":  false,
		"options": map[string]any{"temperature": 0, "num_ctx": localReviewNumCtx},
	})

	url := strings.TrimRight(cred.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: build local review request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cred.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cred.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: local review request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: local provider status %d", resp.StatusCode)
	}

	var cc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &cc); err != nil {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: decode local review response: %w", err)
	}
	if len(cc.Choices) == 0 {
		return FindingsDoc{}, cc.Usage.PromptTokens, cc.Usage.CompletionTokens, fmt.Errorf("coding: local provider returned no choices")
	}
	doc, perr := ParseFindings([]byte(cc.Choices[0].Message.Content))
	return doc, cc.Usage.PromptTokens, cc.Usage.CompletionTokens, perr
}
