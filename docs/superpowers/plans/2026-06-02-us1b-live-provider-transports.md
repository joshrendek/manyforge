# US1b — Live AI Provider Transports (Anthropic + OpenAI-compat) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. The project is **TDD-mandatory** and **bd-tracked** (no TodoWrite). Commit per task. The repo **has a remote** — `git push` at session end per CLAUDE.md. NO `Co-Authored-By` trailer.

**Goal:** Implement the two live HTTP transports that satisfy the `ai.Provider` interface US1a defined — an `anthropic` adapter (Messages API, `tool_use` blocks) and an `openaicompat` adapter (chat/completions, `tool_calls`; user-supplied `base_url` covering OpenAI/Ollama/vLLM) — plus a provider-name→constructor factory, a seeded model registry, and golden-fixture replay tests with **zero live API calls in CI**.

**Architecture:** Each transport is a small struct holding `apiKey`/`baseURL`/`model` + an injected `*http.Client`, implementing `Complete(ctx, ai.Request) (ai.Response, error)`. Pure `build*Request`/`parse*Response` functions translate between the common `ai` schema and each provider's wire format; `Complete` is the thin HTTP shell that calls them and maps HTTP failures onto the `ai.Err*` sentinels. A prod factory `ai.New(cred)` dispatches on `cred.Provider`, always wiring the SSRF-guarded `netsafe` client; tests construct transports directly against an `httptest.Server` with a plain client and replay committed golden fixtures.

**Tech Stack:** Go 1.25 (`github.com/manyforge/manyforge`), `net/http` + `encoding/json` stdlib (NO vendor AI SDK — confirmed absent from `go.mod`), `internal/platform/netsafe` (SSRF-guarded `*http.Client`), `net/http/httptest` (the established in-repo replay style), `internal/platform/ai` (US1a schema/interface/registry), `internal/security_regression` (merge-gate pins).

**Scope boundary:** This plan adds ONLY the live transports, the factory, the registry seed, golden-fixture machinery, and the SSRF pin — all inside `internal/platform/ai/` + one `internal/security_regression/` file. It does NOT touch the run loop, the gate, the approvals queue, agent definitions, `cmd/manyforge` wiring, or config env-vars (the gateway is *constructed* at startup in US3; US1b uses a package-default timeout). bd issue: **manyforge-ma9** (epic **manyforge-deo**).

---

## Background the engineer must not relearn

- **US1a already exists and is GREEN.** The common schema, `Provider` interface, sentinels, `Registry`, and `MockProvider` are done. Read these before starting — every type below comes from them:
  - `internal/platform/ai/provider.go` — `Provider` interface; sentinels `ErrProviderUnavailable` (network/5xx/timeout — retryable), `ErrBadRequest` (4xx — not retryable), `ErrContextLength`.
  - `internal/platform/ai/schema.go` — `Role` (`RoleSystem`/`RoleUser`/`RoleAssistant`/`RoleTool`), `FinishReason` (`FinishStop`/`FinishToolUse`/`FinishLength`/`FinishOther`), `ToolDef{Name,Description,Schema json.RawMessage}`, `ToolCall{ID,Name,Args json.RawMessage}`, `ToolResult{CallID,Content,IsError}`, `Message{Role,Text,ToolCalls,ToolResults}`, `Request{Model,System,Messages,Tools,MaxTokens,Temperature}`, `Usage{InputTokens,OutputTokens}` (+ `Total()`), `Response{Text,ToolCalls,FinishReason,Usage}`.
  - `internal/platform/ai/registry.go` — `Model{ID,Provider,ContextWindow,InputCentsPerMTok,OutputCentsPerMTok,SupportsTools}`, `NewRegistry()`, `(*Registry).Register(Model)`, `(*Registry).Lookup(id) (Model,bool)`.
  - `internal/platform/ai/mock.go` — `MockProvider` (the constructor/`Complete`/`Name()` shape the live transports mirror).
