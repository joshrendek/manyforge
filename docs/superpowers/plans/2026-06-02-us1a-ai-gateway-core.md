# US1a — AI Gateway Core & BYO Credentials — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the provider-agnostic LLM gateway primitives (`internal/platform/ai/`) and the per-business BYO-credential store (`internal/agents/credential.go` + `ai_provider_credential` table) — everything US2–US5 import, fully testable with **no live API calls**.

**Architecture:** A pure-Go gateway: one internal `Request`/`Response`/`Message`/tool schema, a `Provider` interface, a model `Registry` (metadata + cost), and a programmable `MockProvider` (the deterministic test backbone). Credentials are a normal RLS table whose API key is envelope-encrypted via the existing `crypto.Sealer`; a `CredentialService` does CRUD + resolve-and-unseal. Live HTTP transports (anthropic / openai-compat) are deferred to plan US1b — they implement the same `Provider` interface.

**Tech Stack:** Go, `net/http` (stdlib, no vendor SDK), `pgx/v5` + sqlc (`internal/platform/db/dbgen`), `crypto.Sealer` (AES-256-GCM), `testdb` (testcontainers Postgres), `errs` sentinels.

**Scope boundary:** This plan does NOT add live provider HTTP, the run loop, the gate, the queue, or agent definitions — those are US1b / US2 / US3 / US4. It DOES add the `Provider` interface and `MockProvider` so those later plans have something to build and test against.

---

## File structure

| File | Responsibility |
|---|---|
| `internal/platform/ai/schema.go` | Core value types: `Role`, `Message`, `ToolDef`, `ToolCall`, `ToolResult`, `Request`, `Response`, `Usage`, `FinishReason`. Pure data, no behavior. |
| `internal/platform/ai/schema_test.go` | Pins constructor helpers / JSON round-trip of the schema. |
| `internal/platform/ai/provider.go` | The `Provider` interface + gateway error sentinels (`ErrProviderUnavailable`, `ErrBadRequest`, `ErrContextLength`). |
| `internal/platform/ai/registry.go` | `Model` metadata + `Registry` (register/lookup, `CostCents(usage)`); seeded with known models. |
| `internal/platform/ai/registry_test.go` | Lookup + cost-math pins. |
| `internal/platform/ai/mock.go` | `MockProvider`: scripted responses + records received requests. The deterministic backbone for every downstream test. |
| `internal/platform/ai/mock_test.go` | Pins scripting + request capture + exhaustion error. |
| `migrations/0025_ai_provider_credential.up.sql` / `.down.sql` | `ai_provider` enum + `ai_provider_credential` table (RLS, business-scoped). |
| `db/query/ai.sql` | sqlc queries: Insert/Get/List/Delete credential. |
| `internal/platform/db/dbgen/ai.sql.go` | **Generated** by `make generate` — do not hand-edit. |
| `internal/agents/credential.go` | `CredentialService`: CRUD + seal-on-write + `Resolve` (unseal → `ResolvedCredential{Provider, APIKey, BaseURL, Model}`). |
| `internal/agents/credential_test.go` | Unit: validation + Resolve unseal logic against a real in-memory `Sealer` (no DB). |
| `internal/agents/credential_integration_test.go` | `//go:build integration`: CRUD + seal round-trip + ownership/tenant-isolation via `testdb`. |

---

## Task 1: Gateway schema types

**Files:**
- Create: `internal/platform/ai/schema.go`
- Test: `internal/platform/ai/schema_test.go`

- [ ] **Step 1: Write the failing test**

```go
package ai

import (
	"encoding/json"
	"testing"
)

func TestMessageJSONRoundTrip(t *testing.T) {
	in := Message{Role: RoleAssistant, Text: "hi", ToolCalls: []ToolCall{
		{ID: "c1", Name: "get_ticket", Args: json.RawMessage(`{"id":"t1"}`)},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Message
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Role != RoleAssistant || out.Text != "hi" || len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "get_ticket" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestUsageTotal(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 7}
	if u.Total() != 17 {
		t.Errorf("Total() = %d, want 17", u.Total())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run 'TestMessage|TestUsage' -v`
