# Support-Desk Usability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the shipped support desk feel like one product and prove it works end-to-end — add a persistent navigation shell, a repeatable dev seed that fills the desk with real tickets through the ingestion pipeline, and browser-verified regressions for every interactive flow.

**Architecture:** Three independent phases. (A) An Angular app-shell sidebar wraps the router outlet, reusing existing CSS tokens, shown only when authenticated. (B) A new `cmd/seeddemo` Go tool reuses the real `account`, `tenancy`, `inbox.Provisioner`, and `inbox.Service` code to create/verify the demo user, ensure businesses + system inbound addresses, and ingest threaded conversations idempotently — invoked via `make seed-demo`. (C) Drive every support flow in a real browser against the seeded backend, fix genuine bugs, and pin each as a Playwright/unit regression.

**Tech Stack:** Angular 21 (standalone components, signals, `@if/@for`, `inject()`), Go (pgx, argon2id, chi), PostgreSQL (RLS + SECURITY DEFINER ingestion), Vitest (`ng test`), Playwright (`npm run e2e`).

**Verified ground truth (do not re-derive):**
- Auth/session works on all paths; **do not touch** `auth.interceptor.ts`/`auth.service.ts`/`auth.guard.ts`.
- Demo data has **zero** seed in repo; `account`/`principal` are NOT RLS-protected; `business`/`business_closure`/`inbound_address` ARE.
- Ingestion entry for in-process use: `inbox.NewService(*db.DB, blob.Store, inbox.Config, *slog.Logger).Ingest(ctx, RawMessage) (IngestResult, error)`.
- System address is deterministic: `b-<hex(HMAC_SHA256(cfg.InboundSystemAddrSecret, businessID[:16]))[:16]>@<cfg.InboundSystemDomain>`.
- `Provisioner.Handle(ctx, tx, events.Event)` inserts the system address idempotently under a principal-less `WithTx`.

---

## File Structure

**Phase A — App shell (web/)**
- Modify `web/src/app/app.ts` — root component injects `AuthService`/`Router`, exposes `authed`, `profile`, `logout()`.
- Modify `web/src/app/app.html` — sidebar shell when authed; existing topbar+container when not.
- Modify `web/src/app/app.css` — shell grid + sidebar styles (component-scoped).
- Create `web/src/app/app.spec.ts` — unit test for shell render/auth-gating.
- Modify `web/src/app/pages/dashboard.ts` — remove the now-duplicated "Support"/"Sign out"/"Signed in as" header chrome (moved into the shell).
- Create `web/e2e/shell.spec.ts` — persistent-nav + active-state e2e.

**Phase B — Dev seed (Go)**
- Create `cmd/seeddemo/main.go` — the idempotent seed orchestrator.
- Create `cmd/seeddemo/fixtures.go` — the demo conversation fixtures.
- Modify `Makefile` — add `seed-demo` target.

**Phase C — Flow verification (web/)**
- Create `web/e2e/flows-seeded.spec.ts` — regressions for reply/note/triage/inbox-settings discovered while driving the seeded app.
- Modify whichever component files contain genuine bugs found during the drive (unknown until C runs).

**Phase D — Cleanup**
- `bd` issue updates only; no code unless the gate surfaces a regression.

---

## Phase A — App shell

### Task A1: Root component exposes auth state + sidebar data

**Files:**
- Modify: `web/src/app/app.ts`
- Test: `web/src/app/app.spec.ts` (created in A2)

- [ ] **Step 1: Replace `web/src/app/app.ts` with the shell-aware component**

```typescript
import { Component, OnInit, inject, signal } from '@angular/core';
import { Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { AuthService, Profile } from './core/auth.service';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, RouterLink, RouterLinkActive],
  templateUrl: './app.html',
  styleUrl: './app.css',
})
export class App implements OnInit {
  private auth = inject(AuthService);
  private router = inject(Router);

  // Drives the shell: when authenticated we render the persistent sidebar; on the
  // login/signup screens (unauthenticated) we keep the original bare topbar.
  readonly authed = this.auth.isAuthenticated;
  readonly profile = signal<Profile | null>(null);

  ngOnInit(): void {
    // Best-effort identity for the sidebar footer. A failure here is non-fatal:
    // the interceptor already handles refresh/redirect, so we just leave it blank.
    if (this.authed()) {
      this.auth.me().subscribe({ next: (p) => this.profile.set(p), error: () => {} });
    }
  }

  logout(): void {
    this.auth.logout().subscribe({
      next: () => this.toLogin(),
      error: () => this.toLogin(),
    });
  }

  private toLogin(): void {
    this.profile.set(null);
    void this.router.navigateByUrl('/login');
  }
}
```

