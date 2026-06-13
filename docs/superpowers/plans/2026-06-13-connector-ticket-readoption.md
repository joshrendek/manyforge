# Connector Ticket Re-adoption Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On connector create, auto-relink orphaned tickets (and their messages) from a previously-deleted connector to the same provider host, so they resume under the new connector instead of being re-imported as duplicates.

**Architecture:** Three sqlc queries (count candidates, relink winning tickets, relink their messages) called inside the existing RLS-gated `Service.Create` transaction in `internal/connectors/service.go`. Newest-per-`external_id` wins; duplicates stay detached. A `connector.tickets_readopted` audit row records the counts. No DEFINER (the owner has RLS access to their own tickets), no UI.

**Tech Stack:** Go, pgx, sqlc (pinned **v1.27.0** — regenerate with `/opt/homebrew/bin/sqlc generate`, NOT the global v1.31.1), Postgres, testcontainers integration tests.

---

### Task 1: sqlc queries for re-adoption

**Files:**
- Modify: `db/query/connector_manage.sql` (append queries)
- Modify (generated): `internal/platform/db/dbgen/connector_manage.sql.go`, `internal/platform/db/dbgen/querier.go`

- [ ] **Step 1: Add the three queries to `db/query/connector_manage.sql`**

Append:

```sql
-- name: CountReadoptableTickets :one
-- Count detached (native) tickets in this business that belong to the connector's provider host
-- (same scheme://host as base_url). Used to derive the skipped-duplicate count after relink.
SELECT count(*) FROM ticket
WHERE business_id = sqlc.arg('business_id')::uuid
  AND connector_id IS NULL
  AND external_id IS NOT NULL
  AND split_part(external_url, '/', 3) = split_part(sqlc.arg('base_url')::text, '/', 3);

-- name: ReadoptDetachedTickets :many
-- Relink the newest detached ticket per external_id (for this business + provider host) to the
-- new connector; duplicates (older rows sharing an external_id) stay detached so the
-- (connector_id, external_id) unique index is never violated. Returns the relinked ticket ids.
WITH ranked AS (
    SELECT id,
           row_number() OVER (PARTITION BY external_id ORDER BY updated_at DESC) AS rn
    FROM ticket
    WHERE business_id = sqlc.arg('business_id')::uuid
      AND connector_id IS NULL
      AND external_id IS NOT NULL
      AND split_part(external_url, '/', 3) = split_part(sqlc.arg('base_url')::text, '/', 3)
)
UPDATE ticket t
SET connector_id = sqlc.arg('connector_id')::uuid, updated_at = now()
FROM ranked r
WHERE t.id = r.id AND r.rn = 1
RETURNING t.id;

-- name: RelinkReadoptedMessages :exec
-- Restore connector_id on the re-adopted tickets' messages. Gated on external_id IS NOT NULL to
-- satisfy ticket_message_connector_external_chk (connector_id set ⇒ external_id present); messages
-- without an external id correctly stay native.
UPDATE ticket_message
SET connector_id = sqlc.arg('connector_id')::uuid
WHERE business_id = sqlc.arg('business_id')::uuid
  AND ticket_id = ANY(sqlc.arg('ticket_ids')::uuid[])
  AND connector_id IS NULL
  AND external_id IS NOT NULL;
```

- [ ] **Step 2: Regenerate dbgen with the pinned sqlc**

Run: `/opt/homebrew/bin/sqlc generate`
Expected: exit 0, no output. Then verify ONLY the new symbols churned:
Run: `git status -s -- internal/platform/db/dbgen/ | grep -v CLAUDE`
Expected: `M internal/platform/db/dbgen/connector_manage.sql.go` and `M internal/platform/db/dbgen/querier.go` only.
Run: `grep -c 'CountReadoptableTickets\|ReadoptDetachedTickets\|RelinkReadoptedMessages' internal/platform/db/dbgen/connector_manage.sql.go`
Expected: `3` (or more — the const + params + func per query).