Expected: FAIL — `undefined: Message` (package doesn't exist yet).

- [ ] **Step 3: Write minimal implementation**

```go
// Package ai is the provider-agnostic LLM gateway: one internal message/tool
// schema and a Provider interface that hand-rolled transports (anthropic,
// openai-compat) and a MockProvider implement. It holds NO database state and
// NO domain logic — callers pass a resolved credential + a Request and get a
// Response. (Spec 003 SL-A.)
package ai

import "encoding/json"

// Role is a chat message author.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // a tool result fed back to the model
)

// FinishReason is why the model stopped.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"       // natural end / final text
	FinishToolUse   FinishReason = "tool_use"   // model wants tool calls run
	FinishLength    FinishReason = "length"     // hit max tokens
	FinishOther     FinishReason = "other"
)

// ToolDef advertises a tool to the model. Schema is a JSON Schema object.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// ToolCall is the model asking to run a tool. Args is the raw JSON arguments —
// it is UNTRUSTED and must be validated against the tool's schema before use.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ToolResult is the outcome of a tool call, fed back to the model.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// Message is one turn. A RoleTool message carries ToolResults; an assistant
// message may carry Text and/or ToolCalls.
type Message struct {
	Role        Role         `json:"role"`
	Text        string       `json:"text,omitempty"`
	ToolCalls   []ToolCall   `json:"tool_calls,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
}

// Request is a single completion call.
type Request struct {
	Model       string    `json:"model"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	Tools       []ToolDef `json:"tools,omitempty"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
}

