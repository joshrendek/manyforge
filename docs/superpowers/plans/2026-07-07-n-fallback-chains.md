# N-Fallback Chains Per Review Dimension — Implementation Plan (manyforge-7lx)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace each review dimension's single fallback `(fallback_provider, fallback_model)` with an ordered list of N fallbacks (`fallback_chain`), tried in order — probing the whole chain to start on the first live endpoint and falling through the tail on a mid-run failure.

**Architecture:** Migrate the representation to a JSONB ordered list (a 1-element chain == today's single fallback, so behavior is preserved through the migration), then implement chain probing in `resolveLaneCred` and tail iteration in `reviewLane`, then the config UI. Builds on manyforge-azy + manyforge-9er.

**Tech Stack:** Go 1.25, PostgreSQL + sqlc v1.27.0 (pinned, Docker), Angular `web/`.

## Global Constraints
- **sqlc pin v1.27.0:** regen with `docker run --rm -v "$(pwd)":/src -w /src sqlc/sqlc:1.27.0 generate`. `db/schema.sql` is the codegen source — change columns there too, not just migrations. Never hand-edit `internal/platform/db/dbgen/`.
- **Back-compat:** the migration must convert every existing single fallback to a 1-element chain — no operator loses their fallback.
- **Gates unchanged:** every candidate cred still passes `laneCredFor` (egress allowlist + `privateBaseURLBlocked` + `allow_private_base_url`); each `runSandboxLane` self-acquires its endpoint semaphore (manyforge-9er). The chain adds no concurrency surface (entries run sequentially within a lane).
- **`go build ./...` is truth** — ignore stale gopls "dbgen field undefined" diagnostics after regen.
- Verification: `make test`, `go test -tags integration -p 1 ./internal/agents/coding/`, `go test -tags contract ./cmd/...`, `make lint`, `make sec-test`, `cd web && npx ng build && npx ng test --no-watch`.
- Latest existing migration is **0090**; this feature adds **0091**.

## File Structure
- `migrations/0091_review_dimension_fallback_chain.{up,down}.sql` (new)
- `db/schema.sql` — `review_dimension`: drop 2 cols, add `fallback_chain jsonb`
- `db/query/review_dimension.sql` — Insert/Upsert use `fallback_chain`
- `internal/platform/db/dbgen/` — regenerated
- `internal/agents/coding/dimensions.go` — `FallbackEntry` + `Dimension.FallbackChain`
- `internal/agents/coding/panel.go` — `dimensionFromRow` unmarshals `fallback_chain`
- `internal/agents/coding/review_config_service.go` — Input/View/validation/upsert/view
- `internal/agents/coding/fallbackchain.go` — `laneCandidates` + `resolveLaneCred`
- `internal/agents/coding/service.go` — `reviewLane` tail iteration + runJob pre-loop
- `specs/008-review-dimensions/contracts/openapi.yaml` — `fallback_chain` array
- `web/src/app/core/code-review.service.ts` — interfaces
- `web/src/app/pages/code-review/setup.ts` (+ `setup.spec.ts`) — repeatable rows
- `internal/security_regression/` + `*_test.go` — pins/fixtures referencing `Fallback`

Dependencies: T2 needs T1; T3 needs T2; T4 needs T1 (API shape); T5 last.

---

### Task 1: Migrate representation — fallback becomes a chain (behavior-preserving)

Converts the two scalar columns to `fallback_chain jsonb` across DB, domain, config CRUD, and OpenAPI. `resolveLaneCred`/`reviewLane` are updated only enough to compile + preserve current single-fallback behavior by using `FallbackChain[0]` (the real N-chain behavior lands in T2/T3).

**Files:** migration 0091 (new); `db/schema.sql`; `db/query/review_dimension.sql`; dbgen (regen); `dimensions.go`; `panel.go`; `review_config_service.go`; `fallbackchain.go`; `service.go`; `specs/008-review-dimensions/contracts/openapi.yaml`; tests.

**Interfaces produced:**
- `type FallbackEntry struct { Provider string \`json:"provider"\`; Model string \`json:"model"\` }` (in `dimensions.go`)
- `Dimension.FallbackChain []FallbackEntry` (replaces `FallbackProvider`/`FallbackModel`)
- `ReviewDimensionInput.FallbackChain []FallbackEntry` and `ReviewDimensionView.FallbackChain []FallbackEntry`

- [ ] **Step 1: Write the migration.** `migrations/0091_review_dimension_fallback_chain.up.sql`:

```sql
ALTER TABLE review_dimension ADD COLUMN fallback_chain jsonb NOT NULL DEFAULT '[]';

-- Preserve existing single fallbacks as 1-element chains (no config lost).
UPDATE review_dimension
SET fallback_chain = jsonb_build_array(
    jsonb_build_object('provider', fallback_provider::text, 'model', fallback_model))
WHERE fallback_provider IS NOT NULL;

ALTER TABLE review_dimension DROP COLUMN fallback_provider;
ALTER TABLE review_dimension DROP COLUMN fallback_model;
```

`migrations/0091_review_dimension_fallback_chain.down.sql`:

```sql
ALTER TABLE review_dimension ADD COLUMN fallback_provider ai_provider;
ALTER TABLE review_dimension ADD COLUMN fallback_model text NOT NULL DEFAULT '';

-- Restore the FIRST chain entry into the scalar columns (lossy for N>1).
UPDATE review_dimension
SET fallback_provider = (fallback_chain->0->>'provider')::ai_provider,
    fallback_model     = COALESCE(fallback_chain->0->>'model', '')
WHERE jsonb_array_length(fallback_chain) > 0;

ALTER TABLE review_dimension DROP COLUMN fallback_chain;
```

- [ ] **Step 2: Update `db/schema.sql`** (`review_dimension`, ~lines 654-655): remove `fallback_provider ai_provider,` and `fallback_model text NOT NULL DEFAULT '',`; add `fallback_chain jsonb NOT NULL DEFAULT '[]',`.

- [ ] **Step 3: Update `db/query/review_dimension.sql`.** In `InsertReviewDimension` and `UpsertReviewDimension`: remove the `fallback_provider`/`fallback_model` columns + params; add `fallback_chain` with `sqlc.arg('fallback_chain')`. In the Upsert `ON CONFLICT ... DO UPDATE SET`, replace the two fallback assignments with `fallback_chain = EXCLUDED.fallback_chain`. `ListReviewDimensions` selects `*` (auto-includes the new column).

- [ ] **Step 4: Regenerate sqlc.** Run: `docker run --rm -v "$(pwd)":/src -w /src sqlc/sqlc:1.27.0 generate`. Expected: `dbgen.ReviewDimension` now has `FallbackChain []byte` (jsonb) and no `FallbackProvider`/`FallbackModel`; `Insert/UpsertReviewDimensionParams.FallbackChain []byte`.

- [ ] **Step 5: Define the domain type** in `internal/agents/coding/dimensions.go`. Replace `FallbackProvider string` / `FallbackModel string` (lines 25-26) with `FallbackChain []FallbackEntry`, and add above the struct:

```go
// FallbackEntry is one (provider, model) step in a dimension's ordered fallback chain.
type FallbackEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
```

- [ ] **Step 6: Map the jsonb in `panel.go`** (`dimensionFromRow`, ~lines 44-63). Remove the `r.FallbackProvider`/`r.FallbackModel` handling; add:

```go
	if len(r.FallbackChain) > 0 {
		_ = json.Unmarshal(r.FallbackChain, &d.FallbackChain) // best-effort; malformed ⇒ empty chain
	}
```

(ensure `encoding/json` is imported.)

- [ ] **Step 7: Update `review_config_service.go`.** In `ReviewDimensionInput` and `ReviewDimensionView` replace `FallbackProvider`/`FallbackModel` with `FallbackChain []FallbackEntry`. Rewrite the fallback part of `validateDimensionInput` (lines ~105-116):

```go
	for i, fb := range in.FallbackChain {
		if fb.Provider == "" || !knownAIProviders[fb.Provider] {
			return fmt.Errorf("coding: fallback[%d] has unknown/empty provider %q: %w", i, fb.Provider, errs.ErrValidation)
		}
		if strings.TrimSpace(fb.Model) == "" {
			return fmt.Errorf("coding: fallback[%d] model required: %w", i, errs.ErrValidation)
		}
	}
```

In `UpsertDimension` (line ~169-170), replace the two `nullProvider(...)`/scalar params with `FallbackChain: mustMarshalChain(in.FallbackChain)` where you add a small helper `func mustMarshalChain(c []FallbackEntry) []byte { if len(c)==0 { return []byte("[]") }; b, _ := json.Marshal(c); return b }`. In `dimensionViewFromRow` (line ~305-327), replace the scalar reads with `_ = json.Unmarshal(r.FallbackChain, &v.FallbackChain)` (guard `len>0`). Audit-log the chain instead of the two scalars.

- [ ] **Step 8: Keep `fallbackchain.go` + `service.go` compiling with 1-element behavior.** In `resolveLaneCred` (fallbackchain.go ~92-96) replace the `dim.FallbackProvider != ""` block with:

```go
	if len(dim.FallbackChain) > 0 {
		fb := dim.FallbackChain[0]
		lc, ferr := s.laneCredFor(ctx, principalID, businessID, def, fb.Provider, fb.Model)
		if ferr == nil {
			return lc, ""
		}
		slog.Default().InfoContext(ctx, "coding: lane fallback unusable", "dimension", dim.Key, "err", ferr)
	}
```

In `reviewLane` (service.go ~828-829) replace the single-fallback condition + resolve with the `[0]` entry:

```go
	if lr.Err != nil && len(dim.FallbackChain) > 0 && !strings.EqualFold(chosen.Provider, dim.FallbackChain[0].Provider) {
		fb := dim.FallbackChain[0]
		if fbCred, ferr := s.laneCredFor(ctx, principalID, businessID, cred, fb.Provider, fb.Model); ferr == nil {
			// ... existing fbOutDir + runSandboxLane(dim, fbCred, fbOutDir) unchanged ...
```

(This preserves current single-fallback behavior; T2/T3 generalize to the full chain.)

- [ ] **Step 9: Update OpenAPI** `specs/008-review-dimensions/contracts/openapi.yaml`: in `ReviewDimensionInput`, remove `fallback_provider` + `fallback_model`; add:

```yaml
    fallback_chain:
      type: array
      description: 'Ordered fallback (provider, model) pairs, tried in order after the primary.'
      items:
        type: object
        properties:
          provider: { type: string }
          model: { type: string }
        required: [provider, model]
```

- [ ] **Step 10: Fix compile + tests.** Update Go test fixtures that build a `Dimension`/`ReviewDimensionInput` with `FallbackProvider`/`FallbackModel` (`lanecred_test.go` ~52/57, `review_config_service_test.go` ~84-96, `service_lane_test.go`) to `FallbackChain: []FallbackEntry{{Provider:"...", Model:"..."}}`. Add a test in `review_config_service_test.go` that a 3-entry chain round-trips upsert→view, and that a blank-provider entry is rejected.

- [ ] **Step 11: Verify + commit.** `go build ./...`; `make test`; `go test -tags contract ./cmd/...` (008 shape changed — expect the drift test to reflect the new schema, update the golden if the repo uses one); `make lint`. Then:

```bash
git add migrations/0091_* db/schema.sql db/query/review_dimension.sql internal/platform/db/dbgen internal/agents/coding/ specs/008-review-dimensions/contracts/openapi.yaml
git commit -m "feat(review): migrate dimension fallback to an ordered chain (manyforge-7lx)"
```

---

### Task 2: Chain resolution — probe the whole chain for a live start

`resolveLaneCred` now walks `[primary, ...FallbackChain]`, probing each, and returns the chosen cred **plus the not-yet-tried tail**.

**Files:** `internal/agents/coding/fallbackchain.go`; `internal/agents/coding/service.go` (runJob pre-loop); tests.

**Interfaces:**
- Consumes: `Dimension.FallbackChain`, `laneCredFor`, `prober().Live`.
- Produces: `laneCandidates(dim Dimension) []FallbackEntry`; new `resolveLaneCred` signature `(cred AICredential, rest []FallbackEntry, reason string)`.

- [ ] **Step 1: Write failing tests** in `fallbackchain_test.go` using the existing `FakeCredResolver` + a fake prober: (a) primary live → chosen=primary cred, `rest` = the full chain; (b) primary not-live, `fb[0]` live → chosen=fb[0] cred, `rest` = `chain[1:]`; (c) none live but all resolvable → chosen=primary (first resolvable), rest=chain; (d) none resolvable → empty cred + non-empty reason.

- [ ] **Step 2: Run — fails** (`resolveLaneCred` has the old signature + only checks `[0]`).

- [ ] **Step 3: Implement.** Add:

```go
// laneCandidates returns the ordered fallback candidates for a dimension: the primary
// (provider, model) first, then each fallback-chain entry.
func laneCandidates(dim Dimension) []FallbackEntry {
	out := make([]FallbackEntry, 0, len(dim.FallbackChain)+1)
	out = append(out, FallbackEntry{Provider: dim.Provider, Model: dim.Model})
	out = append(out, dim.FallbackChain...)
	return out
}
```

Rewrite `resolveLaneCred`:

```go
func (s *CodeReviewService) resolveLaneCred(ctx context.Context, principalID, businessID uuid.UUID, def AICredential, dim Dimension) (AICredential, []FallbackEntry, string) {
	cands := laneCandidates(dim)
	var firstResolvable AICredential
	firstResolvableIdx := -1
	for i, c := range cands {
		lc, err := s.laneCredFor(ctx, principalID, businessID, def, c.Provider, c.Model)
		if err != nil {
			continue
		}
		if firstResolvableIdx < 0 {
			firstResolvable, firstResolvableIdx = lc, i
		}
		if s.prober().Live(ctx, lc) {
			return lc, cands[i+1:], "" // live start; runtime may continue down the tail
		}
	}
	if firstResolvableIdx >= 0 {
		return firstResolvable, cands[firstResolvableIdx+1:], "" // none live; try the first resolvable
	}
	return AICredential{}, nil, fmt.Sprintf("no reachable reviewbot for %q (check its provider credentials and fallbacks)", dim.Key)
}
```

- [ ] **Step 4: Update the runJob pre-loop** (`service.go` ~644-652) to capture the tail. Alongside `laneCreds map[string]AICredential`, add `laneRest := make(map[string][]FallbackEntry, len(active))`; set `lc, rest, reason := s.resolveLaneCred(...)`; store `laneCreds[dim.Key]=lc` and `laneRest[dim.Key]=rest`. (reviewLane still uses `[0]`-style fallback from Task 1 — that's replaced in Task 3.)

- [ ] **Step 5: Run tests + build.** `go test ./internal/agents/coding/ -run 'LaneCred|ResolveLane' -v`; `go build ./...`; `make lint`.

- [ ] **Step 6: Commit.**

```bash
git add internal/agents/coding/fallbackchain.go internal/agents/coding/service.go internal/agents/coding/fallbackchain_test.go
git commit -m "feat(review): resolveLaneCred probes the whole fallback chain (manyforge-7lx)"
```

---

### Task 3: Runtime chain fallback — iterate the tail on failure

`reviewLane` now, on a lane failure, tries each entry in `laneRest[dim.Key]` in order until one succeeds.

**Files:** `internal/agents/coding/service.go` (`reviewLane`); tests.

**Interfaces:** Consumes `laneRest` (Task 2), `runSandboxLane`, `laneCredFor`.

- [ ] **Step 1: Write failing test** in `service_lane_test.go` (fake sandbox): a dimension with primary=vllm@A and chain `[vllm@B, openrouter]`, where the fake runner fails for A and B and succeeds for openrouter → the returned `laneResult.Provider=="openrouter"`, and the runner was invoked for A then B then openrouter (order asserted). A second test: all fail → original (primary) error returned.

- [ ] **Step 2: Run — fails** (reviewLane only tries `FallbackChain[0]`).

- [ ] **Step 3: Implement.** Replace the Task-1 single-entry fallback block in `reviewLane` with tail iteration:

```go
reviewLane := func(dim Dimension, laneOutDir string) laneResult {
	chosen := laneCreds[dim.Key]
	lr := runSandboxLane(dim, chosen, laneOutDir)
	if lr.Err == nil {
		return lr
	}
	// Runtime fallback (manyforge-7lx): try each not-yet-tried chain entry in order.
	for i, fb := range laneRest[dim.Key] {
		fbCred, ferr := s.laneCredFor(ctx, principalID, businessID, cred, fb.Provider, fb.Model)
		if ferr != nil {
			slog.Default().InfoContext(ctx, "code review lane: fallback entry unusable",
				"dimension", dim.Key, "index", i, "provider", fb.Provider, "err", ferr)
			continue
		}
		slog.Default().InfoContext(ctx, "code review lane: falling back",
			"dimension", dim.Key, "from", chosen.Provider, "to", fbCred.Provider, "index", i)
		fbOutDir := laneOutDir + "-fallback-" + strconv.Itoa(i)
		if err := os.MkdirAll(fbOutDir, 0o777); err != nil {
			continue
		}
		_ = os.Chmod(fbOutDir, 0o777)
		if fbResult := runSandboxLane(dim, fbCred, fbOutDir); fbResult.Err == nil {
			return fbResult
		}
	}
	return lr // all fallbacks exhausted → original failure
}
```

(ensure `strconv` + `os` are imported — they already are in service.go.) Each `runSandboxLane` self-acquires its endpoint semaphore (unchanged), so per-endpoint concurrency still holds; entries run sequentially within the lane.

- [ ] **Step 4: Run tests + build.** `go test ./internal/agents/coding/ -run 'Lane|Fallback' -v`; `go test -race ./internal/agents/coding/ -run 'Lane|Fallback'`; `go build ./...`; `make lint`.

- [ ] **Step 5: Commit.**

```bash
git add internal/agents/coding/service.go internal/agents/coding/service_lane_test.go
git commit -m "feat(review): reviewLane falls through the whole fallback chain at runtime (manyforge-7lx)"
```

---

### Task 4: Frontend — repeatable fallback rows (add / remove / reorder)

**Files:** `web/src/app/core/code-review.service.ts`; `web/src/app/pages/code-review/setup.ts` (+ `setup.spec.ts`).

**Interfaces:** `interface ReviewDimensionFallbackEntry { provider: string; model: string }`; `fallback_chain: ReviewDimensionFallbackEntry[]` on `ReviewDimension`/`ReviewDimensionInput`.

- [ ] **Step 1: Service interfaces.** In `code-review.service.ts` (lines ~85-113) add `ReviewDimensionFallbackEntry` and replace `fallback_provider?`/`fallback_model?` on `ReviewDimension` and `fallback_provider`/`fallback_model` on `ReviewDimensionInput` with `fallback_chain: ReviewDimensionFallbackEntry[]`.

- [ ] **Step 2: DraftRow + serialization** in `setup.ts`: replace `fallback_provider`/`fallback_model` on `DraftRow` (lines ~99-100) with `fallback_chain: { provider: string; model: string }[]`. In `rowFromServer` set `fallback_chain: (d.fallback_chain ?? []).map(f => ({...f}))`; in `toInput` set `fallback_chain: row.fallback_chain.filter(f => f.provider)`.

- [ ] **Step 3: Row-editing methods** in the component:

```typescript
addFallback(row: DraftRow): void { row.fallback_chain.push({ provider: '', model: '' }); }
removeFallback(row: DraftRow, i: number): void { row.fallback_chain.splice(i, 1); }
moveFallback(row: DraftRow, i: number, dir: -1 | 1): void {
  const j = i + dir;
  if (j < 0 || j >= row.fallback_chain.length) return;
  [row.fallback_chain[i], row.fallback_chain[j]] = [row.fallback_chain[j], row.fallback_chain[i]];
}
onFallbackEntryProviderChange(row: DraftRow, i: number, provider: string): void {
  row.fallback_chain[i].provider = provider;
  row.fallback_chain[i].model = '';
}
```

- [ ] **Step 4: Template.** Replace the single fallback select block (setup.ts ~202-223) with a repeatable list: `@for (fb of row.fallback_chain; track $index)` rendering per-entry a provider `<select>` (`data-testid="row-fallback-provider-{{$index}}"`, calling `onFallbackEntryProviderChange(row, $index, $event)`), the model picker (reuse `modelsForProvider`/`isFreeText`/`isOpenRouter` keyed on `fb.provider`), a remove button (`data-testid="row-fallback-remove-{{$index}}"`), and up/down buttons (`moveFallback`). Below the list, a `<button data-testid="row-fallback-add" (click)="addFallback(row)">+ Add fallback</button>`.

- [ ] **Step 5: Spec.** In `setup.spec.ts` update mock `ReviewDimension` payloads to `fallback_chain: [...]`; add a test that Add appends a row, Remove deletes it, reorder swaps entries, and `toInput` drops blank-provider entries. Mount the component and assert the DOM `data-testid`s.

- [ ] **Step 6: Verify + commit.** `cd web && npx ng build && npx ng test --no-watch` (all pass). Then:

```bash
git add web/src/app/core/code-review.service.ts web/src/app/pages/code-review/setup.ts web/src/app/pages/code-review/setup.spec.ts
git commit -m "feat(review): fallback chain add/remove/reorder UI (manyforge-7lx)"
```

---

### Task 5: Source-pins + full gate

**Files:** `internal/security_regression/` and any test referencing `fallback_provider`/`fallback_model`; run the whole gate.

- [ ] **Step 1: Find + update pins.** `grep -rn 'fallback_provider\|fallback_model\|FallbackProvider\|FallbackModel' internal/ web/src --include=*.go --include=*.ts` — every remaining hit should be intentional (comments, migration text). Update any `security_regression` pin or test that asserts on the old scalar shape to the `fallback_chain` shape; keep finding-ids. Add a pin (if not covered) that the migration `0091` converts a single fallback to a 1-element chain (source-pin on the up-SQL's `jsonb_build_array` backfill).

- [ ] **Step 2: Full gate** (report each):
  - `make test`
  - `go test -tags integration -p 1 ./internal/agents/coding/`
  - `go test -tags contract ./cmd/...`
  - `make lint`
  - `make sec-test`
  - `cd web && npx ng build && npx ng test --no-watch`

- [ ] **Step 3: Commit + bd.**

```bash
git add internal/security_regression/ && git commit -m "test(review): pin fallback_chain shape; full gate (manyforge-7lx)"
bd update manyforge-7lx --notes "Implemented per docs/superpowers/plans/2026-07-07-n-fallback-chains.md; full gate green"
```

---

## Verification / Rollout
Migration applies on deploy (version guard); existing single fallbacks auto-convert. After deploy, add chains in the UI — e.g. `192.168.1.171` (vllm/qwen3.6-35b-a3b) as a second fallback after `192.168.2.241` — and ensure both bare hosts are in `sandbox.egressAllow` + `allow_private_base_url` on the creds (manyforge-9er).

## Test Plan Summary
- Migration back-compat (single→1-elem chain; 0-fallback→[]); down restores first entry — source-pin + integration.
- `resolveLaneCred` unit: live-start selection + `rest` tail across the 4 cases.
- `reviewLane` integration: tail iteration order; all-fail returns original; `-race`.
- Config validation + upsert→view round-trip (3-entry chain; blank-entry rejected).
- Contract drift after the 008 shape change.
- Frontend: add/remove/reorder + `toInput` filtering; `ng build`/`ng test`.
- Source-pins updated to the chain shape.
