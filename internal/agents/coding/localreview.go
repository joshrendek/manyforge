package coding

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/connectors/github"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
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

// reviewSchemaLine instructs the model to emit only the findings JSON.
const reviewSchemaLine = `Review the provided diff hunks and output ONLY a single JSON object — no prose, no markdown fences — matching exactly this schema: {"summary": string, "findings": [{"file": string, "line": number|null, "severity": "info"|"warning"|"error", "title": string, "detail": string}]}`

// isLocalProvider reports whether a provider runs on-host (model never leaves the
// machine), in which case reviews go through the host-side direct-API path instead
// of the sandbox+opencode path.
func isLocalProvider(provider string) bool {
	return provider == "ollama" || provider == "vllm"
}

const (
	reviewMaxFileBytes  = 48 << 10 // skip any single file whose rendered hunks exceed this
	reviewMaxTotalBytes = 96 << 10 // default total-diff budget (cloud/opencode path — capable models)
	// localProviderMaxTotalBytes is a TIGHTER total-diff budget for on-host local providers
	// (Ollama/vLLM). Small models can't prompt-eval a large diff quickly — a ~28K-token diff
	// wedged ornith:9b for minutes at every context size we tried — so the local path sends far
	// less. Combined with the isNonReviewableDoc filter (which strips prose/plan files that both
	// waste this budget and derail weak models), this keeps local reviews fast and code-focused.
	localProviderMaxTotalBytes = 32 << 10
	// localReviewNumCtx is the REQUESTED model context. CAVEAT: Ollama's OpenAI-compatible
	// /v1/chat/completions endpoint IGNORES options.num_ctx, so for Ollama the EFFECTIVE window
	// is the server's OLLAMA_CONTEXT_LENGTH (or model default). Providers that honor num_ctx
	// (vLLM; Ollama's native /api/chat) use this directly.
	localReviewNumCtx = 32768
	// localReviewMaxTokens bounds the model's OUTPUT so a single review can't run away.
	// A reasoning model (ornith) with no cap will happily emit tens of thousands of tokens
	// on a large diff — minutes of generation that pins the single-threaded worker and the
	// GPU. This cap keeps a real review (which needs only a few thousand output tokens —
	// reasoning + a few dozen findings) well within budget; a review that exceeds it is
	// truncated, logged (finish_reason=="length"), and its partial output stays visible on
	// the failed review rather than hanging indefinitely.
	localReviewMaxTokens = 8192
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

// localBaseURLBlocked reports whether a local-review base-URL host must be refused.
// The local path dials with a plain (non-egress-proxied) client, so this guard is the
// ONLY thing keeping a run's diff from leaving the machine — it must allow ONLY on-host
// or trusted-LAN model endpoints and block everything else, including PUBLIC hosts.
//
// Note this is the INVERSE emphasis of netsafe.IsBlocked, which permits public IPs (its
// job is to block internal SSRF targets, not external ones). Here public is blocked; only:
//   - Loopback (127/8, ::1): always permitted — a model on the same host is never a pivot.
//   - Private LAN (RFC1918 / IPv6 ULA): permitted ONLY with the AllowPrivateBaseURL trust
//     opt-in (e.g. LM Studio on 192.168.x.x). Cloud-metadata (incl. the fd00:ec2::254 ULA)
//     and link-local stay blocked even then — screened via netsafe so that IMDS list can't
//     drift from the dialer's.
//   - A non-IP hostname passes only for the loopback name "localhost"; we deliberately do
//     NOT resolve arbitrary names here (a bare name could rebind to a public/metadata IP
//     between check and dial). A private-LAN endpoint must therefore be given by IP.
func localBaseURLBlocked(host string, allowPrivate bool) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return strings.ToLower(host) != "localhost"
	}
	if ip.IsLoopback() {
		return false
	}
	// Private LAN requires the opt-in AND must survive netsafe's unconditional
	// metadata/link-local screen (fd00:ec2::254 is an IsPrivate ULA that IMDS uses).
	if allowPrivate && ip.IsPrivate() && !netsafe.IsBlocked(ip, netsafe.Options{AllowPrivate: true}) {
		return false
	}
	return true // public, metadata, link-local, or untrusted/blocked private
}

