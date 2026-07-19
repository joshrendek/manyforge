# Design — Unified draggable provider-priority list per review dimension

- **Date:** 2026-07-19
- **Problem:** Each dimension's provider priority is split across three columns — **Provider** + **Model** (the primary = #1) and a separate **Fallback chain** (#2, #3…). The primary has no drag handle, so you can't drag a provider to #1 or demote the primary; "move OpenRouter to #1" means retyping the Provider/Model dropdowns. (Follows the fallback-only drag shipped in #34.)
- **Goal:** Merge the primary + fallback chain into ONE draggable priority list per dimension. `#1` is just the top of the list (the primary); drag any provider to any position, including promoting a fallback to primary or demoting the old primary. Keep ↑/↓.
- **Scope:** the per-dimension list only. The reviewbot agent chain (already drag-enabled in #34) is unchanged. Frontend-only — the server DTO (`provider`, `model`, `fallback_chain`) is unchanged; we map to/from it at the load/save boundary.

## Data model change (`web/src/app/pages/code-review/setup.ts`)
Replace `DraftRow.provider: string`, `model: string`, `fallback_chain: ReviewDimensionFallbackEntry[]` with a single:
```ts
chain: ReviewDimensionFallbackEntry[]; // ordered priority: chain[0] = primary (#1), chain[1..] = fallbacks
```
`chain` always has **≥1 entry** (chain[0] is the primary, which may be `{provider:'', model:''}` = "use the review credential default").

- **Load** `rowFromServer(d)`: `chain: [{ provider: d.provider ?? '', model: d.model }, ...(d.fallback_chain ?? []).map((f) => ({ ...f }))]`.
- **Load (new row)** `rowFromCatalog(c)`: `chain: [{ provider: '', model: '' }]`.
- **Save** `toInput(row)`: `provider: row.chain[0].provider`, `model: row.chain[0].model`, `fallback_chain: row.chain.slice(1).filter((f) => f.provider)`. (Same server shape as today; blank-provider fallbacks still dropped.)
- **On load** the `ensureProviderModels` warm-up loop (currently `ensureProviderModels(row.provider)`) becomes: for each `row.chain` entry, `ensureProviderModels(entry.provider)`.

## UI change
Collapse the three columns (**Provider**, **Model**, **Fallback chain**) into ONE column **"Provider priority"** (`flex:4`, ≈ the combined width). Header row (`setup.ts:168-170`) loses the three, gains one.

Each dimension renders a `cdkDropList` over `row.chain`; each entry is a `cdkDrag` row: `⠿` handle + a position number (`{{ i + 1 }}.`, with `#1` labelled "primary") + provider `<select>` + the existing per-provider model input (free-text `<input>` with `<datalist>` for ollama/vllm/openrouter/huggingface, plain input for `''`=default, `<select>` otherwise) + ↑/↓ + Remove. A `+ Add provider` button appends `{provider:'', model:''}`.

## Handlers (replace the primary + fallback handlers with unified ones)
- `onPriorityProviderChange(row, i, provider)` — `row.chain[i].provider = provider; row.chain[i].model = ''; ensureProviderModels(provider); bumpRows()`. (Replaces `onProviderChange` + `onFallbackEntryProviderChange`.)
- `addPriority(row)` — `row.chain.push({ provider: '', model: '' }); bumpRows()`. (Replaces `addFallback`.)
- `removePriority(row, i)` — guard `if (row.chain.length <= 1) return;` then `splice(i,1); bumpRows()`. The single remaining primary can't be removed (a dimension needs a #1); to "clear" it, set its provider to `''` (= default). Remove button `[disabled]="row.chain.length <= 1"`.
- `movePriority(row, i, dir)` — swap `chain[i]`/`chain[j]` (replaces `moveFallback`).
- `onPriorityDrop(row, e)` — `moveItemInArray(row.chain, e.previousIndex, e.currentIndex); bumpRows()`. (Distinct name from the agent chain's `onChainDrop`.)

Remove the now-dead `onProviderChange`, `addFallback`, `removeFallback`, `moveFallback`, `onFallbackDrop`, `onFallbackEntryProviderChange`.

## Testids (the old primary + fallback testids are replaced)
`row-priority-list`, and per entry `row-priority-drag-{i}`, `row-priority-provider-{i}`, `row-priority-model-text-{i}` / `row-priority-model-select-{i}`, `row-priority-up-{i}`, `row-priority-down-{i}`, `row-priority-remove-{i}`, plus `row-priority-add`. Index 0 = primary.

## Testing
- **Unit (`setup.spec.ts`):** rewrite the dimension provider/fallback tests to the unified list. Cover: load maps `{provider,model,fallback_chain}` → `chain` with primary first; `toInput` maps `chain` back (primary = chain[0], fallbacks = rest, blank dropped); `onPriorityDrop` promoting chain[1]→[0] (a fallback becomes primary) and demoting; `addPriority`/`removePriority` (incl. the ≥1 guard); provider change clears the model. Existing non-dimension tests (business switch, config/agent chain, presets) stay.
- **e2e (`code-review.spec.ts`):** rewrite the per-dimension fallback drag test to the unified list; ADD the key case — drag entry #2 onto #1 by its grip and assert the promoted provider is now `row-priority-provider-0`, then Save and assert the POST body's `provider`/`model` = the promoted entry and `fallback_chain` = the rest. Keep the reviewbot-chain tests.
- **Real-browser (CLAUDE.md):** drive the drag on `/code-review/setup` in a real browser (Playwright) — promote a fallback to primary, confirm ↑/↓ still work, Save persists.

## Risks
- Existing saved dimensions must round-trip: load→edit→save yields the same primary+fallbacks unless reordered. Covered by the load/save unit tests.
- The wider single column must stay readable in the dense 6-row table — keep per-entry controls compact (reuse `mf-btn-sm`).
- Drag must start from the `⠿` handle only (rows contain `<select>`/`<input>`), as in #34.