- **`internal/agents/credential.go`** — `ResolvedCredential{Provider, APIKey, BaseURL, Model string}` is exactly what the factory consumes. `knownProviders = {"anthropic","openai","ollama","vllm"}` (closed set mirroring migration 0025's `ai_provider` enum). Follow-up `manyforge-uc2` tracks keeping that set ↔ enum in lockstep — NOT this plan's job, but the factory must accept all four names.
- **`internal/platform/netsafe/client.go`** — `func NewClient(timeout time.Duration) *http.Client` returns a client whose dialer resolves the host and refuses RFC1918/loopback/link-local/metadata IPs (`Blocked(nil)==true`, fail-closed). It is the FIRST consumer-less SSRF guard in the repo; US1b is its first caller. **It rejects loopback**, so tests must use `server.Client()`, never `netsafe.NewClient`.
- **No golden-fixture/`testdata` machinery exists yet** (only `mock.go`'s comment "recorded golden fixtures arrive in US1b"). We build it: commit hand-authored fixtures matching real provider wire shapes, replay via `httptest.Server`. An optional env-gated "record" test refreshes a fixture from the live API for maintainers who have a key — skipped by default so CI stays hermetic.
- **Wire-format gotchas:**
  - Anthropic `tool_use.input` is a JSON **object**; OpenAI `tool_calls[].function.arguments` is a JSON **string**. Both map to our `ToolCall.Args json.RawMessage`.
  - Anthropic has only `user`/`assistant` roles + a top-level `system` field. Our `RoleTool` (tool results) maps to a `user` message carrying `tool_result` content blocks; our `RoleSystem`/`Request.System` map to the top-level `system` string.
  - OpenAI keeps `system`/`tool` as first-class message roles.
  - Anthropic endpoint: `POST {base}/v1/messages`, default base `https://api.anthropic.com`, headers `x-api-key` + `anthropic-version: 2023-06-01`. OpenAI-compat endpoint: `POST {base}/chat/completions` (base is expected to already include the `/v1` segment, per OpenAI/Ollama convention), header `Authorization: Bearer <key>` (omitted when key is empty, e.g. Ollama).
- **Make targets:** `make test` (`go test ./...`), `make sec-test` (`-tags integration ./internal/security_regression/...`), `make int-test` (`-tags integration -p 1 ./...`), `make contract-test`, `make lint` (`go vet` + golangci-lint if present). The SSRF pin file is **untagged** so it runs in BOTH `make test` (fast) and `make sec-test`.
- **gopls phantom diagnostics** on new files are STALE this codebase — trust `go build ./...`, never the IDE.

---

## File structure

| File | Responsibility |
|---|---|
| `internal/platform/ai/anthropic.go` | `AnthropicProvider` + Anthropic wire structs + pure `buildAnthropicRequest` / `parseAnthropicResponse` + `Complete` HTTP shell + error mapping. |
| `internal/platform/ai/anthropic_test.go` | Pure-mapping pins + `httptest` round-trip (text + tool_use golden fixtures) + HTTP-error→sentinel pins. |
| `internal/platform/ai/openaicompat.go` | `OpenAICompatProvider` + OpenAI wire structs + pure `buildOpenAIRequest` / `parseOpenAIResponse` + `Complete` HTTP shell + error mapping. |
| `internal/platform/ai/openaicompat_test.go` | Pure-mapping pins + `httptest` round-trip (text + tool_calls golden fixtures) + HTTP-error→sentinel pins. |
| `internal/platform/ai/factory.go` | `New(cred ResolvedCredential) (Provider, error)` — dispatch on provider name, wire `netsafe` client, fail-closed on unknown provider; `defaultRequestTimeout`. |
| `internal/platform/ai/factory_test.go` | Dispatch correctness + unknown-provider → `ErrBadRequest`. |
| `internal/platform/ai/seed.go` | `RegisterDefaults(*Registry)` — known models (claude/gpt-4o…) with pricing + tool support. Completes US1's "Registry seeded for known models". |
| `internal/platform/ai/seed_test.go` | Seeded models resolve + cost math sanity. |
| `internal/platform/ai/fixtures_test.go` | `loadGolden(t,name)` helper + optional env-gated `AI_RECORD` live-refresh tests (skipped by default). |
| `internal/platform/ai/testdata/anthropic_text.json` | Recorded Anthropic Messages response — final text, `stop_reason:end_turn`. |
| `internal/platform/ai/testdata/anthropic_tool_use.json` | Recorded Anthropic response — `tool_use` block, `stop_reason:tool_use`. |
| `internal/platform/ai/testdata/openai_text.json` | Recorded OpenAI chat/completions response — text, `finish_reason:stop`. |
| `internal/platform/ai/testdata/openai_tool_calls.json` | Recorded OpenAI response — `tool_calls`, `finish_reason:tool_calls`. |
| `internal/security_regression/ai_provider_ssrf_pin_test.go` | Pin (untagged, matches `*_pin_test.go` convention): factory's openai-compat path routes through `netsafe` (source-level) AND a RFC1918 `base_url` is refused at dial (behavioral). Reuses existing `mustRead`. |

> **Module note:** the factory needs a resolved-credential type. `internal/platform/ai` is a low-level platform package and must NOT depend on the domain package `internal/agents` (today neither imports the other; US3 will add `agents → ai`). So the factory accepts a **small local `ai.Credential` struct**, not `agents.ResolvedCredential` — the agents layer maps one onto the other at the US3 call site. See Task 7.

---

## Task 1: Anthropic — wire structs + `buildAnthropicRequest` (pure)

**Files:**
- Create: `internal/platform/ai/anthropic.go`
- Test: `internal/platform/ai/anthropic_test.go`

- [ ] **Step 1: Write the failing test**

```go
package ai

import (
	"encoding/json"
	"testing"
)

func TestBuildAnthropicRequest(t *testing.T) {
	req := Request{
		Model:       "claude-sonnet-4-5",
		System:      "you are helpful",
		MaxTokens:   1024,
		Temperature: 0.2,
		Tools: []ToolDef{{
			Name: "get_ticket", Description: "fetch a ticket",
			Schema: json.RawMessage(`{"type":"object"}`),
		}},
		Messages: []Message{
			{Role: RoleUser, Text: "look up t-42"},
			{Role: RoleAssistant, Text: "ok", ToolCalls: []ToolCall{
				{ID: "toolu_1", Name: "get_ticket", Args: json.RawMessage(`{"id":"t-42"}`)},
			}},
			{Role: RoleTool, ToolResults: []ToolResult{
				{CallID: "toolu_1", Content: "open", IsError: false},
			}},
		},
	}

	out := buildAnthropicRequest(req, "claude-sonnet-4-5")

	if out.Model != "claude-sonnet-4-5" || out.MaxTokens != 1024 || out.System != "you are helpful" {
		t.Fatalf("scalar fields wrong: %+v", out)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "get_ticket" || string(out.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tools wrong: %+v", out.Tools)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(out.Messages), out.Messages)
	}
	// user text block
	if out.Messages[0].Role != "user" || len(out.Messages[0].Content) != 1 || out.Messages[0].Content[0].Type != "text" || out.Messages[0].Content[0].Text != "look up t-42" {
		t.Fatalf("user msg wrong: %+v", out.Messages[0])
	}
	// assistant: text block + tool_use block
	if out.Messages[1].Role != "assistant" || len(out.Messages[1].Content) != 2 {
		t.Fatalf("assistant msg wrong: %+v", out.Messages[1])
	}
	if out.Messages[1].Content[1].Type != "tool_use" || out.Messages[1].Content[1].ID != "toolu_1" || out.Messages[1].Content[1].Name != "get_ticket" || string(out.Messages[1].Content[1].Input) != `{"id":"t-42"}` {
		t.Fatalf("tool_use block wrong: %+v", out.Messages[1].Content[1])
	}
	// tool result -> user message with tool_result block
	if out.Messages[2].Role != "user" || out.Messages[2].Content[0].Type != "tool_result" || out.Messages[2].Content[0].ToolUseID != "toolu_1" || out.Messages[2].Content[0].Content != "open" || out.Messages[2].Content[0].IsError {
		t.Fatalf("tool_result block wrong: %+v", out.Messages[2].Content[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run TestBuildAnthropicRequest -v`
Expected: FAIL — `undefined: buildAnthropicRequest`.

- [ ] **Step 3: Write minimal implementation**

```go
package ai

import (
	"encoding/json"
)

// Anthropic Messages API wire format. Hand-rolled (no vendor SDK). Only the
// fields we send/read are modeled; unknown response fields are ignored.

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicVersion        = "2023-06-01"
)

type anthropicReq struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicMessage struct {
	Role    string           `json:"role"` // "user" | "assistant"
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is a polymorphic content block. Type selects which fields are
// populated: text -> Text; tool_use -> ID/Name/Input; tool_result -> ToolUseID/Content/IsError.
type anthropicBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// buildAnthropicRequest maps the common Request onto Anthropic's Messages wire
// format. model overrides req.Model when req.Model is empty (transport default).
func buildAnthropicRequest(req Request, model string) anthropicReq {
	out := anthropicReq{
		Model:       model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		System:      req.System,
	}
	for _, td := range req.Tools {
		out.Tools = append(out.Tools, anthropicTool{
			Name: td.Name, Description: td.Description, InputSchema: td.Schema,
		})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			// Fold a system message into the top-level system field.
			if out.System != "" {
				out.System += "\n\n"
			}
			out.System += m.Text
		case RoleTool:
			var blocks []anthropicBlock
			for _, tr := range m.ToolResults {
				blocks = append(blocks, anthropicBlock{
					Type: "tool_result", ToolUseID: tr.CallID, Content: tr.Content, IsError: tr.IsError,
				})
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: blocks})
		default: // RoleUser / RoleAssistant
			var blocks []anthropicBlock
			if m.Text != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: tc.Args,
				})
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: string(m.Role), Content: blocks})
		}
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run TestBuildAnthropicRequest -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/anthropic.go internal/platform/ai/anthropic_test.go
git commit -m "feat(ai): anthropic request marshaling (US1b)"
```

---

## Task 2: Anthropic — `parseAnthropicResponse` (pure)

**Files:**
- Modify: `internal/platform/ai/anthropic.go`
- Test: `internal/platform/ai/anthropic_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestParseAnthropicResponse_Text(t *testing.T) {
	body := []byte(`{
		"content":[{"type":"text","text":"hello there"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":12,"output_tokens":8}
	}`)
	resp, err := parseAnthropicResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Text != "hello there" || resp.FinishReason != FinishStop {
		t.Fatalf("text/finish wrong: %+v", resp)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 8 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("want no tool calls, got %+v", resp.ToolCalls)
	}
}

func TestParseAnthropicResponse_ToolUse(t *testing.T) {
	body := []byte(`{
		"content":[
			{"type":"text","text":"looking"},
			{"type":"tool_use","id":"toolu_9","name":"get_ticket","input":{"id":"t-42"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":30,"output_tokens":15}
	}`)
	resp, err := parseAnthropicResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Text != "looking" || resp.FinishReason != FinishToolUse {
		t.Fatalf("text/finish wrong: %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "toolu_9" || resp.ToolCalls[0].Name != "get_ticket" || string(resp.ToolCalls[0].Args) != `{"id":"t-42"}` {
		t.Fatalf("tool call wrong: %+v", resp.ToolCalls)
	}
}

func TestParseAnthropicResponse_StopReasons(t *testing.T) {
	cases := map[string]FinishReason{
		"end_turn":      FinishStop,
		"stop_sequence": FinishStop,
		"tool_use":      FinishToolUse,
		"max_tokens":    FinishLength,
		"surprise":      FinishOther,
	}
	for raw, want := range cases {
		body := []byte(`{"content":[{"type":"text","text":"x"}],"stop_reason":"` + raw + `","usage":{"input_tokens":1,"output_tokens":1}}`)
		resp, err := parseAnthropicResponse(body)
		if err != nil {
			t.Fatalf("%s: parse: %v", raw, err)
		}
		if resp.FinishReason != want {
			t.Errorf("stop_reason %q -> %q, want %q", raw, resp.FinishReason, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run TestParseAnthropicResponse -v`
Expected: FAIL — `undefined: parseAnthropicResponse`.

- [ ] **Step 3: Write minimal implementation** (append to `anthropic.go`)

```go
type anthropicResp struct {
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// parseAnthropicResponse maps a 200 Messages body onto the common Response.
func parseAnthropicResponse(body []byte) (Response, error) {
	var ar anthropicResp
	if err := json.Unmarshal(body, &ar); err != nil {
		return Response{}, err
	}
	var out Response
	for _, b := range ar.Content {
		switch b.Type {
		case "text":
			out.Text += b.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Args: b.Input})
		}
	}
	out.FinishReason = anthropicFinish(ar.StopReason)
	out.Usage = Usage{InputTokens: ar.Usage.InputTokens, OutputTokens: ar.Usage.OutputTokens}
	return out, nil
}

func anthropicFinish(raw string) FinishReason {
	switch raw {
	case "end_turn", "stop_sequence":
		return FinishStop
	case "tool_use":
		return FinishToolUse
	case "max_tokens":
		return FinishLength
	default:
		return FinishOther
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run TestParseAnthropicResponse -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/anthropic.go internal/platform/ai/anthropic_test.go
git commit -m "feat(ai): anthropic response parsing + stop-reason mapping (US1b)"
```

---

## Task 3: Anthropic — `AnthropicProvider.Complete` (HTTP) + golden fixtures + error mapping

**Files:**
- Modify: `internal/platform/ai/anthropic.go`
- Create: `internal/platform/ai/testdata/anthropic_text.json`, `internal/platform/ai/testdata/anthropic_tool_use.json`
- Create: `internal/platform/ai/fixtures_test.go` (the shared `loadGolden` helper)
- Test: `internal/platform/ai/anthropic_test.go`

- [ ] **Step 1: Create the golden fixtures**

`internal/platform/ai/testdata/anthropic_text.json`:

```json
{
  "id": "msg_01TextExample",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-5",
  "content": [
    { "type": "text", "text": "Hello! How can I help with your ticket?" }
  ],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": { "input_tokens": 12, "output_tokens": 9 }
}
```

`internal/platform/ai/testdata/anthropic_tool_use.json`:

```json
{
  "id": "msg_01ToolExample",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-5",
  "content": [
    { "type": "text", "text": "Let me look that up." },
    { "type": "tool_use", "id": "toolu_01abc", "name": "get_ticket", "input": { "id": "t-42" } }
  ],
  "stop_reason": "tool_use",
  "usage": { "input_tokens": 31, "output_tokens": 16 }
}
```

- [ ] **Step 2: Create the shared fixture helper**

`internal/platform/ai/fixtures_test.go`:

```go
package ai

import (
	"os"
	"path/filepath"
	"testing"
)

// loadGolden reads a recorded provider response body from testdata/. These are
// real provider wire shapes recorded once and replayed in CI (no live calls).
// Refresh them with AI_RECORD=1 (see TestRecord* below) if a provider changes
// its format.
func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loadGolden %s: %v", name, err)
	}
	return b
}
```

> Do NOT add a `recording()` helper here — it would be `unused` (a `golangci-lint` gate failure: `.golangci.yml` enables `unused`) until Task 10 introduces the only callers. Task 10 adds `recording()` alongside those record tests. `loadGolden` IS used immediately (Task 3's round-trip test), so it's fine here.

- [ ] **Step 3: Write the failing test** (append to `anthropic_test.go`)

```go
import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	// (encoding/json + testing already imported above)
)

func TestAnthropicComplete_GoldenRoundTrip(t *testing.T) {
	cases := []struct {
		fixture  string
		wantText string
		wantTool string // tool name expected, "" for none
		wantFin  FinishReason
	}{
		{"anthropic_text.json", "Hello! How can I help with your ticket?", "", FinishStop},
		{"anthropic_tool_use.json", "Let me look that up.", "get_ticket", FinishToolUse},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			golden := loadGolden(t, tc.fixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages" {
					t.Errorf("path = %q, want /v1/messages", r.URL.Path)
				}
				if r.Header.Get("x-api-key") != "sk-test" {
					t.Errorf("missing x-api-key header")
				}
				if r.Header.Get("anthropic-version") == "" {
					t.Errorf("missing anthropic-version header")
				}
				body, _ := io.ReadAll(r.Body)
				if !json.Valid(body) {
					t.Errorf("request body is not valid JSON")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(golden)
			}))
			defer srv.Close()

			p := NewAnthropicProvider("sk-test", srv.URL, "claude-sonnet-4-5", srv.Client())
			resp, err := p.Complete(context.Background(), Request{
				Model: "claude-sonnet-4-5", MaxTokens: 256,
				Messages: []Message{{Role: RoleUser, Text: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Text != tc.wantText || resp.FinishReason != tc.wantFin {
				t.Fatalf("resp = %+v", resp)
			}
			if tc.wantTool == "" && len(resp.ToolCalls) != 0 {
				t.Fatalf("want no tool calls, got %+v", resp.ToolCalls)
			}
			if tc.wantTool != "" && (len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != tc.wantTool) {
				t.Fatalf("want tool %q, got %+v", tc.wantTool, resp.ToolCalls)
			}
		})
	}
}

func TestAnthropicComplete_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"500_server_error", http.StatusInternalServerError, `{"error":{"type":"api_error","message":"boom"}}`, ErrProviderUnavailable},
		{"429_rate_limit", http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`, ErrProviderUnavailable},
		{"401_auth", http.StatusUnauthorized, `{"error":{"type":"authentication_error","message":"bad key"}}`, ErrBadRequest},
		{"400_prompt_too_long", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000"}}`, ErrContextLength},
		{"400_missing_field", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"messages: at least one required"}}`, ErrBadRequest},
		// I-1 regression: an unrelated "too long" 4xx must NOT be misread as context-length.
		{"400_tool_name_too_long", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"tool name is too long"}}`, ErrBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := NewAnthropicProvider("sk-test", srv.URL, "claude-sonnet-4-5", srv.Client())
			_, err := p.Complete(context.Background(), Request{MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "x"}}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s: status %d -> err %v, want Is(%v)", tc.name, tc.status, err, tc.want)
			}
		})
	}
}

func TestAnthropicName(t *testing.T) {
	if NewAnthropicProvider("k", "", "m", http.DefaultClient).Name() != "anthropic" {
		t.Fatal("Name() != anthropic")
	}
}
```

- [ ] **Step 3b: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run 'TestAnthropicComplete|TestAnthropicName' -v`
Expected: FAIL — `undefined: NewAnthropicProvider`.

- [ ] **Step 4: Write minimal implementation** (append to `anthropic.go`)

```go
import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// AnthropicProvider talks to the Anthropic Messages API over net/http. The
// httpClient is injected so tests can target an httptest.Server; production
// callers pass an SSRF-guarded netsafe client (see factory.New).
type AnthropicProvider struct {
	apiKey     string
	baseURL    string // defaults to anthropicDefaultBaseURL when empty
	model      string // default model when Request.Model is empty
	httpClient *http.Client
}

// NewAnthropicProvider builds a provider. baseURL "" -> the public API; hc nil ->
// http.DefaultClient (callers SHOULD pass netsafe.NewClient or a test client).
func NewAnthropicProvider(apiKey, baseURL, model string, hc *http.Client) *AnthropicProvider {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &AnthropicProvider{apiKey: apiKey, baseURL: baseURL, model: model, httpClient: hc}
}

func (p *AnthropicProvider) Name() string { return "anthropic" }

func (p *AnthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	payload, err := json.Marshal(buildAnthropicRequest(req, model))
	if err != nil {
		return Response{}, fmt.Errorf("ai/anthropic: marshal: %w", ErrBadRequest)
	}

	u, err := url.Parse(p.baseURL)
	if err != nil {
		return Response{}, fmt.Errorf("ai/anthropic: base_url: %w", ErrBadRequest)
	}
	u = u.JoinPath("v1", "messages")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("ai/anthropic: new request: %w", ErrProviderUnavailable)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if p.apiKey != "" {
		httpReq.Header.Set("x-api-key", p.apiKey)
	}

	res, err := p.httpClient.Do(httpReq)
	if err != nil {
		// Network failure, timeout, or SSRF dial-refusal (netsafe) all land here.
		return Response{}, fmt.Errorf("ai/anthropic: transport: %w", ErrProviderUnavailable)
	}
	defer func() { _ = res.Body.Close() }() // errcheck: close error is not actionable
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20)) // cap at 8 MiB

	if res.StatusCode != http.StatusOK {
		return Response{}, anthropicHTTPError(res.StatusCode, body)
	}
	resp, err := parseAnthropicResponse(body)
	if err != nil {
		return Response{}, fmt.Errorf("ai/anthropic: decode: %w", ErrProviderUnavailable)
	}
	return resp, nil
}

// anthropicHTTPError maps a non-200 status + body onto a gateway sentinel. The
// raw upstream body is NEVER surfaced to the caller (Principle II) — only the
// sentinel; callers log server-side.
func anthropicHTTPError(status int, body []byte) error {
	if status >= 500 || status == http.StatusTooManyRequests {
		return fmt.Errorf("ai/anthropic: upstream status %d: %w", status, ErrProviderUnavailable)
	}
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if isContextLengthMessage(e.Error.Message) {
		return fmt.Errorf("ai/anthropic: %w", ErrContextLength)
	}
	return fmt.Errorf("ai/anthropic: upstream status %d: %w", status, ErrBadRequest)
}

// isContextLengthMessage heuristically detects a context/token-limit 4xx from a
// provider error message (both vendors phrase it differently). Patterns are kept
// TIGHT so unrelated 4xx ("tool name is too long") stay ErrBadRequest, not
// ErrContextLength — the agent loop treats the two very differently.
func isContextLengthMessage(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "prompt is too long") ||
		strings.Contains(m, "context length") ||
		strings.Contains(m, "context_length_exceeded") ||
		strings.Contains(m, "maximum context")
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run 'TestAnthropic|TestBuildAnthropic|TestParseAnthropic' -v`
Expected: PASS (all anthropic tests green).

- [ ] **Step 6: Commit**

```bash
git add internal/platform/ai/anthropic.go internal/platform/ai/anthropic_test.go internal/platform/ai/fixtures_test.go internal/platform/ai/testdata/anthropic_text.json internal/platform/ai/testdata/anthropic_tool_use.json
git commit -m "feat(ai): anthropic Complete over net/http + golden replay + error mapping (US1b)"
```

---

## Task 4: OpenAI-compat — wire structs + `buildOpenAIRequest` (pure)

**Files:**
- Create: `internal/platform/ai/openaicompat.go`
- Test: `internal/platform/ai/openaicompat_test.go`

- [ ] **Step 1: Write the failing test**

```go
package ai

import (
	"encoding/json"
	"testing"
)

func TestBuildOpenAIRequest(t *testing.T) {
	req := Request{
		Model:       "gpt-4o",
		System:      "you are helpful",
		MaxTokens:   512,
		Temperature: 0.5,
		Tools: []ToolDef{{
			Name: "get_ticket", Description: "fetch a ticket",
			Schema: json.RawMessage(`{"type":"object"}`),
		}},
		Messages: []Message{
			{Role: RoleUser, Text: "look up t-42"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "call_1", Name: "get_ticket", Args: json.RawMessage(`{"id":"t-42"}`)},
			}},
			{Role: RoleTool, ToolResults: []ToolResult{
				{CallID: "call_1", Content: "open"},
			}},
		},
	}

	out := buildOpenAIRequest(req, "gpt-4o")

	if out.Model != "gpt-4o" || out.MaxTokens != 512 {
		t.Fatalf("scalars wrong: %+v", out)
	}
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" || out.Tools[0].Function.Name != "get_ticket" || string(out.Tools[0].Function.Parameters) != `{"type":"object"}` {
		t.Fatalf("tools wrong: %+v", out.Tools)
	}
	// system message is prepended
	if len(out.Messages) != 4 {
		t.Fatalf("want 4 messages (system+user+assistant+tool), got %d: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "you are helpful" {
		t.Fatalf("system msg wrong: %+v", out.Messages[0])
	}
	if out.Messages[1].Role != "user" || out.Messages[1].Content != "look up t-42" {
		t.Fatalf("user msg wrong: %+v", out.Messages[1])
	}
	// assistant tool call -> arguments is a JSON STRING
	if out.Messages[2].Role != "assistant" || len(out.Messages[2].ToolCalls) != 1 {
		t.Fatalf("assistant msg wrong: %+v", out.Messages[2])
	}
	atc := out.Messages[2].ToolCalls[0]
	if atc.ID != "call_1" || atc.Type != "function" || atc.Function.Name != "get_ticket" || atc.Function.Arguments != `{"id":"t-42"}` {
		t.Fatalf("assistant tool_call wrong: %+v", atc)
	}
	// tool result -> role:tool message
	if out.Messages[3].Role != "tool" || out.Messages[3].ToolCallID != "call_1" || out.Messages[3].Content != "open" {
		t.Fatalf("tool msg wrong: %+v", out.Messages[3])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run TestBuildOpenAIRequest -v`
Expected: FAIL — `undefined: buildOpenAIRequest`.

- [ ] **Step 3: Write minimal implementation**

```go
package ai

import "encoding/json"

// OpenAI-compatible chat/completions wire format. Covers OpenAI, Ollama, and
// vLLM (all expose /chat/completions). Hand-rolled, no vendor SDK.

type openAIReq struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature"`
	Messages    []openAIMessage `json:"messages"`
	Tools       []openAITool    `json:"tools,omitempty"`
}

type openAITool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"` // system|user|assistant|tool
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"` // role:tool only
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON encoded as a STRING
	} `json:"function"`
}

