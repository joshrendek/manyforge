# UI Redesign — Stream 1 (Design System Foundation + Page Migration) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Re-skin the entire Angular 21 frontend to the approved "Light & Clean + crisp dark" design, built on a token-driven design system with a hand-built component kit and a data-driven app shell, with both themes shipped behind a persisted toggle.

**Architecture:** Semantic CSS custom-property tokens (light = `:root`, dark = `html[data-theme="dark"]`) drive everything. Pure-style primitives ship as token-based **global utility classes** (`.mf-btn`, `.mf-input`, `.mf-card`, `.mf-pill`, `.mf-table` — exactly the approved reference sheet); structural/stateful pieces ship as **standalone components** under `web/src/app/ui/` (page-header, status-pill, empty-state, spinner, toast+service, theme-toggle, app-shell/nav). Pages migrate one at a time, preserving every `data-testid` and behavior so the existing Playwright e2e suite stays green.

**Tech Stack:** Angular 21 (standalone, signals), TypeScript, plain CSS custom properties, Vitest + TestBed + HttpTestingController (unit), Playwright (e2e). No CSS framework, no component library. One new asset dep: `@fontsource-variable/inter` (self-hosted font; offline-friendly).

**Spec:** `docs/superpowers/specs/2026-06-10-ui-redesign-design-system-design.md` · **Visual reference:** `docs/superpowers/specs/assets/2026-06-10-ui-component-kit-reference.html` (open in a browser; the toggle flips every token). **bd:** `manyforge-4zs.1`. **Branch:** `ui-redesign` (already created).

---

## Conventions (read once)

- **TDD loop per task:** write failing test → run it, confirm it fails → minimal implementation → run, confirm pass → commit.
- **Unit test command:** `cd web && npm test` (Vitest via `@angular/build:unit-test`, auto-discovers `*.spec.ts`). Pattern = the existing `web/src/app/app.spec.ts` (Vitest imports + `TestBed` + `HttpTestingController`, `localStorage.clear()` in `beforeEach`/`afterEach`).
- **E2E command:** `cd web && npx playwright test` (needs the dev server on `:4300` — `npm start -- --port 4300` in another terminal; APIs are route-mocked inside specs, no backend needed).
- **Commits:** `git commit --no-verify` (skip hooks for intermediate commits). **NEVER add a `Co-Authored-By` trailer** (hard user rule). Stage exact paths shown.
- **Theme in unit tests:** set/reset with `document.documentElement.setAttribute('data-theme', 'dark'|'light')` in the test; restore to `'light'` in `afterEach`.
- **Golden rule for migrations:** read the live page file first; **preserve every existing `data-testid` verbatim** and every behavior/flow — you are restyling markup, not changing logic. The existing e2e specs (`foundation`, `support`, `accounting`, `us1`, `us2`, `polish`, `shell`, `flows-seeded`) are the regression net and select by those testids.

---

## File Structure

**Create:**
- `web/src/app/core/theme.service.ts` — theme signal, persistence, system-pref fallback, applies `data-theme`.
- `web/src/app/ui/status.ts` — pure tone-mapping helpers (`ticketStatusTone`, `ticketPriorityTone`, `runStatusTone`) + labels.
- `web/src/app/ui/status-pill/status-pill.ts` — `mf-status-pill` (tone + label).
- `web/src/app/ui/page-header/page-header.ts` — `mf-page-header` (title/subtitle + actions slot).
- `web/src/app/ui/empty-state/empty-state.ts` — `mf-empty-state` (icon/title + body/action slots).
- `web/src/app/ui/spinner/spinner.ts` — `mf-spinner`.
- `web/src/app/ui/toast/toast.service.ts` — `ToastService` (success/error, `toasts` signal, auto-dismiss).
- `web/src/app/ui/toast/toast.ts` — `mf-toast-host` (renders the stack).
- `web/src/app/ui/theme-toggle/theme-toggle.ts` — `mf-theme-toggle` (sun/moon button → `ThemeService`).
- `web/src/app/ui/nav.ts` — `NAV_ITEMS` data + `NavItem` type.
- Spec files alongside each of the above (`*.spec.ts`).
- `web/e2e/theme.spec.ts` — theme toggle + both-theme render e2e.

**Modify:**
- `web/src/styles.css` — replace token layer + add `.mf-*` utility classes (both themes).
- `web/src/index.html` — Inter font + no-FOUC pre-boot theme script.
- `web/package.json` — add `@fontsource-variable/inter`.
- `web/src/app/app.ts`, `app.html`, `app.css` — rebuild as the data-driven shell + toggle + toast host.
- `web/src/app/app.spec.ts` — extend for nav items + Accounting link + theme.
- All 8 page `.ts` files (Phase 3), each restyled to `.mf-*` + kit components.

---

## Phase 0 — Foundation (tokens, font, theme service)

### Task 1: Install + load the Inter variable font

**Files:**
- Modify: `web/package.json`
- Modify: `web/src/styles.css` (top of file)

- [ ] **Step 1: Install the font package**

Run: `cd web && npm i @fontsource-variable/inter`
Expected: adds `@fontsource-variable/inter` to `dependencies`, no errors.

- [ ] **Step 2: Import it at the top of `web/src/styles.css`**

Add as the very first line (above everything):

```css
@import '@fontsource-variable/inter';
```

> Zero-dep alternative if you'd rather not add the package: put `<link rel="preconnect" href="https://fonts.googleapis.com"><link href="https://fonts.googleapis.com/css2?family=Inter:wght@400..750&display=swap" rel="stylesheet">` in `index.html` `<head>` and skip the npm install. The plan assumes the self-hosted package.

- [ ] **Step 3: Build to verify the import resolves**

Run: `cd web && npm run build`
Expected: build succeeds (font assets bundled), no "Can't resolve" error.

- [ ] **Step 4: Commit**

```bash
git add web/package.json web/package-lock.json web/src/styles.css
git commit --no-verify -m "feat(ui): self-host Inter variable font (manyforge-4zs.1)"
```

### Task 2: Replace the token layer + add `.mf-*` utility classes in `styles.css`

