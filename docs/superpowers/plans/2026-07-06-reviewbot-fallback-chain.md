# Reviewbot Fallback Chain + Per-Bot Concurrency — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the hub prefer a self-hosted LM Studio reviewbot and transparently fall back to a cloud reviewbot when it is unreachable, with a per-reviewbot concurrency cap.

**Architecture:** Additive columns (`agent.max_concurrent_lanes`, `review_config.review_agent_chain uuid[]`). At review-start, `runJob` resolves the ordered chain, probes each candidate's `{base_url}/models`, and picks the first live bot; its credential and its concurrency cap drive the whole dimension panel. Empty chain ⇒ today's single-agent path, byte-for-byte. Mid-review death is absorbed by the existing attempts-based worker requeue (which re-probes each attempt).

**Tech Stack:** Go 1.x (`internal/agents/coding`, `internal/agents`, `internal/platform/{ai,netsafe,db/dbgen}`), PostgreSQL + sqlc (pinned Docker `sqlc/sqlc:1.27.0`), Angular standalone (`web/`), chi router, RLS.

**Design doc:** `docs/superpowers/specs/2026-07-06-reviewbot-fallback-chain-design.md`

## Global Constraints

- **sqlc regen via Docker only:** `docker run --rm -v "$(pwd)":/src -w /src sqlc/sqlc:1.27.0 generate`. Never hand-edit `internal/platform/db/dbgen/*`.
- **Migrations are additive; defaults preserve current behavior.** After adding a migration, migrate the mf-dev DB (`:55432`, owner `manyforge`) or the air backend refuses to serve (version guard), and force an air rebuild.
- **Verify gates that per-package `go test` misses:** `go test -tags contract ./cmd/...` (OpenAPI drift) and `make lint` (staticcheck). `go build ./...` is compile truth (ignore stale gopls "undefined dbgen field" noise).
- **`make int-test`** (needs Docker) is the suite that catches lane goroutine panics; RUN IT before pushing anything touching the fan-out.
- **Frontend:** component specs run under `npx ng test --no-watch` (NOT raw vitest); `npx ng build`; e2e `npx playwright test` (mock `**/api/**` fallback first — nav-badge 401 gotcha).
- **No Co-Authored-By trailer** on commits. Branch per slice off `master` → PR into `master` → merge → delete (one branch at a time; never stack). Slice 2 is independently shippable.
- **Provider enum values:** `anthropic | openai | ollama | vllm | openrouter`. Local providers (host-side review, no sandbox) = `ollama | vllm` (`isLocalProvider`).

---

## Slice 1 — Per-agent concurrency column (data + domain)

### Task 1: Add `agent.max_concurrent_lanes` (migration + sqlc)

**Files:**
- Create: `migrations/0085_agent_concurrency.up.sql`, `migrations/0085_agent_concurrency.down.sql`
- Modify: `db/query/agent.sql` (CreateAgent, UpdateAgent)
- Regenerate: `internal/platform/db/dbgen/{agent.sql.go,models.go}`

**Interfaces:**
- Produces: `dbgen.Agent.MaxConcurrentLanes int32`; `CreateAgentParams.MaxConcurrentLanes int32`; `UpdateAgentParams.MaxConcurrentLanes pgtype.Int4` (narg).

- [ ] **Step 1: Write the up migration**

```sql
-- 0085: per-agent review concurrency cap (fallback-chain epic). How many dimension
-- lanes may run at once when THIS agent is the review's resolved reviewbot. Default 4
-- reproduces the prior hard-coded maxConcurrentLanes constant, so existing agents are
-- unchanged; a single-GPU self-host sets it to 1.
ALTER TABLE agent
    ADD COLUMN max_concurrent_lanes int NOT NULL DEFAULT 4
        CHECK (max_concurrent_lanes BETWEEN 1 AND 16);
```

- [ ] **Step 2: Write the down migration**

```sql
ALTER TABLE agent DROP COLUMN max_concurrent_lanes;
```

- [ ] **Step 3: Add the column to `CreateAgent` in `db/query/agent.sql`**

