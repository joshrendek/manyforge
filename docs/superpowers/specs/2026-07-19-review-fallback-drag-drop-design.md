# Design — Drag-and-drop reordering for the code-review fallback chains

- **Date:** 2026-07-19
- **Goal:** Let the user reorder the review-setup fallback chains by **dragging**, instead of
  clicking ↑/↓ repeatedly (moving OpenRouter to #1 / vLLM to the end is many clicks today, tedious
  enough that retyping feels easier). Position = priority (array index); a one-motion drag replaces
  N button clicks.
- **Scope:** the **two fallback chains** only — the per-dimension model/provider chain and the
  panel-level Reviewbot agent chain. The dimension rows are out of scope (unchanged).
- **Frontend-only.** No backend/API/data-model change — order is already persisted as array
  position via the existing Save buttons.

## Current state (verified)
Both chains are already **structured ordered arrays with working ↑/↓ reorder buttons** — NOT
textareas. So this adds a drag affordance to existing arrays; it is not a textarea→list conversion.

- **Per-dimension fallback chain:** `DraftRow.fallback_chain: ReviewDimensionFallbackEntry[]`
  (`web/src/app/pages/code-review/setup.ts:100-112`; entry `{provider, model}` at
  `web/src/app/core/code-review.service.ts:91-94`). Template `@for (fb of row.fallback_chain; track fb; let i = $index)`
  at `setup.ts:207-248`. Reorder today: `moveFallback(row, i, ±1)` (`setup.ts:522-527`), array swap
  + signal bump. Testids: `row-fallback-list`, `row-fallback-provider-{i}`, `row-fallback-up-{i}`,
  `row-fallback-down-{i}`.
- **Reviewbot agent chain:** `config().review_agent_chain: string[]` (agent UUIDs, primary first;
  `code-review.service.ts:124-133`). Template `setup.ts:309-325`. Reorder today: `moveChain(i, delta)`
  (`setup.ts:589-595`). Testids: `chain-row-{i}`, `chain-up-{i}`, `chain-down-{i}`.
- **Persistence:** fallback chain saved per-row via `saveRow()` → `api.upsertDimension()`
  (`POST …/review-dimensions`), `toInput()` sends `fallback_chain` in array order (`setup.ts:651-664`).
  Agent chain saved via `saveConfig()` → `api.putConfig()` (`PUT …/review-config`), sends
  `review_agent_chain` in array order. **Order = array position; no priority field, no reorder endpoint.**
- **No `@angular/cdk` dependency today** (`web/package.json`: only `@angular/{common,compiler,core,forms,platform-browser,router} ^21.2.0`).

## Decisions
1. **Use Angular CDK drag-drop** (`@angular/cdk`, matched to the Angular major, `^21.x`). Official,
   smooth animation + drop placeholder + `moveItemInArray` helper. Rejected native HTML5 drag
   (clunkier, more code) — the one-dependency cost is worth it.
2. **Keep the ↑/↓ buttons; ADD a drag handle** (do not replace). CDK drag is pointer/touch only with
   **no keyboard reordering**, so ↑/↓ remains the keyboard/screen-reader path (and keeps every
   existing unit + e2e test green). Drag = big moves; ↑/↓ = keyboard + fine nudges.
3. **Drag mutates the in-memory array only; the user still clicks the existing Save button to
   persist** — identical to how ↑/↓ behaves today (no surprise auto-write on drop).

## Implementation
1. `npm install @angular/cdk@^21` in `web/`; import `DragDropModule` (or the standalone
   `CdkDropList`, `CdkDrag`, `CdkDragHandle`) into the setup component's `imports`.
2. **Per-dimension fallback chain** (`setup.ts:207-248`): make the list container a `cdkDropList`
   with `(cdkDropListDropped)="onFallbackDrop(row, $event)"`; each entry a `cdkDrag`; prepend a grip
   handle (`⠿`, `cdkDragHandle`, `aria-label="Drag to reorder fallback entry"`,
   `data-testid="row-fallback-drag-{i}"`). Handler:
   `onFallbackDrop(row, e)` → `moveItemInArray(row.fallback_chain, e.previousIndex, e.currentIndex)`
   then the same signal update `moveFallback` uses (`bumpRows()`), so the change is picked up + saveable.
3. **Reviewbot agent chain** (`setup.ts:309-325`): same pattern → `onChainDrop(e)` →
   `moveItemInArray(this.config().review_agent_chain, e.previousIndex, e.currentIndex)` + `patchConfig`
   (mirror `moveChain`). Handle testid `chain-drag-{i}`.
4. Keep `moveFallback`/`moveChain`, the ↑/↓ buttons, and all existing testids untouched.

## Testing
- **Unit (`setup.spec.ts`, Vitest + TestBed):** keep the existing ↑/↓ reorder specs (`:138`, `:241`).
  Add: call `onFallbackDrop(row, {previousIndex, currentIndex} as CdkDragDrop<…>)` and assert
  `cmp.rows()[k].fallback_chain` reordered; same for `onChainDrop` against
  `cmp.config().review_agent_chain`. (CDK pointer drag isn't simulable in TestBed — test the drop
  handler + `moveItemInArray` directly.)
- **e2e (`web/e2e/code-review.spec.ts`, Playwright):** add a test that drags a fallback entry via
  `page.getByTestId('row-fallback-drag-1').dragTo(page.getByTestId('row-fallback-drag-0'))`, asserts
  the reordered provider/model, clicks Save, and asserts the persisted request body order (mock the
  API with `page.route` as the existing tests do). One equivalent test for the agent chain.
- **Real-browser verification (CLAUDE.md mandate):** before calling it done, drive the actual drag in
  a browser (gstack `$B` / Playwright MCP) — CDK DragDrop needs real pointer events unit tests can't
  exercise. Then the Playwright spec is the regression codification.

## Risks
- CDK version skew with Angular 21 → pin `@angular/cdk` to the matching major.
- Drag vs the `<select>`/`<input>` inside each row stealing the gesture → constrain dragging to the
  grip handle only (`cdkDragHandle`), so clicking the dropdowns/inputs never starts a drag.
- Touch/scroll conflict on mobile → CDK handles this; the handle keeps it scoped.
