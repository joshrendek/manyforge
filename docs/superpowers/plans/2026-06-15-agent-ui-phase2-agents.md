# Agent Management UI — Implementation Plan (Phase 2 of 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a web surface for full agent CRUD (create / list / edit / delete) at `/agents`, plus two read-only backend metadata endpoints (registered tools; model catalog) so the agent form's pickers always reflect the real registry.

**Architecture:** The agent CRUD backend already exists (`agents.AgentService` + `agents.Handler` mounting `/businesses/{id}/agents`, gated `agents.configure`). This phase adds two metadata reads onto the **same** `agents.Handler` via a late-wiring `SetMetadata(...)` setter (the connectors `SetSyncTrigger` pattern) so existing `NewHandler(svc)` tests stay green and there's no chi same-prefix conflict. `/tools` reads a new sorted `ToolRegistry.All()`; `/models` reads `model_pricing` via the blessed non-RLS `DB.WithTx` path (exactly like `LoadModelRegistry`). Frontend mirrors the Angular 21 connectors page (standalone, signals, template-driven `[(ngModel)]`), with the service in `core/`.

**Tech Stack:** Go (chi, pgx, `internal/agents`), Postgres, Angular 21 (standalone, signals, `FormsModule`, `@if/@for`), Playwright.

**Spec:** `docs/superpowers/specs/2026-06-15-agent-management-ui-design.md` (Phase 2).
**bd issue:** `manyforge-1kv`. **Depends on:** Phase 1 plan (`2026-06-15-agent-ui-phase1-credentials.md`) — Phase 2 reuses the `/credentials` nav precedent and assumes the agents-group gate exists (it already does, pre-Phase-1).

> **Prereq:** the `/agents` page is only useful once a credential exists. Land Phase 1 first (or concurrently); Phase 2 does not depend on Phase 1's code, only on its product ordering.

---

## Conventions for every task

Same as Phase 1: `export PATH="$HOME/go/bin:$PATH"`; gates `go build ./...`, `make test`, `make sec-test`, `make lint`; integration `go test -tags integration ./internal/agents/...`; **no new SQL** in this phase (reuses `ListModelPricing`), so **no sqlc run**; trust `go build` over stale gopls; frontend `cd web && npm test|run build|run e2e`. One commit per task. After `bd close`/`--notes`, a separate `chore(bd): …` commit.

---

## File Structure

**Backend (create/modify):**
- Modify `internal/agents/tools.go` — add `ToolRegistry.All() []Tool` (sorted) + `EffectClass.String()`.
- Create `internal/agents/metadata.go` — `ModelInfo`, `ModelCatalog` (+ `modelLister`/`toolLister` seams), the `SetMetadata` setter belongs on `Handler` (in agent_handler.go).
- Modify `internal/agents/agent_handler.go` — `tools`/`models` fields, `SetMetadata`, `listTools`/`listModels`, route registration.
- Modify `internal/agents/agent_handler_test.go` (or a new `metadata_handler_test.go`) — metadata handler tests incl. the `/tools` vs `/{agentID}` precedence test.
- Create `internal/agents/metadata_test.go` — `ToolRegistry.All`/`EffectClass.String` unit tests; `ModelCatalog.ListModels` integration test.
- Modify `cmd/manyforge/main.go` — call `agentH.SetMetadata(...)`.
- Create `internal/security_regression/agent_metadata_gate_pin_test.go` — pin tools/models stay under the `agents.configure` group.
- Modify `specs/003-agent-runtime/contracts/openapi.yaml` — add tools/models schemas + paths.

**Frontend (create/modify):**
- Create `web/src/app/core/agents.service.ts` — service + interfaces + `tools()`/`models()`/`mcpServers()`.
- Create `web/src/app/pages/agents/list.ts`, `web/src/app/pages/agents/agent-form.ts` (+ `.spec.ts` each).
- Modify `web/src/app/app.routes.ts` — add `/agents`.
- Modify `web/src/app/ui/nav.ts` — add "Agents".
- Create `web/e2e/agents.spec.ts` — e2e.

---

## Task 1: `ToolRegistry.All()` + `EffectClass.String()`

**Files:**
- Modify: `internal/agents/tools.go`
- Test: `internal/agents/metadata_test.go` (create)

**Context (verified):** `ToolRegistry` has an unexported `tools map[string]Tool` and only `Get(name)` + `Names()` (unordered). `Tool{Name, Description, SchemaJSON, Effect EffectClass, RequiredPerm, Invoke}`. `EffectClass` consts: `EffectRead, EffectReversible, EffectExternal, EffectIrreversible`.

- [ ] **Step 1: Write the failing unit tests**

Create `internal/agents/metadata_test.go`:

```go
package agents

import "testing"

func TestEffectClassString(t *testing.T) {
	cases := map[EffectClass]string{
		EffectRead:         "read",
		EffectReversible:   "reversible",
		EffectExternal:     "external",
		EffectIrreversible: "irreversible",
		EffectClass(99):    "unknown",
	}
	for ec, want := range cases {
		if got := ec.String(); got != want {
			t.Fatalf("EffectClass(%d).String() = %q, want %q", ec, got, want)
		}
	}
}

func TestToolRegistryAllSorted(t *testing.T) {
	reg := &ToolRegistry{tools: map[string]Tool{
		"send_reply":  {Name: "send_reply", Effect: EffectExternal},
		"read_ticket": {Name: "read_ticket", Effect: EffectRead},
		"set_status":  {Name: "set_status", Effect: EffectReversible},
	}}
	all := reg.All()
	if len(all) != 3 {
		t.Fatalf("want 3 tools, got %d", len(all))
	}
	// All() must be sorted by Name (Names() is explicitly unordered).
	if all[0].Name != "read_ticket" || all[1].Name != "send_reply" || all[2].Name != "set_status" {
		t.Fatalf("All() not sorted by name: %v", []string{all[0].Name, all[1].Name, all[2].Name})
	}
}
```

(This test is in-package `agents` so it can build a `ToolRegistry` literal with the unexported `tools` field.)

- [ ] **Step 2: Run to verify failure**

Run: `export PATH="$HOME/go/bin:$PATH" && go test ./internal/agents/ -run 'TestEffectClassString|TestToolRegistryAllSorted' -v`
Expected: FAIL — `ec.String` and `reg.All` undefined.

- [ ] **Step 3: Implement `String()` and `All()`**

In `internal/agents/tools.go`, after the `EffectClass` const block (~line 30), add:

```go
// String renders the effect class for API/metadata responses. An unknown class
// is "unknown" (fail-closed — matches the gate's unknown-class-to-approval rule).
func (e EffectClass) String() string {
	switch e {
	case EffectRead:
		return "read"
	case EffectReversible:
		return "reversible"
	case EffectExternal:
		return "external"
	case EffectIrreversible:
		return "irreversible"
	default:
		return "unknown"
	}
}
```

After the `Names()` method (~line 87), add (and ensure `"sort"` is imported):

```go
// All returns every registered tool, sorted by name, for metadata/listing.
// (Get/Names remain the hot-path accessors; All is for the read-only API.)
func (r *ToolRegistry) All() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
```

- [ ] **Step 4: Run to verify pass**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test ./internal/agents/ -run 'TestEffectClassString|TestToolRegistryAllSorted' -v`
Expected: build exit 0; both PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/tools.go internal/agents/metadata_test.go
git commit -m "feat(agents): ToolRegistry.All (sorted) + EffectClass.String"
```