- [ ] **Step 2: Commit**

```bash
git add web/src/app/app.ts
git commit -m "feat(web): app-root exposes auth state + sidebar profile/logout"
```

### Task A2: Shell template + styles, with unit test

**Files:**
- Modify: `web/src/app/app.html`
- Modify: `web/src/app/app.css`
- Test: `web/src/app/app.spec.ts`

- [ ] **Step 1: Write the failing unit test** — `web/src/app/app.spec.ts`

```typescript
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { App } from './app';

describe('App shell', () => {
  let mock: HttpTestingController;

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => {
    localStorage.clear();
  });

  it('renders the persistent sidebar with the Support nav when authenticated', () => {
    localStorage.setItem('mf_access', 'tok');
    const fixture = TestBed.createComponent(App);
    fixture.detectChanges(); // ngOnInit fires me()
    mock.expectOne('/api/v1/me').flush({
      id: 'u1', email: 'a@b.test', display_name: 'Ada', email_verified: true, status: 'active',
    });
    fixture.detectChanges();
    const el: HTMLElement = fixture.nativeElement;
    expect(el.querySelector('[data-testid="app-sidebar"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="nav-support"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="nav-dashboard"]')).toBeTruthy();
    expect(el.textContent).toContain('Ada');
  });

  it('renders the bare topbar (no sidebar) when unauthenticated', () => {
    const fixture = TestBed.createComponent(App);
    fixture.detectChanges();
    const el: HTMLElement = fixture.nativeElement;
    expect(el.querySelector('[data-testid="app-sidebar"]')).toBeNull();
    expect(el.querySelector('.topbar')).toBeTruthy();
    mock.expectNone('/api/v1/me');
  });
});
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd web && npx ng test --watch=false --include='src/app/app.spec.ts'`
Expected: FAIL — current `app.html` has no `[data-testid="app-sidebar"]`/`nav-support`.

- [ ] **Step 3: Replace `web/src/app/app.html`**

```html
@if (authed()) {
  <div class="shell">
    <aside class="sidebar" data-testid="app-sidebar">
      <div class="sidebar-brand">Many<span class="brand-accent">Forge</span></div>
      <nav class="sidebar-nav">
        <a routerLink="/dashboard" routerLinkActive="active" data-testid="nav-dashboard">Dashboard</a>
        <a routerLink="/support" routerLinkActive="active" data-testid="nav-support">Support</a>
      </nav>
      <div class="sidebar-foot">
        @if (profile(); as p) {
          <p class="profile" data-testid="sidebar-identity">
            Signed in as <b>{{ p.display_name }}</b><br /><span class="faint">{{ p.email }}</span>
          </p>
        }
        <button class="ghost compact" (click)="logout()" data-testid="sign-out">Sign out</button>
      </div>
    </aside>
    <main class="shell-main"><router-outlet /></main>
  </div>
} @else {
  <header class="topbar">
    <span class="brand">Many<span class="brand-accent">Forge</span></span>
    <span class="tagline">all-in-one founder platform</span>
  </header>
  <main class="container"><router-outlet /></main>
}
```

- [ ] **Step 4: Replace `web/src/app/app.css`** (component-scoped — reuses global tokens)

```css
.shell {
  display: grid;
  grid-template-columns: 232px 1fr;
  min-height: 100vh;
}
.sidebar {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 22px 16px;
  border-right: 1px solid var(--border);
  background: var(--panel);
  position: sticky;
  top: 0;
  height: 100vh;
}
.sidebar-brand {
  font-weight: 700;
  font-size: 18px;
  letter-spacing: -0.02em;
  padding: 4px 10px 18px;
}
.sidebar-nav {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.sidebar-nav a {
  color: var(--muted);
  text-decoration: none;
  font-size: 14px;
  font-weight: 500;
  padding: 9px 12px;
  border-radius: var(--radius-sm);
}
.sidebar-nav a:hover {
  color: var(--text);
  background: var(--panel-3);
  text-decoration: none;
}
.sidebar-nav a.active {
  color: var(--text);
  background: var(--accent-soft);
}
.sidebar-foot {
  margin-top: auto;
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding-top: 16px;
  border-top: 1px solid var(--border);
}
.sidebar-foot .profile {
  font-size: 12.5px;
  line-height: 1.5;
}
.sidebar-foot .faint {
  color: var(--faint);
}
.shell-main {
  max-width: 920px;
  margin: 0 auto;
  width: 100%;
  padding: 40px 28px 80px;
}
/* Narrow screens: stack the sidebar as a top bar. */
@media (max-width: 720px) {
  .shell {
    grid-template-columns: 1fr;
  }
  .sidebar {
    position: static;
    height: auto;
    flex-direction: row;
    align-items: center;
    flex-wrap: wrap;
    gap: 12px;
  }
  .sidebar-brand {
    padding: 0 8px 0 0;
  }
  .sidebar-foot {
    margin: 0 0 0 auto;
    flex-direction: row;
    align-items: center;
    border: 0;
    padding: 0;
  }
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd web && npx ng test --watch=false --include='src/app/app.spec.ts'`
Expected: PASS (both cases).

