# Approvals Queue UI + Safe Action-Summary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship an `/approvals` page (on the Stream-1 design system) where a human approves/denies queued agent actions, plus a server-computed redacted action-summary so each item is self-explanatory.

**Architecture:** Backend adds one pure function `approvalSummary(tool, args)` whose output is added as a `summary` field to the existing approval wire response — raw `args` are never serialized. Frontend adds an `ApprovalsService`, an `/approvals` page using the kit's `mf-table`/`mf-status-pill`/`mf-page-header`/`mf-empty-state`, an effect-class tone map, a nav item with a per-current-business pending-count badge, and ~20s polling.

**Tech Stack:** Go (chi handlers, `internal/agents`), Angular 21 (standalone, signals), Vitest + TestBed + HttpTestingController, Playwright. Reuses the Stream-1 `--mf-*` tokens + component kit.

**Spec:** `docs/superpowers/specs/2026-06-11-approvals-queue-ui-design.md` · **bd:** `manyforge-4zs.2` · **Branch:** `ui-stream2-connectors-approvals` (already created off `ui-redesign`).

---

## Conventions (read once)

- **TDD loop:** failing test → run (fail) → minimal impl → run (pass) → commit.
- **Go tests:** `cd /Users/jigglypuff/dev/manyforge && export PATH="$HOME/go/bin:$PATH" && go test ./internal/agents/...` (fast, no Docker). Security pins: `make sec-test` (the source-level pin needs no DB). Full: `make test`.
- **Frontend tests:** `cd web && npm test` (Vitest). e2e: `cd web && npx playwright test` (needs dev server on :4300 — the controller runs the consolidated suite; per-task verification is unit + build).
- **Commits:** `git commit --no-verify`. **NEVER add a `Co-Authored-By` trailer.** Stage only each task's files.
- **Effect class ints:** `0=read, 1=reversible, 2=external, 3=irreversible` (`internal/agents/tools.go:22-29`).
- **Real tool names that can land in the queue** (gated effects): external → `add_external_comment` `{ticket_id, body_text}`, `transition_external_status` `{ticket_id, status}`, `draft_reply` `{ticket_id, body_text}`; reversible (if a mode gates them) → `set_status` `{ticket_id, status}`, `set_priority` `{ticket_id, priority}`, `set_tags` `{ticket_id, tags[]}`, `set_assignee` `{ticket_id, assignee *uuid}`. Reads never gate (fallback covers them).

---

## File Structure

**Backend (create):**
- `internal/agents/approval_summary.go` — `approvalSummary(tool string, args json.RawMessage) string` + `shortID`/`truncate` helpers.
- `internal/agents/approval_summary_test.go` — unit tests (per tool, truncation, fallback, no-args-leak marshal check).
- `internal/security_regression/approvals_summary_pin_test.go` — source-level pin: `approvalResp` has no `args` field + `toApprovalResp` calls `approvalSummary` + route stays gated by `agents.approve`.

**Backend (modify):**
- `internal/agents/approval_handler.go:64-78` — add `Summary string \`json:"summary"\`` to `approvalResp`; populate in `toApprovalResp` via `approvalSummary(a.Tool, a.Args)`.

**Frontend (create):**
- `web/src/app/core/approvals.service.ts` (+ `.spec.ts`) — `ApprovalsService`: `ApprovalItem` type, `listPending`, `approve`, `deny`, a `pendingCount` signal + `refreshCount(businessId)`.
- `web/src/app/core/current-business.service.ts` (+ `.spec.ts`) — persisted `currentBusinessId` signal (shared by the page + nav badge).
- `web/src/app/pages/approvals/queue.ts` (+ `.spec.ts`) — `ApprovalsQueueComponent`, route `/approvals`.

**Frontend (modify):**
- `web/src/app/ui/status.ts` (+ existing `.spec.ts`) — add `effectClassTone(n)` + `effectClassLabel(n)`.
- `web/src/app/ui/nav.ts` (+ `.spec.ts`) — add the Approvals `NavItem`.
- `web/src/app/app.routes.ts` — add the `/approvals` route.
- `web/src/app/app.ts` / `app.html` — feed the Approvals nav badge from a ~20s poll of `ApprovalsService.pendingCount` for `currentBusinessId`.
- `web/e2e/approvals.spec.ts` — e2e.

---

## Phase A — Backend: safe action-summary

### Task 1: `approvalSummary` pure function (TDD)

**Files:** Create `internal/agents/approval_summary.go` + `internal/agents/approval_summary_test.go`