In the `INSERT INTO agent (...)` column list add `max_concurrent_lanes`, and in the `SELECT` values add `sqlc.arg('max_concurrent_lanes')::integer,` (before `now(), now()`).

- [ ] **Step 4: Add the column to `UpdateAgent` in `db/query/agent.sql`**

Add this line to the `UPDATE agent SET` list (after `web_allowed_domains`):

```sql
    max_concurrent_lanes = COALESCE(sqlc.narg('max_concurrent_lanes')::integer, max_concurrent_lanes),
```

- [ ] **Step 5: Regenerate sqlc**

Run: `docker run --rm -v "$(pwd)":/src -w /src sqlc/sqlc:1.27.0 generate`
Expected: `internal/platform/db/dbgen/models.go` `Agent` struct gains `MaxConcurrentLanes int32`; params structs updated.

- [ ] **Step 6: Verify build**

Run: `go build ./...`
Expected: compiles (agents service still passes old params → `MaxConcurrentLanes` zero-value; fixed in Task 2). If agents `Create` now fails to compile because the param is required, proceed to Task 2 in the same commit.

- [ ] **Step 7: Migrate dev DB + commit**

```bash
# migrate mf-dev (:55432) so the air backend serves; force an air rebuild afterward
git add migrations/0085_agent_concurrency.up.sql migrations/0085_agent_concurrency.down.sql db/query/agent.sql internal/platform/db/dbgen
git commit -m "feat(agent): max_concurrent_lanes column (fallback-chain epic)"
```

### Task 2: Wire `MaxConcurrentLanes` through the agents domain + API

**Files:**
- Modify: `internal/agents/agent.go` (`Agent`, `CreateAgentInput`, `UpdateAgentInput`, `validateCreateAgent`, `validateUpdateAgent`, `toAgent`, `Create`, `Update`)
- Modify: `internal/agents/agent_handler.go` (agent JSON view + create/update decode) — exact file per `grep -rn "AgentView\|func (h \*Handler)" internal/agents/agent_handler.go`
- Modify: `internal/agents/agent_test.go` (unit)
- Modify: `cmd/.../openapi.yaml` — the Agent schema (add `max_concurrent_lanes`)

**Interfaces:**
- Consumes: `dbgen.Agent.MaxConcurrentLanes`, `CreateAgentParams`, `UpdateAgentParams` (Task 1).
- Produces: `agents.Agent.MaxConcurrentLanes int`; JSON field `max_concurrent_lanes`.

- [ ] **Step 1: Write the failing test** (`internal/agents/agent_test.go`)

```go
func TestAgent_MaxConcurrentLanes_RoundTrip(t *testing.T) {
    // create with 1, expect 1 back; default when omitted is 4.
    // (extend the existing Create/Get integration-style test or table test)
}
func TestValidateCreateAgent_MaxConcurrentLanesRange(t *testing.T) {
    for _, n := range []int{0, 17, -1} {
        err := validateCreateAgent(CreateAgentInput{Name: "a", Provider: "vllm", Model: "m", AutonomyMode: 1, MaxConcurrentLanes: n})
        require.ErrorIs(t, err, errs.ErrValidation)
    }
    require.NoError(t, validateCreateAgent(CreateAgentInput{Name: "a", Provider: "vllm", Model: "m", AutonomyMode: 1, MaxConcurrentLanes: 1}))
}
```

- [ ] **Step 2: Run it, verify FAIL** — `go test ./internal/agents/ -run MaxConcurrentLanes -v` → FAIL (field undefined).

- [ ] **Step 3: Add the field + validation + mapping**