- [ ] **Step 3: Build to confirm generated code compiles**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./internal/platform/db/...`
Expected: no output (success).

- [ ] **Step 4: Commit**

```bash
git add db/query/connector_manage.sql internal/platform/db/dbgen/connector_manage.sql.go internal/platform/db/dbgen/querier.go
git commit -m "feat(connectors): re-adoption sqlc queries (manyforge-7zx)"
```

---

### Task 2: wire re-adoption into Service.Create

**Files:**
- Modify: `internal/connectors/service.go` (inside `Create`, after `InsertConnector`, around line 82)

- [ ] **Step 1: Add the relink + audit inside the Create tx**

In `internal/connectors/service.go`, within the `s.DB.WithPrincipal(...)` closure in `Create`, AFTER the `InsertConnector` block returns successfully and BEFORE the existing `connector.created` audit, insert:

```go
		// Re-adopt orphaned tickets from a previously-deleted connector to the same provider host
		// (manyforge-7zx): relink the newest detached ticket per external_id + its messages, so a
		// recreated connector resumes instead of re-importing duplicates. Bounded by existing
		// detached rows (never imports). Same tx → atomic with the create.
		q := dbgen.New(tx)
		candidates, perr := q.CountReadoptableTickets(ctx, dbgen.CountReadoptableTicketsParams{
			BusinessID: businessID, BaseUrl: in.BaseURL,
		})
		if perr != nil {
			return perr
		}
		readoptedIDs, perr := q.ReadoptDetachedTickets(ctx, dbgen.ReadoptDetachedTicketsParams{
			BusinessID: businessID, BaseUrl: in.BaseURL, ConnectorID: id,
		})
		if perr != nil {
			return perr
		}
		if len(readoptedIDs) > 0 {
			if perr := q.RelinkReadoptedMessages(ctx, dbgen.RelinkReadoptedMessagesParams{
				BusinessID: businessID, ConnectorID: id, TicketIds: readoptedIDs,
			}); perr != nil {
				return perr
			}
			rtt := "connector"
			if werr := audit.Write(ctx, tx, audit.Entry{
				BusinessID:       &businessID,
				ActorPrincipalID: &principalID,
				Action:           "connector.tickets_readopted",
				TargetType:       &rtt,
				TargetID:         &id,
				Inputs: map[string]any{
					"readopted_count":         len(readoptedIDs),
					"skipped_duplicate_count": int(candidates) - len(readoptedIDs),
				},
			}); werr != nil {
				return werr
			}
		}
```

Note: the existing audit block below already declares `tt := "connector"` and uses `audit.Write`; the new block uses its own `rtt` and `q` (declared once here, reusable). Verify `dbgen` and `audit` are already imported in `service.go` (they are — used by the surrounding code).

- [ ] **Step 2: Build**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./...`
Expected: no output (success).

- [ ] **Step 3: Commit**

```bash
git add internal/connectors/service.go
git commit -m "feat(connectors): auto-re-adopt detached tickets on create (manyforge-7zx)"
```

---

### Task 3: integration tests

**Files:**
- Create: `internal/connectors/readoption_integration_test.go`

These need the connectors integration harness (`startConn`, `newConnService`, `jiraInput`, `syncIssueSQL`, `tdb.Super`) — the same helpers used by `outbound_integration_test.go`. Inspect `internal/connectors/credential_integration_test.go` and `outbound_integration_test.go` for the exact seed shapes before writing.

- [ ] **Step 1: Write the failing happy-path + dup + host + CHECK tests**

First **read** `internal/connectors/outbound_integration_test.go` and `internal/connectors/credential_integration_test.go` to copy the exact: import block (the `context`, `testdb`, `uuid`, `pgx` imports), the `startConn(t)` return shape (`ctx, tdb, seed` with `seed.businessID` / `seed.principalID`), the `ticket` insert columns used by `syncIssueSQL` (subject/status/priority/tenant_root_id and a VALID `ticket.status` literal such as `"open"`), and the `jiraInput()` helper. The seed SQL below is indicative — make the column names match reality.

Create `internal/connectors/readoption_integration_test.go` with this file header + helper:

```go
//go:build integration

package connectors

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// seedDetachedTicket inserts a native ticket (connector_id NULL) with a preserved external_id +
// external_url under `host`, updated at `updatedAt`. Returns the ticket id. Uses Super (RLS-bypass
// seed role), mirroring outbound_integration_test.go. NOTE: align the column list + status literal
// with syncIssueSQL in outbound_integration_test.go before running.
func seedDetachedTicket(t *testing.T, ctx context.Context, tdb *testdb.TestDB, businessID uuid.UUID, externalID, host string, updatedAt time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	url := fmt.Sprintf("https://%s/browse/%s", host, externalID)
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket (business_id, tenant_root_id, subject, status, priority, external_id, external_url, created_at, updated_at)
		VALUES ($1,$1,'Detached','open','normal',$2,$3, now(), $4) RETURNING id`,
		businessID, externalID, url, updatedAt).Scan(&id); err != nil {
		t.Fatalf("seed detached ticket: %v", err)
	}
	return id
}
```

Then the test functions:

```go
// TestReadopt_RelinksOnCreate: a detached ticket + its message (with external_id) are relinked to
// a newly-created connector for the same host.
func TestReadopt_RelinksOnCreate(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	host := "acme.atlassian.net"
	ticketID := seedDetachedTicket(t, ctx, tdb, seed.businessID, "JIRA-1", host, time.Now().UTC())
	// A message on that ticket WITH an external_id (eligible) and one WITHOUT (must stay native).
	var msgEligible, msgNoExt uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket_message (ticket_id, business_id, tenant_root_id, direction, message_id, external_id, body_text)
		VALUES ($1,$2,$2,'inbound','m-ext-1','jira-c-1','hi') RETURNING id`,
		ticketID, seed.businessID).Scan(&msgEligible); err != nil {
		t.Fatalf("seed eligible msg: %v", err)
	}
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket_message (ticket_id, business_id, tenant_root_id, direction, message_id, body_text)
		VALUES ($1,$2,$2,'inbound','m-no-ext','no ext') RETURNING id`,
		ticketID, seed.businessID).Scan(&msgNoExt); err != nil {
		t.Fatalf("seed no-ext msg: %v", err)
	}

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInputForHost(host))
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Ticket relinked.
	var gotConn *uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket WHERE id=$1`, ticketID).Scan(&gotConn); err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	if gotConn == nil || *gotConn != connID {
		t.Fatalf("ticket connector_id = %v, want %v", gotConn, connID)
	}
	// Eligible message relinked; no-ext message stays native.
	var eligConn, noExtConn *uuid.UUID
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket_message WHERE id=$1`, msgEligible).Scan(&eligConn)
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket_message WHERE id=$1`, msgNoExt).Scan(&noExtConn)
	if eligConn == nil || *eligConn != connID {
		t.Errorf("eligible message connector_id = %v, want %v", eligConn, connID)
	}
	if noExtConn != nil {
		t.Errorf("no-external-id message connector_id = %v, want nil (CHECK keeps it native)", noExtConn)
	}
	// Audit row with readopted_count = 1.
	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE action='connector.tickets_readopted' AND target_id=$1`,
		connID).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 1 {
		t.Errorf("readopted audit rows = %d, want 1", n)
	}
}

// TestReadopt_DuplicateExternalIDKeepsNewest: two orphans share an external_id → newest relinked,
// older stays detached.
func TestReadopt_DuplicateExternalIDKeepsNewest(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	host := "acme.atlassian.net"
	older := seedDetachedTicket(t, ctx, tdb, seed.businessID, "JIRA-9", host, time.Now().UTC().Add(-time.Hour))
	newer := seedDetachedTicket(t, ctx, tdb, seed.businessID, "JIRA-9", host, time.Now().UTC())

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInputForHost(host))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var newerConn, olderConn *uuid.UUID
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket WHERE id=$1`, newer).Scan(&newerConn)
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket WHERE id=$1`, older).Scan(&olderConn)
	if newerConn == nil || *newerConn != connID {
		t.Errorf("newer ticket not relinked: %v", newerConn)
	}
	if olderConn != nil {
		t.Errorf("older duplicate relinked (%v), want nil", olderConn)
	}
	var skipped int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT (inputs->>'skipped_duplicate_count')::int FROM audit_entry WHERE action='connector.tickets_readopted' AND target_id=$1`,
		connID).Scan(&skipped); err != nil {
		t.Fatalf("read skipped count: %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped_duplicate_count = %d, want 1", skipped)
	}
}

// TestReadopt_DifferentHostNotRelinked: an orphan whose host differs is left detached.
func TestReadopt_DifferentHostNotRelinked(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	other := seedDetachedTicket(t, ctx, tdb, seed.businessID, "OTHER-1", "other.atlassian.net", time.Now().UTC())

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInputForHost("acme.atlassian.net"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var conn *uuid.UUID
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket WHERE id=$1`, other).Scan(&conn)
	if conn != nil {
		t.Errorf("different-host ticket relinked (%v), want nil", conn)
	}
	_ = connID
}
```

`jiraInputForHost(host)` is a helper returning a `CreateConnectorInput` whose `BaseURL` is `https://<host>` — model it on the existing `jiraInput()` helper (copy its field values, override `BaseURL`). If `jiraInput()` already lets you set the base URL, use that instead. Read the helper before writing.