// buildOpenAIRequest maps the common Request onto the chat/completions wire
// format. model overrides req.Model when req.Model is empty.
func buildOpenAIRequest(req Request, model string) openAIReq {
	out := openAIReq{Model: model, MaxTokens: req.MaxTokens, Temperature: req.Temperature}
	for _, td := range req.Tools {
		var t openAITool
		t.Type = "function"
		t.Function.Name = td.Name
		t.Function.Description = td.Description
		t.Function.Parameters = td.Schema
		out.Tools = append(out.Tools, t)
	}
	if req.System != "" {
		out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: m.Text})
		case RoleTool:
			for _, tr := range m.ToolResults {
				out.Messages = append(out.Messages, openAIMessage{
					Role: "tool", ToolCallID: tr.CallID, Content: tr.Content,
				})
			}
		case RoleAssistant:
			msg := openAIMessage{Role: "assistant", Content: m.Text}
			for _, tc := range m.ToolCalls {
				var otc openAIToolCall
				otc.ID = tc.ID
				otc.Type = "function"
				otc.Function.Name = tc.Name
				otc.Function.Arguments = string(tc.Args) // object -> JSON string
				msg.ToolCalls = append(msg.ToolCalls, otc)
			}
			out.Messages = append(out.Messages, msg)
		default: // RoleUser
			out.Messages = append(out.Messages, openAIMessage{Role: "user", Content: m.Text})
		}
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run TestBuildOpenAIRequest -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/openaicompat.go internal/platform/ai/openaicompat_test.go
git commit -m "feat(ai): openai-compat request marshaling (US1b)"
```

---

## Task 5: OpenAI-compat — `parseOpenAIResponse` (pure)

**Files:**
- Modify: `internal/platform/ai/openaicompat.go`
- Test: `internal/platform/ai/openaicompat_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestParseOpenAIResponse_Text(t *testing.T) {
	body := []byte(`{
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
	}`)
	resp, err := parseOpenAIResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Text != "hi there" || resp.FinishReason != FinishStop {
		t.Fatalf("text/finish wrong: %+v", resp)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
}

func TestParseOpenAIResponse_ToolCalls(t *testing.T) {
	body := []byte(`{
		"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_9","type":"function","function":{"name":"get_ticket","arguments":"{\"id\":\"t-42\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":28,"completion_tokens":14,"total_tokens":42}
	}`)
	resp, err := parseOpenAIResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.FinishReason != FinishToolUse {
		t.Fatalf("finish wrong: %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_9" || resp.ToolCalls[0].Name != "get_ticket" || string(resp.ToolCalls[0].Args) != `{"id":"t-42"}` {
		t.Fatalf("tool call wrong: %+v", resp.ToolCalls)
	}
}

func TestParseOpenAIResponse_FinishReasons(t *testing.T) {
	cases := map[string]FinishReason{
		"stop":       FinishStop,
		"tool_calls": FinishToolUse,
		"length":     FinishLength,
		"weird":      FinishOther,
	}
	for raw, want := range cases {
		body := []byte(`{"choices":[{"message":{"content":"x"},"finish_reason":"` + raw + `"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		resp, err := parseOpenAIResponse(body)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if resp.FinishReason != want {
			t.Errorf("finish_reason %q -> %q, want %q", raw, resp.FinishReason, want)
		}
	}
}

func TestParseOpenAIResponse_NoChoices(t *testing.T) {
	_, err := parseOpenAIResponse([]byte(`{"choices":[],"usage":{}}`))
	if err == nil {
		t.Fatal("want error on empty choices")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run TestParseOpenAIResponse -v`
Expected: FAIL — `undefined: parseOpenAIResponse`.

- [ ] **Step 3: Write minimal implementation** (append to `openaicompat.go`; the import block now needs `encoding/json` + `errors` — `fmt` and the HTTP imports arrive in Task 6)

```go
import "errors" // add to the existing import block alongside encoding/json

type openAIResp struct {
	Choices []struct {
		Message struct {
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// parseOpenAIResponse maps a 200 chat/completions body onto the common Response.
func parseOpenAIResponse(body []byte) (Response, error) {
	var or openAIResp
	if err := json.Unmarshal(body, &or); err != nil {
		return Response{}, err
	}
	if len(or.Choices) == 0 {
		return Response{}, errors.New("ai/openai: no choices in response")
	}
	ch := or.Choices[0]
	out := Response{
		Text:         ch.Message.Content,
		FinishReason: openAIFinish(ch.FinishReason),
		Usage:        Usage{InputTokens: or.Usage.PromptTokens, OutputTokens: or.Usage.CompletionTokens},
	}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Args: json.RawMessage(tc.Function.Arguments),
		})
	}
	return out, nil
}

func openAIFinish(raw string) FinishReason {
	switch raw {
	case "stop":
		return FinishStop
	case "tool_calls":
		return FinishToolUse
	case "length":
		return FinishLength
	default:
		return FinishOther
	}
}
```

> This step uses only `encoding/json` + `errors`. Task 6 adds `bytes`, `context`, `fmt`, `io`, `net/http`, `net/url`, `strings`. Verify the intermediate state compiles with `go build ./internal/platform/ai/` (no unused imports).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run TestParseOpenAIResponse -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/openaicompat.go internal/platform/ai/openaicompat_test.go
git commit -m "feat(ai): openai-compat response parsing + finish-reason mapping (US1b)"
```

---

## Task 6: OpenAI-compat — `OpenAICompatProvider.Complete` (HTTP) + golden fixtures + error mapping

**Files:**
- Modify: `internal/platform/ai/openaicompat.go`
- Create: `internal/platform/ai/testdata/openai_text.json`, `internal/platform/ai/testdata/openai_tool_calls.json`
- Test: `internal/platform/ai/openaicompat_test.go`

- [ ] **Step 1: Create the golden fixtures**

`internal/platform/ai/testdata/openai_text.json`:

```json
{
  "id": "chatcmpl-textexample",
  "object": "chat.completion",
  "model": "gpt-4o",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "Hello! How can I help with your ticket?" },
      "finish_reason": "stop"
    }
  ],
  "usage": { "prompt_tokens": 11, "completion_tokens": 9, "total_tokens": 20 }
}
```

`internal/platform/ai/testdata/openai_tool_calls.json`:

```json
{
  "id": "chatcmpl-toolexample",
  "object": "chat.completion",
  "model": "gpt-4o",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": null,
        "tool_calls": [
          {
            "id": "call_01abc",
            "type": "function",
            "function": { "name": "get_ticket", "arguments": "{\"id\":\"t-42\"}" }
          }
        ]
      },
      "finish_reason": "tool_calls"
    }
  ],
  "usage": { "prompt_tokens": 28, "completion_tokens": 14, "total_tokens": 42 }
}
```

- [ ] **Step 2: Write the failing test** (append to `openaicompat_test.go`)

```go
import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
)