- [ ] **Step 1: Write the failing test** (`approval_summary_test.go`)

```go
package agents

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApprovalSummary(t *testing.T) {
	tid := "7bbeb32e-7c98-4c8f-966b-70acdb440dce"
	cases := []struct {
		name, tool, args, want string
	}{
		{"external comment", "add_external_comment",
			`{"ticket_id":"` + tid + `","body_text":"Thanks — fix shipped in v2.3"}`,
			`Comment on ticket 7bbeb32e: "Thanks — fix shipped in v2.3"`},
		{"transition", "transition_external_status",
			`{"ticket_id":"` + tid + `","status":"closed"}`,
			"Transition ticket 7bbeb32e → closed"},
		{"draft reply", "draft_reply",
			`{"ticket_id":"` + tid + `","body_text":"Hello"}`,
			`Draft reply on ticket 7bbeb32e: "Hello"`},
		{"set status", "set_status",
			`{"ticket_id":"` + tid + `","status":"solved"}`,
			"Set status of ticket 7bbeb32e → solved"},
		{"set priority", "set_priority",
			`{"ticket_id":"` + tid + `","priority":"high"}`,
			"Set priority of ticket 7bbeb32e → high"},
		{"unassign", "set_assignee",
			`{"ticket_id":"` + tid + `","assignee":null}`,
			"Unassign ticket 7bbeb32e"},
		{"unknown tool falls back to bare name", "mystery_tool",
			`{"secret":"sk-leak"}`, "mystery_tool"},
		{"malformed args falls back", "add_external_comment", `{bad json`, "add_external_comment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := approvalSummary(c.tool, json.RawMessage(c.args))
			if got != c.want {
				t.Fatalf("approvalSummary(%q) = %q, want %q", c.tool, got, c.want)
			}
		})
	}
}

func TestApprovalSummary_TruncatesAndStripsNewlines(t *testing.T) {
	long := strings.Repeat("a", 200) + "\nSECOND LINE"
	got := approvalSummary("add_external_comment",
		json.RawMessage(`{"ticket_id":"7bbeb32e-0000-0000-0000-000000000000","body_text":"`+long+`"}`))
	if strings.Contains(got, "\n") {
		t.Fatal("summary must not contain newlines")
	}
	if strings.Contains(got, "SECOND LINE") || len([]rune(got)) > 120 {
		t.Fatalf("summary not truncated: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatal("expected ellipsis on truncated body")
	}
}
```

- [ ] **Step 2: Run, confirm fail** — `go test ./internal/agents/ -run TestApprovalSummary` → FAIL (undefined: approvalSummary).

- [ ] **Step 3: Implement** (`approval_summary.go`)

```go
package agents

import (
	"encoding/json"
	"fmt"
	"strings"
)

// approvalSummary renders a short, human-readable, REDACTED one-line description
// of a pending action for the approvals queue. It is a presentation helper: it
// NEVER returns raw args. Any unmarshal error or unhandled tool falls back to the
// bare tool name (never an echo of args). Free text is whitespace-collapsed and
// rune-truncated. Ticket identifiers are shortened to an 8-char prefix.
func approvalSummary(tool string, args json.RawMessage) string {
	switch tool {
	case "add_external_comment", "draft_reply":
		var a struct {
			TicketID string `json:"ticket_id"`
			BodyText string `json:"body_text"`
		}
		if json.Unmarshal(args, &a) != nil {
			return tool
		}
		verb := "Comment on"
		if tool == "draft_reply" {
			verb = "Draft reply on"
		}
		return fmt.Sprintf("%s ticket %s: %q", verb, shortID(a.TicketID), truncate(a.BodyText, 80))
	case "transition_external_status", "set_status":
		var a struct {
			TicketID string `json:"ticket_id"`
			Status   string `json:"status"`
		}
		if json.Unmarshal(args, &a) != nil {
			return tool
		}
		verb := "Transition"
		if tool == "set_status" {
			verb = "Set status of"
		}
		return fmt.Sprintf("%s ticket %s → %s", verb, shortID(a.TicketID), truncate(a.Status, 32))
	case "set_priority":
		var a struct {
			TicketID string `json:"ticket_id"`
			Priority string `json:"priority"`
		}
		if json.Unmarshal(args, &a) != nil {
			return tool
		}
		return fmt.Sprintf("Set priority of ticket %s → %s", shortID(a.TicketID), truncate(a.Priority, 32))
	case "set_tags":
		var a struct {
			TicketID string   `json:"ticket_id"`
			Tags     []string `json:"tags"`
		}
		if json.Unmarshal(args, &a) != nil {
			return tool
		}
		return fmt.Sprintf("Set tags of ticket %s: %s", shortID(a.TicketID), truncate(strings.Join(a.Tags, ", "), 60))
	case "set_assignee":
		var a struct {
			TicketID string  `json:"ticket_id"`
			Assignee *string `json:"assignee"`
		}
		if json.Unmarshal(args, &a) != nil {
			return tool
		}
		if a.Assignee == nil {
			return fmt.Sprintf("Unassign ticket %s", shortID(a.TicketID))
		}
		return fmt.Sprintf("Assign ticket %s", shortID(a.TicketID))
	default:
		return tool
	}
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// truncate whitespace-collapses (removing newlines/tabs) then rune-truncates with an ellipsis.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
```

Note: `%q` on the body wraps it in double quotes and Go-escapes any residual special chars (defense in depth). The expected test strings account for this (plain ASCII bodies quote cleanly).

- [ ] **Step 4: Run, confirm pass** — `go test ./internal/agents/ -run TestApprovalSummary` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/approval_summary.go internal/agents/approval_summary_test.go
git commit --no-verify -m "feat(agents): approvalSummary — redacted action-summary for approvals queue (manyforge-4zs.2)"
```

### Task 2: Wire `summary` into the approval wire response (TDD)

**Files:** Modify `internal/agents/approval_handler.go:64-78`; Create test in `internal/agents/approval_summary_test.go` (append)

- [ ] **Step 1: Append the failing test** to `approval_summary_test.go`

```go
func TestToApprovalResp_IncludesSummary_NotArgs(t *testing.T) {
	tid := "7bbeb32e-7c98-4c8f-966b-70acdb440dce"
	item := ApprovalItem{
		Tool:        "transition_external_status",
		Args:        json.RawMessage(`{"ticket_id":"` + tid + `","status":"closed"}`),
		EffectClass: 2,
		State:       "pending",
	}
	resp := toApprovalResp(item)
	if resp.Summary != "Transition ticket 7bbeb32e → closed" {
		t.Fatalf("summary = %q", resp.Summary)
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"args"`) || strings.Contains(string(b), "ticket_id") {
		t.Fatalf("wire response must not leak raw args: %s", b)
	}
	if !strings.Contains(string(b), `"summary"`) {
		t.Fatal("wire response must include summary")
	}
}
```

- [ ] **Step 2: Run, confirm fail** — `go test ./internal/agents/ -run TestToApprovalResp` → FAIL (`resp.Summary` undefined).

- [ ] **Step 3: Implement** — edit `approval_handler.go`. Add the field to `approvalResp` (after `ExpiresAt`):

```go
type approvalResp struct {
	ID          uuid.UUID `json:"id"`
	AgentRunID  uuid.UUID `json:"agent_run_id"`
	Tool        string    `json:"tool"`
	EffectClass int       `json:"effect_class"`
	State       string    `json:"state"`
	ExpiresAt   time.Time `json:"expires_at"`
	Summary     string    `json:"summary"`
}
```

And populate it in `toApprovalResp`:

```go
func toApprovalResp(a ApprovalItem) approvalResp {
	return approvalResp{
		ID: a.ID, AgentRunID: a.AgentRunID, Tool: a.Tool,
		EffectClass: a.EffectClass, State: a.State, ExpiresAt: a.ExpiresAt,
		Summary: approvalSummary(a.Tool, a.Args),
	}
}
```

- [ ] **Step 4: Run, confirm pass** — `go test ./internal/agents/...` → PASS. Also `go build ./...` and `go vet ./internal/agents/`.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/approval_handler.go internal/agents/approval_summary_test.go
git commit --no-verify -m "feat(agents): add redacted summary to approval wire response (manyforge-4zs.2)"
```

### Task 3: Security pin — args never leak, summary stays gated (TDD)

**Files:** Create `internal/security_regression/approvals_summary_pin_test.go`

> The agents wire types are unexported, so this is a SOURCE-LEVEL pin (per the project's pin discipline): it reads the handler/router source and fails loudly if a future refactor reintroduces raw args or drops the summary/gate.

- [ ] **Step 1: Write the failing test**

```go
// Pin: MF-005-approvals — the approvals wire response exposes only a redacted
// `summary`, never raw `args`, and the route stays gated by `agents.approve`.
package security_regression

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func TestApprovalRespNeverExposesArgs(t *testing.T) {
	src := readRepoFile(t, "internal/agents/approval_handler.go")
	// Isolate the approvalResp struct block.
	m := regexp.MustCompile(`(?s)type approvalResp struct \{.*?\}`).FindString(src)
	if m == "" {
		t.Fatal("could not find approvalResp struct")
	}
	if strings.Contains(m, `json:"args"`) || regexp.MustCompile(`\bArgs\b`).MatchString(m) {
		t.Fatalf("approvalResp must not expose args:\n%s", m)
	}
	if !strings.Contains(m, `json:"summary"`) {
		t.Fatal("approvalResp must expose the redacted summary field")
	}
	if !strings.Contains(src, "approvalSummary(a.Tool, a.Args)") {
		t.Fatal("toApprovalResp must populate summary via approvalSummary")
	}
}

func TestApprovalsRouteGatedByApprovePermission(t *testing.T) {
	src := readRepoFile(t, "cmd/manyforge/main.go")
	// The approvals route group must be wrapped by the agents.approve gate.
	if !regexp.MustCompile(`(?s)approvals.*?agentsApprove|agentsApprove.*?approvals`).MatchString(src) {
		t.Fatal("approvals routes must be gated by the agents.approve middleware")
	}
}
```

- [ ] **Step 2: Run, confirm fail** — write a temporary wrong expectation OR run before Task 2's edit is present. Since Task 2 is already done, instead first run with the `summary` assertion to confirm it PASSES (the guard is satisfied), then verify the guard FAILS if redaction is removed: temporarily add `Args json.RawMessage \`json:"args"\`` to `approvalResp`, run `make sec-test` → it FAILS; revert. Document this manual check in the commit body.

Run: `cd /Users/jigglypuff/dev/manyforge && export PATH="$HOME/go/bin:$PATH" && go test ./internal/security_regression/ -run 'TestApprovalResp|TestApprovalsRoute'`
Expected after revert: PASS (2 tests).

- [ ] **Step 3:** (implementation is the test itself — it pins existing Task 1/2 behavior.) Confirm `make sec-test` includes this file (it runs `./internal/security_regression/...`).

Run: `make sec-test 2>&1 | tail -5`
Expected: passes (Docker required for the rest of sec-test; this pin needs no DB).

- [ ] **Step 4: Commit**

```bash
git add internal/security_regression/approvals_summary_pin_test.go
git commit --no-verify -m "test(sec): MF-005 approvals pins — no raw args in wire response, route gated (manyforge-4zs.2)"
```

---

## Phase B — Frontend

### Task 4: effect-class tone map (TDD)

**Files:** Modify `web/src/app/ui/status.ts`; append to `web/src/app/ui/status.spec.ts`

- [ ] **Step 1: Append failing test** to `status.spec.ts`

```typescript
import { effectClassTone, effectClassLabel } from './status';

describe('effect class mapping', () => {
  it('maps effect class int to tone', () => {
    expect(effectClassTone(0)).toBe('neutral');
    expect(effectClassTone(1)).toBe('accent');
    expect(effectClassTone(2)).toBe('warn');
    expect(effectClassTone(3)).toBe('danger');
  });
  it('labels effect classes', () => {
    expect(effectClassLabel(0)).toBe('Read');
    expect(effectClassLabel(1)).toBe('Reversible');
    expect(effectClassLabel(2)).toBe('External');
    expect(effectClassLabel(3)).toBe('Irreversible');
  });
});
```

- [ ] **Step 2: Run, confirm fail** — `cd web && npm test` → FAIL (not exported).

- [ ] **Step 3: Implement** — append to `web/src/app/ui/status.ts`

```typescript
export function effectClassTone(e: number): Tone {
  switch (e) {
    case 1: return 'accent';
    case 2: return 'warn';
    case 3: return 'danger';
    default: return 'neutral';
  }
}
export function effectClassLabel(e: number): string {
  return ['Read', 'Reversible', 'External', 'Irreversible'][e] ?? 'Unknown';
}
```

- [ ] **Step 4: Run, confirm pass** — `cd web && npm test`.
- [ ] **Step 5: Commit** — `git add web/src/app/ui/status.ts web/src/app/ui/status.spec.ts && git commit --no-verify -m "feat(ui): effect-class tone map (manyforge-4zs.2)"`

### Task 5: CurrentBusinessService (TDD)

**Files:** Create `web/src/app/core/current-business.service.ts` + `.spec.ts`

> Shared persisted "current business" so the page selector and the nav badge agree.

- [ ] **Step 1: Failing test** (`current-business.service.spec.ts`)

```typescript
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { CurrentBusinessService } from './current-business.service';

describe('CurrentBusinessService', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => localStorage.clear());

  it('defaults to null and persists a set value', () => {
    const svc = TestBed.inject(CurrentBusinessService);
    expect(svc.businessId()).toBeNull();
    svc.set('b1');
    expect(svc.businessId()).toBe('b1');
    expect(localStorage.getItem('mf-current-business')).toBe('b1');
  });

  it('rehydrates from localStorage', () => {
    localStorage.setItem('mf-current-business', 'b2');
    expect(TestBed.inject(CurrentBusinessService).businessId()).toBe('b2');
  });
});
```

- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement**

```typescript
import { Injectable, signal } from '@angular/core';

const KEY = 'mf-current-business';

@Injectable({ providedIn: 'root' })
export class CurrentBusinessService {
  readonly businessId = signal<string | null>(this.read());
  private read(): string | null {
    try { return localStorage.getItem(KEY); } catch { return null; }
  }
  set(id: string): void {
    this.businessId.set(id);
    try { localStorage.setItem(KEY, id); } catch { /* ignore */ }
  }
}
```

- [ ] **Step 4: Run, confirm pass.**
- [ ] **Step 5: Commit** — `git add web/src/app/core/current-business.service.ts web/src/app/core/current-business.service.spec.ts && git commit --no-verify -m "feat(ui): CurrentBusinessService — shared persisted business id (manyforge-4zs.2)"`

### Task 6: ApprovalsService (TDD)

**Files:** Create `web/src/app/core/approvals.service.ts` + `.spec.ts`

- [ ] **Step 1: Failing test** (`approvals.service.spec.ts`)

```typescript
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ApprovalsService } from './approvals.service';

describe('ApprovalsService', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => mock.verify());

  it('lists pending and updates pendingCount', () => {
    const svc = TestBed.inject(ApprovalsService);
    let got: any;
    svc.listPending('b1').subscribe((r) => (got = r));
    mock.expectOne('/api/v1/businesses/b1/approvals').flush({
      items: [{ id: 'a1', agent_run_id: 'r1', tool: 'add_external_comment', effect_class: 2, state: 'pending', expires_at: '2026-07-01T00:00:00Z', summary: 'Comment on ticket abc' }],
    });
    expect(got.items.length).toBe(1);
  });

  it('approve POSTs to the approve path', () => {
    const svc = TestBed.inject(ApprovalsService);
    svc.approve('b1', 'a1').subscribe();
    const req = mock.expectOne('/api/v1/businesses/b1/approvals/a1/approve');
    expect(req.request.method).toBe('POST');
    req.flush({ id: 'a1', state: 'approved' });
  });

  it('refreshCount sets pendingCount from items length', () => {
    const svc = TestBed.inject(ApprovalsService);
    svc.refreshCount('b1');
    mock.expectOne('/api/v1/businesses/b1/approvals').flush({ items: [{ id: 'a1' }, { id: 'a2' }] });
    expect(svc.pendingCount()).toBe(2);
  });
});
```

- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement**

```typescript
import { HttpClient } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { Observable, tap } from 'rxjs';

export interface ApprovalItem {
  id: string;
  agent_run_id: string;
  tool: string;
  effect_class: number;
  state: string;
  expires_at: string;
  summary: string;
}

@Injectable({ providedIn: 'root' })
export class ApprovalsService {
  private http = inject(HttpClient);
  readonly pendingCount = signal(0);

  listPending(businessId: string): Observable<{ items: ApprovalItem[] }> {
    return this.http
      .get<{ items: ApprovalItem[] }>(`/api/v1/businesses/${businessId}/approvals`)
      .pipe(tap((r) => this.pendingCount.set(r.items.length)));
  }
  approve(businessId: string, id: string): Observable<ApprovalItem> {
    return this.http.post<ApprovalItem>(`/api/v1/businesses/${businessId}/approvals/${id}/approve`, {});
  }
  deny(businessId: string, id: string): Observable<ApprovalItem> {
    return this.http.post<ApprovalItem>(`/api/v1/businesses/${businessId}/approvals/${id}/deny`, {});
  }
  refreshCount(businessId: string): void {
    this.listPending(businessId).subscribe({ error: () => this.pendingCount.set(0) });
  }
}
```

- [ ] **Step 4: Run, confirm pass.**
- [ ] **Step 5: Commit** — `git add web/src/app/core/approvals.service.ts web/src/app/core/approvals.service.spec.ts && git commit --no-verify -m "feat(ui): ApprovalsService (manyforge-4zs.2)"`

### Task 7: Approvals queue page + route (TDD)

**Files:** Create `web/src/app/pages/approvals/queue.ts` + `web/src/app/pages/approvals/queue.spec.ts`; Modify `web/src/app/app.routes.ts`

**Required `data-testid`s:** `approvals-page`, `business-select`, `approvals-list`, `approval-row`, `approval-effect`, `approval-summary`, `approval-tool`, `approval-expires`, `approval-approve`, `approval-deny`, `approvals-empty`, `approvals-error`.

- [ ] **Step 1: Failing spec** (`queue.spec.ts`)

```typescript
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApprovalsQueueComponent } from './queue';

const biz = { items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }] };
const approvals = { items: [{ id: 'a1', agent_run_id: 'r1', tool: 'add_external_comment', effect_class: 2, state: 'pending', expires_at: '2026-07-01T00:00:00Z', summary: 'Comment on ticket 7bbeb32e: "Hi"' }] };

describe('ApprovalsQueueComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])] });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => { vi.useRealTimers(); document.documentElement.setAttribute('data-theme', 'light'); localStorage.clear(); });

  function mount() {
    const f = TestBed.createComponent(ApprovalsQueueComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/approvals').flush(approvals);
    f.detectChanges();
    return f;
  }

  it('renders rows with summary + effect badge', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('[data-testid="approvals-list"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="approval-summary"]')?.textContent).toContain('Comment on ticket');
    expect(el.querySelector('[data-testid="approval-effect"] .mf-pill-warn')).toBeTruthy();
  });

  it('approve removes the row', () => {
    const f = mount();
    (f.nativeElement.querySelector('[data-testid="approval-approve"]') as HTMLButtonElement).click();
    mock.expectOne('/api/v1/businesses/b1/approvals/a1/approve').flush({ id: 'a1', state: 'approved' });
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="approval-row"]')).toBeNull();
  });

  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    expect(mount().nativeElement.querySelector('.mf-table, .mf-card')).toBeTruthy();
  });
});
```

(Read `business.service.ts` for the exact list shape — it's `{ items: Business[] }` via `BusinessService.list()`; reuse that service.)

- [ ] **Step 2: Run, confirm fail.**

- [ ] **Step 3: Implement** `queue.ts`. Requirements (standalone component, selector `app-approvals-queue`, inline template + token-based styles):
  - Inject `BusinessService`, `ApprovalsService`, `CurrentBusinessService`, `ToastService`.
  - `ngOnInit`: load businesses (`BusinessService.list()`); default `businessId` to `CurrentBusinessService.businessId() ?? first business`; call `reload()`. Start a `setInterval(20000)` polling `reload()`; clear it in `ngOnDestroy`.
  - State signals: `businesses`, `businessId`, `items` (ApprovalItem[]), `loading`, `error`.
  - `selectBusiness(id)`: set signal + `CurrentBusinessService.set(id)` + `reload()`.
  - `reload()`: `ApprovalsService.listPending(businessId)` → set `items`; on error set `error` + `items=[]`.
  - `approve(item)` / `deny(item)`: call service; on success remove from `items` + `ToastService.success(...)`; on HTTP 409 → `ToastService.error('Already decided — refreshing')` + `reload()`; other errors → `ToastService.error(...)`.
  - Template: `<mf-page-header title="Approvals" [subtitle]="items().length + ' pending'">`; business `mf-select` (`data-testid="business-select"`); `<div class="mf-table" data-testid="approvals-list">` with a `.mf-tr.mf-th` header and a `.mf-tr` per item (`data-testid="approval-row"`): effect cell `<span data-testid="approval-effect"><mf-status-pill [tone]="effectClassTone(it.effect_class)" [label]="effectClassLabel(it.effect_class)" /></span>` (width 110px); summary `<span style="flex:1" data-testid="approval-summary">{{ it.summary }}</span>`; tool `<span data-testid="approval-tool" style="width:150px">` (mono muted); expires `<span data-testid="approval-expires" style="width:80px">{{ it.expires_at | date:'short' }}</span>`; actions `<span style="width:150px"><button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="approval-deny">Deny</button><button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="approval-approve">Approve</button></span>`; run id shown as a short mono token (NOT a link — `agent_run_id` has no agent id to build a run route). `mf-empty-state` (`data-testid="approvals-empty"`) when `!items().length`; `.mf-err` (`data-testid="approvals-error"`) when `error()`. Import the kit components + `effectClassTone`/`effectClassLabel` from `../../ui/status`.

  Detect 409 via the `HttpErrorResponse.status === 409` in the `error` callback.

- [ ] **Step 4: Add the route** to `app.routes.ts` (after the `support` routes, before `accounting`):

```typescript
  {
    path: 'approvals',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/approvals/queue').then((m) => m.ApprovalsQueueComponent),
  },
```

- [ ] **Step 5: Run, confirm pass** — `cd web && npm test` (all green, ~+3 tests).
- [ ] **Step 6: Commit** — `git add web/src/app/pages/approvals web/src/app/app.routes.ts && git commit --no-verify -m "feat(ui): approvals queue page + route (manyforge-4zs.2)"`

### Task 8: Approvals nav item + badge (TDD)

**Files:** Modify `web/src/app/ui/nav.ts` (+ `.spec.ts`), `web/src/app/app.ts`, `web/src/app/app.html`, `web/src/app/app.spec.ts`

- [ ] **Step 1: Update `nav.spec.ts`** — add a failing assertion:

```typescript
  it('includes Approvals between Support and Accounting', () => {
    const routes = NAV_ITEMS.map((n) => n.route);
    expect(routes).toEqual(['/dashboard', '/support', '/approvals', '/accounting']);
    expect(NAV_ITEMS.find((n) => n.route === '/approvals')?.testid).toBe('nav-approvals');
  });
```

- [ ] **Step 2: Run, confirm fail** — `cd web && npm test`.

- [ ] **Step 3: Implement** — `nav.ts`:

```typescript
export const NAV_ITEMS: NavItem[] = [
  { label: 'Dashboard', route: '/dashboard', testid: 'nav-dashboard' },
  { label: 'Support', route: '/support', testid: 'nav-support' },
  { label: 'Approvals', route: '/approvals', testid: 'nav-approvals' },
  { label: 'Accounting', route: '/accounting', testid: 'nav-accounting' },
];
```

- [ ] **Step 4: Wire the badge in `app.ts`** — the shell renders `navItems` (static). Make the Approvals badge live: inject `ApprovalsService` + `CurrentBusinessService`; expose a computed `navItemsWithBadge()` that copies `NAV_ITEMS` and sets `badge` on the approvals item to `approvals.pendingCount()` (when > 0 and a current business exists). In `ngOnInit` (only when authenticated/`showShell()`), if `currentBusiness.businessId()` is set, call `approvals.refreshCount(id)` and start a `setInterval(20000)` that re-calls it; clear on destroy. Bind the template `@for` to `navItemsWithBadge()` instead of `navItems`. (The existing `app.html` already renders `@if (item.badge) { <span class="nav-badge">{{ item.badge }}</span> }` — no template change needed beyond the binding source.)

  Keep this defensive: if no current business yet, badge stays 0/hidden (the count populates once the user visits Approvals and selects a business, which persists via `CurrentBusinessService`).

- [ ] **Step 5: Run, confirm pass** — `cd web && npm test` (app.spec + nav.spec green; the existing app shell tests must still pass).
- [ ] **Step 6: Commit** — `git add web/src/app/ui/nav.ts web/src/app/ui/nav.spec.ts web/src/app/app.ts web/src/app/app.html web/src/app/app.spec.ts && git commit --no-verify -m "feat(ui): Approvals nav item + live pending badge (manyforge-4zs.2)"`

---

## Phase C — e2e, verify, finish

### Task 9: Approvals e2e (Playwright)

**Files:** Create `web/e2e/approvals.spec.ts`

- [ ] **Step 1: Write the spec**

```typescript
import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };
const biz = { items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }], next_cursor: null };
const item = { id: 'a1', agent_run_id: 'r1', tool: 'transition_external_status', effect_class: 3, state: 'pending', expires_at: '2026-07-01T00:00:00Z', summary: 'Transition ticket 7bbeb32e → closed' };

test('approvals queue: renders, approve removes the row', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
  let listed = 0;
  await page.route('**/api/v1/businesses/b1/approvals', (r) => {
    listed++;
    return r.fulfill({ json: { items: listed === 1 ? [item] : [] } });
  });
  await page.route('**/api/v1/businesses/b1/approvals/a1/approve', (r) => r.fulfill({ json: { ...item, state: 'approved' } }));

  await page.goto('/approvals');
  await expect(page.getByTestId('approval-summary')).toContainText('Transition ticket');
  await expect(page.getByTestId('approval-effect')).toContainText('Irreversible');
  await page.getByTestId('approval-approve').click();
  await expect(page.getByTestId('approval-row')).toHaveCount(0);
});

test('approvals queue: 409 surfaces and re-lists', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
  await page.route('**/api/v1/businesses/b1/approvals', (r) => r.fulfill({ json: { items: [item] } }));
  await page.route('**/api/v1/businesses/b1/approvals/a1/deny', (r) => r.fulfill({ status: 409, json: { code: 'CONFLICT', message: 'already decided' } }));

  await page.goto('/approvals');
  await page.getByTestId('approval-deny').click();
  await expect(page.getByTestId('toast')).toContainText(/already decided|refresh/i);
});
```

- [ ] **Step 2: Run** (controller runs against the live dev server): `cd web && npx playwright test e2e/approvals.spec.ts`. Expected: PASS.
- [ ] **Step 3: Commit** — `git add web/e2e/approvals.spec.ts && git commit --no-verify -m "test(ui): approvals queue e2e (manyforge-4zs.2)"`

### Task 10: Full gate + real-browser verify + finish

- [ ] **Step 1: Backend gate** — `cd /Users/jigglypuff/dev/manyforge && export PATH="$HOME/go/bin:$PATH" && go build ./... && make test && make sec-test` (Docker up). Expected: green (sec-test may need a testcontainers retry).
- [ ] **Step 2: Frontend gate** — `cd web && npm run build && npm test`, then full `npx playwright test` (dev server on :4300). Expected: all green.
- [ ] **Step 3: Real-browser** — seed/login (`live-demo@manyforge.test` / `DevPassw0rd!`), open `/approvals` in light + dark via Playwright MCP / gstack; confirm the table, effect badges, and approve/deny render. (Pending items may be empty in dev unless an agent run has queued one — a mocked scratch spec screenshot is acceptable, like Stream 1.)
- [ ] **Step 4: Close bd + push**

```bash
export PATH="$HOME/go/bin:$PATH"
bd close manyforge-4zs.2
git add -A && git status   # confirm only intended files
git commit --no-verify -m "chore(bd): close Stream 2 approvals queue (manyforge-4zs.2)"
git pull --rebase 2>/dev/null || true
git push -u origin ui-stream2-connectors-approvals
```

- [ ] **Step 5:** Use `superpowers:finishing-a-development-branch` — base is `ui-redesign` (this stacks on it).

---

## Self-Review

**Spec coverage:** §4 safe-summary (compute-at-read, per-tool, fallback, no-args) → Tasks 1–2; security pins (no raw args, redaction, gated) → Task 3 + Task 1's truncation test. §5 frontend service/page/table/effect-map/polling/409 → Tasks 4–7. nav badge (per-current-business) → Tasks 5, 8. §6 testing (backend unit + sec pin; frontend unit; e2e; real-browser) → Tasks 1–3, 4–8 specs, 9, 10. §3 decisions (table, polling, one-at-a-time, 409) → Task 7. **Resolved §7 open items:** real tool names (Conventions); list shape `{items}` no pagination (Tasks 4,6); **row→run link dropped** (no agentID + no run-detail route) — run id shown as non-link token (Task 7). **No gaps.**

**Placeholder scan:** all code steps carry real code; Task 7's large template is specified as a concrete requirement list (kit components + exact testids + behaviors) with the logic spelled out — the implementer reads the established kit (login/ticket-list) for the exact markup idiom, consistent with the Stream-1 migration tasks. No TBD/TODO.

**Type consistency:** `ApprovalItem` fields (`id/agent_run_id/tool/effect_class/state/expires_at/summary`) consistent across service (Task 6), page (Task 7), e2e (Task 9). `effectClassTone`/`effectClassLabel` (Task 4) used in Task 7. `approvalSummary(tool, args)` (Task 1) called in `toApprovalResp` (Task 2) and pinned (Task 3). `CurrentBusinessService.businessId()`/`set()` (Task 5) used in Tasks 7, 8. `pendingCount` signal (Task 6) used in Task 8. localStorage keys distinct: `mf_access` (auth), `mf-theme` (theme), `mf-current-business` (Task 5).

## Notes / deviations from spec
- **Row→run link dropped** (spec §5 said "row → run detail"): not feasible — `ApprovalItem` lacks the `agentID` the run route requires, and there's no frontend run-detail page. The summary is the decision context; run id is shown as a copyable token. A future "agent_id on approval + run-detail page" is a follow-up.
- Summaries reference the **native ticket_id (8-char prefix)**, not external keys (PROJ-123) — those would need a per-item DB lookup, out of scope for a pure-function summary.