// reviewResponseFormat is the OpenAI-compatible structured-output constraint that
// forces the model to emit ONLY a findings JSON object matching FindingsDoc's shape
// (summary + findings[]). Without it, chatty instruction-tuned models stream prose and
// ParseFindings fails (manyforge-6ax). Ollama honors response_format=json_schema on its
// /v1/chat/completions endpoint; providers that ignore it are no worse off than before.
func reviewResponseFormat() map[string]any {
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "code_review_findings",
			"strict": true,
			"schema": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"summary": map[string]any{"type": "string"},
					"findings": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]any{
								"file":     map[string]any{"type": "string"},
								"line":     map[string]any{"type": []string{"integer", "null"}},
								"severity": map[string]any{"type": "string", "enum": []string{"info", "warning", "error"}},
								"title":    map[string]any{"type": "string"},
								"detail":   map[string]any{"type": "string"},
							},
							"required": []string{"file", "line", "severity", "title", "detail"},
						},
					},
				},
				"required": []string{"summary", "findings"},
			},
		},
	}
}

// localReview POSTs the rendered diff payload to a local OpenAI-compatible chat
// endpoint (Ollama/vLLM) as a STREAM and parses the findings with ParseFindings. It
// accumulates the streamed delta.content into a buffer (rendered live in the UI via
// the worker heartbeat → prog.UpdateStream) and parses the full buffer on [DONE]. No
// sandbox/opencode: small local models can't drive opencode's agent loop, and the
// model is on-host so there is nothing to isolate. The model gets NO tools (chat→JSON
// only), so prompt injection can at worst yield bogus advisory findings.
//
// It first constrains the output with response_format=json_schema (manyforge-6ax):
// Ollama honors it and needs it (chatty models otherwise stream prose ParseFindings
// rejects). But some OpenAI-compatible servers (LM Studio) return an EMPTY stream under
// json_schema — so if the schema attempt yields no content, it retries ONCE without
// response_format (plain prompting works there; the model wraps JSON in a ```json fence
// that ParseFindings tolerates). This auto-handles both providers with no config.
// Returns (doc, promptTokens, completionTokens, err). prog may be nil (no-op).
func localReview(ctx context.Context, client *http.Client, cred AICredential, payload string, prog *Progress) (FindingsDoc, int64, int64, error) {
	if localBaseURLBlocked(cred.Host(), cred.AllowPrivateBaseURL) {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: local review base URL host %q is not an allowed local/private address: %w", cred.Host(), errs.ErrValidation)
	}
	if strings.TrimSpace(payload) == "" {
		return FindingsDoc{}, 0, 0, fmt.Errorf("coding: no reviewable changes for local review: %w", errs.ErrValidation)
	}

	buf, promptTokens, completionTokens, chunkCount, err := streamLocalReview(ctx, client, cred, payload, true, prog)
	if err != nil {
		return FindingsDoc{}, promptTokens, completionTokens, err
	}
	// Empty stream under json_schema (LM Studio) → retry once without the constraint.
	if chunkCount == 0 {
		slog.Default().InfoContext(ctx, "local review: empty response under json_schema, retrying without response_format",
			"model", cred.Model)
		buf, promptTokens, completionTokens, chunkCount, err = streamLocalReview(ctx, client, cred, payload, false, prog)
		if err != nil {
			return FindingsDoc{}, promptTokens, completionTokens, err
		}
	}

	if completionTokens == 0 { // usage frame absent → best-effort fallback
		completionTokens = int64(chunkCount)
	}
	doc, perr := ParseFindings([]byte(buf))
	return doc, promptTokens, completionTokens, perr
}

// streamPreview builds the live-progress preview string. Once the model emits final
// answer content, that is shown (the findings JSON forming); until then a reasoning
// model's chain-of-thought is shown under a marker so a long "thinking" phase is
// visibly in-progress rather than an empty box. Never used for parsing.
func streamPreview(content, reasoning string) string {
	if content != "" {
		return content
	}
	if reasoning != "" {
		return "💭 reasoning…\n" + reasoning
	}
	return ""
}