func TestOpenAIComplete_GoldenRoundTrip(t *testing.T) {
	cases := []struct {
		fixture  string
		wantText string
		wantTool string
		wantFin  FinishReason
	}{
		{"openai_text.json", "Hello! How can I help with your ticket?", "", FinishStop},
		{"openai_tool_calls.json", "", "get_ticket", FinishToolUse},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			golden := loadGolden(t, tc.fixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer sk-test" {
					t.Errorf("Authorization = %q, want Bearer sk-test", r.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(golden)
			}))
			defer srv.Close()

			p := NewOpenAICompatProvider("sk-test", srv.URL+"/v1", "gpt-4o", srv.Client())
			resp, err := p.Complete(context.Background(), Request{
				Model: "gpt-4o", MaxTokens: 256,
				Messages: []Message{{Role: RoleUser, Text: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Text != tc.wantText || resp.FinishReason != tc.wantFin {
				t.Fatalf("resp = %+v", resp)
			}
			if tc.wantTool == "" && len(resp.ToolCalls) != 0 {
				t.Fatalf("want no tool calls, got %+v", resp.ToolCalls)
			}
			if tc.wantTool != "" && (len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != tc.wantTool) {
				t.Fatalf("want tool %q, got %+v", tc.wantTool, resp.ToolCalls)
			}
		})
	}
}

func TestOpenAIComplete_NoKeyOmitsAuth(t *testing.T) {
	// Ollama / vLLM: empty key -> no Authorization header.
	golden := loadGolden(t, "openai_text.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no Authorization header for keyless provider, got %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write(golden)
	}))
	defer srv.Close()
	p := NewOpenAICompatProvider("", srv.URL+"/v1", "llama3", srv.Client())
	if _, err := p.Complete(context.Background(), Request{MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "x"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestOpenAIComplete_ErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusInternalServerError, `{"error":{"message":"boom","type":"server_error"}}`, ErrProviderUnavailable},
		{http.StatusTooManyRequests, `{"error":{"message":"rate limited","type":"rate_limit"}}`, ErrProviderUnavailable},
		{http.StatusUnauthorized, `{"error":{"message":"bad key","type":"invalid_request_error"}}`, ErrBadRequest},
		{http.StatusBadRequest, `{"error":{"message":"too many tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`, ErrContextLength},
		{http.StatusBadRequest, `{"error":{"message":"missing field","type":"invalid_request_error"}}`, ErrBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.want.Error(), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := NewOpenAICompatProvider("sk-test", srv.URL+"/v1", "gpt-4o", srv.Client())
			_, err := p.Complete(context.Background(), Request{MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "x"}}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d body %q -> err %v, want Is(%v)", tc.status, tc.body, err, tc.want)
			}
		})
	}
}