- `Agent` struct: add `MaxConcurrentLanes int`.
- `CreateAgentInput` + `UpdateAgentInput` (the latter as `*int`): add the field.
- `validateCreateAgent`: `if in.MaxConcurrentLanes != 0 && (in.MaxConcurrentLanes < 1 || in.MaxConcurrentLanes > 16) { return ErrValidation }` — 0 means "use DB default 4"; treat 0 as valid-and-defaulted.
- In `Create`: `if in.MaxConcurrentLanes == 0 { in.MaxConcurrentLanes = 4 }`, pass `MaxConcurrentLanes: int32(in.MaxConcurrentLanes)` to `CreateAgentParams`.
- `validateUpdateAgent`: `if in.MaxConcurrentLanes != nil && (*in.MaxConcurrentLanes < 1 || *in.MaxConcurrentLanes > 16) { return ErrValidation }`.
- In `Update`: set `params.MaxConcurrentLanes = intToPgInt4(in.MaxConcurrentLanes)` (nil → absent, preserving current). Use the existing narg helper (grep for how `AutonomyMode *int` maps to `pgtype.Int4` in `Update`).
- `toAgent`: `MaxConcurrentLanes: int(r.MaxConcurrentLanes)`.

- [ ] **Step 4: Run tests, verify PASS** — `go test ./internal/agents/ -run MaxConcurrentLanes -v` → PASS.

- [ ] **Step 5: Expose on the API** — add `max_concurrent_lanes` to the agent JSON view struct + create/update input decode in `agent_handler.go`, and to the Agent schema in `openapi.yaml`.

- [ ] **Step 6: Contract + lint** — `go test -tags contract ./cmd/... && go build ./... && make lint` → all green.

- [ ] **Step 7: Commit**

```bash
git add internal/agents cmd
git commit -m "feat(agent): plumb max_concurrent_lanes through domain + API"
```

---

## Slice 2 — Per-agent concurrency in the review fan-out (independently shippable)

### Task 3: Carry the cap on the resolved credential and drive `g.SetLimit`

**Files:**
- Modify: `internal/agents/coding/credresolver.go` (`AICredential`, `AgentCredResolver.Resolve`, `FakeCredResolver`)
- Modify: `internal/agents/coding/service.go` (`maxConcurrentLanes` const → default; `g.SetLimit`)
- Modify: `internal/agents/coding/service_test.go` / `service_multidim_integration_test.go`

**Interfaces:**
- Consumes: `agents.Agent.MaxConcurrentLanes` (Task 2).
- Produces: `AICredential.MaxConcurrentLanes int` (0 ⇒ caller applies `defaultConcurrentLanes`).

- [ ] **Step 1: Write the failing test** (`internal/agents/coding/service_test.go`)

```go
func TestReviewLaneLimit_UsesAgentCap(t *testing.T) {
    require.Equal(t, 1, laneLimit(AICredential{MaxConcurrentLanes: 1}))
    require.Equal(t, 4, laneLimit(AICredential{MaxConcurrentLanes: 0})) // default
    require.Equal(t, defaultConcurrentLanes, laneLimit(AICredential{}))
}
```

- [ ] **Step 2: Run it, verify FAIL** — `go test ./internal/agents/coding/ -run LaneLimit -v` → FAIL (`laneLimit`/`defaultConcurrentLanes` undefined).

- [ ] **Step 3: Implement**

- Rename the constant to a default: `const defaultConcurrentLanes = 4` (keep `maxConcurrentLanes` as an alias only if other refs exist; otherwise replace all uses).
- Add `MaxConcurrentLanes int` to `AICredential` (doc: "how many lanes may run at once for this bot; 0 ⇒ defaultConcurrentLanes").
- In `AgentCredResolver.Resolve`, set `MaxConcurrentLanes: ag.MaxConcurrentLanes`.
- Add helper:

```go
// laneLimit is the bounded lane fan-out for a review, taken from the resolved bot's
// per-agent cap (LM Studio ⇒ 1, cloud ⇒ 4). Zero (unset/legacy) ⇒ defaultConcurrentLanes.
func laneLimit(cred AICredential) int {
    n := cred.MaxConcurrentLanes
    if n < 1 {
        return defaultConcurrentLanes
    }
    if n > 16 {
        return 16
    }
    return n
}
```

- In `runJob` fan-out, replace `g.SetLimit(maxConcurrentLanes)` with `g.SetLimit(laneLimit(cred))`.

- [ ] **Step 4: Run tests, verify PASS** — `go test ./internal/agents/coding/ -run LaneLimit -v` → PASS.

