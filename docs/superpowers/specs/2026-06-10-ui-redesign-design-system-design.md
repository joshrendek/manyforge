# UI Redesign + Design System — Design

**Status:** Approved (design)
**Date:** 2026-06-10
**Author:** Josh Rendek
**Visual reference:** [`assets/2026-06-10-ui-component-kit-reference.html`](assets/2026-06-10-ui-component-kit-reference.html) — open in a browser; the dark/light toggle flips every token live. Every value in this spec matches that sheet.

---

## 1. Context

The Go backend has shipped Specs 001–004; the Angular 21 frontend (`web/`) has lagged and currently covers only **Support** (Spec 002) and **Accounting** (Spec 003), plus auth + dashboard. Spec 004 (connectors) shipped backend-only. The current UI is a cohesive but ad-hoc dark theme: tokens live in `web/src/styles.css`, but most page styling is inline in each `*.ts` component, there is no shared component library, and the nav omits the existing `accounting` route.

This is **Stream 1 of a four-stream UI program** the user sequenced:

1. **Design refresh + design-system foundation** ← *this spec*
2. Catch-up UI for Spec 004 connectors + the autonomy-gate approvals queue
3. Spec 006 — Feedback boards + public portal
4. Spec 005 — CRM UI

Stream 1 establishes the visual language and the reusable foundation (tokens + component kit + shell/nav) that Streams 2–4 consume. Getting it right pays off four times.

## 2. Goals & non-goals

**Goals**
- A new visual direction ("Light & Clean", brand blue) applied to all existing pages.
- A documented, token-driven design system with **light + dark** themes, both shipped, flipped by one toggle.
- A small hand-built **component kit** under `web/src/app/ui/` that all current and future pages use.
- A **data-driven app shell + sidebar nav** ready to absorb Streams 2–4 surfaces.
- Automated tests (unit + Playwright e2e) and real-browser verification of both themes.

**Non-goals (explicitly out of scope for Stream 1)**
- Building Connectors / Approvals / CRM / Feedback **pages** (those are Streams 2–4). Stream 1 only makes the kit + nav *ready* for them.
- Any backend / Go change. This is frontend-only.
- New interaction primitives not needed by the existing 8 pages (modals, datepickers, drag-drop) — added later, per-stream, YAGNI.

## 3. Decisions (locked, with rationale)

| Decision | Choice | Why |
|---|---|---|
| Scope | Redesign **+** systematize (new look, design system) | User chose C+B; three more UI streams ride on this foundation. |
| Visual direction | "Light & Clean" default (white surfaces, airy) | Highest legibility; friendly home for the Stream-3 public portal. |
| Theming | **Light + dark, both shipped**, semantic tokens, persisted toggle | User keeps dark for late-night use; one token layer serves both. |
| Brand accent | **`#0066FF`** (light) / **`#3D8BFF`** (dark), hover `#0052CC` | Exact brand blue pulled from bluescripts.net. |
| Type / density | Inter, comfortable density | Carried from current app; readable, professional. |
| Foundation | **CSS custom-property tokens + hand-built standalone kit**; add **Angular CDK** surgically later for hard primitives only | Matches the repo's zero-framework, plain-CSS, standalone philosophy; full control of the custom aesthetic; tiny bundle; CDK is unstyled so it never fights the look. |

## 4. Design tokens (source of truth)

Defined as CSS custom properties. **Light = `:root`; dark = `html[data-theme="dark"]`.** Components reference **only** `var(--mf-*)` — no raw hex in any component.

### Color