// Usage is token accounting for one call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Total is input+output tokens.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// Response is one completion result.
type Response struct {
	Text         string       `json:"text"`
	ToolCalls    []ToolCall   `json:"tool_calls,omitempty"`
	FinishReason FinishReason `json:"finish_reason"`
	Usage        Usage        `json:"usage"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run 'TestMessage|TestUsage' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/schema.go internal/platform/ai/schema_test.go
git commit -m "feat(ai): gateway message/tool schema (US1a)"
```

---

## Task 2: Provider interface + error sentinels

**Files:**
- Create: `internal/platform/ai/provider.go`

- [ ] **Step 1: Write the failing test** (in `internal/platform/ai/provider_test.go`)

```go
package ai

import (
	"context"
	"errors"
	"testing"
)

// staticProvider is a throwaway to prove the interface is satisfiable.
type staticProvider struct{ resp Response }

func (s staticProvider) Complete(_ context.Context, _ Request) (Response, error) { return s.resp, nil }
func (s staticProvider) Name() string                                            { return "static" }

func TestProviderInterfaceSatisfied(t *testing.T) {
	var p Provider = staticProvider{resp: Response{Text: "ok"}}
	got, err := p.Complete(context.Background(), Request{})
	if err != nil || got.Text != "ok" {
		t.Fatalf("Complete = (%+v, %v)", got, err)
	}
	if !errors.Is(ErrBadRequest, ErrBadRequest) {
		t.Fatal("sentinel identity broken")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run TestProviderInterface -v`
Expected: FAIL — `undefined: Provider`.

- [ ] **Step 3: Write minimal implementation**

```go
package ai

import (
	"context"
	"errors"
)

// Provider is one LLM backend. Implementations: anthropic + openai-compat
// transports (US1b) and MockProvider (this plan). Complete is a SINGLE,
// non-streaming round-trip; the agentic loop lives above this in internal/agents.
type Provider interface {
	// Complete sends one Request and returns the model's Response. It must map
	// transport/HTTP failures to the sentinels below so callers branch uniformly.
	Complete(ctx context.Context, req Request) (Response, error)
	// Name identifies the provider for logs/metrics (e.g. "anthropic").
	Name() string
}

// Gateway error sentinels — wrap with fmt.Errorf("...: %w", Err...). Callers use
// errors.Is. Never surface a raw upstream body to an end user (Principle II).
var (
	ErrProviderUnavailable = errors.New("ai: provider unavailable") // network/5xx/timeout — retryable
	ErrBadRequest          = errors.New("ai: bad request")          // 4xx from provider — not retryable
	ErrContextLength       = errors.New("ai: context length exceeded")
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run TestProviderInterface -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/provider.go internal/platform/ai/provider_test.go
git commit -m "feat(ai): Provider interface + error sentinels (US1a)"
```

---

## Task 3: Model registry + cost

**Files:**
- Create: `internal/platform/ai/registry.go`
- Test: `internal/platform/ai/registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
package ai

import "testing"

func TestRegistryLookupAndCost(t *testing.T) {
	r := NewRegistry()
	r.Register(Model{
		ID: "claude-sonnet-4-6", Provider: "anthropic", ContextWindow: 200000,
		InputCentsPerMTok: 300, OutputCentsPerMTok: 1500, SupportsTools: true,
	})

	m, ok := r.Lookup("claude-sonnet-4-6")
	if !ok || m.Provider != "anthropic" {
		t.Fatalf("Lookup = (%+v, %v)", m, ok)
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("unknown model must not resolve")
	}
	// 1,000,000 in + 1,000,000 out = 300 + 1500 = 1800 cents.
	if c := m.CostCents(Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); c != 1800 {
		t.Errorf("CostCents = %d, want 1800", c)
	}
	// Rounds up a partial: 500k in @ 300/Mtok = 150 cents.
	if c := m.CostCents(Usage{InputTokens: 500_000}); c != 150 {
		t.Errorf("CostCents(500k in) = %d, want 150", c)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run TestRegistry -v`
Expected: FAIL — `undefined: NewRegistry`.

- [ ] **Step 3: Write minimal implementation**

```go
package ai

import "sync"

// Model is registry metadata for one model: pricing (cents per MILLION tokens,
// integer to avoid float drift), context window, and whether it supports tools.
type Model struct {
	ID                 string
	Provider           string
	ContextWindow      int
	InputCentsPerMTok  int64
	OutputCentsPerMTok int64
	SupportsTools      bool
}

// CostCents returns the integer-cent cost of a usage under this model's pricing.
// Math is in (tokens * centsPerMTok) / 1_000_000 with rounding so a sub-million
// call is never free.
func (m Model) CostCents(u Usage) int64 {
	in := ceilDiv(int64(u.InputTokens)*m.InputCentsPerMTok, 1_000_000)
	out := ceilDiv(int64(u.OutputTokens)*m.OutputCentsPerMTok, 1_000_000)
	return in + out
}

func ceilDiv(a, b int64) int64 {
	if a == 0 {
		return 0
	}
	return (a + b - 1) / b
}

// Registry is a concurrency-safe model catalog. Seeded with known models at
// startup; self-hosters register local models (e.g. an ollama tag) too.
type Registry struct {
	mu     sync.RWMutex
	models map[string]Model
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{models: map[string]Model{}} }

// Register adds or replaces a model by ID.
func (r *Registry) Register(m Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models[m.ID] = m
}

// Lookup returns the model by ID and whether it is known.
func (r *Registry) Lookup(id string) (Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[id]
	return m, ok
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run TestRegistry -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/registry.go internal/platform/ai/registry_test.go
git commit -m "feat(ai): model registry + integer-cent cost (US1a)"
```

---

## Task 4: MockProvider (deterministic test backbone)

**Files:**
- Create: `internal/platform/ai/mock.go`
- Test: `internal/platform/ai/mock_test.go`

- [ ] **Step 1: Write the failing test**

```go
package ai

import (
	"context"
	"errors"
	"testing"
)

func TestMockProviderScriptsAndRecords(t *testing.T) {
	m := NewMockProvider(
		Response{ToolCalls: []ToolCall{{ID: "c1", Name: "get_ticket"}}, FinishReason: FinishToolUse},
		Response{Text: "done", FinishReason: FinishStop, Usage: Usage{InputTokens: 5, OutputTokens: 3}},
	)

	r1, err := m.Complete(context.Background(), Request{Model: "x", Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if err != nil || r1.FinishReason != FinishToolUse {
		t.Fatalf("call 1 = (%+v, %v)", r1, err)
	}
	r2, _ := m.Complete(context.Background(), Request{Model: "x"})
	if r2.Text != "done" {
		t.Fatalf("call 2 text = %q", r2.Text)
	}
	// Exhausted: a third call errors so a runaway loop in a test fails loudly.
	if _, err := m.Complete(context.Background(), Request{}); !errors.Is(err, ErrMockExhausted) {
		t.Fatalf("want ErrMockExhausted, got %v", err)
	}
	// It recorded every request it received.
	if len(m.Requests()) != 3 || m.Requests()[0].Messages[0].Text != "hi" {
		t.Fatalf("recorded = %+v", m.Requests())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ai/ -run TestMockProvider -v`
Expected: FAIL — `undefined: NewMockProvider`.

- [ ] **Step 3: Write minimal implementation**

```go
package ai

import (
	"context"
	"errors"
	"sync"
)

// ErrMockExhausted is returned when a MockProvider runs out of scripted
// responses — surfacing an unexpected extra model call in a test loudly.
var ErrMockExhausted = errors.New("ai: mock provider exhausted")

// MockProvider returns pre-scripted Responses in order and records every Request
// it was handed. It is the deterministic backbone for run-loop / gate / triage
// tests — no network, no fixtures (recorded golden fixtures arrive in US1b).
type MockProvider struct {
	mu     sync.Mutex
	queue  []Response
	calls  []Request
}

// NewMockProvider scripts the responses returned by successive Complete calls.
func NewMockProvider(responses ...Response) *MockProvider {
	return &MockProvider{queue: append([]Response(nil), responses...)}
}

// Complete returns the next scripted response, recording the request.
func (m *MockProvider) Complete(_ context.Context, req Request) (Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	if len(m.queue) == 0 {
		return Response{}, ErrMockExhausted
	}
	resp := m.queue[0]
	m.queue = m.queue[1:]
	return resp, nil
}

// Name identifies the provider.
func (m *MockProvider) Name() string { return "mock" }

// Requests returns a copy of every Request handed to Complete, in order.
func (m *MockProvider) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.calls...)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/platform/ai/ -run TestMockProvider -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/mock.go internal/platform/ai/mock_test.go
git commit -m "feat(ai): programmable MockProvider for deterministic tests (US1a)"
```

---

## Task 5: Migration 0025 — ai_provider_credential

**Files:**
- Create: `migrations/0025_ai_provider_credential.up.sql`
- Create: `migrations/0025_ai_provider_credential.down.sql`

> **Pattern source:** mirror `email_domain` in `migrations/0013_support_desk.up.sql` (table shape, composite FK, `tenant_root_id`, `support_tenant_root_immutable` trigger) and its RLS policy in `migrations/0014_support_rls.up.sql`. Copy the RLS policy form verbatim, substituting the table name — the policy must be `business_id IN (SELECT business_id FROM authorized_businesses(current_principal()))` exactly as the support tables use.

- [ ] **Step 1: Write the up migration**

```sql
-- 0025: per-business BYO LLM provider credentials (Spec 003 US1a). The API key is
-- NEVER stored raw — sealed_key_ref holds an opaque crypto.Sealer (AES-256-GCM)
-- ref. Keyless local providers (ollama/vllm) may have a NULL sealed_key_ref.
CREATE TYPE ai_provider AS ENUM ('anthropic', 'openai', 'ollama', 'vllm');

CREATE TABLE ai_provider_credential (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    provider        ai_provider NOT NULL,
    sealed_key_ref  text,            -- opaque Sealer ref; NULL ⇒ keyless local provider
    base_url        text,            -- openai-compat / self-host endpoint; NULL ⇒ provider default
    default_model   text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, provider),  -- one credential per provider per business
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX ai_provider_credential_business_idx
    ON ai_provider_credential (business_id, tenant_root_id);

-- tenant_root_id is immutable after insert (reuse the support trigger fn).
CREATE TRIGGER ai_provider_credential_troot_immutable
    BEFORE UPDATE ON ai_provider_credential
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- RLS: business-scoped, identical form to the support tables (0014).
ALTER TABLE ai_provider_credential ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_provider_credential FORCE ROW LEVEL SECURITY;
CREATE POLICY ai_provider_credential_rls ON ai_provider_credential FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
```

- [ ] **Step 2: Write the down migration**

```sql
DROP TABLE IF EXISTS ai_provider_credential;
DROP TYPE IF EXISTS ai_provider;
```

- [ ] **Step 3: Apply + verify the migration runs clean**

Run: `make migrate-up` (or the project's migrate target — check `Makefile`).
Then verify the down is reversible: `make migrate-down` once, then `make migrate-up` again.
Expected: no errors; `ai_provider_credential` exists with RLS enabled.

> If the exact RLS function name (`authorized_businesses` / `current_principal`) or trigger fn differs, grep `migrations/0014_support_rls.up.sql` for the email_domain policy and copy its precise form before re-running.

- [ ] **Step 4: Commit**

```bash
git add migrations/0025_ai_provider_credential.up.sql migrations/0025_ai_provider_credential.down.sql
git commit -m "feat(db): ai_provider_credential table + RLS, migration 0025 (US1a)"
```

---

## Task 6: sqlc queries + generate

**Files:**
- Create: `db/query/ai.sql`
- Generated: `internal/platform/db/dbgen/ai.sql.go` (via `make generate`)

> **Pattern source:** mirror `db/query/identity.sql` (the `email_domain` queries shown in the exploration) — `INSERT … SELECT … FROM business WHERE b.id = sqlc.arg('business_id')` so the FK + tenant_root come from the parent row; `SELECT … WHERE id = $1 AND business_id = $2` for the ownership-predicate read.

- [ ] **Step 1: Write the queries**

```sql
-- name: InsertAIProviderCredential :one
INSERT INTO ai_provider_credential (
    id, business_id, tenant_root_id, provider, sealed_key_ref, base_url, default_model,
    created_at, updated_at)
SELECT
    $1,
    b.id,
    b.tenant_root_id,
    sqlc.arg('provider')::ai_provider,
    sqlc.arg('sealed_key_ref'),
    sqlc.arg('base_url'),
    sqlc.arg('default_model'),
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: GetAIProviderCredential :one
SELECT * FROM ai_provider_credential
WHERE business_id = $1 AND provider = $2;

-- name: GetAIProviderCredentialByID :one
SELECT * FROM ai_provider_credential
WHERE id = $1 AND business_id = $2;

-- name: ListAIProviderCredentials :many
SELECT * FROM ai_provider_credential
WHERE business_id = $1
ORDER BY provider;

-- name: DeleteAIProviderCredential :execrows
DELETE FROM ai_provider_credential
WHERE id = $1 AND business_id = $2;
```

- [ ] **Step 2: Generate**

Run: `make generate`
Expected: creates `internal/platform/db/dbgen/ai.sql.go` with `InsertAIProviderCredentialParams`, `GetAIProviderCredentialParams`, etc., plus an `AiProviderCredential` model struct. Then `go build ./...` succeeds.

- [ ] **Step 3: Commit**

```bash
git add db/query/ai.sql internal/platform/db/dbgen/
git commit -m "feat(db): ai_provider_credential sqlc queries (US1a)"
```

---

## Task 7: CredentialService — unit (validation + Resolve)

**Files:**
- Create: `internal/agents/credential.go`
- Test: `internal/agents/credential_test.go`

> The seal/unseal logic is unit-testable without a DB: inject a real `*crypto.Sealer` built from a 32-byte test key and exercise `Resolve`'s decrypt path against a fabricated row. CRUD-over-DB is the integration test (Task 8).

- [ ] **Step 1: Write the failing test**

```go
package agents

import (
	"crypto/rand"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/crypto"
)

func newTestSealer(t *testing.T) *crypto.Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	s, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

func TestSealAPIKeyAndResolveRoundTrip(t *testing.T) {
	sealer := newTestSealer(t)
	svc := &CredentialService{Sealer: sealer}

	ref, err := svc.sealAPIKey("sk-secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if ref == "" || ref == "sk-secret" {
		t.Fatalf("ref must be a sealed, non-plaintext string, got %q", ref)
	}

	// Resolve unseals a stored row into a usable credential.
	got, err := svc.resolveRow(storedCredential{
		Provider: "anthropic", SealedKeyRef: &ref, DefaultModel: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.APIKey != "sk-secret" || got.Provider != "anthropic" || got.Model != "claude-sonnet-4-6" {
		t.Fatalf("resolved = %+v", got)
	}
}

func TestResolveKeylessProvider(t *testing.T) {
	svc := &CredentialService{Sealer: newTestSealer(t)}
	got, err := svc.resolveRow(storedCredential{Provider: "ollama", SealedKeyRef: nil, DefaultModel: "llama3"})
	if err != nil {
		t.Fatalf("resolve keyless: %v", err)
	}
	if got.APIKey != "" {
		t.Errorf("keyless provider APIKey = %q, want empty", got.APIKey)
	}
}

func TestValidateInput(t *testing.T) {
	svc := &CredentialService{Sealer: newTestSealer(t)}
	if err := svc.validate(CreateCredentialInput{Provider: "anthropic", DefaultModel: ""}); err == nil {
		t.Error("empty default_model must be a validation error")
	}
	if err := svc.validate(CreateCredentialInput{Provider: "bogus", DefaultModel: "m"}); err == nil {
		t.Error("unknown provider must be a validation error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/ -run 'TestSeal|TestResolve|TestValidate' -v`
Expected: FAIL — `undefined: CredentialService`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package agents is the agent runtime: agent definitions, the run loop, the
// autonomy gate, the approvals queue, and BYO provider credentials. This file is
// the credential store (Spec 003 US1a): CRUD over ai_provider_credential with the
// API key sealed at rest, plus Resolve to hand the gateway a usable credential.
package agents

import (
	"fmt"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// knownProviders is the closed set accepted at the service boundary (mirrors the
// ai_provider enum). Keep in lockstep with migration 0025.
var knownProviders = map[string]bool{"anthropic": true, "openai": true, "ollama": true, "vllm": true}

// CredentialService manages per-business BYO provider credentials. DB is the
// RLS-scoped handle (nil in pure unit tests that only exercise seal/resolve).
type CredentialService struct {
	DB     credentialDB    // injected; see credential_db.go-style wiring in Task 8
	Sealer *crypto.Sealer
}

// CreateCredentialInput is the caller-supplied credential to store.
type CreateCredentialInput struct {
	Provider     string
	APIKey       string // plaintext; sealed before persistence, never stored/logged raw
	BaseURL      string // optional (openai-compat / self-host)
	DefaultModel string
}

// ResolvedCredential is what the gateway needs to build a Provider.
type ResolvedCredential struct {
	Provider string
	APIKey   string // plaintext, in-memory only
	BaseURL  string
	Model    string
}

// storedCredential is the unsealed-at-rest shape (mirrors the dbgen row; defined
// here so seal/resolve are unit-testable without the DB). Task 8 maps the dbgen
// row into this.
type storedCredential struct {
	Provider     string
	SealedKeyRef *string
	BaseURL      *string
	DefaultModel string
}

func (s *CredentialService) validate(in CreateCredentialInput) error {
	if !knownProviders[in.Provider] {
		return fmt.Errorf("agents: unknown provider %q: %w", in.Provider, errs.ErrValidation)
	}
	if in.DefaultModel == "" {
		return fmt.Errorf("agents: default_model required: %w", errs.ErrValidation)
	}
	return nil
}

// sealAPIKey returns an opaque sealed ref for a plaintext key ("" ⇒ no ref).
func (s *CredentialService) sealAPIKey(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	ref, err := s.Sealer.Seal([]byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("agents: seal api key: %w", err)
	}
	return ref, nil
}

// resolveRow unseals a stored credential into a usable ResolvedCredential.
func (s *CredentialService) resolveRow(row storedCredential) (ResolvedCredential, error) {
	out := ResolvedCredential{Provider: row.Provider, Model: row.DefaultModel}
	if row.BaseURL != nil {
		out.BaseURL = *row.BaseURL
	}
	if row.SealedKeyRef != nil && *row.SealedKeyRef != "" {
		key, err := s.Sealer.Open(*row.SealedKeyRef)
		if err != nil {
			return ResolvedCredential{}, fmt.Errorf("agents: unseal api key: %w", err)
		}
		out.APIKey = string(key)
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/ -run 'TestSeal|TestResolve|TestValidate' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/credential.go internal/agents/credential_test.go
git commit -m "feat(agents): credential seal/resolve/validate (US1a)"
```

---

## Task 8: CredentialService — CRUD over the DB + integration test

**Files:**
- Modify: `internal/agents/credential.go` (add the `credentialDB` interface + Create/Get/List/Delete methods)
- Test: `internal/agents/credential_integration_test.go`

> **Pattern source:** `internal/ticketing/service.go` — `s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error { q := dbgen.New(tx); … })`, ownership predicates pushed into SQL, `mapErr(pgx.ErrNoRows) → errs.ErrNotFound`. The integration test mirrors `internal/platform/notify/send_integration_test.go` (testdb.Start, seed via `tdb.Super`, exercise via `tdb.App`).

- [ ] **Step 1: Add the DB-facing methods to credential.go**

```go
import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// credentialDB is the minimal DB surface this service needs — satisfied by the
// real *db.DB. Declared as an interface so unit tests can omit it.
type credentialDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// Create seals the API key and inserts the credential, returning its id.
func (s *CredentialService) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateCredentialInput) (uuid.UUID, error) {
	if err := s.validate(in); err != nil {
		return uuid.Nil, err
	}
	ref, err := s.sealAPIKey(in.APIKey)
	if err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	var refArg *string
	if ref != "" {
		refArg = &ref
	}
	var baseArg *string
	if in.BaseURL != "" {
		baseArg = &in.BaseURL
	}
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, qerr := dbgen.New(tx).InsertAIProviderCredential(ctx, dbgen.InsertAIProviderCredentialParams{
			ID:            id,
			BusinessID:    businessID,
			Provider:      dbgen.AiProvider(in.Provider),
			SealedKeyRef:  refArg,
			BaseUrl:       baseArg,
			DefaultModel:  in.DefaultModel,
		})
		return qerr
	})
	if err != nil {
		return uuid.Nil, mapCredErr(err)
	}
	return id, nil
}

// Resolve fetches + unseals the credential for (business, provider).
func (s *CredentialService) Resolve(ctx context.Context, principalID, businessID uuid.UUID, provider string) (ResolvedCredential, error) {
	var row dbgen.AiProviderCredential
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).GetAIProviderCredential(ctx, dbgen.GetAIProviderCredentialParams{
			BusinessID: businessID, Provider: dbgen.AiProvider(provider),
		})
		row = r
		return qerr
	})
	if err != nil {
		return ResolvedCredential{}, mapCredErr(err)
	}
	return s.resolveRow(storedCredential{
		Provider: string(row.Provider), SealedKeyRef: row.SealedKeyRef,
		BaseURL: row.BaseUrl, DefaultModel: row.DefaultModel,
	})
}