- [ ] **Step 2: Run the tests to verify they FAIL (re-adoption not yet wired if Task 2 skipped) or PASS (if Task 2 done)**

Run: `export PATH="$HOME/go/bin:$PATH" && go test -tags integration -run 'TestReadopt' ./internal/connectors/ -v`
Expected after Tasks 1-2: PASS. (If you are doing strict TDD, run this BEFORE Task 2's edit and confirm `TestReadopt_RelinksOnCreate` fails with `ticket connector_id = <nil>`.)

- [ ] **Step 3: Fix any harness mismatches (column names, helper signatures) until green**

Iterate on the seed SQL / helper calls against the real `outbound_integration_test.go` shapes until the suite passes.

- [ ] **Step 4: Commit**

```bash
git add internal/connectors/readoption_integration_test.go
git commit -m "test(connectors): integration tests for ticket re-adoption (manyforge-7zx)"
```

---

### Task 4: source pin + final gates

**Files:**
- Modify: `internal/connectors/readoption_integration_test.go` (add a source-pin test) OR a new pin under `internal/security_regression/`.

- [ ] **Step 1: Add a source pin that re-adoption stays wired into Create**

Add to the test file:

```go
// TestReadopt_WiredIntoCreate_SourcePin fails loudly if a refactor drops the re-adoption call
// from Service.Create (the behavior is otherwise easy to silently delete).
func TestReadopt_WiredIntoCreate_SourcePin(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	if !strings.Contains(string(src), "ReadoptDetachedTickets") {
		t.Fatal("Service.Create no longer calls ReadoptDetachedTickets — re-adoption was dropped (manyforge-7zx)")
	}
}
```

Add `"os"` and `"strings"` to the test file imports.

- [ ] **Step 2: Run the full connectors suite + quality gates**

Run: `export PATH="$HOME/go/bin:$PATH" && go test -tags integration ./internal/connectors/...`
Expected: ok.
Run: `make test && make sec-test && make lint`
Expected: all exit 0, no FAIL.

- [ ] **Step 3: Commit + close the issue**

```bash
git add internal/connectors/readoption_integration_test.go
git commit -m "test(connectors): source-pin re-adoption wiring (manyforge-7zx)"
bd close manyforge-7zx --reason "Auto-re-adopt detached tickets + messages on connector create (newest-per-external_id, host match), audited. Integration + source-pin tests green."
git add .beads/issues.jsonl && git commit -m "chore(bd): close manyforge-7zx (ticket re-adoption)"
git pull --rebase && git push
```

---

## Notes for the implementer
- **sqlc:** ALWAYS use `/opt/homebrew/bin/sqlc generate` (v1.27.0). The global `~/go/bin/sqlc` is v1.31.1 and churns the whole dbgen layer — do not use it.
- **Read before writing tests:** the integration harness column names (`ticket`, `ticket_message`) and helpers (`startConn`, `jiraInput`, `newConnService`, `tdb.Super`) must be copied from `internal/connectors/outbound_integration_test.go` and `credential_integration_test.go` — the seed SQL in this plan is indicative, not guaranteed column-accurate.
- **Atomicity:** the relink lives in the same `WithPrincipal` tx as the insert + audit; a relink error rolls back the whole create.
- **No DEFINER:** the caller (owner) has RLS access to their own business's tickets, so plain `dbgen` queries under `WithPrincipal` suffice.