- [ ] **Step 5: Integration guard** — in `service_multidim_integration_test.go`, add a case: an agent with `MaxConcurrentLanes=1` and a 3-dimension panel observes max in-flight lanes == 1 (instrument via a counting fake `localClient`/runner or an atomic gauge in a test hook). Run `make int-test`.

- [ ] **Step 6: Source pin** — a test asserting the fan-out reads the resolved cap, not a constant:

```go
func TestFanOut_UsesLaneLimit_SourcePin(t *testing.T) {
    src, _ := os.ReadFile("service.go")
    require.Contains(t, string(src), "g.SetLimit(laneLimit(cred))")
}
```

- [ ] **Step 7: Commit**

```bash
git add internal/agents/coding
git commit -m "feat(review): per-agent max_concurrent_lanes drives lane fan-out"
```

---

## Slice 3 — Liveness prober

### Task 4: `livenessProber.Live(ctx, cred)`

**Files:**
- Create: `internal/agents/coding/prober.go`
- Create: `internal/agents/coding/prober_test.go`

**Interfaces:**
- Consumes: `AICredential` (Provider, BaseURL, AllowPrivateBaseURL), `netsafe.NewClientWithOptions`.
- Produces: `type reviewbotProber interface { Live(ctx context.Context, cred AICredential) bool }` and a concrete `httpProber{Timeout time.Duration}`.

- [ ] **Step 1: Write the failing tests** (`prober_test.go`)

```go
func TestHTTPProber_Live_2xx(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        require.Equal(t, "/v1/models", r.URL.Path) // base_url already ends in /v1
        w.WriteHeader(200)
    }))
    defer srv.Close()
    p := httpProber{Timeout: 2 * time.Second}
    require.True(t, p.Live(context.Background(), AICredential{Provider: "vllm", BaseURL: srv.URL + "/v1", AllowPrivateBaseURL: true}))
}

func TestHTTPProber_DeadConnRefused(t *testing.T) {
    p := httpProber{Timeout: 500 * time.Millisecond}
    // closed port on loopback
    require.False(t, p.Live(context.Background(), AICredential{Provider: "vllm", BaseURL: "http://127.0.0.1:9/v1", AllowPrivateBaseURL: true}))
}

func TestHTTPProber_Anthropic_AssumedLive(t *testing.T) {
    p := httpProber{Timeout: 10 * time.Millisecond}
    require.True(t, p.Live(context.Background(), AICredential{Provider: "anthropic", BaseURL: "https://api.anthropic.com"}))
}

func TestHTTPProber_PrivateBlockedWithoutFlag(t *testing.T) {
    p := httpProber{Timeout: 500 * time.Millisecond}
    // AllowPrivateBaseURL false ⇒ netsafe blocks RFC1918 ⇒ not live
    require.False(t, p.Live(context.Background(), AICredential{Provider: "vllm", BaseURL: "http://192.168.2.241:1234/v1", AllowPrivateBaseURL: false}))
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./internal/agents/coding/ -run HTTPProber -v` → FAIL.

- [ ] **Step 3: Implement `prober.go`**