---

## Task 2: `ModelCatalog` (model_pricing reader)

**Files:**
- Create: `internal/agents/metadata.go`
- Test: `internal/agents/metadata_test.go` (append integration test)

**Context (verified):** `ListModelPricing(ctx)` → `[]dbgen.ListModelPricingRow{ModelID, Provider, …}` (no params struct). `model_pricing` is a system catalog (no RLS) — read it via `DB.WithTx(ctx, fn)` (exactly how `LoadModelRegistry` does it). The package already declares a `modelPricingDB` interface (param of `LoadModelRegistry`) that exposes `WithTx` — reuse it.

- [ ] **Step 1: Write the failing integration test**

Append to `internal/agents/metadata_test.go` — note this needs the testdb, so put it behind the integration tag in a **separate file** `internal/agents/metadata_integration_test.go` to keep the unit file tag-free:

Create `internal/agents/metadata_integration_test.go`:

```go
//go:build integration

package agents_test

import (
	"context"
	"testing"

	"github.com/manyforge/manyforge/internal/agents"
)

func TestModelCatalog_ListModels(t *testing.T) {
	db := newModelTestDB(t) // reuse the testdb helper that LoadModelRegistry's test uses; model_pricing is seeded by migration 0038
	cat := &agents.ModelCatalog{DB: db}
	models, err := cat.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("expected at least one seeded model in model_pricing")
	}
	for _, m := range models {
		if m.Provider == "" || m.ModelID == "" {
			t.Fatalf("model row missing provider/model_id: %+v", m)
		}
	}
}
```

(Use the same testdb constructor the existing `LoadModelRegistry`/`model_pricing` test uses — open `internal/agents/model_pricing_test.go` to copy the exact helper instead of `newModelTestDB`.)

- [ ] **Step 2: Run to verify failure**

Run: `export PATH="$HOME/go/bin:$PATH" && go test -tags integration ./internal/agents/ -run TestModelCatalog_ListModels -v`
Expected: FAIL — `agents.ModelCatalog` / `ModelInfo` undefined.

- [ ] **Step 3: Implement `metadata.go`**

Create `internal/agents/metadata.go`:

```go
package agents

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// ModelInfo is the non-pricing projection of a catalog model, for the agent
// form's model picker.
type ModelInfo struct {
	Provider string
	ModelID  string
}

// modelLister is the metadata seam the agent handler reads models through.
type modelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// toolLister is the metadata seam the agent handler reads tools through.
// Satisfied by *ToolRegistry.
type toolLister interface {
	All() []Tool
}

var _ toolLister = (*ToolRegistry)(nil)
var _ modelLister = (*ModelCatalog)(nil)

// ModelCatalog reads the model_pricing system catalog. It is NOT RLS-scoped
// (model_pricing has no tenant), so it uses WithTx (no principal) — the same
// path LoadModelRegistry uses. modelPricingDB (declared in model_pricing.go)
// exposes WithTx.
type ModelCatalog struct{ DB modelPricingDB }

// ListModels returns the enabled catalog models (provider + id), ordered by id.
func (c *ModelCatalog) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var rows []dbgen.ListModelPricingRow
	err := c.DB.WithTx(ctx, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).ListModelPricing(ctx)
		rows = r
		return e
	})
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, ModelInfo{Provider: r.Provider, ModelID: r.ModelID})
	}
	return out, nil
}
```

> **Verify before building:** confirm the `modelPricingDB` interface name + that it declares `WithTx(ctx, func(pgx.Tx) error) error` (open `internal/agents/model_pricing.go`). If the interface is named differently or doesn't expose `WithTx`, either reuse the real one or define a local `type modelCatalogDB interface { WithTx(context.Context, func(pgx.Tx) error) error }` and use `*db.DB` at the call site (it satisfies it). Also confirm the pgx import path matches the rest of the package (`github.com/jackc/pgx/v5`).

- [ ] **Step 4: Run to verify pass**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test -tags integration ./internal/agents/ -run TestModelCatalog_ListModels -v`
Expected: build exit 0; PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/metadata.go internal/agents/metadata_integration_test.go
git commit -m "feat(agents): ModelCatalog reads model_pricing via WithTx"
```

---

## Task 3: Metadata endpoints on `agents.Handler` (`/tools`, `/models`)

**Files:**
- Modify: `internal/agents/agent_handler.go`
- Test: `internal/agents/agent_handler_test.go` (append) or new `internal/agents/metadata_handler_test.go`

**Context (verified):** `Handler struct{ svc agentCRUD }`, `NewHandler(svc)`, and `ProtectedRoutes` mounting `/businesses/{id}/agents` with `/`, `/{agentID}`. chi matches static segments before `/{agentID}`, so `/tools` + `/models` registered in the same subtree resolve correctly. Late-wire the listers via a setter so existing `NewHandler(svc)` tests (no metadata) keep passing; handlers nil-guard. The group is already gated `agents.configure` in main.go, so no new gate is needed.

- [ ] **Step 1: Write the failing handler tests**

Create `internal/agents/metadata_handler_test.go`:

```go
package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

type fakeTools struct{ items []agents.Tool }

func (f fakeTools) All() []agents.Tool { return f.items }

type fakeModels struct{ items []agents.ModelInfo }

func (f fakeModels) ListModels(context.Context) ([]agents.ModelInfo, error) { return f.items, nil }

// mountAgentsWithMeta builds the agents handler with metadata wired and a
// principal+{id} context, reusing the package's principal-injection helper.
func mountAgentsWithMeta(t *testing.T) http.Handler {
	t.Helper()
	h := agents.NewHandler(stubAgentCRUD{}) // reuse the existing fake agent svc from agent_handler_test.go
	h.SetMetadata(
		fakeTools{items: []agents.Tool{{Name: "read_ticket", Description: "read", Effect: agents.EffectRead, RequiredPerm: "tickets.read"}}},
		fakeModels{items: []agents.ModelInfo{{Provider: "anthropic", ModelID: "claude-opus-4-8"}}},
	)
	r := chi.NewRouter()
	r.Use(injectPrincipal(uuid.New())) // reuse the helper used by the existing agent handler tests
	h.ProtectedRoutes(r)
	return r
}

func TestAgentMetadata_ListTools(t *testing.T) {
	srv := mountAgentsWithMeta(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/businesses/"+uuid.New().String()+"/agents/tools", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Items []struct {
			Name         string `json:"name"`
			Effect       string `json:"effect"`
			RequiredPerm string `json:"required_perm"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Name != "read_ticket" || got.Items[0].Effect != "read" {
		t.Fatalf("unexpected tools payload: %s", rec.Body.String())
	}
}