- [ ] **Step 6: Commit**

```bash
git add web/src/app/app.html web/src/app/app.css web/src/app/app.spec.ts
git commit -m "feat(web): persistent sidebar shell (authed) with active nav + identity"
```

### Task A3: De-duplicate dashboard header; keep e2e green

**Files:**
- Modify: `web/src/app/pages/dashboard.ts`
- Test: existing `web/e2e/support.spec.ts` (nav-support test) + `web/e2e/foundation.spec.ts`

The shell now owns global nav + sign-out. Remove the dashboard's duplicate "Support" link and "Sign out" button so they don't appear twice. **Keep** the `data-testid="nav-support"` semantics alive — it now lives in the sidebar (same testid), so the existing `support.spec.ts` test that clicks `nav-support` on `/dashboard` still passes (the sidebar renders on `/dashboard` when authed).

- [ ] **Step 1: Edit the dashboard header region** in `web/src/app/pages/dashboard.ts` (the `.spread` block, ~lines 16–28). Replace:

```html
        <div class="row">
          <a class="linklike" routerLink="/support" data-testid="nav-support">Support</a>
          <button class="ghost compact" (click)="logout()">Sign out</button>
        </div>
```

with (the header keeps only the page title + identity; nav/sign-out are in the shell):

```html
```

(Delete the `<div class="row">…</div>` block entirely.) If `logout()`/`RouterLink`/`Router` become unused in `dashboard.ts` after this, remove the now-dead members and imports so `ng build` stays warning-clean.

- [ ] **Step 2: Run the dashboard unit test + build**