| Token | Light | Dark | Use |
|---|---|---|---|
| `--mf-bg` | `#f6f7f9` | `#0a0a0c` | App canvas |
| `--mf-surface` | `#ffffff` | `#131418` | Cards, sidebar |
| `--mf-surface-2` | `#f1f3f7` | `#1b1d24` | Hover / raised |
| `--mf-surface-inset` | `#ffffff` | `#0f1014` | Inputs |
| `--mf-border` | `#e6e8ee` | `#232530` | Hairlines |
| `--mf-border-strong` | `#d4d8e0` | `#313340` | Hover borders |
| `--mf-text` | `#0f172a` | `#f3f4f6` | Primary ink |
| `--mf-text-muted` | `#64748b` | `#9ca3af` | Secondary |
| `--mf-text-faint` | `#94a3b8` | `#6b7280` | Captions/timestamps |
| `--mf-text-on-accent` | `#ffffff` | `#ffffff` | Text on accent fills |
| `--mf-accent` | `#0066ff` | `#3d8bff` | Brand / primary |
| `--mf-accent-hover` | `#0052cc` | `#5a9cff` | Primary hover |
| `--mf-accent-soft` | `#e8f0ff` | `rgba(0,102,255,.18)` | Soft accent bg (active nav, info pill) |
| `--mf-accent-text` | `#0052cc` | `#bcd6ff` | Text on accent-soft |
| `--mf-danger` / `-soft` / `-text` | `#dc2626` / `#fef2f2` / `#b91c1c` | `#f87171` / `rgba(248,113,113,.14)` / `#fca5a5` | Error/urgent |
| `--mf-warn` / `-soft` / `-text` | `#d97706` / `#fffbeb` / `#b45309` | `#f59e0b` / `rgba(245,158,11,.14)` / `#fcd34d` | Pending/caution |
| `--mf-success` / `-soft` / `-text` | `#16a34a` / `#f0fdf4` / `#15803d` | `#22c55e` / `rgba(34,197,94,.14)` / `#4ade80` | Resolved/ok |
| `--mf-info` / `-soft` / `-text` | = accent | = accent | Informational (aliases accent) |

### Shape, depth, scale

| Token | Light | Dark |
|---|---|---|
| `--mf-ring` (focus) | `0 0 0 3px rgba(0,102,255,.30)` | `0 0 0 3px rgba(61,139,255,.35)` |
| `--mf-shadow-sm` | `0 1px 2px rgba(16,24,40,.06)` | `0 1px 2px rgba(0,0,0,.4)` |
| `--mf-shadow` | `0 4px 16px -6px rgba(16,24,40,.12)` | `0 16px 40px -22px rgba(0,0,0,.7)` |

- **Radius:** `--mf-radius` 12px · `--mf-radius-sm` 8px · `--mf-radius-pill` 999px
- **Spacing scale:** 4 / 8 / 12 / 16 / 20 / 24 / 32 / 40 (`--mf-space-1`…`-8`)
- **Type scale:** `--fs-xs` 12 · `--fs-sm` 13 · `--fs-base` 14 · `--fs-md` 15 · `--fs-lg` 17 · `--fs-xl` 20 · `--fs-2xl` 24; weights 400/500/550/600/650/680/750
- **Font:** `--mf-font` = `'Inter', ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif`

## 5. Theming mechanism

- **`ThemeService`** (Angular signal-based): on init reads `localStorage('mf-theme')`, falling back to `prefers-color-scheme`. Exposes `theme()` signal + `setTheme()` / `toggle()`, which set `data-theme` on `document.documentElement` and persist to `localStorage`.
- **No FOUC:** a tiny inline script in `index.html` sets `data-theme` from `localStorage`/`matchMedia` *before* Angular boots, so first paint is correct.
- **Toggle control** (`theme-toggle`, sun/moon) lives in the sidebar foot.
- **Reduced motion:** wrap transitions in `@media (prefers-reduced-motion: no-preference)`.

## 6. Component kit (`web/src/app/ui/`)

Standalone, presentational, token-only components. Scope = what the existing 8 pages need (YAGNI for the rest):