func TestAgentMetadata_ListModels(t *testing.T) {
	srv := mountAgentsWithMeta(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/businesses/"+uuid.New().String()+"/agents/models", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var got struct {
		Items []struct {
			Provider string `json:"provider"`
			ModelID  string `json:"model_id"`
		} `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Items) != 1 || got.Items[0].ModelID != "claude-opus-4-8" {
		t.Fatalf("unexpected models payload: %s", rec.Body.String())
	}
}

// Precedence: /agents/tools must hit listTools, NOT getAgent (which would try to
// parse "tools" as a UUID). Confirms the static route wins over /{agentID}.
func TestAgentMetadata_ToolsNotCapturedByAgentID(t *testing.T) {
	srv := mountAgentsWithMeta(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/businesses/"+uuid.New().String()+"/agents/tools", nil)
	srv.ServeHTTP(rec, req)
	// getAgent on "tools" would 404 (bad uuid) — listTools returns 200 with items.
	if rec.Code != http.StatusOK {
		t.Fatalf("static /tools route did not win over /{agentID}: got %d", rec.Code)
	}
}

// When metadata is not wired (plain NewHandler), the endpoints 404 — old tests stay valid.
func TestAgentMetadata_NilWhenUnset(t *testing.T) {
	h := agents.NewHandler(stubAgentCRUD{})
	r := chi.NewRouter()
	r.Use(injectPrincipal(uuid.New()))
	h.ProtectedRoutes(r)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/businesses/"+uuid.New().String()+"/agents/tools", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 when metadata unset, got %d", rec.Code)
	}
	_ = httpx.MaxJSONBodyBytes // keep httpx import if otherwise unused; drop if not needed
}
```

> **Adapt to the real test helpers:** `stubAgentCRUD{}` and `injectPrincipal(...)` are placeholders for whatever the existing `agent_handler_test.go` already defines (the fake agent service + the principal-context middleware). Open that file and reuse its actual names; delete the `httpx` keep-import line if it isn't needed.

- [ ] **Step 2: Run to verify failure**

Run: `export PATH="$HOME/go/bin:$PATH" && go test ./internal/agents/ -run TestAgentMetadata -v`
Expected: FAIL — `h.SetMetadata` undefined; routes not registered.

- [ ] **Step 3: Implement fields, setter, handlers, and routes**

In `internal/agents/agent_handler.go`:

(a) Extend the `Handler` struct:

```go
// Handler exposes agent-definition CRUD + read-only metadata (tools/models) over
// HTTP. Mounted behind the agents.configure RequirePermission gate. Metadata
// listers are optional (late-wired via SetMetadata); when nil, those endpoints 404.
type Handler struct {
	svc    agentCRUD
	tools  toolLister
	models modelLister
}
```

(b) Add the setter after `NewHandler`:

```go
// SetMetadata late-wires the tool registry + model catalog that back the
// /agents/tools and /agents/models read endpoints. Optional — the connectors
// SetSyncTrigger late-wiring pattern — so plain NewHandler(svc) keeps working.
func (h *Handler) SetMetadata(tools toolLister, models modelLister) {
	h.tools = tools
	h.models = models
}
```

(c) Register the static metadata routes inside the existing `/businesses/{id}/agents` Route, **before** `/{agentID}`:

```go
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/agents", func(r chi.Router) {
		r.Get("/", h.listAgents)
		r.Post("/", h.createAgent)
		// Static metadata routes — chi matches these before /{agentID}.
		r.Get("/tools", h.listTools)
		r.Get("/models", h.listModels)
		r.Get("/{agentID}", h.getAgent)
		r.Patch("/{agentID}", h.updateAgent)
		r.Delete("/{agentID}", h.deleteAgent)
	})
}
```

(d) Add the two handlers + their DTOs (place near `agentResp`):

```go
type toolResp struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Effect       string `json:"effect"`
	RequiredPerm string `json:"required_perm,omitempty"`
}

type modelResp struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
}

func (h *Handler) listTools(w http.ResponseWriter, r *http.Request) {
	if h.tools == nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	all := h.tools.All()
	out := make([]toolResp, 0, len(all))
	for _, t := range all {
		out = append(out, toolResp{
			Name:         t.Name,
			Description:  t.Description,
			Effect:       t.Effect.String(),
			RequiredPerm: t.RequiredPerm,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	if h.models == nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	models, err := h.models.ListModels(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]modelResp, 0, len(models))
	for _, m := range models {
		out = append(out, modelResp{Provider: m.Provider, ModelID: m.ModelID})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}
```

(The metadata handlers don't parse principal/business — the route group's `agents.configure` gate already validated both; the data is a global catalog.)

- [ ] **Step 4: Run to verify pass (and that existing agent tests still pass)**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test ./internal/agents/ -run 'TestAgent' -v`
Expected: build exit 0; new `TestAgentMetadata_*` PASS; pre-existing agent handler tests still PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/agent_handler.go internal/agents/metadata_handler_test.go
git commit -m "feat(agents): /agents/tools + /agents/models metadata endpoints"
```

---

## Task 4: Wire metadata in main.go

**Files:**
- Modify: `cmd/manyforge/main.go`

**Context (verified):** `agentH := agents.NewHandler(agentSvc)` at ~line 151. `agents.NewToolRegistry(ticketSvc, connGateway)` is called inline elsewhere (lines 173, 199, 311-312); both args are in scope in `func main()`. `connGateway` is declared at line 170 and gets its final connector-backed value inside the `if len(cfg.ConnectorMasterKey) > 0 {…}` block (closes ~line 315). Tool *descriptors* don't depend on the gateway value (it's only captured for Invoke), but to avoid any surprise, wire metadata **after** that block. `database` satisfies `modelPricingDB`/the WithTx seam.

- [ ] **Step 1: Call `SetMetadata` after the connector wiring block**

In `cmd/manyforge/main.go`, after the `if len(cfg.ConnectorMasterKey) > 0 { … }` block (~line 315) and before the handler-aggregate struct literal (~line 487), add:

```go
	// Late-wire the agent handler's read-only metadata endpoints (/agents/tools,
	// /agents/models). The registry is built from the same inputs as the engine's
	// (ticketSvc + the resolved connGateway); only descriptors are read, never
	// invoked. The model catalog reads model_pricing via WithTx (no RLS).
	agentH.SetMetadata(
		agents.NewToolRegistry(ticketSvc, connGateway),
		&agents.ModelCatalog{DB: database},
	)
```

- [ ] **Step 2: Build + full test suite**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && make test`
Expected: build exit 0; all PASS.

- [ ] **Step 3: Commit**

```bash
git add cmd/manyforge/main.go
git commit -m "feat(agents): wire agent metadata endpoints in main"
```

---

## Task 5: Security-regression pin (metadata stays gated)

**Files:**
- Create: `internal/security_regression/agent_metadata_gate_pin_test.go`

**Context:** The metadata endpoints live inside `agents.Handler.ProtectedRoutes`, which main.go mounts under the `agents.configure` group. Pin both facts at source level so a refactor that moves the routes out of the gated group fails CI.

- [ ] **Step 1: Write the pin**

Create `internal/security_regression/agent_metadata_gate_pin_test.go`:

```go
package security_regression

import (
	"os"
	"strings"
	"testing"
)

// FINDING: manyforge-1kv — /agents/tools and /agents/models expose the tool and
// model catalogs and MUST stay behind the agents.configure gate (they live in the
// agents handler subtree, which main.go mounts under h.agentsConfigure).

func TestAgentMetadataRoutesRegistered(t *testing.T) {
	src, err := os.ReadFile("../agents/agent_handler.go")
	if err != nil {
		t.Fatalf("read handler: %v", err)
	}
	s := string(src)
	for _, want := range []string{`r.Get("/tools", h.listTools)`, `r.Get("/models", h.listModels)`} {
		if !strings.Contains(s, want) {
			t.Fatalf("agent metadata route missing: %s", want)
		}
	}
}

func TestAgentGroupStaysConfigureGated(t *testing.T) {
	src, err := os.ReadFile("../../cmd/manyforge/main.go")
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "ag.Use(h.agentsConfigure)") || !strings.Contains(s, "h.agents.ProtectedRoutes(ag)") {
		t.Fatal("agents handler group is no longer gated on agents.configure")
	}
}
```

(If main.go's agents group uses a different router variable than `ag`, match it.)

- [ ] **Step 2: Run + sec-test**

Run: `export PATH="$HOME/go/bin:$PATH" && go test ./internal/security_regression/ -run 'TestAgentMetadata|TestAgentGroup' -v && make sec-test`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/security_regression/agent_metadata_gate_pin_test.go
git commit -m "test(sec): pin agent metadata endpoints under agents.configure"
```