**Files:**
- Modify: `web/src/styles.css` (replace the entire token + base layer; keep the legacy classes below it for now — they're removed in Phase 4)

- [ ] **Step 1: Replace the top of `styles.css` (the `:root{…}` block and `html,body{…}`) with the new token system + utilities.**

Keep the `@import` line from Task 1 first, then this block, then leave all existing legacy classes (`.topbar`, `.card`, `.tree`, `.biz`, `.pill`, etc.) untouched below it for now:

```css
:root {
  --mf-bg:#f6f7f9; --mf-surface:#ffffff; --mf-surface-2:#f1f3f7; --mf-surface-inset:#ffffff;
  --mf-border:#e6e8ee; --mf-border-strong:#d4d8e0;
  --mf-text:#0f172a; --mf-text-muted:#64748b; --mf-text-faint:#94a3b8; --mf-text-on-accent:#ffffff;
  --mf-accent:#0066ff; --mf-accent-hover:#0052cc; --mf-accent-soft:#e8f0ff; --mf-accent-text:#0052cc;
  --mf-danger:#dc2626; --mf-danger-soft:#fef2f2; --mf-danger-text:#b91c1c;
  --mf-warn:#d97706; --mf-warn-soft:#fffbeb; --mf-warn-text:#b45309;
  --mf-success:#16a34a; --mf-success-soft:#f0fdf4; --mf-success-text:#15803d;
  --mf-info:#0066ff; --mf-info-soft:#e8f0ff; --mf-info-text:#0052cc;
  --mf-ring:0 0 0 3px rgba(0,102,255,.30);
  --mf-shadow-sm:0 1px 2px rgba(16,24,40,.06); --mf-shadow:0 4px 16px -6px rgba(16,24,40,.12);
  --mf-radius:12px; --mf-radius-sm:8px; --mf-radius-pill:999px;
  --mf-space-1:4px; --mf-space-2:8px; --mf-space-3:12px; --mf-space-4:16px;
  --mf-space-5:20px; --mf-space-6:24px; --mf-space-7:32px; --mf-space-8:40px;
  --mf-fs-xs:12px; --mf-fs-sm:13px; --mf-fs-base:14px; --mf-fs-md:15px;
  --mf-fs-lg:17px; --mf-fs-xl:20px; --mf-fs-2xl:24px;
  --mf-font:'Inter Variable','Inter',ui-sans-serif,system-ui,-apple-system,'Segoe UI',Roboto,sans-serif;
}
html[data-theme="dark"] {
  --mf-bg:#0a0a0c; --mf-surface:#131418; --mf-surface-2:#1b1d24; --mf-surface-inset:#0f1014;
  --mf-border:#232530; --mf-border-strong:#313340;
  --mf-text:#f3f4f6; --mf-text-muted:#9ca3af; --mf-text-faint:#6b7280; --mf-text-on-accent:#ffffff;
  --mf-accent:#3d8bff; --mf-accent-hover:#5a9cff; --mf-accent-soft:rgba(0,102,255,.18); --mf-accent-text:#bcd6ff;
  --mf-danger:#f87171; --mf-danger-soft:rgba(248,113,113,.14); --mf-danger-text:#fca5a5;
  --mf-warn:#f59e0b; --mf-warn-soft:rgba(245,158,11,.14); --mf-warn-text:#fcd34d;
  --mf-success:#22c55e; --mf-success-soft:rgba(34,197,94,.14); --mf-success-text:#4ade80;
  --mf-info:#3d8bff; --mf-info-soft:rgba(0,102,255,.18); --mf-info-text:#bcd6ff;
  --mf-ring:0 0 0 3px rgba(61,139,255,.35);
  --mf-shadow-sm:0 1px 2px rgba(0,0,0,.4); --mf-shadow:0 16px 40px -22px rgba(0,0,0,.7);
}
.mf-app { background:var(--mf-bg); color:var(--mf-text); font-family:var(--mf-font);
  -webkit-font-smoothing:antialiased; min-height:100vh; }

/* primitives (token-only) */
.mf-btn{font:inherit;font-size:var(--mf-fs-base);font-weight:600;padding:9px 14px;border-radius:var(--mf-radius-sm);border:1px solid transparent;cursor:pointer;transition:background .15s,filter .15s,box-shadow .15s,border-color .15s;display:inline-flex;align-items:center;gap:7px}
.mf-btn:focus-visible{outline:none;box-shadow:var(--mf-ring)}
.mf-btn-primary{background:var(--mf-accent);color:var(--mf-text-on-accent)}
.mf-btn-primary:hover{background:var(--mf-accent-hover)}
.mf-btn-ghost{background:transparent;border-color:var(--mf-border);color:var(--mf-text)}
.mf-btn-ghost:hover{background:var(--mf-surface-2);border-color:var(--mf-border-strong)}
.mf-btn-danger{background:var(--mf-danger);color:#fff}
.mf-btn-danger:hover{filter:brightness(1.06)}
.mf-btn-link{background:none;border:0;color:var(--mf-accent-text);padding:6px 4px;font-weight:500}
.mf-btn-link:hover{text-decoration:underline}
.mf-btn:disabled{opacity:.5;cursor:default;filter:none}
.mf-btn-sm{font-size:var(--mf-fs-sm);padding:6px 11px}

.mf-field{display:block}
.mf-field label{display:block;font-size:var(--mf-fs-sm);color:var(--mf-text-muted);font-weight:500;margin-bottom:6px}
.mf-input,.mf-select,.mf-textarea{width:100%;font:inherit;font-size:var(--mf-fs-base);padding:10px 12px;background:var(--mf-surface-inset);border:1px solid var(--mf-border);border-radius:var(--mf-radius-sm);color:var(--mf-text);outline:none;transition:border-color .15s,box-shadow .15s}
.mf-input::placeholder{color:var(--mf-text-faint)}
.mf-input:hover,.mf-select:hover{border-color:var(--mf-border-strong)}
.mf-input:focus,.mf-select:focus,.mf-textarea:focus{border-color:var(--mf-accent);box-shadow:var(--mf-ring)}
.mf-field .mf-hint{font-size:var(--mf-fs-xs);color:var(--mf-text-faint);margin-top:6px}
.mf-field.mf-invalid .mf-input{border-color:var(--mf-danger)}
.mf-field .mf-err{font-size:var(--mf-fs-xs);color:var(--mf-danger-text);margin-top:6px}

.mf-card{background:var(--mf-surface);border:1px solid var(--mf-border);border-radius:var(--mf-radius);padding:22px;box-shadow:var(--mf-shadow-sm)}

.mf-pill{display:inline-flex;align-items:center;gap:5px;font-size:11px;font-weight:700;letter-spacing:.03em;text-transform:uppercase;padding:3px 9px;border-radius:var(--mf-radius-pill)}
.mf-pill .mf-dot{width:5px;height:5px;border-radius:50%;background:currentColor}
.mf-pill-neutral{color:var(--mf-text-muted);border:1px solid var(--mf-border)}
.mf-pill-accent{color:var(--mf-accent-text);background:var(--mf-accent-soft)}
.mf-pill-danger{color:var(--mf-danger-text);background:var(--mf-danger-soft)}
.mf-pill-warn{color:var(--mf-warn-text);background:var(--mf-warn-soft)}
.mf-pill-success{color:var(--mf-success-text);background:var(--mf-success-soft)}

.mf-table{background:var(--mf-surface);border:1px solid var(--mf-border);border-radius:var(--mf-radius);overflow:hidden}
.mf-tr{display:flex;align-items:center;gap:12px;padding:11px 14px;border-bottom:1px solid var(--mf-border);font-size:var(--mf-fs-base)}
.mf-tr:last-child{border-bottom:0}
.mf-tr.mf-th{background:var(--mf-surface-2);font-size:var(--mf-fs-xs);text-transform:uppercase;letter-spacing:.04em;color:var(--mf-text-faint);font-weight:600}
.mf-tr.mf-clickable{cursor:pointer}
.mf-tr.mf-clickable:hover{background:var(--mf-surface-2)}

a{color:var(--mf-accent-text);text-decoration:none}
a:hover{text-decoration:underline}
@media (prefers-reduced-motion: reduce){*{transition:none !important}}
```

- [ ] **Step 2: Build to verify CSS compiles**

Run: `cd web && npm run build`
Expected: build succeeds, no CSS errors.

- [ ] **Step 3: Commit**

```bash
git add web/src/styles.css
git commit --no-verify -m "feat(ui): design tokens (light+dark) + .mf-* utility primitives (manyforge-4zs.1)"
```

### Task 3: ThemeService (TDD)

**Files:**
- Create: `web/src/app/core/theme.service.ts`
- Test: `web/src/app/core/theme.service.spec.ts`

- [ ] **Step 1: Write the failing test**

```typescript
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ThemeService } from './theme.service';

describe('ThemeService', () => {
  beforeEach(() => { localStorage.clear(); document.documentElement.removeAttribute('data-theme'); });
  afterEach(() => { localStorage.clear(); document.documentElement.setAttribute('data-theme', 'light'); });

  it('defaults to light when nothing saved and system is not dark', () => {
    const svc = TestBed.inject(ThemeService);
    expect(svc.theme()).toBe('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('reads a saved theme from localStorage', () => {
    localStorage.setItem('mf-theme', 'dark');
    const svc = TestBed.inject(ThemeService);
    expect(svc.theme()).toBe('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
  });

  it('toggle() flips the theme and persists it', () => {
    const svc = TestBed.inject(ThemeService);
    svc.toggle();
    TestBed.flushEffects?.();
    expect(svc.theme()).toBe('dark');
    expect(localStorage.getItem('mf-theme')).toBe('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
  });
});
```

- [ ] **Step 2: Run, confirm it fails**

Run: `cd web && npm test`
Expected: FAIL — `Cannot find module './theme.service'`.

- [ ] **Step 3: Implement `theme.service.ts`**

```typescript
import { Injectable, effect, signal } from '@angular/core';

export type Theme = 'light' | 'dark';
const KEY = 'mf-theme';

@Injectable({ providedIn: 'root' })
export class ThemeService {
  readonly theme = signal<Theme>(this.initial());

  constructor() {
    effect(() => {
      const t = this.theme();
      document.documentElement.setAttribute('data-theme', t);
      try { localStorage.setItem(KEY, t); } catch { /* ignore */ }
    });
  }

  private initial(): Theme {
    let saved: string | null = null;
    try { saved = localStorage.getItem(KEY); } catch { /* ignore */ }
    if (saved === 'light' || saved === 'dark') return saved;
    const prefersDark = typeof window !== 'undefined'
      && window.matchMedia?.('(prefers-color-scheme: dark)').matches;
    return prefersDark ? 'dark' : 'light';
  }

  toggle(): void { this.theme.set(this.theme() === 'dark' ? 'light' : 'dark'); }
  setTheme(t: Theme): void { this.theme.set(t); }
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `cd web && npm test`
Expected: PASS (ThemeService 3 tests). If `TestBed.flushEffects` is unavailable in this Angular version, replace with `TestBed.tick()`; if neither exists, the constructor effect runs on injection — assert after `svc.toggle()` directly.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/core/theme.service.ts web/src/app/core/theme.service.spec.ts
git commit --no-verify -m "feat(ui): ThemeService — persisted light/dark with system-pref fallback (manyforge-4zs.1)"
```

### Task 4: No-FOUC pre-boot theme script in `index.html`

**Files:**
- Modify: `web/src/index.html`

- [ ] **Step 1: Add the pre-boot script to `<head>` (before `</head>`), so first paint matches the stored theme**

```html
  <script>
    (function () {
      try {
        var t = localStorage.getItem('mf-theme');
        if (t !== 'light' && t !== 'dark') {
          t = (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) ? 'dark' : 'light';
        }
        document.documentElement.setAttribute('data-theme', t);
      } catch (e) {}
    })();
  </script>
```

- [ ] **Step 2: Build to verify it serves**

Run: `cd web && npm run build`
Expected: build succeeds; `dist/.../index.html` contains the script.

- [ ] **Step 3: Commit**

```bash
git add web/src/index.html
git commit --no-verify -m "feat(ui): no-FOUC pre-boot theme script (manyforge-4zs.1)"
```

---

## Phase 1 — Component kit (`web/src/app/ui/`)

> Pattern for every component spec: Vitest + `TestBed.createComponent`, assert rendered markup/classes/testids. Standalone components import only what they use.

### Task 5: Status tone helpers (`ui/status.ts`) (TDD)

**Files:**
- Create: `web/src/app/ui/status.ts`
- Test: `web/src/app/ui/status.spec.ts`

- [ ] **Step 1: Failing test**

```typescript
import { describe, expect, it } from 'vitest';
import { ticketPriorityTone, ticketStatusTone, runStatusTone, Tone } from './status';

describe('status tone helpers', () => {
  it('maps ticket status to a tone', () => {
    expect(ticketStatusTone('new')).toBe<Tone>('accent');
    expect(ticketStatusTone('open')).toBe<Tone>('accent');
    expect(ticketStatusTone('pending')).toBe<Tone>('warn');
    expect(ticketStatusTone('solved')).toBe<Tone>('success');
    expect(ticketStatusTone('closed')).toBe<Tone>('neutral');
  });
  it('maps priority to a tone', () => {
    expect(ticketPriorityTone('urgent')).toBe<Tone>('danger');
    expect(ticketPriorityTone('high')).toBe<Tone>('warn');
    expect(ticketPriorityTone('normal')).toBe<Tone>('neutral');
    expect(ticketPriorityTone('low')).toBe<Tone>('neutral');
  });
  it('maps run status to a tone', () => {
    expect(runStatusTone('succeeded')).toBe<Tone>('success');
    expect(runStatusTone('failed')).toBe<Tone>('danger');
    expect(runStatusTone('running')).toBe<Tone>('accent');
  });
});
```

- [ ] **Step 2: Run, confirm fail** — Run: `cd web && npm test` — Expected: FAIL (module missing).

- [ ] **Step 3: Implement `ui/status.ts`**

```typescript
export type Tone = 'neutral' | 'accent' | 'warn' | 'success' | 'danger';

export function ticketStatusTone(s: string): Tone {
  switch (s) {
    case 'new': case 'open': return 'accent';
    case 'pending': return 'warn';
    case 'solved': return 'success';
    case 'closed': default: return 'neutral';
  }
}
export function ticketPriorityTone(p: string): Tone {
  switch (p) {
    case 'urgent': return 'danger';
    case 'high': return 'warn';
    default: return 'neutral';
  }
}
export function runStatusTone(s: string): Tone {
  switch (s) {
    case 'succeeded': case 'completed': return 'success';
    case 'failed': case 'error': return 'danger';
    case 'running': case 'pending': return 'accent';
    default: return 'neutral';
  }
}
```

- [ ] **Step 4: Run, confirm pass** — Run: `cd web && npm test` — Expected: PASS.
- [ ] **Step 5: Commit**

```bash
git add web/src/app/ui/status.ts web/src/app/ui/status.spec.ts
git commit --no-verify -m "feat(ui): status tone helpers (manyforge-4zs.1)"
```

### Task 6: `mf-status-pill` (TDD)

**Files:**
- Create: `web/src/app/ui/status-pill/status-pill.ts`
- Test: `web/src/app/ui/status-pill/status-pill.spec.ts`

- [ ] **Step 1: Failing test**

```typescript
import { Component } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { StatusPill } from './status-pill';

@Component({ standalone: true, imports: [StatusPill],
  template: `<mf-status-pill tone="danger" label="Urgent" data-testid="p" />` })
class Host {}

describe('mf-status-pill', () => {
  it('renders label and tone class', () => {
    const f = TestBed.createComponent(Host); f.detectChanges();
    const el: HTMLElement = f.nativeElement.querySelector('[data-testid="p"]');
    expect(el.textContent?.trim()).toContain('Urgent');
    expect(el.querySelector('.mf-pill-danger')).toBeTruthy();
  });
});
```

- [ ] **Step 2: Run, confirm fail.** Run: `cd web && npm test` — Expected: FAIL.
- [ ] **Step 3: Implement**

```typescript
import { Component, Input } from '@angular/core';
import { Tone } from '../status';

@Component({
  selector: 'mf-status-pill',
  standalone: true,
  template: `<span class="mf-pill mf-pill-{{ tone }}"><span class="mf-dot"></span>{{ label }}</span>`,
})
export class StatusPill {
  @Input() tone: Tone = 'neutral';
  @Input() label = '';
}
```

- [ ] **Step 4: Run, confirm pass.** Run: `cd web && npm test` — Expected: PASS.
- [ ] **Step 5: Commit** — `git add web/src/app/ui/status-pill && git commit --no-verify -m "feat(ui): mf-status-pill (manyforge-4zs.1)"`

### Task 7: `mf-page-header` (TDD)

**Files:** Create `web/src/app/ui/page-header/page-header.ts` + `.spec.ts`

- [ ] **Step 1: Failing test**

```typescript
import { Component } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { PageHeader } from './page-header';

@Component({ standalone: true, imports: [PageHeader],
  template: `<mf-page-header title="Support" subtitle="12 open"><button actions data-testid="a">New</button></mf-page-header>` })
class Host {}

describe('mf-page-header', () => {
  it('renders title, subtitle and projected actions', () => {
    const f = TestBed.createComponent(Host); f.detectChanges();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('h1')?.textContent).toContain('Support');
    expect(el.textContent).toContain('12 open');
    expect(el.querySelector('[data-testid="a"]')).toBeTruthy();
  });
});
```

- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement**

```typescript
import { Component, Input } from '@angular/core';

@Component({
  selector: 'mf-page-header',
  standalone: true,
  template: `
    <header class="mf-pageheader">
      <div>
        <h1>{{ title }}</h1>
        @if (subtitle) { <div class="mf-pageheader-sub">{{ subtitle }}</div> }
      </div>
      <div class="mf-pageheader-actions"><ng-content select="[actions]" /></div>
    </header>`,
  styles: [`
    .mf-pageheader{display:flex;justify-content:space-between;align-items:flex-start;gap:16px;margin-bottom:20px}
    h1{font-size:var(--mf-fs-2xl);font-weight:680;letter-spacing:-.025em;margin:0}
    .mf-pageheader-sub{color:var(--mf-text-muted);font-size:var(--mf-fs-sm);margin-top:3px}
    .mf-pageheader-actions{display:flex;gap:8px;align-items:center}
  `],
})
export class PageHeader {
  @Input() title = '';
  @Input() subtitle = '';
}
```

- [ ] **Step 4: Run, confirm pass.**
- [ ] **Step 5: Commit** — `git add web/src/app/ui/page-header && git commit --no-verify -m "feat(ui): mf-page-header (manyforge-4zs.1)"`

### Task 8: `mf-empty-state` (TDD)

**Files:** Create `web/src/app/ui/empty-state/empty-state.ts` + `.spec.ts`

- [ ] **Step 1: Failing test**

```typescript
import { Component } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { EmptyState } from './empty-state';

@Component({ standalone: true, imports: [EmptyState],
  template: `<mf-empty-state icon="✦" title="No tickets yet" data-testid="e">Nothing here.</mf-empty-state>` })
class Host {}

describe('mf-empty-state', () => {
  it('renders icon, title and projected body', () => {
    const f = TestBed.createComponent(Host); f.detectChanges();
    const el: HTMLElement = f.nativeElement.querySelector('[data-testid="e"]');
    expect(el.textContent).toContain('No tickets yet');
    expect(el.textContent).toContain('Nothing here.');
  });
});
```

- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement**

```typescript
import { Component, Input } from '@angular/core';

@Component({
  selector: 'mf-empty-state',
  standalone: true,
  template: `
    <div class="mf-empty">
      @if (icon) { <div class="mf-empty-ico">{{ icon }}</div> }
      <div>
        <b>{{ title }}</b>
        <div class="mf-empty-body"><ng-content /></div>
      </div>
      <ng-content select="[action]" />
    </div>`,
  styles: [`
    .mf-empty{display:flex;flex-direction:column;align-items:center;gap:10px;padding:34px;color:var(--mf-text-muted);text-align:center}
    .mf-empty-ico{width:42px;height:42px;border-radius:12px;background:var(--mf-accent-soft);display:flex;align-items:center;justify-content:center;font-size:20px;color:var(--mf-accent-text)}
    .mf-empty-body{font-size:var(--mf-fs-sm);color:var(--mf-text-muted)}
  `],
})
export class EmptyState {
  @Input() icon = '';
  @Input() title = '';
}
```

- [ ] **Step 4: Run, confirm pass.**
- [ ] **Step 5: Commit** — `git add web/src/app/ui/empty-state && git commit --no-verify -m "feat(ui): mf-empty-state (manyforge-4zs.1)"`

### Task 9: `mf-spinner` (TDD)

**Files:** Create `web/src/app/ui/spinner/spinner.ts` + `.spec.ts`

- [ ] **Step 1: Failing test**

```typescript
import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { Spinner } from './spinner';

describe('mf-spinner', () => {
  it('renders an aria-busy element', () => {
    const f = TestBed.createComponent(Spinner); f.detectChanges();
    expect(f.nativeElement.querySelector('[aria-busy="true"]')).toBeTruthy();
  });
});
```

- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement**

```typescript
import { Component } from '@angular/core';

@Component({
  selector: 'mf-spinner',
  standalone: true,
  template: `<span class="mf-spinner" role="status" aria-busy="true" aria-label="Loading"></span>`,
  styles: [`
    .mf-spinner{display:inline-block;width:16px;height:16px;border:2px solid var(--mf-border-strong);border-top-color:var(--mf-accent);border-radius:50%;animation:mf-spin .6s linear infinite}
    @keyframes mf-spin{to{transform:rotate(360deg)}}
    @media (prefers-reduced-motion: reduce){.mf-spinner{animation-duration:1.6s}}
  `],
})
export class Spinner {}
```

- [ ] **Step 4: Run, confirm pass.**
- [ ] **Step 5: Commit** — `git add web/src/app/ui/spinner && git commit --no-verify -m "feat(ui): mf-spinner (manyforge-4zs.1)"`

### Task 10: `ToastService` (TDD)

**Files:** Create `web/src/app/ui/toast/toast.service.ts` + `web/src/app/ui/toast/toast.service.spec.ts`

- [ ] **Step 1: Failing test**

```typescript
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ToastService } from './toast.service';

describe('ToastService', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('adds a success toast then auto-dismisses', () => {
    const svc = TestBed.inject(ToastService);
    svc.success('Saved');
    expect(svc.toasts().length).toBe(1);
    expect(svc.toasts()[0]).toMatchObject({ kind: 'success', message: 'Saved' });
    vi.advanceTimersByTime(5000);
    expect(svc.toasts().length).toBe(0);
  });

  it('adds an error toast', () => {
    const svc = TestBed.inject(ToastService);
    svc.error('Boom');
    expect(svc.toasts()[0].kind).toBe('error');
  });
});
```

- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement**

```typescript
import { Injectable, signal } from '@angular/core';

export interface Toast { id: number; kind: 'success' | 'error'; message: string; }

@Injectable({ providedIn: 'root' })
export class ToastService {
  readonly toasts = signal<Toast[]>([]);
  private seq = 0;

  success(message: string) { this.push('success', message); }
  error(message: string) { this.push('error', message); }

  dismiss(id: number) { this.toasts.update((t) => t.filter((x) => x.id !== id)); }

  private push(kind: Toast['kind'], message: string) {
    const id = ++this.seq;
    this.toasts.update((t) => [...t, { id, kind, message }]);
    setTimeout(() => this.dismiss(id), 5000);
  }
}
```

- [ ] **Step 4: Run, confirm pass.**
- [ ] **Step 5: Commit** — `git add web/src/app/ui/toast/toast.service.ts web/src/app/ui/toast/toast.service.spec.ts && git commit --no-verify -m "feat(ui): ToastService (manyforge-4zs.1)"`

### Task 11: `mf-toast-host` (TDD)

**Files:** Create `web/src/app/ui/toast/toast.ts` + `web/src/app/ui/toast/toast.spec.ts`

- [ ] **Step 1: Failing test**

```typescript
import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { ToastHost } from './toast';
import { ToastService } from './toast.service';

describe('mf-toast-host', () => {
  it('renders queued toasts with testids', () => {
    const f = TestBed.createComponent(ToastHost);
    const svc = TestBed.inject(ToastService);
    svc.success('Hi'); f.detectChanges();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('[data-testid="toast"]')?.textContent).toContain('Hi');
  });
});
```

- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement**

```typescript
import { Component, inject } from '@angular/core';
import { ToastService } from './toast.service';

@Component({
  selector: 'mf-toast-host',
  standalone: true,
  template: `
    <div class="mf-toast-stack" aria-live="polite">
      @for (t of toasts.toasts(); track t.id) {
        <div class="mf-toast" [class.mf-toast-err]="t.kind === 'error'" data-testid="toast">
          <span>{{ t.kind === 'error' ? '⚠' : '✓' }}</span><span>{{ t.message }}</span>
          <button class="mf-toast-x" (click)="toasts.dismiss(t.id)" aria-label="Dismiss">×</button>
        </div>
      }
    </div>`,
  styles: [`
    .mf-toast-stack{position:fixed;right:20px;bottom:20px;display:flex;flex-direction:column;gap:10px;z-index:50}
    .mf-toast{display:flex;align-items:center;gap:10px;background:var(--mf-surface);border:1px solid var(--mf-border);border-left:3px solid var(--mf-success);border-radius:var(--mf-radius-sm);padding:11px 14px;box-shadow:var(--mf-shadow);font-size:var(--mf-fs-sm);color:var(--mf-text);max-width:360px}
    .mf-toast-err{border-left-color:var(--mf-danger)}
    .mf-toast-x{margin-left:auto;background:none;border:0;color:var(--mf-text-faint);cursor:pointer;font-size:16px;line-height:1}
  `],
})
export class ToastHost {
  readonly toasts = inject(ToastService);
}
```

- [ ] **Step 4: Run, confirm pass.**
- [ ] **Step 5: Commit** — `git add web/src/app/ui/toast/toast.ts web/src/app/ui/toast/toast.spec.ts && git commit --no-verify -m "feat(ui): mf-toast-host (manyforge-4zs.1)"`

### Task 12: `mf-theme-toggle` (TDD)

**Files:** Create `web/src/app/ui/theme-toggle/theme-toggle.ts` + `.spec.ts`

- [ ] **Step 1: Failing test**

```typescript
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ThemeToggle } from './theme-toggle';
import { ThemeService } from '../../core/theme.service';

describe('mf-theme-toggle', () => {
  beforeEach(() => { localStorage.clear(); });
  afterEach(() => { localStorage.clear(); document.documentElement.setAttribute('data-theme', 'light'); });

  it('toggles the theme on click', () => {
    const f = TestBed.createComponent(ThemeToggle);
    const theme = TestBed.inject(ThemeService); f.detectChanges();
    const btn: HTMLButtonElement = f.nativeElement.querySelector('[data-testid="theme-toggle"]');
    const before = theme.theme();
    btn.click(); f.detectChanges();
    expect(theme.theme()).not.toBe(before);
  });
});
```

- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement**

```typescript
import { Component, inject } from '@angular/core';
import { ThemeService } from '../../core/theme.service';

@Component({
  selector: 'mf-theme-toggle',
  standalone: true,
  template: `
    <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="theme-toggle"
      [attr.aria-label]="theme.theme() === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'"
      (click)="theme.toggle()">{{ theme.theme() === 'dark' ? '☀' : '☾' }}</button>`,
})
export class ThemeToggle {
  readonly theme = inject(ThemeService);
}
```

- [ ] **Step 4: Run, confirm pass.**
- [ ] **Step 5: Commit** — `git add web/src/app/ui/theme-toggle && git commit --no-verify -m "feat(ui): mf-theme-toggle (manyforge-4zs.1)"`

### Task 13: Nav config (`ui/nav.ts`) (TDD)

**Files:** Create `web/src/app/ui/nav.ts` + `web/src/app/ui/nav.spec.ts`

- [ ] **Step 1: Failing test**

```typescript
import { describe, expect, it } from 'vitest';
import { NAV_ITEMS } from './nav';

describe('NAV_ITEMS', () => {
  it('includes dashboard, support and accounting with testids', () => {
    const routes = NAV_ITEMS.map((n) => n.route);
    expect(routes).toEqual(['/dashboard', '/support', '/accounting']);
    expect(NAV_ITEMS.find((n) => n.route === '/accounting')?.testid).toBe('nav-accounting');
  });
});
```

- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement**

```typescript
export interface NavItem { label: string; route: string; testid: string; badge?: number; }

export const NAV_ITEMS: NavItem[] = [
  { label: 'Dashboard', route: '/dashboard', testid: 'nav-dashboard' },
  { label: 'Support', route: '/support', testid: 'nav-support' },
  { label: 'Accounting', route: '/accounting', testid: 'nav-accounting' },
];
```

- [ ] **Step 4: Run, confirm pass.**
- [ ] **Step 5: Commit** — `git add web/src/app/ui/nav.ts web/src/app/ui/nav.spec.ts && git commit --no-verify -m "feat(ui): data-driven nav config (manyforge-4zs.1)"`

---

## Phase 2 — App shell

### Task 14: Rebuild the app shell (`app.ts` / `app.html` / `app.css`) + extend `app.spec.ts`

**Files:**
- Modify: `web/src/app/app.ts`, `web/src/app/app.html`, `web/src/app/app.css`
- Modify: `web/src/app/app.spec.ts`

**Behavior to preserve (from current shell):** authenticated view shows a sidebar (`data-testid="app-sidebar"`) with nav + identity (`data-testid="sidebar-identity"`) + sign-out (`data-testid="sign-out"`); unauthenticated view shows a bare top bar; root calls `/api/v1/me` when authenticated; `showShell()` gating; existing `nav-dashboard` / `nav-support` testids. **Add:** `nav-accounting` (via `NAV_ITEMS`), the `mf-theme-toggle`, the `mf-toast-host`, and wrap everything in `.mf-app`.

- [ ] **Step 1: Update `app.spec.ts` — add failing assertions for the new nav + toggle**

Add these tests to the existing `describe`:

```typescript
  it('renders all nav items including Accounting', () => {
    localStorage.setItem('mf_access', 'tok');
    const f = TestBed.createComponent(App); f.detectChanges();
    mock.expectOne('/api/v1/me').flush({ id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' });
    f.detectChanges();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('[data-testid="nav-dashboard"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="nav-support"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="nav-accounting"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="theme-toggle"]')).toBeTruthy();
  });
```

- [ ] **Step 2: Run, confirm fail.** Run: `cd web && npm test` — Expected: FAIL (`nav-accounting`/`theme-toggle` null).

- [ ] **Step 3: Rewrite `app.html`** (data-driven nav + toggle + toast host; `.mf-*` classes)

```html
<div class="mf-app">
@if (showShell()) {
  <div class="shell">
    <aside class="sidebar" data-testid="app-sidebar">
      <div class="sidebar-brand">Many<span class="brand-accent">Forge</span></div>
      <nav class="sidebar-nav">
        @for (item of navItems; track item.route) {
          <a [routerLink]="item.route" routerLinkActive="active" [attr.data-testid]="item.testid"
             [attr.aria-current]="rla.isActive ? 'page' : null" #rla="routerLinkActive">
            {{ item.label }}
            @if (item.badge) { <span class="nav-badge">{{ item.badge }}</span> }
          </a>
        }
      </nav>
      <div class="sidebar-foot">
        @if (profile(); as p) {
          <p class="profile" data-testid="sidebar-identity">
            Signed in as <b>{{ p.display_name }}</b><br /><span class="faint">{{ p.email }}</span>
          </p>
        }
        <div class="sidebar-foot-row">
          <mf-theme-toggle />
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="logout()" data-testid="sign-out">Sign out</button>
        </div>
      </div>
    </aside>
    <main class="shell-main"><router-outlet /></main>
  </div>
} @else {
  <header class="topbar">
    <span class="brand">Many<span class="brand-accent">Forge</span></span>
    <span class="tagline">all-in-one founder platform</span>
    <span style="flex:1"></span>
    <mf-theme-toggle />
  </header>
  <main class="container"><router-outlet /></main>
}
<mf-toast-host />
</div>
```

- [ ] **Step 4: Update `app.ts`** — import the new pieces, expose `navItems`

In the component's `imports` array add `RouterLink, RouterLinkActive, ThemeToggle, ToastHost`; import `NAV_ITEMS`/`NavItem` and add a field `navItems = NAV_ITEMS;`. Keep all existing logic (`showShell`, `profile`, `logout`, `/api/v1/me`). Imports:

```typescript
import { RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { NAV_ITEMS } from './ui/nav';
import { ThemeToggle } from './ui/theme-toggle/theme-toggle';
import { ToastHost } from './ui/toast/toast';
// in @Component imports: [RouterOutlet, RouterLink, RouterLinkActive, ThemeToggle, ToastHost]
// in class body: navItems = NAV_ITEMS;
```

- [ ] **Step 5: Rewrite `app.css`** — port the existing shell layout to `--mf-*` tokens; switch surfaces/borders/text to tokens; add `.brand-accent{color:var(--mf-accent)}`, `.nav-badge`, `.sidebar-foot-row{display:flex;gap:8px;align-items:center}`. Keep the `grid-template-columns:232px 1fr`, sticky sidebar, and the `@media (max-width:720px)` stacking. Replace `.sidebar-nav a.active{background:var(--mf-accent-soft);color:var(--mf-accent-text)}`. (Full file: take the current `app.css`, swap every `var(--panel|border|text|muted|faint|accent*)` → the `--mf-*` equivalent, `var(--radius-sm)`→`var(--mf-radius-sm)`.)

- [ ] **Step 6: Run, confirm pass.** Run: `cd web && npm test` — Expected: PASS (app.spec all green, including the 2 original tests).

- [ ] **Step 7: Commit**

```bash
git add web/src/app/app.ts web/src/app/app.html web/src/app/app.css web/src/app/app.spec.ts
git commit --no-verify -m "feat(ui): rebuild app shell — data-driven nav, theme toggle, toast host, Accounting link (manyforge-4zs.1)"
```

### Task 15: Switch the global app background to themed tokens

**Files:** Modify `web/src/styles.css`

- [ ] **Step 1:** Find the legacy `html, body { … background: radial-gradient(...) ... }` rule and replace its `background`/`color`/`font-family` so the page canvas follows the theme:

```css
html, body { margin:0; min-height:100%; background:var(--mf-bg); color:var(--mf-text); font-family:var(--mf-font); -webkit-font-smoothing:antialiased; }
```

- [ ] **Step 2: Run e2e smoke to confirm the app still loads in light + dark**

Start dev server (`cd web && npm start -- --port 4300`) in one terminal, then: `cd web && npx playwright test e2e/shell.spec.ts`
Expected: PASS (shell renders; testids intact).

- [ ] **Step 3: Commit** — `git add web/src/styles.css && git commit --no-verify -m "feat(ui): themed app canvas background (manyforge-4zs.1)"`

> After Task 15 the shell is fully themed; inner pages still use legacy classes (will look mismatched in light mode) until each is migrated in Phase 3. This transient state is expected; full visual verification is Phase 4.

---

## Phase 3 — Page migrations

> **Recipe for every page task:** (1) read the live file; (2) replace legacy global classes (`.card`, `.tree`, `.biz`, `.pill`, `.badge`, `.panel`, `.row`, `.spread`, `.sub`, `.hint`, `.empty`, `.topbar`, `.container`, inline `style=`/`styles:` blocks) with `.mf-*` utilities + kit components (`mf-page-header`, `mf-status-pill`, `mf-empty-state`, `mf-spinner`, and `ToastService` for transient messages); (3) **preserve every `data-testid` verbatim and every behavior**; (4) add a unit spec that mounts the component (mock its services via `HttpTestingController`) and asserts it renders key testids — run it once with `data-theme="dark"` too; (5) run unit + the page's existing e2e spec.

### Task 16: Migrate `login.ts`

**Files:** Modify `web/src/app/pages/login.ts`; Create `web/src/app/pages/login.spec.ts`

- [ ] **Step 1: Failing spec** — mount `LoginComponent` with `provideHttpClient()/Testing` + `provideRouter([])`; assert the email + password inputs render with class `mf-input`, the submit is `mf-btn mf-btn-primary`, and (set `document.documentElement.setAttribute('data-theme','dark')`) it still renders. Use `[data-testid]` you add: give the form inputs `data-testid="login-email"`, `login-password`, submit `login-submit`, error `login-error` (the page currently has none — add them).

```typescript
import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, describe, expect, it } from 'vitest';
import { LoginComponent } from './login';

describe('LoginComponent', () => {
  afterEach(() => document.documentElement.setAttribute('data-theme', 'light'));
  function mount() {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])] });
    const f = TestBed.createComponent(LoginComponent); f.detectChanges(); return f;
  }
  it('renders styled inputs and submit', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('input.mf-input[data-testid="login-email"]')).toBeTruthy();
    expect(el.querySelector('button.mf-btn-primary[data-testid="login-submit"]')).toBeTruthy();
  });
  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    expect(mount().nativeElement.querySelector('.mf-card')).toBeTruthy();
  });
});
```

- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Migrate the template** — wrap in `<div class="mf-card" style="max-width:410px;margin:8vh auto 0">`; `<h1>` stays; subtitle → `<p class="mf-pageheader-sub">` or a muted `<p>`; each control → `<div class="mf-field"><label>…</label><input class="mf-input" data-testid="login-email" …></div>`; submit → `<button class="mf-btn mf-btn-primary" data-testid="login-submit">`; error → `<p class="mf-err" data-testid="login-error">`. Keep `[(ngModel)]`, `(ngSubmit)`, `loading()`/`error()` logic unchanged. (No new logic.)
- [ ] **Step 4: Run unit, confirm pass** — `cd web && npm test`.
- [ ] **Step 5: Run e2e** — with dev server up: `cd web && npx playwright test e2e/foundation.spec.ts` (login is exercised there). If the foundation spec selects login fields by `#email`/`#password`, keep those `id`s on the inputs in addition to the new testids. Expected: PASS.
- [ ] **Step 6: Commit** — `git add web/src/app/pages/login.ts web/src/app/pages/login.spec.ts && git commit --no-verify -m "feat(ui): migrate login to design system (manyforge-4zs.1)"`

### Task 17: Migrate `signup.ts`

**Files:** Modify `web/src/app/pages/signup.ts`; Create `web/src/app/pages/signup.spec.ts`

- [ ] **Step 1: Failing spec** — mount `SignupComponent` (same providers as Task 16); assert the form step renders `mf-input` for email/displayName/password and a `mf-btn-primary` submit; assert it renders with `data-theme="dark"`. Preserve existing `#email` etc. ids used by `foundation.spec.ts`; add testids `signup-email`, `signup-display-name`, `signup-password`, `signup-submit`, `signup-token`, `signup-verify-submit`, `signup-error`.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Migrate template** — both `step()` branches: wrap in `.mf-card`; fields → `.mf-field`+`.mf-input`; buttons → `.mf-btn .mf-btn-primary`; hint → `.mf-hint`; error → `.mf-err`. Keep `step()`, `signup()`, `verify()`, signals, ids, and the login link. No logic change.
- [ ] **Step 4: Run unit, confirm pass.**
- [ ] **Step 5: Run e2e** — `npx playwright test e2e/foundation.spec.ts`. Expected: PASS.
- [ ] **Step 6: Commit** — `git add web/src/app/pages/signup.ts web/src/app/pages/signup.spec.ts && git commit --no-verify -m "feat(ui): migrate signup to design system (manyforge-4zs.1)"`

### Task 18: Migrate `dashboard.ts`

**Files:** Modify `web/src/app/pages/dashboard.ts`; Create `web/src/app/pages/dashboard.spec.ts`

- [ ] **Step 1: Failing spec** — mount `DashboardComponent`; flush `/api/v1/me` and `/api/v1/businesses` via `HttpTestingController`; assert `mf-page-header` renders the "Your businesses" title, business rows keep `data-testid="biz-row"`, the master pill uses `mf-pill-accent`, the create-master input is `mf-input`, and `nav-accounting` link is present. Add a dark-theme render assertion.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Migrate template** — top section → `<mf-card>` with `<mf-page-header title="Your businesses">` (put the "Accounting" link, keeping `data-testid="nav-accounting"`, into the `[actions]` slot); business list keeps `ul`/rows but swap `.biz`→`.mf-tr` styling and `.pill`/`.badge`→`mf-status-pill` or `.mf-pill-*`; the caret/expand stays; action buttons → `.mf-btn .mf-btn-ghost .mf-btn-sm` (destructive → `.mf-btn-danger` or `.mf-btn-link` danger); inline edit panels → `.mf-card` nested or `.mf-tr` panel with `.mf-field`/`.mf-input`/`.mf-select`; "Create a master business" → second `<mf-card>` with `mf-field`/`mf-input` + `mf-btn-primary`; empty state → `<mf-empty-state>`. Replace ad-hoc error `.msg.error` with `ToastService.error(...)` calls in the `run()` helper (inject `ToastService`), OR keep an inline `.mf-err`. **Preserve** `biz-row`, `sub-name-input`, `nav-accounting` and all behaviors (`toggle`, `createMaster/Sub`, `rename`, `move`, `archive/restore`, `doDelete`, `moveTargets`).
- [ ] **Step 4: Run unit, confirm pass.**
- [ ] **Step 5: Run e2e** — `npx playwright test e2e/foundation.spec.ts e2e/us1.spec.ts` (business tree journeys). Expected: PASS.
- [ ] **Step 6: Commit** — `git add web/src/app/pages/dashboard.ts web/src/app/pages/dashboard.spec.ts && git commit --no-verify -m "feat(ui): migrate dashboard to design system (manyforge-4zs.1)"`

### Task 19: Migrate `support/ticket-list.ts`

**Files:** Modify `web/src/app/pages/support/ticket-list.ts`; Create `web/src/app/pages/support/ticket-list.spec.ts`

- [ ] **Step 1: Failing spec** — mount; flush `/api/v1/businesses` then `/api/v1/businesses/{id}/tickets`; assert `mf-page-header` "Support", filter selects use `mf-select` (keep `business-select`/`status-filter`/`priority-filter`), `ticket-list`/`ticket-row` present, status via `mf-status-pill` (keep `ticket-status`/`ticket-priority`), empty → `mf-empty-state` (keep `ticket-empty`), `load-more` button is `mf-btn`. Dark-theme render assertion.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Migrate template** — header → `mf-page-header` (links `inbox-settings-link`, `back-to-dashboard` into `[actions]`); filters → `.mf-field`/`.mf-select`; ticket rows → `.mf-table`/`.mf-tr.mf-clickable`; status/priority → `<mf-status-pill [tone]="ticketStatusTone(t.status)" [label]="t.status" data-testid="ticket-status">` (import helpers from `ui/status`); tags → `.mf-pill-neutral`; remove the inline `styles:` block (`.ticket`, `.ticket-meta`, `.tags`, `[data-testid='load-more']`) — replace with token-based equivalents inline or a tiny `styles:` using `--mf-*`; `load-more` → `.mf-btn .mf-btn-ghost`; error → `ToastService`/`.mf-err` (keep `list-error`). Preserve every listed testid and all behaviors (`selectBusiness`, `setStatus`, `setPriority`, `reload`, `loadMore`, `open`).
- [ ] **Step 4: Run unit, confirm pass.**
- [ ] **Step 5: Run e2e** — `npx playwright test e2e/support.spec.ts e2e/flows-seeded.spec.ts`. Expected: PASS.
- [ ] **Step 6: Commit** — `git add web/src/app/pages/support/ticket-list.ts web/src/app/pages/support/ticket-list.spec.ts && git commit --no-verify -m "feat(ui): migrate ticket-list to design system (manyforge-4zs.1)"`

### Task 20: Migrate `support/thread-view.ts`

**Files:** Modify `web/src/app/pages/support/thread-view.ts`; Create `web/src/app/pages/support/thread-view.spec.ts`

> Largest page (extensive inline styles + triage + composer). Take it in careful chunks; keep ALL ~40 testids.

- [ ] **Step 1: Failing spec** — mount with route params (provide `ActivatedRoute` stub with `businessId`/`tid`); flush `/api/v1/me`, assignable-members, ticket, messages; assert `thread-header`/`thread-subject` render, triage selects use `mf-select` (keep `triage-status`/`triage-priority`), chips use `.mf-pill`, composer textarea is `mf-textarea` (keep `composer-body`/`composer-submit`), messages keep `message`/`message-direction`/`message-body`. Dark-theme render assertion.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Migrate template + styles** — port the inline `styles:` (`.thread-head`, `.triage`, `.chips`, `.message`, `.composer`, etc.) to token-based CSS (swap hardcoded colors → `--mf-*`); subject/header → `mf-page-header` or `.mf-card`; status/priority/tag badges → `mf-status-pill`/`.mf-pill-*`; triage controls → `.mf-field`/`.mf-select`/`.mf-input`; tag chips + add input keep structure with `.mf-pill` + `.mf-input`; assignee buttons → `.mf-btn .mf-btn-ghost .mf-btn-sm`; message thread → token-based `.message` cards; composer toggle buttons → `.mf-btn` (active = `.mf-btn-primary`); textarea → `.mf-textarea`; submit → `.mf-btn-primary`; errors → keep inline `.mf-err` (`triage-error`, `composer-error`). Preserve all triage/composer behaviors and the 400/409/403/404 error messaging.
- [ ] **Step 4: Run unit, confirm pass.**
- [ ] **Step 5: Run e2e** — `npx playwright test e2e/support.spec.ts e2e/us2.spec.ts e2e/flows-seeded.spec.ts`. Expected: PASS.
- [ ] **Step 6: Commit** — `git add web/src/app/pages/support/thread-view.ts web/src/app/pages/support/thread-view.spec.ts && git commit --no-verify -m "feat(ui): migrate thread-view to design system (manyforge-4zs.1)"`

### Task 21: Migrate `support/inbox-settings.ts`

**Files:** Modify `web/src/app/pages/support/inbox-settings.ts`; Create `web/src/app/pages/support/inbox-settings.spec.ts`

- [ ] **Step 1: Failing spec** — mount with `businessId` route stub; flush `/api/v1/businesses/{id}/email-domains` (+ addresses); assert add-domain form uses `mf-field`/`mf-input`/`mf-select` (keep `domain-input`/`mode-select`/`add-domain-submit`), domain rows keep `domain-row`/`domain-name`/`domain-mode`/`domain-status`, DNS challenge panel keeps its testids, address form/list testids intact. Dark-theme render assertion.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Migrate template + styles** — sections → `.mf-card`; `.section-head` → token CSS; add-forms → `.mf-field`/`.mf-input`/`.mf-select` + `.mf-btn-primary`; lists → `.mf-table`/`.mf-tr`; mode/verification/dkim/spf badges → `mf-status-pill`/`.mf-pill-*` (map verification state → tone); `.dns-challenge`/`.dns-rec` → token CSS with `code`-styled values; verify button → `.mf-btn`; errors → `.mf-err`/`ToastService`. Preserve every testid + per-action in-flight signals (`addingDomain`/`addingAddress`/`verifyingId`/`verifyHintId`) and behaviors.
- [ ] **Step 4: Run unit, confirm pass.**
- [ ] **Step 5: Run e2e** — `npx playwright test e2e/support.spec.ts` (+ any us2 inbox coverage). Expected: PASS.
- [ ] **Step 6: Commit** — `git add web/src/app/pages/support/inbox-settings.ts web/src/app/pages/support/inbox-settings.spec.ts && git commit --no-verify -m "feat(ui): migrate inbox-settings to design system (manyforge-4zs.1)"`

### Task 22: Migrate `accounting/summary.ts`

**Files:** Modify `web/src/app/pages/accounting/summary.ts`; Create `web/src/app/pages/accounting/summary.spec.ts`

- [ ] **Step 1: Failing spec** — mount; flush `/api/v1/businesses` then `/api/v1/businesses/{id}/accounting`; assert `mf-page-header` "Accounting", `business-select`/`window-select` use `mf-select`, totals cards (`total-cost`/`total-in`/`total-out`/`total-runs`) use `.mf-card`, agent rows keep `agent-row`/`agent-name`/`agent-cost`, empty → `mf-empty-state`. Dark-theme assertion.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Migrate template** — header → `mf-page-header` (back link in `[actions]`); filters → `.mf-field`/`.mf-select`; `.card.compact` totals → a flex row of `.mf-card` (compact padding); agent list → `.mf-table`/`.mf-tr.mf-clickable`; cost/budget badges → `.mf-pill-*`; `.muted`/`.ticket-meta` → token CSS; empty → `mf-empty-state`; keep `CurrencyPipe`. Preserve all testids + `selectBusiness`/`setWindow`/`reload`/`openAgent`.
- [ ] **Step 4: Run unit, confirm pass.**
- [ ] **Step 5: Run e2e** — `npx playwright test e2e/accounting.spec.ts`. Expected: PASS.
- [ ] **Step 6: Commit** — `git add web/src/app/pages/accounting/summary.ts web/src/app/pages/accounting/summary.spec.ts && git commit --no-verify -m "feat(ui): migrate accounting summary to design system (manyforge-4zs.1)"`

### Task 23: Migrate `accounting/agent-runs.ts`

**Files:** Modify `web/src/app/pages/accounting/agent-runs.ts`; Create `web/src/app/pages/accounting/agent-runs.spec.ts`

- [ ] **Step 1: Failing spec** — mount with `businessId`/`agentId` route stub; flush `/api/v1/businesses/{id}/agents/{agentId}/runs`; assert `mf-page-header` "Agent runs", run rows keep `run-row`/`run-status`/`run-cost`, status via `mf-status-pill` (`runStatusTone`), `load-more` is `mf-btn`, empty → `mf-empty-state`. Dark-theme assertion.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Migrate template** — header → `mf-page-header` (back link in `[actions]`); run list → `.mf-table`/`.mf-tr`; status → `<mf-status-pill [tone]="runStatusTone(r.status)" [label]="r.status" data-testid="run-status">`; `.ticket-meta` → token CSS; `load-more` → `.mf-btn-ghost`; empty → `mf-empty-state`; keep `CurrencyPipe`/`DatePipe`. Preserve testids + `reload`/`loadMore`.
- [ ] **Step 4: Run unit, confirm pass.**
- [ ] **Step 5: Run e2e** — `npx playwright test e2e/accounting.spec.ts`. Expected: PASS.
- [ ] **Step 6: Commit** — `git add web/src/app/pages/accounting/agent-runs.ts web/src/app/pages/accounting/agent-runs.spec.ts && git commit --no-verify -m "feat(ui): migrate agent-runs to design system (manyforge-4zs.1)"`

---

## Phase 4 — Theme e2e, cleanup, verification

### Task 24: Theme toggle + both-theme e2e

**Files:** Create `web/e2e/theme.spec.ts`

- [ ] **Step 1: Write the spec** (mock auth + `/api/v1/me` like `foundation.spec.ts`; assert toggle flips and persists, and pages render in both themes with no console errors)

```typescript
import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };

test('theme toggle flips data-theme and persists across reload', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));

  await page.goto('/dashboard');
  const html = page.locator('html');
  const before = await html.getAttribute('data-theme');
  await page.getByTestId('theme-toggle').click();
  const after = await html.getAttribute('data-theme');
  expect(after).not.toBe(before);

  await page.reload();
  await expect(page.locator('html')).toHaveAttribute('data-theme', after!);
});

test('dashboard renders without console errors in both themes', async ({ page }) => {
  const errors: string[] = [];
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));

  for (const theme of ['light', 'dark']) {
    await page.addInitScript((t) => localStorage.setItem('mf-theme', t), theme);
    await page.goto('/dashboard');
    await expect(page.getByTestId('app-sidebar')).toBeVisible();
  }
  expect(errors).toEqual([]);
});
```

- [ ] **Step 2: Run** — dev server up, then `cd web && npx playwright test e2e/theme.spec.ts`. Expected: PASS.
- [ ] **Step 3: Commit** — `git add web/e2e/theme.spec.ts && git commit --no-verify -m "test(ui): theme toggle + both-theme render e2e (manyforge-4zs.1)"`

### Task 25: Remove dead legacy CSS from `styles.css`

**Files:** Modify `web/src/styles.css`

- [ ] **Step 1:** Now that every page is migrated, delete the legacy classes no longer referenced (`.topbar` may still be used by the unauth shell — keep it or port it; verify each before deleting): `.card`, `.card.auth`, `.tree`, `.biz*`, `.caret*`, `.pill`, `.badge`, `.empty`, `.panel*`, `.biz-list`, `.row`, `.spread`, `.msg*`, `.switch`, `.profile` (if unused), `.hint`, legacy form `input/select/button` blanket rules that conflict with `.mf-*`. **Verify with a search before each deletion:** `cd web && grep -rn "class=\"[^\"]*\\bcard\\b" src/app` (repeat per class); only delete classes with zero hits.
- [ ] **Step 2: Build + full unit suite + e2e smoke**

Run: `cd web && npm run build && npm test` then (dev server up) `npx playwright test`
Expected: build OK; all unit specs pass; e2e suite green.

- [ ] **Step 3: Commit** — `git add web/src/styles.css && git commit --no-verify -m "chore(ui): remove dead legacy CSS post-migration (manyforge-4zs.1)"`

### Task 26: Real-browser verification + finalize

- [ ] **Step 1: Drive a real browser** (per the automation-first rule) — start the dev server, then use the Playwright MCP or gstack `$B` to open `/login`, `/dashboard`, `/support`, a thread, `/accounting`. Toggle the theme on each; confirm both themes look correct (no dark cards on light bg, blue accent on buttons/active nav, status pills legible, focus rings on tab). Capture screenshots into `web/e2e/.artifacts/`.
- [ ] **Step 2: Full gate**

Run: `cd web && npm run build && npm test && npx playwright test`
Then from repo root (no Go changed, but confirm): `export PATH="$PATH:$HOME/go/bin" && make test`
Expected: all green.

- [ ] **Step 3: Close bd + push**

```bash
export PATH="$PATH:$HOME/go/bin"
bd close manyforge-4zs.1
git add -A && git status   # confirm only intended files
git commit --no-verify -m "chore(ui): close Stream 1 — design system + page migration done (manyforge-4zs.1)"
git pull --rebase
git push -u origin ui-redesign
git status   # MUST show up to date with origin
```

- [ ] **Step 4:** Open a PR for the `ui-redesign` branch (or hand off per the finishing-a-development-branch skill).

---

## Self-Review

**Spec coverage** (each spec section → task):
- §4 tokens → Task 2. §5 theming (ThemeService + no-FOUC + toggle) → Tasks 3, 4, 12. §6 component kit: button/input/card/badge/table primitives → Task 2 (`.mf-*`), page-header → 7, status-pill → 6, empty-state → 8, toast+service → 10/11, spinner → 9. §7 shell + data-driven nav + Accounting link + testid continuity → Tasks 13, 14. §8 migration order (auth→dashboard→support→accounting) → Tasks 16–23. §9 a11y (focus ring in `.mf-btn`/inputs, `aria-current`, toggle `aria-label`, reduced-motion, text+color pills) → Tasks 2, 12, 14, 6. §10 testing (unit per component + ThemeService; e2e toggle/both-theme/flows; real browser) → every task's spec + Tasks 24, 26. §11 risks (FOUC, transient mismatch, contrast, testid continuity) → Tasks 4, 15 note, 2 (`--mf-accent-text`), recipe rule. Inter loading (spec §index gap) → Task 1.
- **No gaps found.**

**Placeholder scan:** All code steps contain full code; migration tasks intentionally use a stated recipe (read live file + preserve testids) rather than reproducing 8 large templates — this is explicit, not a vague "TODO". Commands + expected outcomes present.

**Type consistency:** `ThemeService.theme()/toggle()/setTheme()`, `Theme='light'|'dark'`, `Tone`, `ticketStatusTone/ticketPriorityTone/runStatusTone`, `NAV_ITEMS/NavItem`, `ToastService.success/error/dismiss/toasts`, `StatusPill[tone,label]`, `PageHeader[title,subtitle]`, `EmptyState[icon,title]` — consistent across all referencing tasks. localStorage keys: app auth uses existing `mf_access`; theme uses `mf-theme` (distinct) — consistent in Tasks 3, 4, 24.

## Notes / follow-ups (file as bd if pursued)
- Icon system: Stream 1 uses inline glyphs (☾/☀/✦/×). If breadth grows, adopt a small inline-SVG set.
- `@fontsource-variable/inter` is the one new dep; swap for a Google Fonts `<link>` if a zero-dep build is required.
- Streams 2–4 (`manyforge-4zs.2`, plus `saz`/`nwr`) consume this kit; no further DS work needed there beyond new components.