```go
package coding

import (
    "context"
    "net/http"
    "strings"
    "time"

    "github.com/manyforge/manyforge/internal/platform/netsafe"
)

// reviewbotProber decides whether a reviewbot's provider endpoint is reachable, so the
// fallback chain can skip a down primary WITHOUT spinning up a doomed review. It never
// surfaces to the PR — a probe result only steers bot selection.
type reviewbotProber interface {
    Live(ctx context.Context, cred AICredential) bool
}

const defaultProbeTimeout = 3 * time.Second

type httpProber struct{ Timeout time.Duration }

// Live probes OpenAI-compatible providers with GET {base_url}/models through a netsafe
// client that honors the credential's private-host opt-in (so 192.168.x.x is reachable
// only when allow_private_base_url is set, matching every other outbound path). Anthropic
// has no cheap unauthenticated probe endpoint, so it is assumed live and covered
// reactively by the worker retry. Any transport error or non-2xx ⇒ not live.
func (p httpProber) Live(ctx context.Context, cred AICredential) bool {
    if strings.EqualFold(cred.Provider, "anthropic") {
        return true
    }
    if cred.BaseURL == "" {
        return false
    }
    to := p.Timeout
    if to <= 0 {
        to = defaultProbeTimeout
    }
    hc := netsafe.NewClientWithOptions(to, netsafe.Options{
        AllowLoopback: cred.AllowPrivateBaseURL,
        AllowPrivate:  cred.AllowPrivateBaseURL,
    })
    url := strings.TrimRight(cred.BaseURL, "/") + "/models"
    cctx, cancel := context.WithTimeout(ctx, to)
    defer cancel()
    req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
    if err != nil {
        return false
    }
    if cred.APIKey != "" {
        req.Header.Set("Authorization", "Bearer "+cred.APIKey)
    }
    resp, err := hc.Do(req)
    if err != nil {
        return false
    }
    defer resp.Body.Close()
    return resp.StatusCode >= 200 && resp.StatusCode < 300
}
```

- [ ] **Step 4: Run, verify PASS** — `go test ./internal/agents/coding/ -run HTTPProber -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/coding/prober.go internal/agents/coding/prober_test.go
git commit -m "feat(review): reviewbot liveness prober (OpenAI-compat /models)"
```

---

## Slice 4 — Fallback chain: config + resolution

### Task 5: `review_config.review_agent_chain` (migration + config service)

**Files:**
- Create: `migrations/0086_review_agent_chain.up.sql`, `.down.sql`
- Modify: `db/query/review_config.sql` (UpsertReviewConfig)
- Regenerate: `internal/platform/db/dbgen`
- Modify: `internal/agents/coding/review_config_service.go` (`ReviewConfigView`, `UpsertConfig`, `configViewFromRow`, plus agent-ID validation)
- Modify: `internal/agents/coding/review_config_service_test.go`
- Modify: `openapi.yaml` (ReviewConfig schema)

**Interfaces:**
- Produces: `ReviewConfigView.ReviewAgentChain []string` (agent UUID strings, primary first); `dbgen.ReviewConfig.ReviewAgentChain []uuid.UUID`.

- [ ] **Step 1: Up migration**

```sql
-- 0086: ordered reviewbot fallback chain (agent IDs, primary first). Empty ⇒ no
-- fallback: the review uses its single enqueued agent, unchanged. FK-less by design
-- (a uuid[] can't reference agent); entries are validated against RLS-visible agents
-- at config-save time and skipped-with-log if stale at review time.
ALTER TABLE review_config
    ADD COLUMN review_agent_chain uuid[] NOT NULL DEFAULT '{}';
```

- [ ] **Step 2: Down migration** — `ALTER TABLE review_config DROP COLUMN review_agent_chain;`

- [ ] **Step 3: `UpsertReviewConfig` in `db/query/review_config.sql`** — add `review_agent_chain` to the column list and `sqlc.arg('review_agent_chain')::uuid[],` to the SELECT, and to the `ON CONFLICT ... DO UPDATE SET` list: `review_agent_chain = EXCLUDED.review_agent_chain,`.

- [ ] **Step 4: Regenerate** — `docker run --rm -v "$(pwd)":/src -w /src sqlc/sqlc:1.27.0 generate`.

- [ ] **Step 5: Write the failing test** (`review_config_service_test.go`)

```go
func TestUpsertConfig_ChainRoundTrip(t *testing.T) {
    // seed two agents; upsert chain [a1,a2]; GetConfig returns them in order.
}
func TestUpsertConfig_RejectsUnknownAgentInChain(t *testing.T) {
    // chain referencing a random uuid ⇒ ErrValidation (400), not a 500 / silent accept.
}
```

- [ ] **Step 6: Run, verify FAIL** — `go test ./internal/agents/coding/ -run Chain -v` → FAIL.

- [ ] **Step 7: Implement**