---

## Task 6: OpenAPI — tools/models schemas + paths

**Files:**
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml`

- [ ] **Step 1: Add schemas + paths**

Under `components.schemas` add:

```yaml
    AgentToolDescriptor:
      type: object
      properties:
        name: { type: string }
        description: { type: string }
        effect: { type: string, enum: [read, reversible, external, irreversible, unknown] }
        required_perm: { type: string }
      required: [name, effect]
    AgentModelDescriptor:
      type: object
      properties:
        provider: { type: string }
        model_id: { type: string }
      required: [provider, model_id]
```

Under `paths` add (both gated `agents.configure`):

```yaml
  /businesses/{id}/agents/tools:
    get:
      summary: List the tools an agent can be granted
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema:
                type: object
                properties:
                  items: { type: array, items: { $ref: '#/components/schemas/AgentToolDescriptor' } }
        '404': { description: Not found / not permitted }
  /businesses/{id}/agents/models:
    get:
      summary: List the model catalog for the agent model picker
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema:
                type: object
                properties:
                  items: { type: array, items: { $ref: '#/components/schemas/AgentModelDescriptor' } }
        '404': { description: Not found / not permitted }
```

- [ ] **Step 2: Validate + commit**

Run: `python3 -c "import yaml; yaml.safe_load(open('specs/003-agent-runtime/contracts/openapi.yaml')); print('ok')"`
Expected: `ok`.

```bash
git add specs/003-agent-runtime/contracts/openapi.yaml
git commit -m "docs(openapi): agent tools/models metadata paths"
```

---

## Task 7: Frontend service — `core/agents.service.ts`

**Files:**
- Create: `web/src/app/core/agents.service.ts`

**Context (verified):** Service in `core/`, `businessId` as an argument, base `/api/v1/businesses/${businessId}/agents`. Agent fields from `agentResp`. The MCP-servers picker reuses `GET /businesses/{id}/mcp_servers` → `{ items: [{ id, business_id, name, url, enabled, … }] }` (write-only auth omitted).

- [ ] **Step 1: Write the service**

Create `web/src/app/core/agents.service.ts`:

```ts
import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { AIProvider } from './ai-credentials.service';

export interface Agent {
  id: string;
  business_id: string;
  principal_id: string;
  name: string;
  provider: AIProvider;
  model: string;
  system_prompt: string;
  allowed_tools: string[];
  autonomy_mode: number; // 1 Assist, 2 Queue-writes, 3 Autonomous
  enabled: boolean;
  monthly_budget_cents: number;
  allowed_mcp_servers: string[];
  retriage_on_reply: boolean;
  created_at: string;
  updated_at: string;
}

export interface CreateAgentBody {
  name: string;
  provider: AIProvider;
  model: string;
  system_prompt: string;
  allowed_tools: string[];
  autonomy_mode: number;
  enabled: boolean;
  monthly_budget_cents: number;
  allowed_mcp_servers: string[];
  retriage_on_reply: boolean;
}

// Provider is immutable on update, so it is never part of the patch body.
export type UpdateAgentBody = Partial<Omit<CreateAgentBody, 'provider'>>;

export interface ToolDescriptor {
  name: string;
  description: string;
  effect: 'read' | 'reversible' | 'external' | 'irreversible' | 'unknown';
  required_perm?: string;
}

export interface ModelDescriptor {
  provider: string;
  model_id: string;
}

export interface McpServerRef {
  id: string;
  name: string;
  url: string;
  enabled: boolean;
}

@Injectable({ providedIn: 'root' })
export class AgentsService {
  private http = inject(HttpClient);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/agents`;
  }

  list(businessId: string): Observable<{ items: Agent[] }> {
    return this.http.get<{ items: Agent[] }>(this.base(businessId));
  }
  create(businessId: string, body: CreateAgentBody): Observable<Agent> {
    return this.http.post<Agent>(this.base(businessId), body);
  }
  update(businessId: string, id: string, body: UpdateAgentBody): Observable<Agent> {
    return this.http.patch<Agent>(`${this.base(businessId)}/${id}`, body);
  }
  remove(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/${id}`);
  }
  tools(businessId: string): Observable<{ items: ToolDescriptor[] }> {
    return this.http.get<{ items: ToolDescriptor[] }>(`${this.base(businessId)}/tools`);
  }
  models(businessId: string): Observable<{ items: ModelDescriptor[] }> {
    return this.http.get<{ items: ModelDescriptor[] }>(`${this.base(businessId)}/models`);
  }
  mcpServers(businessId: string): Observable<{ items: McpServerRef[] }> {
    return this.http.get<{ items: McpServerRef[] }>(`/api/v1/businesses/${businessId}/mcp_servers`);
  }
}
```

> If a `core/mcp.service.ts` already exposes an MCP-server list method, inject and reuse it instead of the `mcpServers()` method above (grep `web/src/app/core` for an existing MCP service first).

- [ ] **Step 2: Typecheck via build**

Run: `cd web && npm run build`
Expected: succeeds.

- [ ] **Step 3: Commit**

```bash
git add web/src/app/core/agents.service.ts
git commit -m "feat(web): AgentsService (CRUD + tools/models/mcp metadata)"
```

---

## Task 8: Frontend — agent form component

**Files:**
- Create: `web/src/app/pages/agents/agent-form.ts`
- Test: `web/src/app/pages/agents/agent-form.spec.ts`

**Context (verified):** Mirror `connector-form.ts` (standalone, `FormsModule`, signals, `describe()` error mapping). On create, load `tools()`/`models()`/`mcpServers()` to populate pickers. Provider immutable on edit (disabled select). Model is a dropdown filtered by provider for catalog providers (anthropic/openai) and a free-text input for self-host (ollama/vllm). Budget shown in dollars, sent as cents. Edit prefills and PATCHes changed fields.

- [ ] **Step 1: Write the failing unit spec**

Create `web/src/app/pages/agents/agent-form.spec.ts`:

```ts
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting, HttpTestingController } from '@angular/common/http/testing';
import { AgentFormComponent } from './agent-form';