func TestOpenAIName(t *testing.T) {
	if NewOpenAICompatProvider("k", "http://x/v1", "m", http.DefaultClient).Name() != "openai-compat" {
		t.Fatal("Name() != openai-compat")
	}
}
```

- [ ] **Step 2b: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run 'TestOpenAIComplete|TestOpenAIName' -v`
Expected: FAIL — `undefined: NewOpenAICompatProvider`.

- [ ] **Step 3: Write minimal implementation** (append to `openaicompat.go`; ensure imports include `bytes`, `context`, `io`, `net/http`, `net/url`, `strings`, `fmt`)

```go
// OpenAICompatProvider talks to any OpenAI-compatible /chat/completions endpoint
// (OpenAI, Ollama, vLLM). baseURL is USER-SUPPLIED for self-host, so production
// callers MUST inject an SSRF-guarded netsafe client (see factory.New).
type OpenAICompatProvider struct {
	apiKey     string
	baseURL    string // includes the version segment, e.g. https://api.openai.com/v1
	model      string
	httpClient *http.Client
}

// NewOpenAICompatProvider builds a provider. hc nil -> http.DefaultClient
// (callers SHOULD pass netsafe.NewClient or a test client). An empty apiKey
// omits the Authorization header (Ollama / vLLM).
func NewOpenAICompatProvider(apiKey, baseURL, model string, hc *http.Client) *OpenAICompatProvider {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &OpenAICompatProvider{apiKey: apiKey, baseURL: baseURL, model: model, httpClient: hc}
}

func (p *OpenAICompatProvider) Name() string { return "openai-compat" }

func (p *OpenAICompatProvider) Complete(ctx context.Context, req Request) (Response, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	payload, err := json.Marshal(buildOpenAIRequest(req, model))
	if err != nil {
		return Response{}, fmt.Errorf("ai/openai: marshal: %w", ErrBadRequest)
	}

	u, err := url.Parse(p.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return Response{}, fmt.Errorf("ai/openai: base_url %q: %w", p.baseURL, ErrBadRequest)
	}
	u = u.JoinPath("chat", "completions")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("ai/openai: new request: %w", ErrProviderUnavailable)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	res, err := p.httpClient.Do(httpReq)
	if err != nil {
		// Network failure, timeout, or SSRF dial-refusal (netsafe) all land here.
		return Response{}, fmt.Errorf("ai/openai: transport: %w", ErrProviderUnavailable)
	}
	defer func() { _ = res.Body.Close() }() // errcheck: close error is not actionable
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20)) // cap at 8 MiB

	if res.StatusCode != http.StatusOK {
		return Response{}, openAIHTTPError(res.StatusCode, body)
	}
	resp, err := parseOpenAIResponse(body)
	if err != nil {
		return Response{}, fmt.Errorf("ai/openai: decode: %w", ErrProviderUnavailable)
	}
	return resp, nil
}

// openAIHTTPError maps a non-200 status + body onto a gateway sentinel. The raw
// upstream body is NEVER surfaced (Principle II).
func openAIHTTPError(status int, body []byte) error {
	if status >= 500 || status == http.StatusTooManyRequests {
		return fmt.Errorf("ai/openai: upstream status %d: %w", status, ErrProviderUnavailable)
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error.Code == "context_length_exceeded" || isContextLengthMessage(e.Error.Message) {
		return fmt.Errorf("ai/openai: %w", ErrContextLength)
	}
	return fmt.Errorf("ai/openai: upstream status %d: %w", status, ErrBadRequest)
}
```