Run: `cd web && npx ng test --watch=false --include='src/app/pages/dashboard.spec.ts' && npx ng build`
Expected: PASS + clean build. If the dashboard spec asserted the removed Sign-out/Support, update those assertions to target the shell (or drop them — they're covered by `app.spec.ts`).

- [ ] **Step 3: Run the affected e2e**

Run: `cd web && npx playwright test e2e/support.spec.ts e2e/foundation.spec.ts`
Expected: PASS. The `nav-support` click on `/dashboard` resolves to the sidebar link.

- [ ] **Step 4: Commit**

```bash
git add web/src/app/pages/dashboard.ts web/src/app/pages/dashboard.spec.ts
git commit -m "refactor(web): move dashboard nav/sign-out into the app shell"
```

### Task A4: e2e — persistent nav + active state across routes

**Files:**
- Create: `web/e2e/shell.spec.ts`

- [ ] **Step 1: Write the e2e** (mock-backed, mirroring `support.spec.ts` `installInboxStack` style — reuse its auth + businesses mocks pattern)

```typescript
import { expect, Page, test } from '@playwright/test';

const BIZ_ID = 'biz-1';

async function installAuth(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({
      json: { id: 'u1', email: 'owner@manyforge.test', display_name: 'Owner', email_verified: true, status: 'active' },
    }),
  );
  await page.route('**/api/v1/businesses**', (route) =>
    route.fulfill({ json: { items: [{ id: BIZ_ID, parent_id: null, tenant_root_id: BIZ_ID, name: 'Acme', status: 'active' }], next_cursor: null } }),
  );
  await page.route('**/api/v1/businesses/*/tickets**', (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );
}

test('the sidebar persists across dashboard and support with correct active state', async ({ page }) => {
  await installAuth(page);

  await page.goto('/dashboard');
  await expect(page.getByTestId('app-sidebar')).toBeVisible();
  await expect(page.getByTestId('nav-dashboard')).toHaveClass(/active/);

  await page.getByTestId('nav-support').click();
  await expect(page).toHaveURL(/\/support$/);
  await expect(page.getByTestId('app-sidebar')).toBeVisible(); // still there — not an island
  await expect(page.getByTestId('nav-support')).toHaveClass(/active/);
  await expect(page.getByTestId('sidebar-identity')).toContainText('Owner');
});

test('the sidebar is absent on the login screen', async ({ page }) => {
  await page.goto('/login');
  await expect(page.getByTestId('app-sidebar')).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Welcome back' })).toBeVisible();
});
```

- [ ] **Step 2: Run it**

Run: `cd web && npx playwright test e2e/shell.spec.ts`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add web/e2e/shell.spec.ts
git commit -m "test(web): e2e for persistent shell nav + active state"
```

---

## Phase B — Dev seed through the real ingestion pipeline

### Task B1: Conversation fixtures

**Files:**
- Create: `cmd/seeddemo/fixtures.go`

- [ ] **Step 1: Write the fixtures** (stable Message-IDs ⇒ idempotent re-runs)

```go
package main

// conversation is one ticket: the first message opens it; each subsequent message
// threads onto it via In-Reply-To. Message-IDs are STABLE so re-running the seed is
// an idempotent no-op (the DEFINER dedupes on (tenant_root_id, message_id)).
type seedMsg struct {
	From      string // RFC5322 From (display + addr)
	Subject   string
	MessageID string // without angle brackets; must be globally stable per seed
	InReplyTo string // "" for the opening message
	Body      string
}

type conversation struct {
	Key  string // short slug used to build stable message-ids per business
	Msgs []seedMsg
}

// conversationsFor returns the demo conversations for a business slug. Content spans
// a realistic mix so the list shows variety (open/threaded, different requesters).
func conversationsFor(bizSlug string) []conversation {
	return []conversation{
		{
			Key: "pw",
			Msgs: []seedMsg{
				{From: "Jane Customer <jane@example.com>", Subject: "Cannot reset my password",
					MessageID: "seed-" + bizSlug + "-pw-1@demo.manyforge.test", InReplyTo: "",
					Body: "Hi, the reset link in your email returns 'token expired' every time. Help?"},
				{From: "Jane Customer <jane@example.com>", Subject: "Re: Cannot reset my password",
					MessageID: "seed-" + bizSlug + "-pw-2@demo.manyforge.test",
					InReplyTo: "seed-" + bizSlug + "-pw-1@demo.manyforge.test",
					Body: "Still stuck — I tried three times over the last hour."},
			},
		},
		{
			Key: "billing",
			Msgs: []seedMsg{
				{From: "Marcus Reed <marcus@globex.test>", Subject: "Double charged this month",
					MessageID: "seed-" + bizSlug + "-billing-1@demo.manyforge.test", InReplyTo: "",
					Body: "I see two identical charges on my card for the same invoice. Please refund one."},
			},
		},
		{
			Key: "feature",
			Msgs: []seedMsg{
				{From: "Priya Nair <priya@initech.test>", Subject: "Feature request: CSV export",
					MessageID: "seed-" + bizSlug + "-feature-1@demo.manyforge.test", InReplyTo: "",
					Body: "Would love a way to export my data as CSV from the dashboard."},
			},
		},
	}
}
```

- [ ] **Step 2: Commit**

```bash
git add cmd/seeddemo/fixtures.go
git commit -m "feat(seed): demo conversation fixtures (stable message-ids)"
```

### Task B2: Seed orchestrator — user, businesses, addresses, ingest

**Files:**
- Create: `cmd/seeddemo/main.go`

- [ ] **Step 1: Write the orchestrator**

```go
// Command seeddemo idempotently fills a dev database with a demo support desk:
// the live-demo user, the Acme Holdings business tree, each business's system
// inbound address, and a handful of threaded conversations ingested through the
// REAL inbox pipeline. Re-running is a no-op. App-role only (no superuser needed).
//
//	make seed-demo   # loads .air.env then runs this
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/tenancy"
)

const (
	demoEmail    = "live-demo@manyforge.test"
	demoName     = "Live Demo"
	demoPassword = "DevPassw0rd!"
	masterName   = "Acme Holdings"
)

var subNames = []string{"Engineering", "Platform Team", "Sales"}

// bizSlug maps a business name to the short slug used in fixture message-ids.
func bizSlug(name string) string {
	switch name {
	case masterName:
		return "acme"
	case "Engineering":
		return "eng"
	case "Platform Team":
		return "plat"
	case "Sales":
		return "sales"
	default:
		return "biz"
	}
}