function flushMetadata(http: HttpTestingController) {
  http.expectOne('/api/v1/businesses/b1/agents/tools').flush({
    items: [{ name: 'read_ticket', description: 'read', effect: 'read', required_perm: 'tickets.read' }],
  });
  http.expectOne('/api/v1/businesses/b1/agents/models').flush({
    items: [
      { provider: 'anthropic', model_id: 'claude-opus-4-8' },
      { provider: 'openai', model_id: 'gpt-5' },
    ],
  });
  http.expectOne('/api/v1/businesses/b1/mcp_servers').flush({
    items: [{ id: 'm1', name: 'docs', url: 'https://x', enabled: true }],
  });
}

describe('AgentFormComponent', () => {
  let fixture: ComponentFixture<AgentFormComponent>;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [AgentFormComponent],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    fixture = TestBed.createComponent(AgentFormComponent);
    fixture.componentInstance.businessId = 'b1';
    fixture.detectChanges(); // triggers ngOnInit metadata loads
    http = TestBed.inject(HttpTestingController);
    flushMetadata(http);
  });

  it('emits a create payload with cents-converted budget and selected tool', () => {
    const c = fixture.componentInstance;
    c.name = 'Triage Bot';
    c.provider.set('anthropic');
    c.model = 'claude-opus-4-8';
    c.systemPrompt = 'Be helpful';
    c.toggleTool('read_ticket');
    c.budgetDollars = 25; // → 2500 cents
    let saved = false;
    c.saved.subscribe(() => (saved = true));

    c.submit();

    const req = http.expectOne('/api/v1/businesses/b1/agents');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual(
      jasmine.objectContaining({
        name: 'Triage Bot',
        provider: 'anthropic',
        model: 'claude-opus-4-8',
        allowed_tools: ['read_ticket'],
        monthly_budget_cents: 2500,
      }),
    );
    req.flush({
      id: 'a1', business_id: 'b1', principal_id: 'p1', name: 'Triage Bot', provider: 'anthropic',
      model: 'claude-opus-4-8', system_prompt: 'Be helpful', allowed_tools: ['read_ticket'],
      autonomy_mode: 1, enabled: true, monthly_budget_cents: 2500, allowed_mcp_servers: [],
      retriage_on_reply: false, created_at: '', updated_at: '',
    });
    expect(saved).toBeTrue();
  });

  it('filters the model dropdown by provider', () => {
    const c = fixture.componentInstance;
    c.provider.set('openai');
    expect(c.modelsForProvider().map((m) => m.model_id)).toEqual(['gpt-5']);
    c.provider.set('anthropic');
    expect(c.modelsForProvider().map((m) => m.model_id)).toEqual(['claude-opus-4-8']);
  });
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npm test`
Expected: FAIL — `./agent-form` cannot resolve.

- [ ] **Step 3: Implement the form component**

Create `web/src/app/pages/agents/agent-form.ts`:

```ts
import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, OnInit, Output, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  Agent, AgentsService, CreateAgentBody, McpServerRef, ModelDescriptor, ToolDescriptor, UpdateAgentBody,
} from '../../../core/agents.service';
import { AIProvider } from '../../../core/ai-credentials.service';

const SELF_HOST: AIProvider[] = ['ollama', 'vllm'];

@Component({
  selector: 'app-agent-form',
  imports: [FormsModule],
  template: `
    <form class="mf-card mf-form" (ngSubmit)="submit()" data-testid="agent-form">
      <div class="mf-field">
        <label for="ag-name">Name</label>
        <input id="ag-name" class="mf-input" type="text" data-testid="agent-name" name="name" [(ngModel)]="name" />
      </div>

      <div class="mf-field">
        <label for="ag-provider">Provider</label>
        <select id="ag-provider" class="mf-select" data-testid="agent-provider" name="provider"
                [ngModel]="provider()" (ngModelChange)="onProviderChange($event)" [disabled]="mode === 'edit'">
          <option value="anthropic">Anthropic</option>
          <option value="openai">OpenAI</option>
          <option value="ollama">Ollama (self-host)</option>
          <option value="vllm">vLLM (self-host)</option>
        </select>
        @if (mode === 'edit') { <small class="mf-hint">Provider can't change after creation.</small> }
      </div>

      <div class="mf-field">
        <label for="ag-model">Model</label>
        @if (isSelfHost()) {
          <input id="ag-model" class="mf-input" type="text" data-testid="agent-model-text" name="model"
                 [(ngModel)]="model" placeholder="e.g. llama3.1:70b" />
        } @else {
          <select id="ag-model" class="mf-select" data-testid="agent-model-select" name="model" [(ngModel)]="model">
            <option value="" disabled>Choose a model…</option>
            @for (m of modelsForProvider(); track m.model_id) {
              <option [value]="m.model_id">{{ m.model_id }}</option>
            }
          </select>
        }
      </div>

      <div class="mf-field">
        <label for="ag-prompt">System prompt</label>
        <textarea id="ag-prompt" class="mf-input" rows="4" data-testid="agent-system-prompt" name="system_prompt"
                  [(ngModel)]="systemPrompt"></textarea>
      </div>

      <div class="mf-field">
        <span class="mf-label">Allowed tools</span>
        <div data-testid="agent-tools">
          @for (t of tools(); track t.name) {
            <label class="mf-check">
              <input type="checkbox" [attr.data-testid]="'agent-tool-' + t.name"
                     [checked]="selectedTools().has(t.name)" (change)="toggleTool(t.name)" />
              {{ t.name }} <span class="mf-hint">({{ t.effect }}{{ t.required_perm ? ', needs ' + t.required_perm : '' }})</span>
            </label>
          }
        </div>
      </div>

      <div class="mf-field">
        <label for="ag-autonomy">Autonomy mode</label>
        <select id="ag-autonomy" class="mf-select" data-testid="agent-autonomy" name="autonomy_mode"
                [ngModel]="autonomyMode()" (ngModelChange)="autonomyMode.set(+$event)">
          <option [value]="1">1 — Assist (auto safe writes, queue risky)</option>
          <option [value]="2">2 — Queue all writes</option>
          <option [value]="3">3 — Autonomous</option>
        </select>
      </div>

      <div class="mf-field">
        <label for="ag-budget">Monthly budget (USD)</label>
        <input id="ag-budget" class="mf-input" type="number" min="0" step="1" data-testid="agent-budget"
               name="budget" [(ngModel)]="budgetDollars" />
      </div>

      @if (mcpServers().length > 0) {
        <div class="mf-field">
          <span class="mf-label">MCP servers</span>
          <div data-testid="agent-mcp">
            @for (s of mcpServers(); track s.id) {
              <label class="mf-check">
                <input type="checkbox" [attr.data-testid]="'agent-mcp-' + s.id"
                       [checked]="selectedMcp().has(s.id)" (change)="toggleMcp(s.id)" />
                {{ s.name }}
              </label>
            }
          </div>
        </div>
      }

      <label class="mf-check"><input type="checkbox" data-testid="agent-enabled" name="enabled" [(ngModel)]="enabled" /> Enabled</label>
      <label class="mf-check"><input type="checkbox" data-testid="agent-retriage" name="retriage" [(ngModel)]="retriageOnReply" /> Re-triage when the user replies</label>

      @if (error()) { <p class="mf-err" data-testid="agent-form-error">{{ error() }}</p> }

      <div class="mf-form-actions">
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="cancelled.emit()">Cancel</button>
        <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="agent-form-submit"
                [disabled]="submitting() || !valid()">
          {{ submitting() ? 'Saving…' : (mode === 'create' ? 'Create agent' : 'Save') }}
        </button>
      </div>
    </form>
  `,
})
export class AgentFormComponent implements OnInit {
  @Input() businessId = '';
  @Input() mode: 'create' | 'edit' = 'create';
  @Input() agent: Agent | null = null;
  @Output() saved = new EventEmitter<Agent>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(AgentsService);