// streamLocalReview performs ONE chat/completions streaming attempt and returns the
// accumulated content plus token usage and the delta-chunk count. When useSchema is
// true it sends response_format=json_schema (manyforge-6ax); false omits it (the
// empty-stream fallback path). It never parses — the caller decides whether to retry
// and then parses the returned buffer.
func streamLocalReview(ctx context.Context, client *http.Client, cred AICredential, payload string, useSchema bool, prog *Progress) (string, int64, int64, int, error) {
	body := map[string]any{
		"model": cred.Model,
		"messages": []map[string]string{
			{"role": "system", "content": reviewInstructions + "\n\n" + reviewSchemaLine},
			{"role": "user", "content": "Diff hunks to review:\n" + payload},
		},
		"stream": true,
		// Bound output so a reasoning model can't run away on a large diff (pins the
		// worker + GPU for minutes). max_tokens is the OpenAI-standard field; max_completion_tokens
		// is its newer alias — send both so servers that renamed it still honor the cap.
		"max_tokens":            localReviewMaxTokens,
		"max_completion_tokens": localReviewMaxTokens,
		// In OpenAI-compatible streaming, usage is omitted unless explicitly requested;
		// without this, token accounting silently goes to 0.
		"stream_options": map[string]any{"include_usage": true},
		"options":        map[string]any{"temperature": 0, "num_ctx": localReviewNumCtx},
	}
	if useSchema {
		// Force the model to emit ONLY the findings JSON (manyforge-6ax). Chatty
		// instruction-tuned models (e.g. ornith) otherwise stream prose/markdown that
		// ParseFindings rejects. Ollama honors this; LM Studio returns empty under it
		// (handled by the caller's retry-without-schema).
		body["response_format"] = reviewResponseFormat()
	}
	reqBody, _ := json.Marshal(body)

	url := strings.TrimRight(cred.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("coding: build local review request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if cred.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cred.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("coding: local review request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", 0, 0, 0, fmt.Errorf("coding: local provider status %d", resp.StatusCode)
	}

	var (
		buf              strings.Builder // final-answer content — the ONLY text parsed into findings
		reasoning        strings.Builder // chain-of-thought (reasoning models) — shown live, never parsed
		promptTokens     int64
		completionTokens int64
		chunkCount       int
		droppedFrames    int
		finishReason     string
		lastUpdate       time.Time
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20) // SSE frames can be large
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
					// reasoning_content carries a reasoning model's chain-of-thought
					// (ornith, deepseek-r1, qwq, …) separately from the final answer.
					// It is surfaced live in the preview but NEVER parsed as findings.
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				// FinishReason is "length" when the model was cut off at the token
				// limit (→ truncated, unparseable JSON), "stop" on a clean finish.
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if jerr := json.Unmarshal([]byte(data), &chunk); jerr != nil {
			// A well-formed keep-alive is an empty/comment line (skipped above), so a
			// non-JSON data: frame here is anomalous. Count it rather than dropping it
			// silently — a provider bug that corrupts the stream then leaves a breadcrumb
			// (warned below) instead of an unexplained ParseFindings failure.
			droppedFrames++
			continue
		}
		if len(chunk.Choices) > 0 {
			if fr := chunk.Choices[0].FinishReason; fr != "" {
				finishReason = fr
			}
			d := chunk.Choices[0].Delta
			updated := false
			if d.ReasoningContent != "" {
				reasoning.WriteString(d.ReasoningContent) // preview only
				updated = true
			}
			if d.Content != "" {
				buf.WriteString(d.Content)
				chunkCount++ // counts FINAL-answer chunks: 0 ⇒ "empty" ⇒ caller retries
				updated = true
			}
			// Throttle progress writes to ~2/s — redaction + marshal happen on the
			// heartbeat's Snapshot(); updating the shared buffer per token is wasteful.
			// Show reasoning while the model thinks, then the answer as it forms, so a
			// long-reasoning model never looks hung with an empty preview.
			if updated && time.Since(lastUpdate) > 500*time.Millisecond {
				prog.UpdateStream(chunkCount, streamPreview(buf.String(), reasoning.String()))
				lastUpdate = time.Now()
			}
		}
		if chunk.Usage != nil {
			promptTokens = chunk.Usage.PromptTokens
			completionTokens = chunk.Usage.CompletionTokens
		}
	}
	if serr := sc.Err(); serr != nil {
		return "", promptTokens, completionTokens, chunkCount, fmt.Errorf("coding: read local review stream: %w", serr)
	}
	prog.UpdateStream(chunkCount, streamPreview(buf.String(), reasoning.String())) // final flush

	// Surface dropped frames (manyforge-6ax follow-up / gemini review): a malformed
	// provider frame can corrupt the accumulated JSON and fail ParseFindings; without
	// this the drop was silent and undebuggable. Byte content is not logged (may echo
	// diff/secret material) — only the count and model.
	if droppedFrames > 0 {
		slog.Default().WarnContext(ctx, "local review: dropped unparseable stream frames",
			"count", droppedFrames, "model", cred.Model)
	}

	// finish_reason=="length" ⇒ the model hit its token limit mid-output, so the JSON is
	// truncated and ParseFindings will reject it as "empty findings output". Log the real
	// cause explicitly (with token counts) so a truncated review is diagnosable rather
	// than a mysterious parse failure.
	if finishReason == "length" {
		slog.Default().WarnContext(ctx, "local review: model output truncated at token limit — findings JSON is incomplete",
			"model", cred.Model, "completion_tokens", completionTokens, "content_bytes", buf.Len())
	}

	return buf.String(), promptTokens, completionTokens, chunkCount, nil
}
