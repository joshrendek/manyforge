# Review Fallback-Chain Drag-and-Drop — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add Angular CDK drag-and-drop reordering to the two code-review fallback chains (per-dimension model chain + reviewbot agent chain), keeping the existing ↑/↓ buttons.

**Architecture:** Frontend-only. Both chains are already ordered arrays reordered by array position; add `cdkDropList`/`cdkDrag` + a drag handle whose drop handler calls `moveItemInArray`, mirroring the existing `moveFallback`/`moveChain` persistence (mutate + `bumpRows()` / copy + `patchConfig`). No backend/API change; the existing Save buttons persist array order.

**Tech Stack:** Angular 21 standalone component (`web/src/app/pages/code-review/setup.ts`), `@angular/cdk` drag-drop, Vitest + TestBed (unit), Playwright (e2e).

## Global Constraints
- **Add `@angular/cdk` matched to Angular's major** (`^21`). It is not currently a dependency.
- **Keep the ↑/↓ buttons and every existing `data-testid`** (`row-fallback-up-{i}`, `chain-up-{i}`, etc.) — a11y (CDK drag has no keyboard reorder) + existing tests depend on them.
- **Drag mutates the in-memory array only; persistence is the existing Save button** (no auto-save on drop), identical to ↑/↓ today.
- Dragging must start ONLY from the grip handle (`cdkDragHandle`), so the row's `<select>`/`<input>` still work normally.
- New handles get testids `row-fallback-drag-{i}` and `chain-drag-{i}`.

---

### Task 1: Add @angular/cdk + import drag-drop directives

**Files:** Modify `web/package.json`; `web/src/app/pages/code-review/setup.ts:1-18` (imports), `:126` (`@Component.imports`).

- [ ] **Step 1: Install the dependency**

Run (in `web/`): `npm install @angular/cdk@^21`
Expected: `@angular/cdk` added to `package.json` dependencies at a `^21.x` version; `package-lock.json` updated.

- [ ] **Step 2: Import the directives + helpers in the component**

Add to the top-of-file imports in `setup.ts`:
```ts
import { CdkDropList, CdkDrag, CdkDragHandle, CdkDragDrop, moveItemInArray } from '@angular/cdk/drag-drop';
```
Add the three directives to the standalone `imports` array (`setup.ts:126`):
```ts
  imports: [FormsModule, PageHeader, Spinner, EmptyState, CdkDropList, CdkDrag, CdkDragHandle],
```

- [ ] **Step 3: Verify it builds**