func mapCredErr(err error) error {
	if err == nil {
		return nil
	}
	if errorsIsNoRows(err) {
		return fmt.Errorf("agents: credential not found: %w", errs.ErrNotFound)
	}
	return err
}
```

> Add `func errorsIsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }` and the `errors` import, OR reuse the project's existing `mapErr` helper if `internal/agents` can import it — check `internal/ticketing/service.go` for the canonical `mapErr` and prefer copying its exact body to avoid divergence.

- [ ] **Step 2: Write the integration test**

```go
//go:build integration

package agents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestCredentialCRUDRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	// seedAgentTenant returns a business + an agent principal authorized on it.
	// (Mirror the seed helpers in notify/ticketing integration tests; seed via
	// tdb.Super, then act via tdb.App through the service's WithPrincipal.)
	ten := seedAgentTenant(ctx, t, tdb)

	svc := &CredentialService{DB: tdb.App, Sealer: newTestSealer(t)}

	id, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
		Provider: "anthropic", APIKey: "sk-live", DefaultModel: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("create returned nil id")
	}

	got, err := svc.Resolve(ctx, ten.principalID, ten.businessID, "anthropic")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.APIKey != "sk-live" || got.Model != "claude-sonnet-4-6" {
		t.Fatalf("resolved = %+v", got)
	}

	// The raw key is NEVER in the column — only the sealed ref.
	var stored *string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT sealed_key_ref FROM ai_provider_credential WHERE id=$1`, id).Scan(&stored); err != nil {
		t.Fatalf("read sealed ref: %v", err)
	}
	if stored == nil || *stored == "sk-live" {
		t.Fatalf("api key stored unsealed: %v", stored)
	}

	// A different tenant cannot resolve it (RLS / no-oracle not-found).
	other := seedAgentTenant(ctx, t, tdb)
	if _, err := svc.Resolve(ctx, other.principalID, other.businessID, "anthropic"); err == nil {
		t.Fatal("cross-tenant Resolve must fail (not found)")
	}
}
```