func main() {
	if err := run(); err != nil {
		slog.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("MANYFORGE_DATABASE_URL is required (source .air.env)")
	}
	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	principalID, err := ensureUser(ctx, database, logger)
	if err != nil {
		return err
	}
	logger.Info("demo user ready", "principal", principalID)

	businesses, err := ensureBusinesses(ctx, database, principalID, logger)
	if err != nil {
		return err
	}

	// System inbound addresses, provisioned synchronously via the real Provisioner.
	prov := inbox.NewProvisioner(database, inbox.ProvisionConfig{
		SystemDomain: cfg.InboundSystemDomain,
		SystemKey:    cfg.InboundSystemAddrSecret,
	}, logger)
	for _, b := range businesses {
		if err := provisionAddress(ctx, database, prov, b); err != nil {
			return fmt.Errorf("provision %s: %w", b.Name, err)
		}
	}

	// Real ingestion. Attachments unused, so a throwaway file:// store satisfies the ctor.
	store, err := blob.Open(ctx, "file://"+os.TempDir()+"/manyforge-seed-blobs")
	if err != nil {
		return fmt.Errorf("open blob store: %w", err)
	}
	defer store.Close()
	inboxSvc := inbox.NewService(database, store, inbox.Config{
		ReplyTokenKey:       cfg.InboundReplyTokenSecret,
		AttachmentMaxBytes:  cfg.AttachmentMaxBytes,
		InboundSystemDomain: cfg.InboundSystemDomain,
	}, logger)

	created, dup := 0, 0
	for _, b := range businesses {
		addr := systemAddress(cfg.InboundSystemAddrSecret, cfg.InboundSystemDomain, b.ID)
		for _, conv := range conversationsFor(bizSlug(b.Name)) {
			for _, m := range conv.Msgs {
				res, err := inboxSvc.Ingest(ctx, inbox.RawMessage{
					Provider:          "seed",
					EnvelopeRecipient: addr,
					EnvelopeSender:    m.From,
					ReceivedAt:        time.Now(),
					Raw:               rfc822(m.From, addr, m.Subject, m.MessageID, m.InReplyTo, m.Body),
				})
				if err != nil {
					return fmt.Errorf("ingest %s: %w", m.MessageID, err)
				}
				if res.Duplicate {
					dup++
				} else {
					created++
				}
			}
		}
		logger.Info("seeded business", "name", b.Name, "address", addr)
	}
	logger.Info("seed complete", "messages_created", created, "messages_skipped_duplicate", dup)
	return nil
}

// ensureUser returns the demo human principal id, creating + verifying the account
// on first run. account/principal are not RLS-protected, so a plain WithTx works.
func ensureUser(ctx context.Context, database *db.DB, logger *slog.Logger) (uuid.UUID, error) {
	acctSvc := &account.Service{DB: database, TokenTTL: 24 * time.Hour, Now: time.Now}

	var principalID uuid.UUID
	lookup := func() (bool, error) {
		var found bool
		err := database.WithTx(ctx, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			acc, err := q.GetAccountByEmail(ctx, demoEmail)
			if err != nil {
				return nil // treat as not-found; caller will create
			}
			prin, err := q.GetPrincipalByAccount(ctx, db.PGUUID(acc.ID))
			if err != nil {
				return err
			}
			principalID = prin.ID
			found = true
			return nil
		})
		return found, err
	}

	found, err := lookup()
	if err != nil {
		return uuid.Nil, fmt.Errorf("lookup user: %w", err)
	}
	if found {
		return principalID, nil
	}

	_, token, err := acctSvc.Signup(ctx, demoEmail, demoName, demoPassword)
	if err != nil {
		return uuid.Nil, fmt.Errorf("signup: %w", err)
	}
	if err := acctSvc.VerifyEmail(ctx, token); err != nil {
		return uuid.Nil, fmt.Errorf("verify: %w", err)
	}
	logger.Info("created demo account", "email", demoEmail)
	found, err = lookup()
	if err != nil || !found {
		return uuid.Nil, fmt.Errorf("principal not found after signup: %w", err)
	}
	return principalID, nil
}