> Remove the temporary `var _ = fmt.Sprintf` line from Task 5 now that `fmt` is used for real. Run `go build ./internal/platform/ai/` to confirm no unused-import / unused-var error.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run 'TestOpenAI|TestBuildOpenAI|TestParseOpenAI' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/openaicompat.go internal/platform/ai/openaicompat_test.go internal/platform/ai/testdata/openai_text.json internal/platform/ai/testdata/openai_tool_calls.json
git commit -m "feat(ai): openai-compat Complete over net/http + golden replay + error mapping (US1b)"
```

---

## Task 7: Provider factory — `New(cred)` with `netsafe` wiring + fail-closed dispatch

**Files:**
- Create: `internal/platform/ai/factory.go`
- Test: `internal/platform/ai/factory_test.go`

**Design:** the factory takes a small local `Credential` struct (NOT `agents.ResolvedCredential`, to avoid an import cycle — `internal/agents` imports `internal/platform/ai`). The agents layer maps its `ResolvedCredential` onto this struct at the call site in US3. The factory always builds the SSRF-guarded `netsafe` client; unknown providers fail closed with `ErrBadRequest`.

- [ ] **Step 1: Write the failing test**

```go
package ai

import (
	"errors"
	"testing"
)

func TestFactoryDispatch(t *testing.T) {
	cases := []struct {
		provider string
		wantName string
	}{
		{"anthropic", "anthropic"},
		{"openai", "openai-compat"},
		{"ollama", "openai-compat"},
		{"vllm", "openai-compat"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			base := ""
			if tc.wantName == "openai-compat" {
				base = "https://api.example.com/v1" // openai-compat requires a base_url
			}
			p, err := New(Credential{Provider: tc.provider, APIKey: "k", BaseURL: base, Model: "m"})
			if err != nil {
				t.Fatalf("New(%s): %v", tc.provider, err)
			}
			if p.Name() != tc.wantName {
				t.Fatalf("provider %q -> Name %q, want %q", tc.provider, p.Name(), tc.wantName)
			}
		})
	}
}