- [ ] **Step 3: Implement `seedAgentTenant`**

Create `internal/agents/testsupport_integration_test.go` (`//go:build integration`). Mirror the seed helper in `internal/platform/notify/send_integration_test.go` / `internal/security_regression` (insert a tenant root + business + an `agent` principal via `tdb.Super`, return `{businessID, principalID uuid.UUID}`). Grep those files for the exact insert statements that create a business + an authorized principal and copy them.

- [ ] **Step 4: Run the integration test**

Run: `go test -tags integration ./internal/agents/ -run TestCredentialCRUDRoundTrip -v`
Expected: PASS — create, resolve round-trips the key, the column holds only a sealed ref, and a foreign tenant gets not-found.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/credential.go internal/agents/credential_integration_test.go internal/agents/testsupport_integration_test.go
git commit -m "feat(agents): credential CRUD over RLS DB + integration test (US1a)"
```

---

## Task 9: Full-gate check

- [ ] **Step 1: Run the whole gate**

```bash
make test && make int-test && make contract-test && make lint
```
Expected: all PASS, lint 0 issues. (The new `internal/platform/ai` + `internal/agents` packages compile, unit tests pass; the credential integration test passes under the `integration` tag.)

- [ ] **Step 2: Commit any lint fixups, then stop**

US1a is complete: the gateway primitives (`Provider`, schema, registry, `MockProvider`) and the sealed BYO-credential store exist and are tested. Next plan (US1b) implements the live `anthropic` + `openai-compat` transports against this `Provider` interface; US2 builds agent definitions on the same DB pattern.

---

## Self-review notes (author)
- **Spec coverage (US1):** provider abstraction → Provider interface (T2) + MockProvider (T4); model registry + cost → T3; BYO sealed keys → migration (T5) + queries (T6) + service (T7–T8). Live transports (anthropic/openai-compat) are the explicit US1b follow-on, not dropped.
- **Type consistency:** `Provider.Complete(ctx, Request) (Response, error)` is used identically in T2/T4; `ResolvedCredential{Provider,APIKey,BaseURL,Model}` consistent T7/T8; `storedCredential` fields match the dbgen row mapped in T8.
- **Known soft-pointers (deliberate, not placeholders):** the RLS policy (T5) and `mapErr`/seed helpers (T8) instruct exact replication of named existing files (`0014_support_rls.up.sql`, `ticketing/service.go`, notify integration seeds) because copying the canonical pattern is safer than re-deriving it — the engineer must open those files and mirror them.
- **dbgen field names** (`SealedKeyRef`, `BaseUrl`, `DefaultModel`, `AiProvider`) are sqlc's predicted output; if `make generate` produces different casing, adjust the call sites in T8 to match the generated struct.