Run (in `web/`): `npm run build` (or `npx tsc -p tsconfig.app.json --noEmit`)
Expected: compiles clean (directives resolve; no unused-import error since they're used in Task 2/3 — if the build runs before Task 2, temporarily expect only the "used in template" resolution; do Tasks 1-3 together before building if needed).

- [ ] **Step 4: Commit**
```bash
git add web/package.json web/package-lock.json web/src/app/pages/code-review/setup.ts
git commit -m "build(review-ui): add @angular/cdk drag-drop for fallback reorder"
```

---

### Task 2: Drag-and-drop the per-dimension fallback chain

**Files:** Modify `web/src/app/pages/code-review/setup.ts` — template `:208-247`, add handler near `moveFallback` (`:522`). Test: `web/src/app/pages/code-review/setup.spec.ts`.

**Interfaces:**
- Consumes: `moveItemInArray`, `CdkDragDrop` (Task 1); existing `bumpRows()` (`:615`), `DraftRow.fallback_chain: ReviewDimensionFallbackEntry[]`.
- Produces: `onFallbackDrop(row: DraftRow, e: CdkDragDrop<ReviewDimensionFallbackEntry[]>): void`.

- [ ] **Step 1: Write the failing unit test**

Add to `setup.spec.ts` (mirror the existing fallback reorder test ~`:138`; build the component so `rows()[0].fallback_chain` has ≥2 entries with distinct providers, then):
```ts
it('reorders the fallback chain via drag-drop (onFallbackDrop)', () => {
  const row = cmp.rows()[0];
  // seed two distinct entries
  row.fallback_chain = [
    { provider: 'openrouter', model: 'gpt-4o' },
    { provider: 'vllm', model: 'qwen' },
  ];
  cmp.onFallbackDrop(row, { previousIndex: 0, currentIndex: 1 } as CdkDragDrop<ReviewDimensionFallbackEntry[]>);
  expect(cmp.rows()[0].fallback_chain.map((f) => f.provider)).toEqual(['vllm', 'openrouter']);
});
```
(Import `CdkDragDrop` + `ReviewDimensionFallbackEntry` in the spec.)

- [ ] **Step 2: Run it — expect FAIL**

Run (in `web/`): `npx vitest run src/app/pages/code-review/setup.spec.ts -t "drag-drop"`
Expected: FAIL — `cmp.onFallbackDrop is not a function`.

- [ ] **Step 3: Add the handler**

After `moveFallback` (`setup.ts:527`):
```ts
  // onFallbackDrop reorders the chain by array position (drag equivalent of moveFallback).
  onFallbackDrop(row: DraftRow, e: CdkDragDrop<ReviewDimensionFallbackEntry[]>): void {
    moveItemInArray(row.fallback_chain, e.previousIndex, e.currentIndex);
    this.bumpRows();
  }
```

- [ ] **Step 4: Wire the template**

`setup.ts:208` — the list container gains a drop list:
```html
<div data-testid="row-fallback-list" cdkDropList (cdkDropListDropped)="onFallbackDrop(row, $event)" style="display:flex;flex-direction:column;gap:6px">
```
`setup.ts:210` — each entry becomes draggable:
```html
<div style="display:flex;gap:6px;align-items:center" cdkDrag [attr.data-testid]="'row-fallback-entry-' + i">
```
Immediately inside that entry `<div>` (before the `<select>` at `:211`), add the grip handle:
```html
<span class="mf-drag-handle" cdkDragHandle role="button" tabindex="-1" [attr.data-testid]="'row-fallback-drag-' + i"
      [attr.aria-label]="'Drag to reorder fallback ' + (i + 1) + ' for ' + row.label" style="cursor:grab;user-select:none;color:var(--mf-text-muted)">⠿</span>
```
Leave the ↑/↓/Remove buttons (`:234-239`) unchanged.

- [ ] **Step 5: Run tests — expect PASS**

Run (in `web/`): `npx vitest run src/app/pages/code-review/setup.spec.ts`
Expected: the new test PASSES and the existing fallback ↑/↓ test still PASSES.

- [ ] **Step 6: Commit**
```bash
git add web/src/app/pages/code-review/setup.ts web/src/app/pages/code-review/setup.spec.ts
git commit -m "feat(review-ui): drag-drop reorder the per-dimension fallback chain"
```

---

### Task 3: Drag-and-drop the reviewbot agent chain

**Files:** Modify `setup.ts` — template `:309-321`, add handler near `moveChain` (`:589`). Test: `setup.spec.ts`.

**Interfaces:**
- Consumes: `moveItemInArray`, `CdkDragDrop` (Task 1); existing `patchConfig()` (`:555`), `config().review_agent_chain: string[]`.
- Produces: `onChainDrop(e: CdkDragDrop<string[]>): void`.

- [ ] **Step 1: Write the failing unit test**

Add to `setup.spec.ts` (mirror the existing chain reorder test ~`:241`; seed `config().review_agent_chain` with ≥2 ids):
```ts
it('reorders the reviewbot chain via drag-drop (onChainDrop)', () => {
  cmp.patchConfig({ review_agent_chain: ['agent-a', 'agent-b'] });
  cmp.onChainDrop({ previousIndex: 0, currentIndex: 1 } as CdkDragDrop<string[]>);
  expect(cmp.config().review_agent_chain).toEqual(['agent-b', 'agent-a']);
});
```

- [ ] **Step 2: Run it — expect FAIL** (`cmp.onChainDrop is not a function`)

Run: `npx vitest run src/app/pages/code-review/setup.spec.ts -t "reviewbot chain via drag-drop"`

- [ ] **Step 3: Add the handler**

After `moveChain` (`setup.ts:595`):
```ts
  // onChainDrop reorders the agent chain by array position (drag equivalent of moveChain).
  onChainDrop(e: CdkDragDrop<string[]>): void {
    const chain = [...this.config().review_agent_chain];
    moveItemInArray(chain, e.previousIndex, e.currentIndex);
    this.patchConfig({ review_agent_chain: chain });
  }
```

- [ ] **Step 4: Wire the template**

`setup.ts:309` — container:
```html
<div data-testid="chain-list" cdkDropList (cdkDropListDropped)="onChainDrop($event)" style="display:flex;flex-direction:column;gap:6px;margin-top:6px">
```
`setup.ts:311` — each entry:
```html
<div style="display:flex;gap:8px;align-items:center" cdkDrag [attr.data-testid]="'chain-row-' + i">
```
Add the grip handle right after that entry `<div>` opens (before the `{{ i + 1 }}.` span at `:312`):
```html
<span class="mf-drag-handle" cdkDragHandle role="button" tabindex="-1" [attr.data-testid]="'chain-drag-' + i"
      [attr.aria-label]="'Drag to reorder ' + agentName(id)" style="cursor:grab;user-select:none;color:var(--mf-text-muted)">⠿</span>
```
Leave ↑/↓/Remove (`:314-319`) unchanged.

- [ ] **Step 5: Run tests — expect PASS** (new test + existing chain ↑/↓ test)

Run: `npx vitest run src/app/pages/code-review/setup.spec.ts`

- [ ] **Step 6: Commit**
```bash
git add web/src/app/pages/code-review/setup.ts web/src/app/pages/code-review/setup.spec.ts
git commit -m "feat(review-ui): drag-drop reorder the reviewbot agent chain"
```

---

### Task 4: e2e drag test + real-browser verification

**Files:** Modify `web/e2e/code-review.spec.ts` (add a drag test near the existing chain-reorder test ~`:344-391`).

- [ ] **Step 1: Add a Playwright drag test**

Add a test that navigates to `/code-review/setup` (reuse the existing setup-page mocking with `page.route`), seeds a fallback chain with two distinct entries, then drags entry #2 onto entry #1's handle and asserts the reordered provider, then Saves and asserts the persisted request body order. Skeleton (adapt selectors/mocks to the existing setup test):
```ts
test('drag-reorders a dimension fallback chain and saves the new order', async ({ page }) => {
  // …existing setup-page mock + navigation, add two fallback entries…
  await page.getByTestId('row-fallback-drag-1').dragTo(page.getByTestId('row-fallback-drag-0'));
  await expect(page.getByTestId('row-fallback-provider-0')).toHaveValue('vllm'); // was #2, now #1
  // …click the row Save; assert the POST /review-dimensions body fallback_chain order…
});
```

- [ ] **Step 2: Run the e2e test**

Run (in `web/`): `npx playwright test e2e/code-review.spec.ts -g "drag-reorders"`
Expected: PASS. (If `dragTo` is flaky for CDK, fall back to Playwright's manual `mouse.move`/`down`/`up` over the handle centers.)

- [ ] **Step 3: Real-browser verification (CLAUDE.md mandate)**

Bring up the app and drive the actual drag in a real browser (gstack `$B` on `/code-review/setup`, or the Playwright MCP): confirm a fallback entry drags smoothly by its grip, the order updates, ↑/↓ still work, and Save persists. This catches CDK pointer/injection issues unit tests can't.

- [ ] **Step 4: Commit**
```bash
git add web/e2e/code-review.spec.ts
git commit -m "test(review-ui): e2e drag-reorder fallback chain"
```

---

## Self-Review
- **Spec coverage:** CDK dep + import (Task 1) ✓ · fallback chain drag + keep ↑/↓ (Task 2) ✓ · agent chain drag + keep ↑/↓ (Task 3) ✓ · persistence unchanged (handlers reuse `bumpRows`/`patchConfig`, no save change) ✓ · unit + e2e + real-browser tests (Tasks 2–4) ✓ · handle-only drag + testids (Global Constraints, wired in 2/3) ✓.
- **Placeholder scan:** the only non-literal is the e2e test body (Task 4 Step 1), intentionally a skeleton because it must adapt to the existing `code-review.spec.ts` setup-mock harness — the drag action + assertion lines are concrete.
- **Type consistency:** `onFallbackDrop(row, CdkDragDrop<ReviewDimensionFallbackEntry[]>)` and `onChainDrop(CdkDragDrop<string[]>)` used consistently in tests + handlers + templates; `moveItemInArray`/`bumpRows`/`patchConfig` names match the existing code.