// ensureBusinesses returns the master + three subs, creating any that are missing.
func ensureBusinesses(ctx context.Context, database *db.DB, principalID uuid.UUID, logger *slog.Logger) ([]tenancy.Business, error) {
	ten := &tenancy.Service{DB: database}
	existing, err := ten.ListBusinesses(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("list businesses: %w", err)
	}
	byName := map[string]tenancy.Business{}
	for _, b := range existing {
		byName[b.Name] = b
	}

	master, ok := byName[masterName]
	if !ok {
		master, err = ten.CreateMasterBusiness(ctx, principalID, masterName)
		if err != nil {
			return nil, fmt.Errorf("create master: %w", err)
		}
		logger.Info("created master business", "name", masterName)
	}
	out := []tenancy.Business{master}
	for _, name := range subNames {
		if b, ok := byName[name]; ok {
			out = append(out, b)
			continue
		}
		b, err := ten.CreateSubBusiness(ctx, principalID, master.ID, name)
		if err != nil {
			return nil, fmt.Errorf("create sub %s: %w", name, err)
		}
		logger.Info("created sub business", "name", name)
		out = append(out, b)
	}
	return out, nil
}

// provisionAddress drives the REAL Provisioner.Handle in a principal-less tx,
// inserting the deterministic system address (idempotent: a replay is a no-op).
func provisionAddress(ctx context.Context, database *db.DB, prov *inbox.Provisioner, b tenancy.Business) error {
	payload, err := json.Marshal(map[string]any{
		"business_id":    b.ID,
		"tenant_root_id": b.TenantRootID,
	})
	if err != nil {
		return err
	}
	return database.WithTx(ctx, func(tx pgx.Tx) error {
		return prov.Handle(ctx, tx, events.Event{Topic: events.TopicBusinessCreated, Payload: payload})
	})
}
```

- [ ] **Step 2: Verify `events.Event` field names + `events.TopicBusinessCreated` + `db.PGUUID`**

Run: `grep -nE "type Event struct|TopicBusinessCreated|func PGUUID" internal/platform/events/*.go internal/platform/db/*.go`
Expected: confirm `Event` has `Topic` and `Payload []byte` (or `json.RawMessage`), `TopicBusinessCreated` exists, and `db.PGUUID(uuid.UUID) pgtype.UUID` exists. **If field/func names differ, adjust the code above to match before building** (e.g. `Payload` may be `json.RawMessage` — `json.Marshal` returns `[]byte` which assigns to either).

- [ ] **Step 3: Add the RFC822 + system-address helpers** — append to `cmd/seeddemo/main.go`

```go
// rfc822 builds a minimal text/plain message (mirrors internal/inbox test helper).
func rfc822(from, to, subject, messageID, inReplyTo, body string) []byte {
	msg := "From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
		"Message-ID: <" + messageID + ">\r\n"
	if inReplyTo != "" {
		msg += "In-Reply-To: <" + inReplyTo + ">\r\n" +
			"References: <" + inReplyTo + ">\r\n"
	}
	msg += "MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
	return []byte(msg)
}

// systemAddress re-derives a business's deterministic system inbound address. This
// MUST match internal/inbox/provision.go's derivation exactly (same key + length).
func systemAddress(key []byte, domain string, businessID uuid.UUID) string {
	mac := hmac.New(sha256.New, key)
	id := businessID
	mac.Write(id[:])
	return fmt.Sprintf("b-%s@%s", hex.EncodeToString(mac.Sum(nil))[:16], domain)
}
```

Add imports `crypto/hmac`, `crypto/sha256`, `encoding/hex` to the file's import block.

- [ ] **Step 4: Build**

Run: `go build ./cmd/seeddemo`
Expected: compiles. Fix any signature drift surfaced by Step 2.

- [ ] **Step 5: Commit**

```bash
git add cmd/seeddemo/main.go
git commit -m "feat(seed): idempotent demo seeder via real account/tenancy/inbox services"
```

### Task B3: Makefile target

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Add the target** (match the existing `dev:` style; load `.air.env` so DB URL + secrets are present)

```makefile
seed-demo:
	set -a; . ./.air.env; set +a; $(GO) run ./cmd/seeddemo
```

- [ ] **Step 2: Commit**

```bash
git add Makefile
git commit -m "feat(seed): make seed-demo target"
```

### Task B4: Run the seed and verify in a real browser

**Files:** none (verification)

- [ ] **Step 1: Ensure the dev DB is up and run the seed**

Run: `make seed-demo`
Expected: logs "demo user ready", "seeded business" ×4, "seed complete messages_created=N messages_skipped_duplicate=0".

- [ ] **Step 2: Run it AGAIN to prove idempotency**

Run: `make seed-demo`
Expected: "messages_created=0 messages_skipped_duplicate=N" (every message a duplicate; no new tickets).

- [ ] **Step 3: Verify in the browser** (dev stack on :4300; login `live-demo@manyforge.test` / `DevPassw0rd!`)

Drive with Playwright MCP (or `$B`): log in → `/support` → for the Acme business confirm ticket rows render with subjects ("Cannot reset my password", "Double charged this month", "Feature request: CSV export"); open the password ticket and confirm the thread shows **two** messages (root + reply). Confirm switching the business selector shows tickets for each sub. Capture the network: ticket-list `GET …/tickets` returns items (not empty).

- [ ] **Step 4: Commit** (no code; record verification in the bd issue notes)

### Task B5: Document the seed

**Files:**
- Modify: `README.md` (the dev-run section)

- [ ] **Step 1: Add a short "Seed demo data" note** under the existing run instructions:

```markdown
### Seed demo data

With the dev DB up, `make seed-demo` idempotently creates the `live-demo@manyforge.test`
user (password `DevPassw0rd!`), the Acme Holdings business tree, each business's system
inbound address, and a few threaded support conversations ingested through the real inbox
pipeline. Safe to re-run.
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs(seed): document make seed-demo"
```

---

## Phase C — Drive every flow, fix what's real, pin regressions

This phase is exploratory: with seeded data, drive each interactive flow in a real browser and fix genuine defects. Each flow gets a deterministic, mock-backed Playwright regression in the existing `support.spec.ts` style (CI needs no live backend). Where a flow already has coverage, add a seeded-scenario assertion that would have caught the empty-desk blind spot.

### Task C1: Reply + internal note (US2)

- [ ] **Step 1: Drive it** — open the password ticket; type a reply; submit; confirm the outbound message appends to the thread and the composer clears. Toggle to "internal note"; submit; confirm it appends as a note. Watch the network for `POST …/reply` (200) and `POST …/note` (200). Note any console error, dead button, or unappended message.
- [ ] **Step 2: If a genuine bug is found**, write a failing unit/e2e test reproducing it (mock-backed), then fix the component minimally. **If no bug**, add the regression below.
- [ ] **Step 3: Regression** — append to `web/e2e/flows-seeded.spec.ts`:

```typescript
import { expect, Page, test } from '@playwright/test';

const BIZ = 'biz-1';
const TID = 'ticket-1';

async function stack(page: Page, opts: { reply?: boolean } = {}) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'a');
    localStorage.setItem('mf_refresh', 'r');
  });
  await page.route('**/api/v1/me', (r) =>
    r.fulfill({ json: { id: 'u1', email: 'me@x.test', display_name: 'Me', email_verified: true, status: 'active' } }));
  await page.route('**/api/v1/businesses', (r) =>
    r.fulfill({ json: { items: [{ id: BIZ, parent_id: null, tenant_root_id: BIZ, name: 'Acme', status: 'active' }], next_cursor: null } }));
  await page.route(`**/api/v1/businesses/${BIZ}/assignable-members`, (r) =>
    r.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route(`**/api/v1/businesses/${BIZ}/tickets/${TID}/messages`, (r) =>
    r.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route(`**/api/v1/businesses/${BIZ}/tickets/${TID}`, (r) =>
    r.fulfill({ json: ticket() }));
  await page.route(`**/api/v1/businesses/${BIZ}/tickets/${TID}/reply`, (r) =>
    r.fulfill({ json: { id: 'm-out', direction: 'outbound', body_text: 'thanks, looking into it', author_principal_id: 'u1', message_id: 'out-1', created_at: '2024-01-01T00:00:00Z', references: [] } }));
}

function ticket() {
  return {
    id: TID, business_id: BIZ, tenant_root_id: BIZ, subject: 'Cannot reset my password',
    status: 'new', priority: 'normal', assignee_principal_id: null,
    requester: { id: 'r1', tenant_root_id: BIZ, email: 'jane@example.com', display_name: 'Jane Customer', contact_id: null, first_seen_at: '2024-01-01T00:00:00Z', last_seen_at: '2024-01-01T00:00:00Z' },
    tags: [], message_count: 1, last_message_at: '2024-01-01T00:00:00Z', created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z',
  };
}

test('US2: a reply posts and appends to the thread', async ({ page }) => {
  await stack(page);
  await page.goto(`/support/${BIZ}/${TID}`);
  await expect(page.getByTestId('thread-header')).toBeVisible();
  await page.getByTestId('composer-body').fill('thanks, looking into it');
  await page.getByTestId('composer-send').click();
  await expect(page.getByText('thanks, looking into it')).toBeVisible();
});
```

> **Note:** the exact composer test-ids (`composer-body`/`composer-send`) must match `thread-view.ts`. Grep them first: `grep -n "data-testid" web/src/app/pages/support/thread-view.ts`; substitute the real ids.

- [ ] **Step 4: Run** `cd web && npx playwright test e2e/flows-seeded.spec.ts` → PASS. **Commit.**

### Task C2: Triage — status / priority / assignee / tags (US3)

- [ ] **Step 1: Drive it** against the seeded ticket: change status `new→pending`, change priority, "Assign to me", add and remove a tag. Confirm each `PATCH …/tickets/{tid}` fires with only the changed field and the UI reflects the returned ticket; reload to confirm persistence. Note any defect.
- [ ] **Step 2:** fix any genuine bug (failing test first), else add a regression mirroring the existing `installTriageStack` test for one mutation against seeded shape.
- [ ] **Step 3:** Run + commit.

### Task C3: Inbox settings — domain + address (US4)

- [ ] **Step 1: Drive it** at `/support/:bizId/settings/inbox`: add an email domain, view the DNS challenge surface, click Verify (expect the pending hint), add an inbound address on a verified domain. Note any defect (the filed `manyforge-mu7` single-`saving()` serialization is a known follow-up — confirm whether it manifests).
- [ ] **Step 2:** fix any genuine bug (failing test first), else add a regression for the add-domain → challenge-render path.
- [ ] **Step 3:** Run + commit.

### Task C4: Full e2e sweep

- [ ] **Step 1:** Run `cd web && npx playwright test` → all specs PASS (including new shell + flows-seeded).
- [ ] **Step 2: Commit** any final test adjustments.

---

## Phase D — Cleanup & gate

### Task D1: Reconcile `manyforge-bhg`

- [ ] **Step 1:** `bd update manyforge-bhg` — set status to closed with a note: "Not reproducing. Verified in browser: expired-access+valid-refresh transparently refreshes (401→/auth/refresh 200→retry 200); invalid-refresh and no-token both redirect to /login. Network trace + token-iat evidence in docs/superpowers/specs/2026-06-02-support-desk-usability-design.md." (Do not claim a code fix — there was none.)

### Task D2: bd bookkeeping

- [ ] **Step 1:** Create/close the bd issues for Phase A/B/C under the support-desk area; record the seed command and verification outcomes in notes.

### Task D3: Full merge gate

- [ ] **Step 1:** Run the complete gate:

```bash
make test && make int-test && make contract-test && make lint && (cd web && npm run e2e)
```

Expected: all GREEN. `make int-test` needs Docker/colima (~4–5 min). `make lint` runs vet + `~/go/bin/golangci-lint run ./...` (0 issues).

- [ ] **Step 2: Commit** any fixes the gate surfaces. Do not push until the gate is green.

---

## Self-Review

**Spec coverage:**
- Deliverable 1 (app shell) → Tasks A1–A4. ✓
- Deliverable 2 (dev seed via real pipeline) → Tasks B1–B5 (user/businesses/addresses/ingest, idempotent, `make seed-demo`). ✓
- Deliverable 3 (drive + fix + regressions) → Tasks C1–C4. ✓
- `bhg` reconciliation → D1. ✓
- Full gate stays green → D3. ✓
- Testing (shell unit + e2e; seed idempotency; flow regressions) → A2/A4, B4, C1–C4. ✓

**Placeholder scan:** Phase C is intentionally exploratory (cannot pre-write fixes for unknown bugs) but each task gives a concrete drive procedure AND a concrete regression template with real selectors-to-grep. No "TBD" in load-bearing code. The one deliberate code-deletion step (A3) shows the exact block to remove.

**Type/name consistency:** `inbox.NewService`/`inbox.Config`/`inbox.RawMessage`/`inbox.Provisioner`/`inbox.ProvisionConfig`, `tenancy.Service{DB}`/`ListBusinesses`/`CreateMasterBusiness`/`CreateSubBusiness`/`tenancy.Business{ID,TenantRootID,Name}`, `account.Service{DB,TokenTTL,Now}`/`Signup`/`VerifyEmail`, `dbgen.New`/`GetAccountByEmail`/`GetPrincipalByAccount`, `db.Open`/`WithTx`/`PGUUID` all match the verified ground truth. **B2 Step 2 explicitly verifies `events.Event`/`TopicBusinessCreated`/`PGUUID` names before building** — the one place names were inferred rather than read verbatim.

**Risk flagged:** if `events.Event.Payload` is typed `json.RawMessage`, `json.Marshal`'s `[]byte` still assigns. If `ListBusinesses` requires a verified account, ensureUser verifies before ensureBusinesses runs.