  name = '';
  provider = signal<AIProvider>('anthropic');
  model = '';
  systemPrompt = '';
  autonomyMode = signal<number>(1);
  enabled = true;
  retriageOnReply = false;
  budgetDollars = 0;

  tools = signal<ToolDescriptor[]>([]);
  allModels = signal<ModelDescriptor[]>([]);
  mcpServers = signal<McpServerRef[]>([]);
  selectedTools = signal<Set<string>>(new Set());
  selectedMcp = signal<Set<string>>(new Set());

  submitting = signal(false);
  error = signal('');

  modelsForProvider = computed(() => this.allModels().filter((m) => m.provider === this.provider()));
  isSelfHost = computed(() => SELF_HOST.includes(this.provider()));

  ngOnInit(): void {
    this.api.tools(this.businessId).subscribe({ next: (r) => this.tools.set(r.items ?? []), error: () => {} });
    this.api.models(this.businessId).subscribe({ next: (r) => this.allModels.set(r.items ?? []), error: () => {} });
    this.api.mcpServers(this.businessId).subscribe({ next: (r) => this.mcpServers.set(r.items ?? []), error: () => {} });

    if (this.mode === 'edit' && this.agent) {
      const a = this.agent;
      this.name = a.name;
      this.provider.set(a.provider);
      this.model = a.model;
      this.systemPrompt = a.system_prompt;
      this.autonomyMode.set(a.autonomy_mode);
      this.enabled = a.enabled;
      this.retriageOnReply = a.retriage_on_reply;
      this.budgetDollars = Math.round(a.monthly_budget_cents / 100);
      this.selectedTools.set(new Set(a.allowed_tools ?? []));
      this.selectedMcp.set(new Set(a.allowed_mcp_servers ?? []));
    }
  }

  onProviderChange(p: AIProvider): void {
    this.provider.set(p);
    this.model = ''; // reset model when provider changes (catalog list differs)
  }

  toggleTool(name: string): void {
    this.selectedTools.update((s) => {
      const next = new Set(s);
      next.has(name) ? next.delete(name) : next.add(name);
      return next;
    });
  }

  toggleMcp(id: string): void {
    this.selectedMcp.update((s) => {
      const next = new Set(s);
      next.has(id) ? next.delete(id) : next.add(id);
      return next;
    });
  }

  valid(): boolean {
    return this.name.trim().length > 0 && this.model.trim().length > 0;
  }

  submit(): void {
    if (this.submitting() || !this.valid()) return;
    this.submitting.set(true);
    this.error.set('');
    const cents = Math.round((this.budgetDollars || 0) * 100);
    const obs =
      this.mode === 'edit' && this.agent
        ? this.api.update(this.businessId, this.agent.id, this.buildUpdate(cents))
        : this.api.create(this.businessId, this.buildCreate(cents));
    obs.subscribe({
      next: (a) => {
        this.submitting.set(false);
        this.saved.emit(a);
      },
      error: (e: HttpErrorResponse) => {
        this.submitting.set(false);
        this.error.set(this.describe(e));
      },
    });
  }

  private buildCreate(cents: number): CreateAgentBody {
    return {
      name: this.name.trim(),
      provider: this.provider(),
      model: this.model.trim(),
      system_prompt: this.systemPrompt,
      allowed_tools: [...this.selectedTools()],
      autonomy_mode: this.autonomyMode(),
      enabled: this.enabled,
      monthly_budget_cents: cents,
      allowed_mcp_servers: [...this.selectedMcp()],
      retriage_on_reply: this.retriageOnReply,
    };
  }

  // Edit sends the full editable set (provider omitted — it's immutable).
  private buildUpdate(cents: number): UpdateAgentBody {
    return {
      name: this.name.trim(),
      model: this.model.trim(),
      system_prompt: this.systemPrompt,
      allowed_tools: [...this.selectedTools()],
      autonomy_mode: this.autonomyMode(),
      enabled: this.enabled,
      monthly_budget_cents: cents,
      allowed_mcp_servers: [...this.selectedMcp()],
      retriage_on_reply: this.retriageOnReply,
    };
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Rejected. Check the values and try again.';
    }
    if (e.status === 409) return 'An agent with that name already exists.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not save. Please try again.';
  }
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd web && npm test`
Expected: `AgentFormComponent` specs PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/agents/agent-form.ts web/src/app/pages/agents/agent-form.spec.ts
git commit -m "feat(web): agent create/edit form"
```

---

## Task 9: Frontend — agents list component

**Files:**
- Create: `web/src/app/pages/agents/list.ts`
- Test: `web/src/app/pages/agents/list.spec.ts`

**Context (verified):** Mirror `connectors/list.ts` with edit support (an `editId` signal that swaps the row for `<app-agent-form mode="edit">`). Business-select + `CurrentBusinessService` seeding identical to the credentials list (Phase 1 Task 9). No health poll.

- [ ] **Step 1: Write the failing unit spec**

Create `web/src/app/pages/agents/list.spec.ts`:

```ts
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting, HttpTestingController } from '@angular/common/http/testing';
import { AgentsListComponent } from './list';

describe('AgentsListComponent', () => {
  let fixture: ComponentFixture<AgentsListComponent>;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [AgentsListComponent],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    fixture = TestBed.createComponent(AgentsListComponent);
    fixture.detectChanges();
    http = TestBed.inject(HttpTestingController);
  });

  it('loads businesses then lists agents', () => {
    http.expectOne('/api/v1/businesses').flush({
      items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
      next_cursor: null,
    });
    http.expectOne('/api/v1/businesses/b1/agents').flush({
      items: [{
        id: 'a1', business_id: 'b1', principal_id: 'p1', name: 'Triage', provider: 'anthropic', model: 'claude-opus-4-8',
        system_prompt: '', allowed_tools: [], autonomy_mode: 1, enabled: true, monthly_budget_cents: 2500,
        allowed_mcp_servers: [], retriage_on_reply: false, created_at: '', updated_at: '',
      }],
    });
    expect(fixture.componentInstance.items().length).toBe(1);
    expect(fixture.componentInstance.items()[0].name).toBe('Triage');
  });
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npm test`
Expected: FAIL — `./list` cannot resolve.

- [ ] **Step 3: Implement the list component**

Create `web/src/app/pages/agents/list.ts`:

```ts
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Agent, AgentsService } from '../../../core/agents.service';
import { BusinessService } from '../../../core/business.service';
import { CurrentBusinessService } from '../../../core/current-business.service';
import { Business } from '../../../core/tree';
import { EmptyState } from '../../../ui/empty-state/empty-state';
import { PageHeader } from '../../../ui/page-header/page-header';
import { Spinner } from '../../../ui/spinner/spinner';
import { ToastService } from '../../../ui/toast/toast.service';
import { AgentFormComponent } from './agent-form';

const MODE_LABELS: Record<number, string> = { 1: 'Assist', 2: 'Queue writes', 3: 'Autonomous' };

@Component({
  selector: 'app-agents-list',
  imports: [FormsModule, PageHeader, EmptyState, Spinner, AgentFormComponent],
  template: `
    <mf-page-header title="Agents" subtitle="Automated agents that act on your tickets">
      @if (loading()) { <mf-spinner /> }
    </mf-page-header>

    <div class="mf-filters">
      <div class="mf-field" style="flex:1 1 220px">
        <label for="ag-biz-select">Business</label>
        <select id="ag-biz-select" class="mf-select" data-testid="business-select"
                [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)" name="biz">
          <option value="" disabled>Choose a business…</option>
          @for (b of businesses(); track b.id) {
            <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
          }
        </select>
      </div>
      <div style="display:flex;align-items:flex-end">
        <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="agent-add-toggle"
                (click)="showAdd.set(!showAdd())" [disabled]="!businessId()">
          {{ showAdd() ? 'Close' : 'Add agent' }}
        </button>
      </div>
    </div>

    @if (showAdd() && businessId()) {
      <app-agent-form mode="create" [businessId]="businessId()" (saved)="onCreated()" (cancelled)="showAdd.set(false)" />
    }

    @if (error()) { <p class="mf-err" data-testid="agents-error">{{ error() }}</p> }

    @if (!loading() && items().length === 0 && businessId()) {
      <mf-empty-state message="No agents yet. Add one (you'll need an AI credential first)." />
    }

    @if (items().length > 0) {
      <div class="mf-card">
        <table class="mf-table">
          <thead>
            <tr class="mf-tr">
              <th class="mf-th">Name</th><th class="mf-th">Model</th><th class="mf-th">Autonomy</th>
              <th class="mf-th">Enabled</th><th class="mf-th">Budget</th><th class="mf-th"></th>
            </tr>
          </thead>
          <tbody>
            @for (a of items(); track a.id) {
              <tr class="mf-tr" data-testid="agent-row">
                <td data-testid="agent-name-cell">{{ a.name }}</td>
                <td>{{ a.provider }} / {{ a.model }}</td>
                <td>{{ modeLabel(a.autonomy_mode) }}</td>
                <td>{{ a.enabled ? 'yes' : 'no' }}</td>
                <td>\${{ (a.monthly_budget_cents / 100).toFixed(0) }}</td>
                <td style="text-align:right">
                  @if (confirmDeleteId() === a.id) {
                    <span class="mf-err" data-testid="agent-delete-confirm" style="font-size:var(--mf-fs-xs)">Delete {{ a.name }}?</span>
                    <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="agent-delete-no" (click)="confirmDeleteId.set('')">Cancel</button>
                    <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="agent-delete-yes" (click)="remove(a)">Delete</button>
                  } @else {
                    <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="agent-edit" (click)="startEdit(a)">Edit</button>
                    <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="agent-delete" (click)="confirmDeleteId.set(a.id)">Delete</button>
                  }
                </td>
              </tr>
              @if (editId() === a.id) {
                <tr><td colspan="6">
                  <app-agent-form mode="edit" [businessId]="businessId()" [agent]="a"
                                  (saved)="onEdited()" (cancelled)="editId.set('')" />
                </td></tr>
              }
            }
          </tbody>
        </table>
      </div>
    }
  `,
})
export class AgentsListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private api = inject(AgentsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<Agent[]>([]);
  loading = signal(false);
  error = signal('');
  showAdd = signal(false);
  editId = signal<string>('');
  confirmDeleteId = signal<string>('');

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id;
        if (id) {
          this.businessId.set(id);
          this.current.set(id);
          this.reload();
        }
      },
      error: () => this.error.set('Could not load businesses'),
    });
  }

  modeLabel(m: number): string {
    return MODE_LABELS[m] ?? String(m);
  }

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.confirmDeleteId.set('');
    this.editId.set('');
    this.showAdd.set(false);
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.loading.set(true);
    this.api.list(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return;
        this.items.set(r.items ?? []);
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        if (this.businessId() !== biz) return;
        this.items.set([]);
        this.error.set('Could not load agents');
        this.loading.set(false);
      },
    });
  }

  startEdit(a: Agent): void {
    this.editId.set(a.id);
    this.showAdd.set(false);
    this.confirmDeleteId.set('');
  }

  onCreated(): void {
    this.showAdd.set(false);
    this.toast.success('Agent created');
    this.reload();
  }

  onEdited(): void {
    this.editId.set('');
    this.toast.success('Agent updated');
    this.reload();
  }

  remove(a: Agent): void {
    this.api.remove(this.businessId(), a.id).subscribe({
      next: () => {
        this.items.update((xs) => xs.filter((x) => x.id !== a.id));
        this.confirmDeleteId.set('');
        this.toast.success('Agent deleted');
      },
      error: (e: HttpErrorResponse) => {
        this.confirmDeleteId.set('');
        this.toast.error(e.status === 404 ? 'Not found' : 'Delete failed');
      },
    });
  }
}
```

> Confirm the shared UI selectors / `ToastService` method names against the real files (same note as Phase 1 Task 9).

- [ ] **Step 4: Run to verify pass**

Run: `cd web && npm test`
Expected: `AgentsListComponent` spec PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/agents/list.ts web/src/app/pages/agents/list.spec.ts
git commit -m "feat(web): agents list page with inline edit"
```

---

## Task 10: Route + nav for `/agents`

**Files:**
- Modify: `web/src/app/app.routes.ts`
- Modify: `web/src/app/ui/nav.ts`

- [ ] **Step 1: Add the route**

In `web/src/app/app.routes.ts`, add:

```ts
  {
    path: 'agents',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/agents/list').then((m) => m.AgentsListComponent),
  },
```

- [ ] **Step 2: Add the nav entry**

In `web/src/app/ui/nav.ts`, add (place near the other admin links):

```ts
  { label: 'Agents', route: '/agents', testid: 'nav-agents' },
```

- [ ] **Step 3: Build + unit test**

Run: `cd web && npm run build && npm test`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add web/src/app/app.routes.ts web/src/app/ui/nav.ts
git commit -m "feat(web): /agents route + nav entry"
```

---

## Task 11: Frontend e2e for agents

**Files:**
- Create: `web/e2e/agents.spec.ts`

**Context (verified):** Mirror `connectors.spec.ts`. Mock `/agents`, `/agents/tools`, `/agents/models`, `/mcp_servers`. The create test asserts the POST body (tools selected, budget→cents). Include the **403 → "no access"** path by fulfilling the list with status 404 and asserting the page shows the no-access/error message.

- [ ] **Step 1: Write the e2e spec**

Create `web/e2e/agents.spec.ts`:

```ts
import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };
const biz = {
  items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
  next_cursor: null,
};
const agent = {
  id: 'a1', business_id: 'b1', principal_id: 'p1', name: 'Triage', provider: 'anthropic', model: 'claude-opus-4-8',
  system_prompt: '', allowed_tools: ['read_ticket'], autonomy_mode: 1, enabled: true, monthly_budget_cents: 2500,
  allowed_mcp_servers: [], retriage_on_reply: false, created_at: '2026-06-15T00:00:00Z', updated_at: '2026-06-15T00:00:00Z',
};

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

async function metadata(page: import('@playwright/test').Page) {
  await page.route('**/api/v1/businesses/b1/agents/tools', (r) =>
    r.fulfill({ json: { items: [{ name: 'read_ticket', description: 'read', effect: 'read', required_perm: 'tickets.read' }] } }));
  await page.route('**/api/v1/businesses/b1/agents/models', (r) =>
    r.fulfill({ json: { items: [{ provider: 'anthropic', model_id: 'claude-opus-4-8' }] } }));
  await page.route('**/api/v1/businesses/b1/mcp_servers', (r) => r.fulfill({ json: { items: [] } }));
}

test('agents: lists agents', async ({ page }) => {
  await auth(page);
  await metadata(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) => r.fulfill({ json: { items: [agent] } }));
  await page.goto('/agents');
  await expect(page.getByTestId('agent-name-cell')).toContainText('Triage');
});

test('agents: create an agent (tools + budget→cents)', async ({ page }) => {
  await auth(page);
  await metadata(page);
  let created = false;
  let body: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/agents', (r) => {
    if (r.request().method() === 'POST') {
      created = true;
      body = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({ status: 201, json: agent });
    }
    return r.fulfill({ json: { items: created ? [agent] : [] } });
  });
  await page.goto('/agents');
  await page.getByTestId('agent-add-toggle').click();
  await page.getByTestId('agent-name').fill('Triage');
  await page.getByTestId('agent-model-select').selectOption('claude-opus-4-8');
  await page.getByTestId('agent-tool-read_ticket').check();
  await page.getByTestId('agent-budget').fill('25');
  await page.getByTestId('agent-form-submit').click();
  await expect(page.getByTestId('agent-name-cell')).toContainText('Triage');
  expect(body).not.toBeNull();
  expect(body!['allowed_tools']).toEqual(['read_ticket']);
  expect(body!['monthly_budget_cents']).toBe(2500);
});

test('agents: edit an agent', async ({ page }) => {
  await auth(page);
  await metadata(page);
  let patched = false;
  await page.route('**/api/v1/businesses/b1/agents', (r) =>
    r.fulfill({ json: { items: [patched ? { ...agent, name: 'Triage 2' } : agent] } }));
  await page.route('**/api/v1/businesses/b1/agents/a1', (r) => {
    if (r.request().method() === 'PATCH') {
      patched = true;
      return r.fulfill({ json: { ...agent, name: 'Triage 2' } });
    }
    return r.fulfill({ json: agent });
  });
  await page.goto('/agents');
  await page.getByTestId('agent-edit').click();
  await page.getByTestId('agent-name').fill('Triage 2');
  await page.getByTestId('agent-form-submit').click();
  await expect(page.getByTestId('agent-name-cell')).toContainText('Triage 2');
});

test('agents: delete with confirm', async ({ page }) => {
  await auth(page);
  await metadata(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) => r.fulfill({ json: { items: [agent] } }));
  await page.route('**/api/v1/businesses/b1/agents/a1', (r) =>
    r.request().method() === 'DELETE' ? r.fulfill({ status: 204, body: '' }) : r.fulfill({ json: agent }));
  await page.goto('/agents');
  await page.getByTestId('agent-delete').click();
  await expect(page.getByTestId('agent-delete-confirm')).toContainText('Delete Triage');
  await page.getByTestId('agent-delete-yes').click();
  await expect(page.getByTestId('agent-row')).toHaveCount(0);
});

test('agents: no access shows an error', async ({ page }) => {
  await auth(page);
  await metadata(page);
  await page.route('**/api/v1/businesses/b1/agents', (r) =>
    r.fulfill({ status: 404, json: { code: 'NOT_FOUND', message: 'not found' } }));
  await page.goto('/agents');
  await expect(page.getByTestId('agents-error')).toContainText('Could not load agents');
});
```

- [ ] **Step 2: Run the e2e**

Run (dev server :4300 up): `cd web && npm run e2e -- e2e/agents.spec.ts`
Expected: all PASS.

- [ ] **Step 3: Commit**

```bash
git add web/e2e/agents.spec.ts
git commit -m "test(web): e2e for agents page"
```

---

## Task 12: Phase 2 verification & PR

- [ ] **Step 1: Full backend gate**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && make test && make sec-test && make lint && go test -tags integration ./internal/agents/ -run 'TestModelCatalog'`
Expected: all exit 0 / PASS.

- [ ] **Step 2: Full frontend gate**

Run: `cd web && npm run build && npm test && npm run e2e -- e2e/agents.spec.ts`
Expected: all PASS.

- [ ] **Step 3: Manual smoke (real stack)**

Backend on :8081 (with `MANYFORGE_AI_MASTER_KEY` set so a credential can exist), web on :4300, `mf-dev` DB. Log in (`live-demo@manyforge.test` / `DevPassw0rd!`), ensure a credential exists at `/credentials/ai`, then at `/agents`: create an agent (tools/models/MCP pickers populated), edit it, delete it.

- [ ] **Step 4: Open the PR into master**

```bash
git push -u origin <branch>
gh pr create --base master --title "Agent Management UI (agent-management Phase 2)" --body "Implements Phase 2 of docs/superpowers/specs/2026-06-15-agent-management-ui-design.md (bd manyforge-1kv). Adds /agents/tools + /agents/models metadata endpoints and the /agents CRUD page."
```

- [ ] **Step 5: Close bd**

Run: `export PATH="$HOME/go/bin:$PATH" && bd close manyforge-1kv` then a `chore(bd): close manyforge-1kv (agent-management + provider-credentials UI)` commit.

---

## Self-Review (completed by plan author)

- **Spec coverage (Phase 2):** agent CRUD UI ✓ (Tasks 8,9); `/agents/tools` + `/agents/models` metadata endpoints, registry-sourced so pickers don't drift ✓ (Tasks 1–4); chi static-vs-`/{agentID}` collision avoided + explicitly tested ✓ (Task 3); provider immutable on edit ✓ (Task 8, disabled select + omitted from patch); model free-text fallback for ollama/vllm ✓ (Task 8); autonomy/budget(dollars↔cents)/enabled/MCP/retriage fields ✓ (Task 8); nav + route ✓ (Task 10); sec pin for gating ✓ (Task 5); test plan (handler unit incl. precedence, catalog integration, sec pin, FE unit, e2e incl. 403 path) ✓.
- **Type consistency:** `ToolDescriptor.effect` enum (TS, Task 7) matches `EffectClass.String()` outputs (Task 1) and `toolResp.effect` (Task 3). `ModelDescriptor{provider,model_id}` (Task 7) matches `modelResp` json tags (Task 3) and `ModelInfo` (Task 2). `Agent`/`CreateAgentBody`/`UpdateAgentBody` (Task 7) match `agentResp` + the create handler's inline decode struct (verified from the real handler). `SetMetadata(toolLister, modelLister)` (Task 3) matches the seams in `metadata.go` (Task 2) and the fakes in tests (Task 3).
- **Placeholder scan:** deferred specifics are all "open the named existing file and reuse its helper" (testdb constructors Task 2; `stubAgentCRUD`/`injectPrincipal` Task 3; `modelPricingDB` confirm Task 2; shared-UI selectors Task 9; existing core MCP service Task 7) — each points to a concrete file. No "TBD"/"add validation"/"similar to" placeholders.
- **Sequencing:** Backend Tasks 1→4 are prerequisites for the FE form's pickers; FE Tasks 7→10 then e2e (11). Each task ends green and commits independently.