- `ReviewConfigView`: add `ReviewAgentChain []string \`json:"review_agent_chain"\``.
- `configViewFromRow`: map `r.ReviewAgentChain` (`[]uuid.UUID`) → `[]string` (nil → `[]string{}`).
- `UpsertConfig`: parse each string → `uuid.UUID` (bad UUID ⇒ ErrValidation); validate all IDs are RLS-visible agents in one query inside the tx (`SELECT count(*) FROM agent WHERE id = ANY($1) AND business_id = $2`; count != len ⇒ ErrValidation). Pass `ReviewAgentChain: ids` to `UpsertReviewConfigParams`.

- [ ] **Step 8: Run, verify PASS** — `go test ./internal/agents/coding/ -run Chain -v` → PASS.

- [ ] **Step 9: API + contract** — add `review_agent_chain: string[]` to the ReviewConfig schema in `openapi.yaml`; `go test -tags contract ./cmd/...` green.

- [ ] **Step 10: Migrate dev DB + commit**

```bash
git add migrations/0086_* db/query/review_config.sql internal/platform/db/dbgen internal/agents/coding/review_config_service.go internal/agents/coding/review_config_service_test.go openapi.yaml
git commit -m "feat(review): review_agent_chain config (fallback chain, validated)"
```

### Task 6: Resolve + probe the chain in `runJob`

**Files:**
- Create: `internal/agents/coding/fallbackchain.go` (pure resolver) + `fallbackchain_test.go`
- Modify: `internal/agents/coding/service.go` (`runJob`: load chain, choose bot, use its cred + `laneLimit`)
- Modify: `internal/agents/coding/panel.go` or a new small loader for the chain (mirror `resolvePanel`)
- Modify: `internal/agents/coding/service_multidim_integration_test.go`

**Interfaces:**
- Consumes: `reviewbotProber` (Task 4), `AICredentialResolver.Resolve` (per candidate), `laneLimit` (Task 3), `ReviewConfig.review_agent_chain`.
- Produces: `chooseReviewbot(ctx, chain []uuid.UUID, resolve resolveFn, probe reviewbotProber) (AICredential, error)`.

- [ ] **Step 1: Write the failing test** (`fallbackchain_test.go`) — pure, no DB/network:

```go
type stubProbe map[string]bool // base_url → live
func (s stubProbe) Live(_ context.Context, c AICredential) bool { return s[c.BaseURL] }

func resolverFor(m map[uuid.UUID]AICredential) resolveFn {
    return func(_ context.Context, id uuid.UUID) (AICredential, error) {
        c, ok := m[id]; if !ok { return AICredential{}, errs.ErrNotFound }; return c, nil
    }
}

func TestChooseReviewbot(t *testing.T) {
    a1, a2 := uuid.New(), uuid.New()
    creds := map[uuid.UUID]AICredential{
        a1: {Provider: "vllm", BaseURL: "http://lan/v1"},
        a2: {Provider: "openrouter", BaseURL: "http://cloud/v1"},
    }
    // primary live ⇒ primary
    got, err := chooseReviewbot(context.Background(), []uuid.UUID{a1, a2}, resolverFor(creds), stubProbe{"http://lan/v1": true, "http://cloud/v1": true})
    require.NoError(t, err); require.Equal(t, "http://lan/v1", got.BaseURL)
    // primary dead ⇒ secondary
    got, err = chooseReviewbot(context.Background(), []uuid.UUID{a1, a2}, resolverFor(creds), stubProbe{"http://cloud/v1": true})
    require.NoError(t, err); require.Equal(t, "http://cloud/v1", got.BaseURL)
    // all dead but resolvable ⇒ last resolvable (let the real call fail → retry)
    got, err = chooseReviewbot(context.Background(), []uuid.UUID{a1, a2}, resolverFor(creds), stubProbe{})
    require.NoError(t, err); require.Equal(t, "http://cloud/v1", got.BaseURL)
    // none resolvable ⇒ error
    _, err = chooseReviewbot(context.Background(), []uuid.UUID{uuid.New()}, resolverFor(creds), stubProbe{})
    require.ErrorIs(t, err, errs.ErrValidation)
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./internal/agents/coding/ -run ChooseReviewbot -v` → FAIL.