func TestFactoryUnknownProvider(t *testing.T) {
	_, err := New(Credential{Provider: "definitely-not-real", APIKey: "k"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unknown provider err = %v, want Is(ErrBadRequest)", err)
	}
}

func TestFactoryOpenAICompatRequiresBaseURL(t *testing.T) {
	_, err := New(Credential{Provider: "openai", APIKey: "k", BaseURL: ""})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("missing base_url err = %v, want Is(ErrBadRequest)", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run TestFactory -v`
Expected: FAIL — `undefined: New`, `undefined: Credential`.

- [ ] **Step 3: Write minimal implementation**

```go
package ai

import (
	"fmt"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// defaultRequestTimeout bounds a single provider round-trip. US3 will make this
// configurable when it constructs the gateway at startup; US1b uses a constant.
const defaultRequestTimeout = 60 * time.Second

// Credential is the minimal resolved credential the factory needs to build a
// Provider. It deliberately mirrors agents.ResolvedCredential by VALUE (not by
// import) so internal/platform/ai stays free of any internal/agents dependency
// (agents imports ai, not the reverse).
type Credential struct {
	Provider string // anthropic | openai | ollama | vllm
	APIKey   string // plaintext, in-memory only
	BaseURL  string // required for openai-compat/self-host; ignored for anthropic default
	Model    string // default model
}

// New builds the live Provider for a resolved credential. The returned provider
// uses an SSRF-guarded netsafe HTTP client (a user-supplied openai-compat
// base_url cannot reach RFC1918/metadata IPs). Unknown providers fail closed.
//
// Provider-name -> transport mapping (keep in sync with agents.knownProviders /
// the ai_provider PG enum — see manyforge-uc2):
//
//	anthropic                 -> AnthropicProvider
//	openai | ollama | vllm    -> OpenAICompatProvider
func New(cred Credential) (Provider, error) {
	hc := netsafe.NewClient(defaultRequestTimeout)
	switch cred.Provider {
	case "anthropic":
		return NewAnthropicProvider(cred.APIKey, cred.BaseURL, cred.Model, hc), nil
	case "openai", "ollama", "vllm":
		if cred.BaseURL == "" {
			return nil, fmt.Errorf("ai: openai-compat provider %q requires a base_url: %w", cred.Provider, ErrBadRequest)
		}
		return NewOpenAICompatProvider(cred.APIKey, cred.BaseURL, cred.Model, hc), nil
	default:
		return nil, fmt.Errorf("ai: unknown provider %q: %w", cred.Provider, ErrBadRequest)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run TestFactory -v`
Expected: PASS.

> The dispatch tests build a provider but never call `Complete` (no network). `netsafe.NewClient` construction is cheap and makes no DNS/dial calls until used.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/factory.go internal/platform/ai/factory_test.go
git commit -m "feat(ai): provider factory with netsafe SSRF-guarded client + fail-closed dispatch (US1b)"
```

---

## Task 8: Registry seed — `RegisterDefaults`

**Files:**
- Create: `internal/platform/ai/seed.go`
- Test: `internal/platform/ai/seed_test.go`

**Why now:** the design lists "model `Registry` — Seeded for known models" under US1; US1a built the empty registry but left it unseeded. Seeding here completes US1 and gives US3's cost capture real pricing to look up. Pricing is a documented snapshot — verify against the provider pricing pages when it matters; self-hosters `Register` their own local models at runtime.

- [ ] **Step 1: Write the failing test**

```go
package ai

import "testing"

func TestRegisterDefaults(t *testing.T) {
	r := NewRegistry()
	RegisterDefaults(r)

	for _, id := range []string{"claude-sonnet-4-5", "gpt-4o", "gpt-4o-mini"} {
		m, ok := r.Lookup(id)
		if !ok {
			t.Fatalf("model %q not seeded", id)
		}
		if m.InputCentsPerMTok <= 0 || m.OutputCentsPerMTok <= 0 {
			t.Errorf("model %q has non-positive pricing: %+v", id, m)
		}
		if !m.SupportsTools {
			t.Errorf("model %q should support tools", id)
		}
	}

	// Cost math sanity: 1M input + 1M output tokens == (in + out) cents-per-MTok.
	m, _ := r.Lookup("gpt-4o")
	got := m.CostCents(Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	want := m.InputCentsPerMTok + m.OutputCentsPerMTok
	if got != want {
		t.Errorf("CostCents(1M,1M) = %d, want %d", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run TestRegisterDefaults -v`
Expected: FAIL — `undefined: RegisterDefaults`.

- [ ] **Step 3: Write minimal implementation**

```go
package ai

// RegisterDefaults seeds the registry with known hosted models. Pricing is in
// integer cents per MILLION tokens (snapshot — verify against the provider
// pricing page before relying on it for billing; this is a BYO-key guardrail,
// not an invoice). Self-hosters Register local models (e.g. an ollama tag) at
// runtime; those have zero cost.
func RegisterDefaults(r *Registry) {
	for _, m := range []Model{
		{ID: "claude-sonnet-4-5", Provider: "anthropic", ContextWindow: 200_000, InputCentsPerMTok: 300, OutputCentsPerMTok: 1500, SupportsTools: true},
		{ID: "claude-opus-4-1", Provider: "anthropic", ContextWindow: 200_000, InputCentsPerMTok: 1500, OutputCentsPerMTok: 7500, SupportsTools: true},
		{ID: "claude-haiku-4-5", Provider: "anthropic", ContextWindow: 200_000, InputCentsPerMTok: 100, OutputCentsPerMTok: 500, SupportsTools: true},
		{ID: "gpt-4o", Provider: "openai", ContextWindow: 128_000, InputCentsPerMTok: 250, OutputCentsPerMTok: 1000, SupportsTools: true},
		{ID: "gpt-4o-mini", Provider: "openai", ContextWindow: 128_000, InputCentsPerMTok: 15, OutputCentsPerMTok: 60, SupportsTools: true},
	} {
		r.Register(m)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run TestRegisterDefaults -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/seed.go internal/platform/ai/seed_test.go
git commit -m "feat(ai): seed registry with known models + pricing (US1b, completes US1 registry)"
```

---

## Task 9: SSRF security-regression pin (merge gate)

**Files:**
- Create: `internal/security_regression/ai_provider_ssrf_pin_test.go`

**Why:** design §3.5 + US8 require a pin that a user-supplied `base_url` cannot reach RFC1918/metadata IPs. This is the FIRST `netsafe` consumer, so the pin proves both (a) the factory wires `netsafe` at the source level and (b) a private-IP `base_url` is actually refused at dial time. The file is **untagged** (matching the package's `*_pin_test.go` convention for source/behavioral pins, e.g. `agent_containment_pin_test.go`, `loop_guard_pin_test.go`) so it runs in `make test` (fast) AND `make sec-test`. It reuses the package's existing untagged `mustRead` helper (`escalation_pin_test.go`) — do NOT redefine it (duplicate symbol = compile error).

- [ ] **Step 1: Write the test**

```go
// Finding: US1b / Spec 003 §3.5 — a user-supplied openai-compat base_url MUST
// route through the SSRF-guarded netsafe client and cannot reach RFC1918 /
// loopback / metadata IPs. See manyforge-ma9.
package security_regression

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/ai"
)

// Behavioral pin: an openai-compat provider built by the factory with a private
// base_url fails (dial refused by netsafe) — it does NOT reach the host.
func TestAIProviderFactory_RefusesPrivateBaseURL(t *testing.T) {
	privates := []string{
		"http://10.0.0.1/v1",
		"http://127.0.0.1:9999/v1",
		"http://169.254.169.254/v1",       // cloud metadata
		"http://192.168.1.1/v1",
	}
	for _, base := range privates {
		t.Run(base, func(t *testing.T) {
			p, err := ai.New(ai.Credential{Provider: "openai", APIKey: "k", BaseURL: base, Model: "gpt-4o"})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = p.Complete(context.Background(), ai.Request{
				MaxTokens: 16, Messages: []ai.Message{{Role: ai.RoleUser, Text: "x"}},
			})
			if !errors.Is(err, ai.ErrProviderUnavailable) {
				t.Fatalf("private base_url %q -> err %v, want Is(ErrProviderUnavailable) (dial refused)", base, err)
			}
		})
	}
}

// Source-level pin: the factory constructs its HTTP client via netsafe. A
// refactor that drops netsafe.NewClient from factory.go fails here loudly, even
// if behavior were masked.
func TestAIFactory_UsesNetsafeSource(t *testing.T) {
	const path = "../platform/ai/factory.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "netsafe" && sel.Sel.Name == "NewClient" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatalf("factory.go no longer calls netsafe.NewClient — SSRF guard dropped")
	}
	// Belt-and-suspenders: the prod factory must NOT fall back to a bare client.
	// mustRead is the package's existing untagged helper (escalation_pin_test.go).
	src := mustRead(t, path)
	if strings.Contains(src, "http.DefaultClient") {
		t.Fatalf("factory.go references http.DefaultClient — prod path must use netsafe only")
	}
}
```

> `mustRead(t, path) string` already exists in the package (untagged, `escalation_pin_test.go`). Do NOT redefine it and do NOT add an `os` import — the helper owns the read.

- [ ] **Step 2: Run the test**

Run: `go test ./internal/security_regression/ -run 'TestAIProviderFactory|TestAIFactory' -v`
Expected: PASS. (The behavioral test makes a dial attempt to a private/IP-literal host that netsafe refuses *before* connecting — no external network reached, no DNS for IP literals.)

> If the behavioral test is slow or flaky in a sandbox that itself blocks egress, it should STILL pass (a refused/blocked dial is the success condition). If it hangs, the netsafe dialer's 10s inner timeout bounds it.

- [ ] **Step 3: Commit**

```bash
git add internal/security_regression/ai_provider_ssrf_pin_test.go
git commit -m "test(sec): pin openai-compat base_url SSRF guard via netsafe (US1b, US8 gate)"
```

---

## Task 10: Optional live-record helpers + full-gate verification

**Files:**
- Modify: `internal/platform/ai/fixtures_test.go`

- [ ] **Step 1: Add env-gated record tests** (append to `fixtures_test.go`)

These refresh the golden fixtures from the LIVE API for maintainers who set `AI_RECORD=1` and supply a key. They are skipped by default, keeping CI hermetic.

```go
import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// recording reports whether AI_RECORD mode is on (maintainer refresh of golden
// fixtures against the live API). Off in CI. Defined here — alongside its only
// callers — to keep the `unused` linter happy in the intermediate commits.
func recording() bool { return os.Getenv("AI_RECORD") != "" }

// TestRecordAnthropicFixture refreshes testdata/anthropic_text.json from the
// live Anthropic API. Run: AI_RECORD=1 ANTHROPIC_API_KEY=sk-... go test \
//   ./internal/platform/ai/ -run TestRecordAnthropicFixture -v
func TestRecordAnthropicFixture(t *testing.T) {
	if !recording() {
		t.Skip("set AI_RECORD=1 to refresh golden fixtures from the live API")
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	p := NewAnthropicProvider(key, "", "claude-sonnet-4-5", nil)
	resp, err := p.Complete(context.Background(), Request{
		Model: "claude-sonnet-4-5", MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Text: "Say hello in one short sentence."}},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	t.Logf("recorded anthropic response: text=%q finish=%s usage=%+v", resp.Text, resp.FinishReason, resp.Usage)
	// NOTE: this logs the mapped Response for inspection. To regenerate the raw
	// fixture, temporarily capture the raw body in Complete or use a proxy; the
	// committed fixtures are hand-authored to the documented wire shape and
	// rarely need regeneration. Path for reference:
	_ = filepath.Join("testdata", "anthropic_text.json")
}

// TestRecordOpenAIFixture mirrors the above for an OpenAI-compatible endpoint.
// Run: AI_RECORD=1 OPENAI_API_KEY=sk-... OPENAI_BASE_URL=https://api.openai.com/v1 \
//   go test ./internal/platform/ai/ -run TestRecordOpenAIFixture -v
func TestRecordOpenAIFixture(t *testing.T) {
	if !recording() {
		t.Skip("set AI_RECORD=1 to refresh golden fixtures from the live API")
	}
	key := os.Getenv("OPENAI_API_KEY")
	base := os.Getenv("OPENAI_BASE_URL")
	if key == "" || base == "" {
		t.Skip("OPENAI_API_KEY / OPENAI_BASE_URL not set")
	}
	p := NewOpenAICompatProvider(key, base, "gpt-4o", nil)
	resp, err := p.Complete(context.Background(), Request{
		Model: "gpt-4o", MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Text: "Say hello in one short sentence."}},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	t.Logf("recorded openai response: text=%q finish=%s usage=%+v", resp.Text, resp.FinishReason, resp.Usage)
}
```

> The record tests deliberately log the *mapped* `Response` rather than auto-overwriting the raw JSON — round-tripping a live body into a committed fixture risks leaking account-specific IDs and is rarely needed (the hand-authored fixtures already match the documented wire shape). They exist to *verify* the transport against a real endpoint on demand. If a provider changes its wire format, capture the raw body (e.g. via an `httputil.DumpResponse` tweak) and update the `testdata/*.json` by hand.

- [ ] **Step 2: Verify the record tests skip cleanly with no env**

Run: `go test ./internal/platform/ai/ -run TestRecord -v`
Expected: PASS with both subtests SKIPPED.

- [ ] **Step 3: Run the full AI package suite**

Run: `go test ./internal/platform/ai/ -v`
Expected: PASS — all US1a + US1b tests (schema, provider, registry, mock, anthropic, openaicompat, factory, seed, fixtures).

- [ ] **Step 4: Run the full project gate**

```bash
make test && make contract-test && make lint
make int-test   # testcontainers Postgres, Docker required, -p 1, ~6 min
make sec-test   # includes the new untagged SSRF pin
```
Expected: ALL GREEN. `make lint` = `go vet ./...` (0 issues) + golangci-lint if installed.

> If `make int-test` is unavailable in this environment (no Docker), run at minimum `make test && make sec-test && make contract-test && make lint && go build ./...` and note int-test was deferred — but per CLAUDE.md "no pre-existing failures," it MUST be run before the work is called done.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/fixtures_test.go
git commit -m "test(ai): env-gated live-record helpers for golden fixtures (US1b)"
```

- [ ] **Step 6: Close the bd issue**

```bash
bd close manyforge-ma9
```

---

## Self-review (run after the plan is executed)

**Spec coverage (design §5 US1 + US8 portions assigned to US1b):**
- ✅ `anthropic` transport (Messages API, `tool_use`) — Tasks 1–3.
- ✅ `openaicompat` transport (chat/completions, `tool_calls`; OpenAI/Ollama/vLLM via `base_url`) — Tasks 4–6.
- ✅ "a fixture round-trips a completion + tool-call through each transport" (US1 independent test) — golden round-trip tests in Tasks 3 & 6.
- ✅ model `Registry` "seeded for known models" — Task 8.
- ✅ self-host `base_url` SSRF-guard pin (US8) — Task 9.
- ✅ "no live API calls in tests" (design §4) — golden replay; live calls are env-gated + skipped (Task 10).
- ✅ HTTP failures mapped to `ai.Err*` sentinels; raw upstream body never surfaced (design §3.5, Principle II) — error-mapping tests in Tasks 3 & 6.
- ✅ outbound provider HTTP through `netsafe` (design §3.5) — factory (Task 7) + pin (Task 9).
- ⏭️ NOT in US1b (correctly deferred): run loop, gate, queue, agent CRUD, `cmd/manyforge`/config env wiring, budget cap (US2–US5); end-to-end multi-provider config exercise (rest of US8); per-provider live recording in CI.

**Type consistency check:**
- `NewAnthropicProvider(apiKey, baseURL, model string, hc *http.Client) *AnthropicProvider` — used identically in Tasks 3 & 10. ✔
- `NewOpenAICompatProvider(apiKey, baseURL, model string, hc *http.Client) *OpenAICompatProvider` — Tasks 6 & 10. ✔
- `New(Credential) (Provider, error)` + `Credential{Provider,APIKey,BaseURL,Model}` — Tasks 7 & 9. ✔
- `buildAnthropicRequest(req, model)` / `buildOpenAIRequest(req, model)` signatures match their tests. ✔
- `Name()` returns: anthropic → `"anthropic"`, openai-compat → `"openai-compat"`, factory dispatch test asserts these exact strings. ✔
- `loadGolden(t, name)` / `recording()` defined once in `fixtures_test.go`, used in Tasks 3, 6, 10. ✔
- `isContextLengthMessage` defined in `anthropic.go` (Task 3), reused by `openAIHTTPError` (Task 6) — same package, no redefinition. ✔

**Import-cycle check:** `factory.go` imports `netsafe` only; it defines its OWN `Credential` rather than importing `internal/agents`, so `internal/platform/ai` keeps zero dependency on `internal/agents` (verified: today neither package imports the other). ✔

**Placeholder scan:** no bare TBDs. Task 5's import block is explicitly scoped (`encoding/json` + `errors`, rest in Task 6) and Task 9 reuses the verified-existing `mustRead` — both have complete, unambiguous instructions. ✔

---

## Execution handoff

**Plan complete and saved to `docs/superpowers/plans/2026-06-02-us1b-live-provider-transports.md`.**

Per the resume decision (Plan + execute), execution proceeds via **superpowers:subagent-driven-development** — a fresh subagent per task with two-stage review between tasks. The gate (`make test && make contract-test && make lint`, plus `make int-test` + `make sec-test`) must be GREEN before `bd close manyforge-ma9` and the session-end push.