| Component | Responsibility |
|---|---|
| `mf-button` | Variants: `primary` / `ghost` / `danger` / `link`; sizes `sm`/base; `disabled` + loading. Focus ring. |
| `mf-field` + `mf-input` / `mf-select` / `mf-textarea` | Labeled control with hint + error/invalid state. |
| `mf-card` | Surface + border + `shadow-sm` + radius container. |
| `mf-page-header` | Title + subtitle + actions slot. |
| `mf-table` (list-row pattern) | Header row + hover rows; the support/accounting list layout. |
| `mf-badge` + `mf-status-pill` | Semantic status → pill (urgent/open/pending/resolved + neutral/master). Text+color (never color-alone). |
| `mf-empty-state` | Icon + title + body + optional action. |
| `mf-toast` (+ `ToastService`) | Transient success/error notifications; replaces ad-hoc `.msg` divs. |
| `mf-spinner` | Inline/async loading. |
| `app-shell` + sidebar nav + `theme-toggle` | Layout, data-driven nav, theme toggle. |

Each component ships with a `*.spec.ts` (renders variants + a11y attributes).

## 7. App shell & navigation

- Replace `app.html` / `app.css` with an `app-shell` exposing a **data-driven** nav (array of `{ label, route, icon?, badgeCount? }`).
- **Stream 1 nav items (real routes only):** Dashboard · Support · **Accounting** (fixes the current gap where `accounting` has a route but no link).
- Nav config has a `badgeCount` slot (feeds the future Approvals count). Streams 2–4 add an item in one line.
- Auth pages (login/signup) keep the centered-card layout (no shell).
- Responsive: sidebar collapses to a top bar below 720px (retain current behavior).
- Preserve existing `data-testid` attributes (`app-sidebar`, `nav-*`, `sidebar-identity`, `sign-out`) and extend with `nav-accounting`, `theme-toggle`.

## 8. Migration scope & order (Stream 1)

Incremental, dependency-first. Each page swaps inline styles for kit components + tokens and deletes dead CSS (e.g. legacy `.biz-list`).

1. **Foundation:** tokens in `styles.css` (both themes) → `ThemeService` + no-FOUC script → `app-shell` + nav + toggle.
2. **Component kit** under `web/src/app/ui/`.
3. **Pages, in order:** auth (login, signup) → dashboard → support (ticket-list, thread-view, inbox-settings) → accounting (summary, agent-runs).

## 9. Accessibility

- WCAG **AA** contrast verified in **both** themes. Note: `#0066FF` is used for fills/accents and `white`-on-accent buttons; body text uses ink tokens; **text on `--mf-accent-soft` uses the darker `--mf-accent-text`** to clear AA.
- `:focus-visible` ring (`--mf-ring`) on every interactive element.
- `aria-current="page"` on the active nav link; `aria-label` on the theme toggle reflecting next state.
- Status conveyed by **text + color**, never color alone.
- Honor `prefers-reduced-motion`.

## 10. Testing plan

- **Unit (`ng test`):** one spec per kit component (variants render, a11y attributes present); `ThemeService` (localStorage persistence, `prefers-color-scheme` fallback, toggle flips `data-theme`).
- **Playwright e2e (`web/e2e`):**
  - Theme toggle: flipping sets `data-theme` and **persists across reload**.
  - Each migrated page renders in **both** themes with **zero console errors**.
  - Core flows still pass: login → dashboard, support list renders, open a thread.
  - Optional: per-theme screenshot snapshots for visual regression.
- **Real-browser verification** (Playwright MCP / gstack `$B`) of both themes before claiming done — per the automation-first rule.
- **Full repo gate** run even though no Go changed (`make test` etc. should be unaffected; confirm).

## 11. Risks & notes

- **Theme flash (FOUC):** mitigated by the pre-boot inline script (§5).
- **Broad touch surface:** migration edits every existing page `.ts` (inline styles today). Land page-by-page to keep diffs reviewable.
- **Contrast traps:** the brand blue must not be used as small body text on white; enforce via the `--mf-accent-text` token and the AA check.
- **`data-testid` continuity:** keep existing ids so current e2e selectors don't break.

## 12. Out-of-scope follow-ups (file as bd issues)

- Streams 2–4 (their own specs/plans).
- Icon system (decide: inline SVG set vs. a lightweight icon lib) — Stream 1 uses a minimal inline-SVG set; revisit if breadth grows.