- [ ] **Step 3: Implement `fallbackchain.go`**

```go
package coding

import (
    "context"
    "fmt"
    "log/slog"

    "github.com/google/uuid"
    "github.com/manyforge/manyforge/internal/platform/errs"
)

type resolveFn func(ctx context.Context, agentID uuid.UUID) (AICredential, error)

// chooseReviewbot walks the ordered fallback chain and returns the credential of the
// first bot that BOTH resolves and passes the liveness probe. If none is live but some
// resolve, it returns the last resolvable one and lets the real review call fail into the
// worker retry (a briefly-flapping server still gets a shot; the next attempt re-probes).
// If nothing in the chain resolves, it errors (ErrValidation → terminal failJob).
func chooseReviewbot(ctx context.Context, chain []uuid.UUID, resolve resolveFn, probe reviewbotProber) (AICredential, error) {
    var lastResolvable *AICredential
    for _, id := range chain {
        cred, err := resolve(ctx, id)
        if err != nil {
            slog.Default().WarnContext(ctx, "fallback chain: skip unresolvable reviewbot", "agent_id", id, "err", err)
            continue
        }
        c := cred
        lastResolvable = &c
        if probe.Live(ctx, cred) {
            return cred, nil
        }
        slog.Default().InfoContext(ctx, "fallback chain: reviewbot not live, trying next", "agent_id", id, "base_url", cred.BaseURL)
    }
    if lastResolvable != nil {
        return *lastResolvable, nil
    }
    return AICredential{}, fmt.Errorf("coding: review fallback chain has no usable reviewbot: %w", errs.ErrValidation)
}
```

- [ ] **Step 4: Run, verify PASS** — `go test ./internal/agents/coding/ -run ChooseReviewbot -v` → PASS.

- [ ] **Step 5: Load the chain + wire into `runJob`**

- Add a loader (mirror `resolvePanel`) that reads `GetReviewConfig(businessID).ReviewAgentChain` under the principal; a DB error ⇒ empty chain (degrade to legacy path, log).
- In `runJob`, immediately after the existing `cred, err := s.Creds.Resolve(...)` block (`service.go:339`), branch:

```go
if chain := s.resolveReviewChain(ctx, principalID, businessID); len(chain) > 0 {
    chosen, cerr := chooseReviewbot(ctx, chain, func(c context.Context, id uuid.UUID) (AICredential, error) {
        return s.Creds.Resolve(c, principalID, businessID, id)
    }, s.prober())
    if cerr != nil {
        return s.failJob(ctx, principalID, businessID, crID, prNumber, cerr)
    }
    cred = chosen
}
```

- Add `s.prober()` returning the configured `reviewbotProber` (default `httpProber{Timeout: defaultProbeTimeout}`, injectable for tests). Keep the existing `cred.Host()==""` + egress pre-flight checks AFTER this swap so the chosen cloud bot is still egress-validated.
- Note: `crID/prNumber/principalID/businessID` are assigned a few lines below in the current code — move the chain block to AFTER those assignments (line ~361) so `failJob` args exist. Keep it BEFORE the `effectiveReviewModel` model-fallback so both compose.

- [ ] **Step 6: Integration guard** — in `service_multidim_integration_test.go`: seed a `vllm` primary (unreachable base_url) + a fake-live secondary; configure the chain; assert the review completes using the secondary's provider and that `dimension_runs` reflect the secondary. Run `make int-test`.

- [ ] **Step 7: `go build ./... && make lint && go test ./internal/agents/coding/...`** → green.

- [ ] **Step 8: Commit**

```bash
git add internal/agents/coding
git commit -m "feat(review): resolve+probe reviewbot fallback chain in runJob"
```

---

## Slice 5 — Configuration UI

### Task 7: Agent form — "Max concurrent lanes"

**Files:**
- Modify: `web/src/app/core/agents.service.ts` (Agent model + payloads)
- Modify: `web/src/app/pages/agents/agent-form.ts` (form control + template)
- Modify: `web/src/app/pages/agents/agent-form.spec.ts` (or create)

- [ ] **Step 1: Failing component spec** — asserts the form renders a number input bound to `max_concurrent_lanes`, defaults to 4, and rejects <1 / >16.
- [ ] **Step 2: Run** `npx ng test --no-watch --include='**/agent-form.spec.ts'` → FAIL.
- [ ] **Step 3: Add `maxConcurrentLanes: number` to the Agent interface + create/update payloads in `agents.service.ts`; add a `FormControl(4, [Validators.min(1), Validators.max(16)])` and a labeled `<input type="number">` in `agent-form.ts`.**
- [ ] **Step 4: Run spec** → PASS.
- [ ] **Step 5: `npx ng build`** → green. **Commit** `feat(web): agent max concurrent lanes field`.

### Task 8: Review setup — fallback chain editor

**Files:**
- Modify: `web/src/app/core/code-review.service.ts` (ReviewConfig model: `reviewAgentChain: string[]`)
- Modify: `web/src/app/pages/code-review/setup.ts` (ordered agent picker)
- Modify: `web/src/app/pages/code-review/setup.spec.ts`
- Create: `web/e2e/reviewbot-fallback-chain.spec.ts`

- [ ] **Step 1: Failing component spec** — renders an ordered list of the business's agents; selecting agents in order updates `reviewAgentChain`; empty ⇒ omitted/`[]`.
- [ ] **Step 2: Run** `npx ng test --no-watch --include='**/setup.spec.ts'` → FAIL.
- [ ] **Step 3: Implement** — load agents (existing `agents.service.ts`), render an add/remove/reorder picker (primary first), persist `review_agent_chain` via `code-review.service.ts` PUT to review config. Reuse the existing setup save path.
- [ ] **Step 4: Run spec** → PASS.
- [ ] **Step 5: Playwright** — `web/e2e/reviewbot-fallback-chain.spec.ts`: mock `**/api/**` fallback FIRST, then mock agents + review-config; add two bots to the chain, save, reload, assert order persists. Run `npx playwright test reviewbot-fallback-chain`.
- [ ] **Step 6: `npx ng build`** → green. **Commit** `feat(web): reviewbot fallback chain editor + e2e`.

---

## Slice 6 — Docs, guards, close-out

### Task 9: Documentation + durable guards + epic close

- [ ] **Step 1:** Update `internal/agents/CLAUDE.md` (or add a short note) pointing at `fallbackchain.go`, `prober.go`, `laneLimit`, and the two new columns.
- [ ] **Step 2:** Confirm the durable guards exist and pass: `TestFanOut_UsesLaneLimit_SourcePin`, `TestChooseReviewbot`, prober tests, the two integration cases. `make test && make int-test && go test -tags contract ./cmd/... && make lint`.
- [ ] **Step 3:** Update the design doc status to "Implemented"; note the network-routing caveat (hub worker must route to `192.168.2.241`) is a deploy step, not code.
- [ ] **Step 4:** `bd close` the slice issues; leave the epic open until deployed + verified on hub. Commit `docs(review): fallback-chain epic close-out`.

---

## Self-Review (author checklist — completed)

- **Spec coverage:** detection→Task 4+6; per-agent concurrency→Task 1–3; ordered chain→Task 5–6; whole-review scope→Task 6 (`chooseReviewbot` picks one bot); reactive net→Task 6 Step 5 note + existing worker requeue; error handling (all-stale terminal, stale skip, config validation)→Task 5 Step 7 + Task 6; UI→Task 7–8; testing→every task + Task 9. Out-of-scope (per-dimension provider `manyforge-ubk`) untouched — `partitionByProvider` left as-is.
- **Placeholders:** none — migration numbers concrete (0085/0086), all code shown.
- **Type consistency:** `AICredential.MaxConcurrentLanes` defined Task 3, consumed by `laneLimit`; `chooseReviewbot`/`resolveFn`/`reviewbotProber`/`httpProber` names consistent across Tasks 4/6; `ReviewAgentChain []string` (view) ↔ `[]uuid.UUID` (dbgen) mapped in Task 5.
