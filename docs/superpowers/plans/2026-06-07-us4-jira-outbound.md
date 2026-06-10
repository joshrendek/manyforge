# US4 — Jira Outbound + Full Bidirectional Sync + Conflict Finalization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make ManyForge tickets push changes *back* to Jira — a native reply becomes a Jira comment, a native ticket can be escalated into a new Jira issue with its `external_id` written back, and inbound external-wins conflicts are audited when a locally-edited scalar is clobbered.

**Architecture:** A purpose-built `connector_outbound_op` queue table (populated in the source write's transaction) plus a principal-less **OutboundDispatcher** background poller modeled on the existing `Reconciler` (no DB tx held across the HTTP call). The dispatcher posts to Jira through the SSRF-safe netsafe client already used inbound, then writes the resulting `external_id` back onto the native message/ticket via SECURITY DEFINER functions, auditing every external write. Conflict finalization is a `CREATE OR REPLACE` of the existing inbound `sync_inbound_external_issue` DEFINER that audits any scalar where *both* sides diverged from the last snapshot.

**Tech Stack:** Go 1.x, pgx/v5, sqlc (dbgen), PostgreSQL (SECURITY DEFINER + RLS + composite FKs), `net/http` + `internal/platform/netsafe` (SSRF-safe), `internal/platform/crypto` Sealer, `internal/platform/audit`, testcontainers (`//go:build integration`), `httptest` Jira stub.

---

## Design decisions (read before implementing)

These resolve genuine forks the spec/US3-review left open. The spec-compliance reviewer should validate them against `docs/superpowers/specs/2026-06-06-external-connectors-design.md` §5.3 + §7.

1. **Outbound is a poller + an op table, NOT a Bus subscriber on `connector.outbound.sync`.**
   *Why:* US4 bd-note (b) (from the US3 final review) requires outbound to follow the reconciler's **no-tx-across-HTTP** pattern, NOT the inbound subscriber's in-tx `FetchIssue`. A `events.Bus` subscriber structurally *cannot* honor that: the outbox worker (`internal/platform/events/outbox.go`) holds a savepoint tx for the entire `Handle(ctx, tx, e)` call, so any HTTP inside it is in-tx. A dedicated poller (claim in tx#1 → HTTP with no tx → write-back in tx#2) is the only shape that honors note (b). It also gives the `attempts`/`status` state a non-idempotent external API needs.
   *Spec reconciliation (§5.3 "enqueues `connector.outbound.sync` in the source write's tx"):* The intent — *the source write transactionally records outbound work; a worker later posts + writes back + audits* — is fully satisfied. The work is recorded into a purpose-built `connector_outbound_op` row (in the source tx) instead of a generic `outbox` row. The `TopicConnectorOutboundSync` const (declared in US2, **currently referenced nowhere**) stays unused; removing it is a trivial follow-up, out of scope here.

2. **Outbound dispatch is ADDITIVE to the existing email enqueue; native email is NOT suppressed for connector-linked tickets.**
   *Why:* Suppressing the `ticket.replied` email for connector-linked tickets is a product/UX decision (double-notify vs single-channel) with no spec mandate and real regression risk to the email path's tests. US4 keeps the existing `events.Enqueue(..., TopicTicketReplied, ...)` untouched and *adds* the outbound-op enqueue. Whether to suppress email is filed as follow-up `manyforge-a7j.8` (see "Follow-ups").

3. **Idempotency anchor = the native row's `external_id`.** The dispatcher skips the POST if the linked message/ticket already carries an `external_id` (a prior attempt succeeded). Re-delivery after a committed write-back is a no-op. The residual duplicate window (crash after a successful Jira POST but before the write-back tx commits) is inherent to at-least-once delivery against an API with no client idempotency key, and is documented — not closed — by US4.

4. **"New connector-linked ticket → issue" is an explicit escalation, not an auto-push.** There is no native ticket-create path in the codebase, and the `ticket_connector_external_chk` CHECK (`connector_id IS NULL OR external_id IS NOT NULL`) forbids a "linked but not-yet-pushed" ticket state. So US4 adds `connectors.Service.EnqueueOutboundCreateIssue(ctx, principal, business, ticketID, connectorID)` — it validates ownership and records a `create_issue` op; the dispatcher calls `CreateIssue`, then atomically sets `connector_id`+`external_id`+`external_url` on the ticket (satisfying the CHECK in one statement). This is the plumbing US6's gated tools will later drive.

5. **Outbound HTTP reuses the existing Jira client + netsafe factory** (`internal/connectors/jira`, built via `Registry.BuildSystem`). No new HTTP client; SSRF safety is inherited from US3's `netsafe.NewClientWithOptions`.

---

## File structure

**New files:**
- `migrations/0045_connector_outbound.up.sql` / `.down.sql` — `connector_outbound_op` table + outbound DEFINERs.
- `migrations/0046_connector_conflict_audit.up.sql` / `.down.sql` — `CREATE OR REPLACE sync_inbound_external_issue` with both-changed audit.
- `db/query/connector_outbound.sql` — RLS-scoped producer inserts (sqlc → dbgen).
- `internal/connectors/outbound.go` — `OutboundDispatcher` poller + payload helpers.
- `internal/connectors/outbound_integration_test.go` — `//go:build integration` dispatcher round-trips.
- `internal/connectors/jira/testdata/create_issue_response.json` — golden fixture.
- `internal/security_regression/mf_004_us4_outbound_test.go` — conflict + SSRF + idempotency pins.

**Modified files:**
- `internal/connectors/connector.go` — add `CreateIssue` to the interface + `ExternalIssueDraft` struct.
- `internal/connectors/connector_test.go` — add `CreateIssue` to `fakeConnector`.
- `internal/connectors/jira/client.go` — implement `CreateIssue` + `jiraCreateIssueResponse`.
- `internal/connectors/jira/client_test.go` — `CreateIssue` unit test.
- `internal/connectors/service.go` — add `EnqueueOutboundCreateIssue`.
- `internal/ticketing/service.go` — producer hook in `Reply`.
- `db/schema.sql` — mirror the `connector_outbound_op` table (no DEFAULT/RLS/trigger).
- `cmd/manyforge/main.go` — construct + start `OutboundDispatcher`.

**Conventions (from HANDOFF.md "Gotchas" — do not relearn):**
- Trust `go build ./...` / `go test`, NOT gopls (it falsely reports `undefined: dbgen.X` and "No packages found" on `//go:build integration` files).
- Nullable types: `uuid NULL`→`pgtype.UUID`; `text NULL`→`*string`; `timestamptz NULL`→`pgtype.Timestamptz`; `text NOT NULL`→`string`.
- Two casings: Go service structs use `BaseURL`; dbgen uses `BaseUrl`/`ExternalUrl` (Url, not URL).
- DEFINER fns are called via raw `tx.QueryRow`/`tx.Exec`, NOT sqlc-wrapped. Producer inserts (principal-ful) ARE sqlc.
- Every new table: `business_id`+`tenant_root_id`, composite `FOREIGN KEY (x_id, tenant_root_id) REFERENCES parent(id, tenant_root_id)`, `support_tenant_root_immutable()` trigger, RLS (`authorized_businesses(current_principal())`), `GRANT ... TO manyforge_app`. Mirror columns into `db/schema.sql` (strip DEFAULT/RLS/trigger).
- Every DEFINER: `SECURITY DEFINER SET search_path = public`, derive tenancy internally from `connector_id`, then `REVOKE ALL ON FUNCTION fn(argtypes) FROM PUBLIC;` + `GRANT EXECUTE ON FUNCTION fn(argtypes) TO manyforge_app;`.
- Commits: `--no-verify`, NO `Co-Authored-By`. The bd hook re-exports `.beads/issues.jsonl` on commit — `git add` it.
- After editing `db/schema.sql`/`db/query/`: `make generate` (sqlc). Pre-flight: `export PATH="$PATH:$HOME/go/bin"`.
- Connectors-only fast loop: `go test -tags integration -p 1 ./internal/connectors/ -v` (needs Docker; testcontainers auto-applies `migrations/`).

---

### Task 1: `CreateIssue` on the connector interface + Jira client

**Files:**
- Modify: `internal/connectors/connector.go`
- Modify: `internal/connectors/connector_test.go`
- Modify: `internal/connectors/jira/client.go`
- Create: `internal/connectors/jira/testdata/create_issue_response.json`
- Modify: `internal/connectors/jira/client_test.go`

- [x] **Step 1: Write the failing Jira-client unit test**

Add to `internal/connectors/jira/client_test.go` (it already has `newTestClient`, `loadGolden`, `httptest`):

```go
func TestCreateIssue(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadGolden(t, "create_issue_response.json"))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "ops@acme.test", "tok", "whsec", srv.Client())
	iss, err := c.CreateIssue(context.Background(), connectors.ExternalIssueDraft{
		ProjectKey: "SUP", IssueType: "Task", Summary: "Login broken", Description: "user can't log in",
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if iss.ExternalID != "SUP-42" {
		t.Fatalf("external id = %q, want SUP-42", iss.ExternalID)
	}
	if gotMethod != http.MethodPost || gotPath != "/rest/api/3/issue" {
		t.Fatalf("request = %s %s, want POST /rest/api/3/issue", gotMethod, gotPath)
	}
	if gotAuth == "" {
		t.Fatalf("missing basic auth")
	}
	fields, _ := gotBody["fields"].(map[string]any)
	if fields == nil || fields["summary"] != "Login broken" {
		t.Fatalf("fields summary wrong: %+v", gotBody)
	}
	proj, _ := fields["project"].(map[string]any)
	if proj == nil || proj["key"] != "SUP" {
		t.Fatalf("project key wrong: %+v", fields)
	}
}

func TestCreateIssueRejectsEmptyProject(t *testing.T) {
	c := newTestClient("https://acme.atlassian.net", "ops@acme.test", "tok", "whsec", http.DefaultClient)
	_, err := c.CreateIssue(context.Background(), connectors.ExternalIssueDraft{IssueType: "Task", Summary: "x"})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream for empty project key", err)
	}
}
```

Create `internal/connectors/jira/testdata/create_issue_response.json`:

```json
{"id":"10042","key":"SUP-42","self":"https://acme.atlassian.net/rest/api/3/issue/10042"}
```

- [x] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/connectors/jira/ -run TestCreateIssue -v`
Expected: FAIL — `c.CreateIssue undefined` and `connectors.ExternalIssueDraft undefined`.

- [x] **Step 3: Add the interface method + draft struct**

In `internal/connectors/connector.go`, add after `ExternalIssue` (line 35) the draft struct:

```go
// ExternalIssueDraft is the minimal payload to create a new external issue (US4 outbound).
type ExternalIssueDraft struct {
	ProjectKey    string // required: external project/space key the issue is created in
	IssueType     string // required: external issue type name (e.g. "Task")
	Summary       string // maps from native ticket.subject
	Description    string // maps from the native message body
	ReporterEmail string // best-effort; empty is fine
}
```

In the `TicketingConnector` interface (after line 52, the `PostComment` line) add:

```go
	// CreateIssue creates a new external issue from a native ticket, returning its
	// assigned external id + URL. Used by US4 outbound "new linked ticket -> issue".
	CreateIssue(ctx context.Context, draft ExternalIssueDraft) (ExternalIssue, error)
```

- [x] **Step 4: Add the stub to `fakeConnector`**

In `internal/connectors/connector_test.go`, after the `PostComment` method (line 23) add:

```go
func (f *fakeConnector) CreateIssue(ctx context.Context, draft ExternalIssueDraft) (ExternalIssue, error) {
	return ExternalIssue{ExternalID: "JIRA-NEW", URL: "https://example.test/JIRA-NEW", Title: draft.Summary}, nil
}
```

- [x] **Step 5: Implement `CreateIssue` on the Jira client**

In `internal/connectors/jira/client.go`, add after `PostComment` (after line 141). It mirrors `PostComment`'s `doJSON` usage and uses `buildADFDoc` for the description:

```go
// CreateIssue creates a new Jira issue in the configured project, returning its key + URL.
func (c *client) CreateIssue(ctx context.Context, draft connectors.ExternalIssueDraft) (connectors.ExternalIssue, error) {
	if draft.ProjectKey == "" || draft.IssueType == "" {
		return connectors.ExternalIssue{}, fmt.Errorf("jira: project key and issue type required: %w", ErrUpstream)
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return connectors.ExternalIssue{}, fmt.Errorf("jira: invalid base_url: %w", ErrUpstream)
	}
	issueURL := base.JoinPath("rest", "api", "3", "issue")

	fields := map[string]any{
		"project":   map[string]any{"key": draft.ProjectKey},
		"issuetype": map[string]any{"name": draft.IssueType},
		"summary":   draft.Summary,
	}
	if draft.Description != "" {
		fields["description"] = buildADFDoc(draft.Description)
	}
	payload, err := json.Marshal(map[string]any{"fields": fields})
	if err != nil {
		return connectors.ExternalIssue{}, fmt.Errorf("jira: marshal create: %w", ErrUpstream)
	}

	var resp jiraCreateIssueResponse
	if err := c.doJSON(ctx, http.MethodPost, issueURL.String(), payload, &resp); err != nil {
		return connectors.ExternalIssue{}, err
	}
	return connectors.ExternalIssue{
		ExternalID: resp.Key,
		URL:        base.JoinPath("browse", resp.Key).String(),
		Title:      draft.Summary,
	}, nil
}
```

Add the response struct next to `jiraComment` (after line 416):

```go
type jiraCreateIssueResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}
```

- [x] **Step 6: Run the unit + interface tests to verify they pass**

Run: `go test ./internal/connectors/jira/ -run TestCreateIssue -v && go test ./internal/connectors/ -run TestFakeConnector -v`
Expected: PASS. Then `go build ./...` (clean — confirms every other `TicketingConnector` implementer compiles).

- [x] **Step 7: Commit**

```bash
gofmt -w internal/connectors/
git add internal/connectors/connector.go internal/connectors/connector_test.go internal/connectors/jira/ .beads/issues.jsonl
git commit --no-verify -m "feat(connectors/jira): T1 — CreateIssue on connector interface + Jira client (manyforge-a7j.4)"
```

> ✅ **Task 1 DONE** (commit `170e929`). TDD: 3 client tests pass (`TestCreateIssue`, `TestCreateIssueRejectsEmptyProject`, `TestCreateIssueOmitsEmptyDescription`); `go build ./...` clean. Spec-compliance ✅, code-quality APPROVED (re-reviewed after 2 accepted minor fixes: added empty-`Description` branch test + corrected `CreateIssue` doc comment). `manyforge-a7j.4.1` closed.

---

### Task 2: Migration 0045 — outbound op table + DEFINERs

**Files:**
- Create: `migrations/0045_connector_outbound.up.sql`
- Create: `migrations/0045_connector_outbound.down.sql`
- Modify: `db/schema.sql`
- Create/Modify: `internal/connectors/outbound_integration_test.go`

- [x] **Step 1: Write the failing SQL-level integration test**

Create `internal/connectors/outbound_integration_test.go` (white-box, package `connectors`):

```go
//go:build integration

package connectors

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestOutboundOpClaimComplete exercises the 0045 DEFINERs at the SQL level:
// enqueue a comment op, claim it, complete it, and assert the native message
// got its external_id stamped and the op is done.
func TestOutboundOpClaimComplete(t *testing.T) {
	tdb := startConn(t)
	ctx := context.Background()
	seed := seedConnectorTenant(ctx, t, tdb) // existing US3 helper: business+tenant+principal+connector+ticket

	// Insert a connector-linked native outbound message awaiting dispatch (external_id NULL).
	var msgID uuid.UUID
	err := tdb.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO ticket_message (ticket_id, business_id, tenant_root_id, direction, message_id, body_text)
			VALUES ($1,$2,$3,'outbound','m-out-1','please retry')
			RETURNING id`,
			seed.TicketID, seed.BusinessID, seed.TenantRootID).Scan(&msgID)
	})
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}

	// Enqueue a comment op (principal-less raw insert here is fine — RLS is bypassed by the owner role used in tests).
	var opID uuid.UUID
	if err := tdb.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO connector_outbound_op (business_id, tenant_root_id, connector_id, ticket_id, message_id, op_type, body)
			VALUES ($1,$2,$3,$4,$5,'comment','please retry') RETURNING id`,
			seed.BusinessID, seed.TenantRootID, seed.ConnectorID, seed.TicketID, msgID).Scan(&opID)
	}); err != nil {
		t.Fatalf("enqueue op: %v", err)
	}

	// Claim: marks in_progress, returns the op with the ticket's external_id + body.
	var claimedOp, claimedMsg uuid.UUID
	var opType, body string
	var ticketExt *string
	if err := tdb.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT op_id, op_type, message_id, ticket_external_id, body
			FROM claim_outbound_ops(10) LIMIT 1`).
			Scan(&claimedOp, &opType, &claimedMsg, &ticketExt, &body)
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimedOp != opID || opType != "comment" || claimedMsg != msgID {
		t.Fatalf("claim mismatch: op=%v type=%v msg=%v", claimedOp, opType, claimedMsg)
	}

	// Complete: stamp external_id back onto the message + mark op done.
	if err := tdb.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT complete_outbound_comment($1,$2,$3,$4)`,
			opID, msgID, seed.ConnectorID, "jira-comment-99")
		return e
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var gotExt *string
	var gotStatus string
	_ = tdb.WithTx(ctx, func(tx pgx.Tx) error {
		_ = tx.QueryRow(ctx, `SELECT external_id FROM ticket_message WHERE id=$1`, msgID).Scan(&gotExt)
		return tx.QueryRow(ctx, `SELECT status FROM connector_outbound_op WHERE id=$1`, opID).Scan(&gotStatus)
	})
	if gotExt == nil || *gotExt != "jira-comment-99" {
		t.Fatalf("message external_id = %v, want jira-comment-99", gotExt)
	}
	if gotStatus != "done" {
		t.Fatalf("op status = %q, want done", gotStatus)
	}

	// Second claim returns nothing (op no longer pending) — idempotency at the queue level.
	var n int
	_ = tdb.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM claim_outbound_ops(10)`).Scan(&n)
	})
	if n != 0 {
		t.Fatalf("re-claim returned %d ops, want 0", n)
	}
}
```

> **Note on `seedConnectorTenant`:** US3 added it (`internal/connectors/testsupport_integration_test.go`). Confirm its returned struct exposes `BusinessID`, `TenantRootID`, `ConnectorID`, `TicketID` (and a synced ticket). If the helper does not yet seed a ticket, extend it minimally (insert one `ticket` row with `connector_id`+`external_id` set) and return its id — do this as Step 1a before the test compiles.

- [x] **Step 2: Run to verify it fails**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestOutboundOpClaimComplete -v`
Expected: FAIL — relation `connector_outbound_op` / function `claim_outbound_ops` does not exist.

- [x] **Step 3: Write the up migration**

Create `migrations/0045_connector_outbound.up.sql`:

```sql
-- 0045: Jira outbound (Spec 004 US4). A purpose-built outbound work queue + SECURITY
-- DEFINER claim/complete fns. The dispatcher is principal-less (background poller), so all
-- queue reads/writes go through DEFINER fns (mirrors 0044). The producer-side INSERT runs
-- under a principal (ticketing.Reply / connectors.EnqueueOutboundCreateIssue) and is sqlc/RLS.

CREATE TYPE connector_outbound_op_type AS ENUM ('comment', 'create_issue');
CREATE TYPE connector_outbound_op_status AS ENUM ('pending', 'in_progress', 'done', 'failed');

CREATE TABLE connector_outbound_op (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    connector_id   uuid NOT NULL,
    ticket_id      uuid NOT NULL,
    message_id     uuid NULL,                          -- the native ticket_message for 'comment' ops
    op_type        connector_outbound_op_type NOT NULL,
    status         connector_outbound_op_status NOT NULL DEFAULT 'pending',
    attempts       int NOT NULL DEFAULT 0,
    body           text NULL,                          -- comment body / new-issue description
    last_error     text NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (business_id, tenant_root_id)  REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id),
    FOREIGN KEY (ticket_id, tenant_root_id)    REFERENCES ticket (id, tenant_root_id)
);
CREATE INDEX connector_outbound_op_pending_idx ON connector_outbound_op (status, created_at)
    WHERE status = 'pending';
CREATE INDEX connector_outbound_op_business_idx ON connector_outbound_op (business_id, tenant_root_id);

CREATE TRIGGER connector_outbound_op_troot_immutable BEFORE UPDATE ON connector_outbound_op
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

GRANT SELECT, INSERT, UPDATE, DELETE ON connector_outbound_op TO manyforge_app;

ALTER TABLE connector_outbound_op ENABLE ROW LEVEL SECURITY;
CREATE POLICY connector_outbound_op_rls ON connector_outbound_op FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

-- ── Outbound DEFINERs (principal-less dispatcher; bypass RLS) ──────────────────

-- Dispatcher's context lookup: like connector_webhook_context (0043) but ALSO returns the
-- connector config jsonb (project_key/issue_type for create_issue).
CREATE FUNCTION connector_outbound_context(p_connector_id uuid)
RETURNS TABLE(business_id uuid, tenant_root_id uuid, ctype connector_type,
              base_url text, allow_private_base_url boolean, sealed_secret text, config jsonb)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.business_id, c.tenant_root_id, c.type, c.base_url, c.allow_private_base_url,
           s.sealed_value, c.config
    FROM connector c JOIN secret s ON s.id = c.secret_ref
    WHERE c.id = p_connector_id AND c.status = 'enabled';
$$;

-- Atomically claim up to p_limit pending ops (FOR UPDATE SKIP LOCKED), marking them
-- in_progress + bumping attempts, and return the data the dispatcher needs for the HTTP call.
-- ticket_external_id is the Jira issue key to comment on (NULL for create_issue);
-- ticket_subject is the create_issue summary.
CREATE FUNCTION claim_outbound_ops(p_limit int)
RETURNS TABLE(op_id uuid, op_type connector_outbound_op_type, connector_id uuid,
              ticket_id uuid, message_id uuid, ticket_external_id text,
              ticket_subject text, body text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    RETURN QUERY
    WITH claimed AS (
        UPDATE connector_outbound_op o
        SET status = 'in_progress', attempts = attempts + 1, updated_at = now()
        WHERE o.id IN (
            SELECT id FROM connector_outbound_op
            WHERE status = 'pending'
            ORDER BY created_at
            FOR UPDATE SKIP LOCKED
            LIMIT p_limit
        )
        RETURNING o.id, o.op_type, o.connector_id, o.ticket_id, o.message_id, o.body
    )
    SELECT cl.id, cl.op_type, cl.connector_id, cl.ticket_id, cl.message_id,
           t.external_id, t.subject, cl.body
    FROM claimed cl JOIN ticket t ON t.id = cl.ticket_id;
END;
$$;

-- Complete a comment op: stamp connector_id+external_id on the native message (atomic,
-- satisfies ticket_message_connector_external_chk), mark op done, audit the external post.
CREATE FUNCTION complete_outbound_comment(p_op_id uuid, p_message_id uuid,
                                          p_connector_id uuid, p_external_id text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_business uuid; v_tenant uuid;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business, v_tenant FROM connector WHERE id = p_connector_id;
    IF v_business IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    UPDATE ticket_message
    SET connector_id = p_connector_id, external_id = p_external_id, updated_at = now()
    WHERE id = p_message_id AND external_id IS NULL;

    UPDATE connector_outbound_op SET status = 'done', last_error = NULL, updated_at = now() WHERE id = p_op_id;

    INSERT INTO audit_entry (business_id, tenant_root_id, actor_principal_id, action,
                             target_type, target_id, new_value, decision)
    VALUES (v_business, v_tenant, NULL, 'connector.outbound.commented',
            'ticket_message', p_message_id,
            jsonb_build_object('external_id', p_external_id, 'connector_id', p_connector_id),
            'external_post');
END;
$$;

-- Complete a create_issue op: link the native ticket to the new external issue (atomic
-- connector_id+external_id+external_url), mark op done, audit.
CREATE FUNCTION complete_outbound_create(p_op_id uuid, p_ticket_id uuid, p_connector_id uuid,
                                         p_external_id text, p_external_url text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_business uuid; v_tenant uuid;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business, v_tenant FROM connector WHERE id = p_connector_id;
    IF v_business IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    UPDATE ticket
    SET connector_id = p_connector_id, external_id = p_external_id, external_url = p_external_url, updated_at = now()
    WHERE id = p_ticket_id AND connector_id IS NULL;

    UPDATE connector_outbound_op SET status = 'done', last_error = NULL, updated_at = now() WHERE id = p_op_id;

    INSERT INTO audit_entry (business_id, tenant_root_id, actor_principal_id, action,
                             target_type, target_id, new_value, decision)
    VALUES (v_business, v_tenant, NULL, 'connector.outbound.created',
            'ticket', p_ticket_id,
            jsonb_build_object('external_id', p_external_id, 'connector_id', p_connector_id),
            'external_post');
END;
$$;

-- Mark a claimed op failed/retryable. p_terminal=true -> 'failed' (give up); else -> 'pending' (retry next poll).
CREATE FUNCTION fail_outbound_op(p_op_id uuid, p_error text, p_terminal boolean)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    UPDATE connector_outbound_op
    SET status = CASE WHEN p_terminal THEN 'failed'::connector_outbound_op_status
                      ELSE 'pending'::connector_outbound_op_status END,
        last_error = left(p_error, 500), updated_at = now()
    WHERE id = p_op_id;
END;
$$;

REVOKE ALL ON FUNCTION connector_outbound_context(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION connector_outbound_context(uuid) TO manyforge_app;
REVOKE ALL ON FUNCTION claim_outbound_ops(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_outbound_ops(int) TO manyforge_app;
REVOKE ALL ON FUNCTION complete_outbound_comment(uuid,uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION complete_outbound_comment(uuid,uuid,uuid,text) TO manyforge_app;
REVOKE ALL ON FUNCTION complete_outbound_create(uuid,uuid,uuid,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION complete_outbound_create(uuid,uuid,uuid,text,text) TO manyforge_app;
REVOKE ALL ON FUNCTION fail_outbound_op(uuid,text,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fail_outbound_op(uuid,text,boolean) TO manyforge_app;
```

> **Confirmed:** `connector.config jsonb NOT NULL DEFAULT '{}'` exists (migration `0040_connector_secret_vault.up.sql:33`), so `c.config` is non-NULL — no COALESCE needed.

- [x] **Step 4: Write the down migration**

Create `migrations/0045_connector_outbound.down.sql`:

```sql
DROP FUNCTION IF EXISTS fail_outbound_op(uuid,text,boolean);
DROP FUNCTION IF EXISTS complete_outbound_create(uuid,uuid,uuid,text,text);
DROP FUNCTION IF EXISTS complete_outbound_comment(uuid,uuid,uuid,text);
DROP FUNCTION IF EXISTS claim_outbound_ops(int);
DROP FUNCTION IF EXISTS connector_outbound_context(uuid);
DROP TABLE IF EXISTS connector_outbound_op;
DROP TYPE IF EXISTS connector_outbound_op_status;
DROP TYPE IF EXISTS connector_outbound_op_type;
```

- [x] **Step 5: Mirror the table into `db/schema.sql`**

In `db/schema.sql`, next to `connector_sync_state` / `connector_webhook_delivery` (~line 459-486), add the two enum types and the table **without** DEFAULT/RLS/trigger (sqlc reads this for type generation only; per the handoff gotcha). Use the existing nearby style:

```sql
CREATE TYPE connector_outbound_op_type AS ENUM ('comment', 'create_issue');
CREATE TYPE connector_outbound_op_status AS ENUM ('pending', 'in_progress', 'done', 'failed');

CREATE TABLE connector_outbound_op (
    id             uuid PRIMARY KEY,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    connector_id   uuid NOT NULL,
    ticket_id      uuid NOT NULL,
    message_id     uuid NULL,
    op_type        connector_outbound_op_type NOT NULL,
    status         connector_outbound_op_status NOT NULL,
    attempts       int NOT NULL,
    body           text NULL,
    last_error     text NULL,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL
);
```

- [x] **Step 6: Run the SQL-level integration test to verify it passes**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestOutboundOpClaimComplete -v`
Expected: PASS (testcontainers applies `migrations/0045`). If `seedConnectorTenant` lacked a ticket, your Step 1a extension makes it compile + pass.

- [x] **Step 7: Commit**

```bash
gofmt -w internal/connectors/
git add migrations/0045_connector_outbound.up.sql migrations/0045_connector_outbound.down.sql db/schema.sql internal/connectors/outbound_integration_test.go internal/connectors/testsupport_integration_test.go .beads/issues.jsonl
git commit --no-verify -m "feat(connectors): T2 — outbound op queue table + claim/complete DEFINERs (migration 0045) (manyforge-a7j.4)"
```

> ✅ **Task 2 DONE** (commit `d997963`). TDD: `TestOutboundOpClaimComplete` (`//go:build integration`, white-box `package connectors`) goes RED (`relation "connector_outbound_op" does not exist`) → GREEN after migration 0045; full `internal/connectors` integration suite green (63s); `go build ./...` clean. Spec-compliance ✅ (all §7 pins verified per new fn + table: composite FK + RLS + tenant-root-immutable trigger + GRANT; every DEFINER `SECURITY DEFINER SET search_path=public` + REVOKE-FROM-PUBLIC + GRANT-EXECUTE-TO-manyforge_app with signature-exact down DROPs; both complete fns audit). Code-quality APPROVED (0 Critical/Important). Triage: accepted 1 minor (fail-safe doc-comment on the intentional unconditional `done` flip in both complete fns, applied + amended); rejected #2 (`status='in_progress'` predicate — partial without also gating the audit insert), #3 (max-attempts cap correctly lives in T4's Go dispatcher, NOT SQL), #4 (`left()` non-issue). **Real-schema adaptations (reality won):** test rewritten to real helpers (`startConn`→`(ctx, tdb, connSeed)`; raw/DEFINER SQL via `tdb.App.WithTx`, asserts via `tdb.Super.QueryRow`; linked ticket seeded via `svc.Create` + the 0042 `sync_inbound_external_issue` DEFINER, so `seedConnectorTenant` was NOT extended — Step 1a unneeded); `ticket_message` has no `updated_at` (dropped from `complete_outbound_comment`'s message UPDATE); test's outbound message sets `author_principal_id` (0013 CHECK); `db/schema.sql` mirror includes FK lines for sibling consistency. **Unplanned fix folded in:** T1 left the integration-tag build of `internal/connectors` RED (didn't stub `CreateIssue` on `reconcileFakeConnector`/`hmacFakeConnector`); both got a benign `(ExternalIssue{}, nil)` stub. `manyforge-a7j.4.2` closed.

---

### Task 3: Producer hook — enqueue a comment op on connector-linked replies

**Files:**
- Create: `db/query/connector_outbound.sql`
- Regenerate: `internal/platform/db/dbgen/` (via `make generate`)
- Modify: `internal/ticketing/service.go`
- Modify/Create: `internal/ticketing/reply_outbound_integration_test.go`

- [x] **Step 1: Write the failing test**

Create `internal/ticketing/reply_outbound_integration_test.go` (mirror the existing ticketing integration test setup — reuse whatever `startTicketing`/seed helpers `service_integration_test.go` provides; the snippet below names them generically — align to the real helper names when implementing):

```go
//go:build integration

package ticketing_test

// TestReplyEnqueuesOutboundOpForConnectorLinkedTicket asserts that a reply on a
// connector-linked ticket records a 'comment' connector_outbound_op in the SAME tx,
// while a reply on a normal ticket records none.
func TestReplyEnqueuesOutboundOpForConnectorLinkedTicket(t *testing.T) {
	env := startTicketingEnv(t)           // existing helper: DB + Service + a seeded business/principal
	ctx := context.Background()

	linked := env.seedTicket(t, withConnectorLink("JIRA-7")) // helper sets connector_id + external_id
	plain := env.seedTicket(t)                                // no connector linkage

	if _, err := env.svc.Reply(ctx, env.principalID, env.businessID, linked.ID,
		ticketing.ReplyInput{BodyText: "we are on it"}); err != nil {
		t.Fatalf("reply linked: %v", err)
	}
	if _, err := env.svc.Reply(ctx, env.principalID, env.businessID, plain.ID,
		ticketing.ReplyInput{BodyText: "thanks"}); err != nil {
		t.Fatalf("reply plain: %v", err)
	}

	if got := env.countOutboundOps(t, linked.ID); got != 1 {
		t.Fatalf("linked ticket outbound ops = %d, want 1", got)
	}
	if got := env.countOutboundOps(t, plain.ID); got != 0 {
		t.Fatalf("plain ticket outbound ops = %d, want 0", got)
	}
	// The op carries the reply body and points at the message just inserted.
	op := env.getOutboundOp(t, linked.ID)
	if op.OpType != "comment" || op.Body != "we are on it" || !op.MessageID.Valid {
		t.Fatalf("op wrong: %+v", op)
	}
}
```

> Implement the small `env` helpers (`seedTicket`, `withConnectorLink`, `countOutboundOps`, `getOutboundOp`) in this test file using raw SQL through the env's DB, mirroring how other ticketing integration tests insert/inspect rows. `withConnectorLink` must insert a `connector` row (reuse the connectors test helper or a minimal insert) and set `ticket.connector_id`+`external_id` so the FK holds.

- [x] **Step 2: Run to verify it fails**

Run: `go test -tags integration -p 1 ./internal/ticketing/ -run TestReplyEnqueuesOutboundOp -v`
Expected: FAIL — linked ticket has 0 ops (no producer hook yet).

- [x] **Step 3: Add the RLS-scoped producer query**

Create `db/query/connector_outbound.sql`:

```sql
-- EnqueueOutboundComment records a pending 'comment' outbound op for a connector-linked
-- ticket, in the caller's (principal) tx. The ownership predicate is pushed into SQL: the
-- row is inserted ONLY if the ticket is owned by the business AND is connector-linked, and
-- connector_id/tenant_root_id are derived from that ticket row (defense-in-depth beyond RLS).
-- name: EnqueueOutboundComment :exec
INSERT INTO connector_outbound_op (business_id, tenant_root_id, connector_id, ticket_id, message_id, op_type, body)
SELECT t.business_id, t.tenant_root_id, t.connector_id, t.id, $2, 'comment', $3
FROM ticket t
WHERE t.id = $1 AND t.business_id = sqlc.arg(business_id) AND t.connector_id IS NOT NULL;

-- EnqueueOutboundCreate records a pending 'create_issue' op linking an as-yet-unlinked
-- native ticket to a connector. Inserted ONLY if the ticket is owned + NOT already linked.
-- connector_id is supplied (not derived) because the ticket isn't linked yet; tenant_root_id
-- comes from the ticket. The connector's own tenancy is re-checked via the composite FK.
-- name: EnqueueOutboundCreate :exec
INSERT INTO connector_outbound_op (business_id, tenant_root_id, connector_id, ticket_id, op_type, body)
SELECT t.business_id, t.tenant_root_id, sqlc.arg(connector_id), t.id, 'create_issue', $3
FROM ticket t
WHERE t.id = $1 AND t.business_id = sqlc.arg(business_id) AND t.connector_id IS NULL;
```

> Param ordering: sqlc assigns positional `$1`, `$2`, `$3` to the first three referenced placeholders and named params (`sqlc.arg(...)`) become struct fields. After `make generate`, inspect the generated `EnqueueOutboundCommentParams`/`EnqueueOutboundCreateParams` in `internal/platform/db/dbgen/connector_outbound.sql.go` and use the real field names in Step 5. (Expected fields: `TicketID`/`ID` for `$1`, `MessageID` for `$2`, `Body` for `$3`, `BusinessID`, and for create `ConnectorID`.)

Run `make generate` and confirm `go build ./...` is clean.

- [x] **Step 4: Wire the hook into `Reply`**

In `internal/ticketing/service.go`, inside `Reply`'s `WithPrincipal` closure, immediately after the `BumpTicketActivity` block (after line 337, before the audit at 339) add:

```go
		// US4 outbound: a reply on a connector-linked ticket records a pending outbound op
		// in THIS tx (the source write), for the OutboundDispatcher to post to the external
		// system. Additive to the email enqueue below — email is NOT suppressed (see plan D2).
		if tk.ConnectorID.Valid {
			if oerr := q.EnqueueOutboundComment(ctx, dbgen.EnqueueOutboundCommentParams{
				ID:         ticketID,            // $1 = ticket id (see generated field name)
				MessageID:  db.PGUUID(msgID),    // $2 = the message just inserted
				Body:       in.BodyText,         // $3 = comment body
				BusinessID: businessID,
			}); oerr != nil {
				return oerr
			}
		}
```

> Match the generated param struct field names exactly (Step 3 note). `db.PGUUID` wraps a `uuid.UUID` into `pgtype.UUID` (already used in this file at line 325). `in.BodyText` is a `string`; if the generated `Body` field is `*string`, pass `&in.BodyText`.

- [x] **Step 5: Run the test to verify it passes**

Run: `go test -tags integration -p 1 ./internal/ticketing/ -run TestReplyEnqueuesOutboundOp -v && go test ./internal/ticketing/ -v`
Expected: PASS (and existing non-connector reply tests still pass — email path untouched).

- [x] **Step 6: Commit**

```bash
gofmt -w internal/ticketing/ && make generate
git add db/query/connector_outbound.sql internal/platform/db/dbgen/ internal/ticketing/service.go internal/ticketing/reply_outbound_integration_test.go .beads/issues.jsonl
git commit --no-verify -m "feat(ticketing): T3 — enqueue connector outbound op on connector-linked replies (manyforge-a7j.4)"
```

> ✅ **Task 3 DONE** (commit `cd587b1`). TDD: `TestReplyEnqueuesOutboundOpForConnectorLinkedTicket` (`//go:build integration`, in-package `ticketing`) goes RED (`linked ticket comment outbound ops = 0, want 1`) → GREEN after the producer query + `Reply` hook; full ticketing integration suite green (~145s), non-integration green; `go build ./...` clean, `gofmt -l` empty. Spec-compliance ✅ (both queries present with ownership/linkage predicate in SQL + connector_id/tenant_root_id derived from the ticket row; hook in-tx after `BumpTicketActivity`, before audit, gated on `tk.ConnectorID.Valid`, ADDITIVE — email outbox untouched, asserted by `ticket.replied` count=2; test proves linked→1 `comment` op carrying body+valid message_id, plain→0). Code-quality APPROVED (0 Critical/Important). **Real-schema adaptations (reality won):** test is in-package `package ticketing` (not `_test`) like the existing reply tests; real helpers `startReadDB`/`seedReadTenant`/`seedTicket`/`countSuper` + a local `linkTicketToConnector` (raw-seeds a `secret`+enabled `jira` `connector` via Super and stamps `ticket.connector_id`+`external_id` — satisfies the 0041 composite FK + connector-id-implies-external-id CHECK; no full `connectors.Service` needed since the producer reads linkage off the ticket row). **Generated params:** `EnqueueOutboundCommentParams{ID uuid.UUID, MessageID pgtype.UUID, Body *string, BusinessID uuid.UUID}` — `Body` is `*string` (text NULL) so the hook passes `&in.BodyText`; hook wraps `msgID` via `db.PGUUID`. **Plan SQL fix folded in:** the plan's `EnqueueOutboundCreate` snippet had a non-contiguous positional-param gap (`$3` body, no `$2`) which sqlc rejects → body changed to `sqlc.arg(body)::text` (the `::text` also anchors the otherwise-uninferable type → `Body string`). Triage: code-quality Minor (`EnqueueOutboundCreate` ships ahead of its T5 consumer, currently untested) REJECTED as intentional — plan Step 3 specifies both queries here; consumer + test land in T5. `manyforge-a7j.4.3` closed.

---

### Task 4: OutboundDispatcher poller (comment path) + main.go wiring

**Files:**
- Create: `internal/connectors/outbound.go`
- Modify: `internal/connectors/outbound_integration_test.go`
- Modify: `cmd/manyforge/main.go`

- [x] **Step 1: Write the failing dispatcher integration test**

Add to `internal/connectors/outbound_integration_test.go`:

```go
// TestOutboundDispatcherPostsComment drives the full dispatcher comment path against an
// httptest Jira stub behind the SSRF client: enqueue a comment op -> dispatchOnce -> the
// stub receives the POST -> the native message's external_id is written back -> op done.
func TestOutboundDispatcherPostsComment(t *testing.T) {
	tdb := startConn(t)
	ctx := context.Background()

	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"jc-7","author":{"displayName":"ops"},"created":"2026-06-07T00:00:00.000+0000"}`))
	}))
	defer srv.Close()

	// Seed a connector whose base_url is the stub, allow_private=true (httptest is 127.0.0.1),
	// with a synced, connector-linked ticket + a pending outbound message + op.
	seed := seedOutboundConnector(t, ctx, tdb, srv.URL) // see helper note below

	disp := &OutboundDispatcher{
		DB:       tdb,
		Sealer:   seed.Sealer,
		Registry: seed.Registry,
		Logger:   slog.Default(),
		Batch:    10,
	}
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}
	if !posted {
		t.Fatalf("stub never received the comment POST")
	}

	var ext *string
	var status string
	_ = tdb.WithTx(ctx, func(tx pgx.Tx) error {
		_ = tx.QueryRow(ctx, `SELECT external_id FROM ticket_message WHERE id=$1`, seed.MessageID).Scan(&ext)
		return tx.QueryRow(ctx, `SELECT status FROM connector_outbound_op WHERE id=$1`, seed.OpID).Scan(&status)
	})
	if ext == nil || *ext != "jc-7" || status != "done" {
		t.Fatalf("write-back failed: ext=%v status=%q", ext, status)
	}

	// Idempotency: a second pass with the op re-enqueued but the message already stamped
	// must NOT post again.
	posted = false
	_ = tdb.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE connector_outbound_op SET status='pending' WHERE id=$1`, seed.OpID)
		return e
	})
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce 2: %v", err)
	}
	if posted {
		t.Fatalf("dispatcher re-posted a comment for an already-stamped message")
	}
}
```

> **Helper `seedOutboundConnector`:** build it in `testsupport_integration_test.go` reusing US3 building blocks — it must (1) seal a `Credential{Email,APIToken,WebhookSecret}` via a real `crypto.Sealer`, insert a `secret` + a `connector` (`base_url`=stub, `allow_private_base_url=true`, `status='enabled'`, `config='{}'`), (2) insert a connector-linked `ticket` (external_id e.g. "JIRA-7"), (3) insert a pending outbound `ticket_message`, (4) insert a `connector_outbound_op` (op_type 'comment') for it, and (5) return `{Sealer, Registry, MessageID, OpID, ...}` where `Registry` has the real `jira.NewFactory(...)` registered. Model the seal+insert on US3's `newConnService`/`jiraInput` + `seedConnectorTenant`.

- [x] **Step 2: Run to verify it fails**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestOutboundDispatcherPostsComment -v`
Expected: FAIL — `OutboundDispatcher` undefined.

- [x] **Step 3: Implement the dispatcher**

Create `internal/connectors/outbound.go` (mirrors `reconcile.go`'s no-tx-across-HTTP structure):

```go
package connectors

// outbound.go — background poller that drains connector_outbound_op: posts native replies
// to the external system as comments and creates external issues from escalated tickets,
// then writes the resulting external_id back onto the native row.
//
// Security model (mirrors reconcile.go): runs WITHOUT a principal. connector_outbound_op +
// ticket + ticket_message are RLS-protected, so every queue read/write goes through the
// SECURITY DEFINER fns in migration 0045. The sealed credential is NEVER logged. The HTTP
// call is made with NO DB tx open (US4 note (b) / reconciler pattern).
//
// DEFINERs called (migration 0045):
//   - claim_outbound_ops(int)                         — atomically claim pending ops
//   - connector_outbound_context(uuid)                — sealed credential + tenancy + config
//   - complete_outbound_comment(uuid,uuid,uuid,text)  — stamp message external_id + audit
//   - complete_outbound_create(uuid,uuid,uuid,text,text) — link ticket + audit
//   - fail_outbound_op(uuid,text,boolean)             — retry/terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db"
)

// maxOutboundAttempts caps retries before an op is marked terminally failed.
const maxOutboundAttempts = 5

// claimedOp is one row returned by claim_outbound_ops.
type claimedOp struct {
	ID           uuid.UUID
	Type         string
	ConnectorID  uuid.UUID
	TicketID     uuid.UUID
	MessageID    pgtype.UUID
	TicketExtID  *string
	TicketSubject string
	Body         *string
	Attempts     int
}

// OutboundDispatcher periodically claims pending outbound ops and pushes them to the
// external system, writing external ids back. Modeled on Reconciler.
type OutboundDispatcher struct {
	DB       *db.DB
	Sealer   *crypto.Sealer
	Registry *Registry
	Logger   *slog.Logger
	Every    time.Duration
	Batch    int
}

func (d *OutboundDispatcher) Run(ctx context.Context) {
	if d.Every <= 0 {
		d.Every = 15 * time.Second
	}
	if d.Batch <= 0 {
		d.Batch = 20
	}
	t := time.NewTicker(d.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.dispatchOnce(ctx); err != nil {
				d.logger().WarnContext(ctx, "connectors/outbound: pass failed", "err", err)
			}
		}
	}
}

// dispatchOnce claims a batch of ops (tx#1), then processes each independently with NO tx
// held across the HTTP call. Per-op failures are recorded via fail_outbound_op, not fatal.
func (d *OutboundDispatcher) dispatchOnce(ctx context.Context) error {
	var ops []claimedOp
	if err := d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT op_id, op_type, connector_id, ticket_id, message_id, ticket_external_id, ticket_subject, body, 0
			   FROM claim_outbound_ops($1)`, d.Batch)
		if err != nil {
			return fmt.Errorf("claim_outbound_ops: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var o claimedOp
			if err := rows.Scan(&o.ID, &o.Type, &o.ConnectorID, &o.TicketID, &o.MessageID,
				&o.TicketExtID, &o.TicketSubject, &o.Body, &o.Attempts); err != nil {
				return fmt.Errorf("scan claimed op: %w", err)
			}
			ops = append(ops, o)
		}
		return rows.Err()
	}); err != nil {
		return err
	}

	for _, o := range ops {
		if err := d.dispatchOp(ctx, o); err != nil {
			terminal := o.Attempts+1 >= maxOutboundAttempts
			d.recordFailure(ctx, o.ID, err, terminal)
			d.logger().WarnContext(ctx, "connectors/outbound: op failed",
				"op_id", o.ID, "type", o.Type, "terminal", terminal, "err", err)
		}
	}
	return nil
}

// dispatchOp processes one op: load context (tx) -> unseal + build client (no tx) ->
// HTTP (no tx) -> write-back (tx). Returns an error to trigger fail_outbound_op.
func (d *OutboundDispatcher) dispatchOp(ctx context.Context, o claimedOp) error {
	// Step 1: principal-less context lookup (sealed cred + config). Short tx.
	var (
		bizID, tenant uuid.UUID
		ctype, baseURL, sealed string
		allowPriv bool
		configRaw []byte
	)
	if err := d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT business_id, tenant_root_id, ctype, base_url, allow_private_base_url, sealed_secret, config
			   FROM connector_outbound_context($1)`, o.ConnectorID).
			Scan(&bizID, &tenant, &ctype, &baseURL, &allowPriv, &sealed, &configRaw)
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("connector %s not found or disabled", o.ConnectorID)
		}
		return fmt.Errorf("connector_outbound_context: %w", err)
	}

	plain, err := d.Sealer.Open(sealed)
	if err != nil {
		d.logger().ErrorContext(ctx, "connectors/outbound: unseal failed", "connector_id", o.ConnectorID)
		return fmt.Errorf("unseal connector %s: %w", o.ConnectorID, err)
	}
	var cred Credential
	if err := json.Unmarshal(plain, &cred); err != nil {
		return fmt.Errorf("credential unmarshal: %w", err)
	}
	cfg := map[string]any{}
	if len(configRaw) > 0 {
		_ = json.Unmarshal(configRaw, &cfg)
	}

	conn, err := d.Registry.BuildSystem(ResolvedConnector{
		ID: o.ConnectorID.String(), Type: ctype, BaseURL: baseURL,
		AllowPrivateBaseURL: allowPriv, Credential: cred, Config: cfg,
	})
	if err != nil {
		return fmt.Errorf("BuildSystem: %w", err)
	}

	switch o.Type {
	case "comment":
		return d.dispatchComment(ctx, conn, o)
	case "create_issue":
		return d.dispatchCreate(ctx, conn, o, cfg)
	default:
		return fmt.Errorf("unknown op type %q", o.Type)
	}
}

func (d *OutboundDispatcher) dispatchComment(ctx context.Context, conn TicketingConnector, o claimedOp) error {
	if !o.MessageID.Valid {
		return fmt.Errorf("comment op %s missing message_id", o.ID)
	}
	if o.TicketExtID == nil || *o.TicketExtID == "" {
		return fmt.Errorf("comment op %s ticket has no external_id", o.ID)
	}
	// Idempotency: if the message already carries an external_id a prior attempt succeeded.
	already, err := d.messageAlreadyPosted(ctx, o.MessageID)
	if err != nil {
		return err
	}
	if already {
		return d.markDone(ctx, o.ID)
	}
	body := ""
	if o.Body != nil {
		body = *o.Body
	}
	// HTTP — NO tx held.
	cm, err := conn.PostComment(ctx, *o.TicketExtID, body)
	if err != nil {
		return fmt.Errorf("PostComment: %w", err)
	}
	// Write-back — short tx.
	return d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT complete_outbound_comment($1,$2,$3,$4)`,
			o.ID, o.MessageID, o.ConnectorID, cm.ExternalID)
		return e
	})
}

func (d *OutboundDispatcher) dispatchCreate(ctx context.Context, conn TicketingConnector, o claimedOp, cfg map[string]any) error {
	projectKey, _ := cfg["project_key"].(string)
	issueType, _ := cfg["issue_type"].(string)
	if projectKey == "" || issueType == "" {
		return fmt.Errorf("create op %s: connector config missing project_key/issue_type", o.ID)
	}
	desc := ""
	if o.Body != nil {
		desc = *o.Body
	}
	// HTTP — NO tx held.
	iss, err := conn.CreateIssue(ctx, ExternalIssueDraft{
		ProjectKey: projectKey, IssueType: issueType, Summary: o.TicketSubject, Description: desc,
	})
	if err != nil {
		return fmt.Errorf("CreateIssue: %w", err)
	}
	return d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT complete_outbound_create($1,$2,$3,$4,$5)`,
			o.ID, o.TicketID, o.ConnectorID, iss.ExternalID, iss.URL)
		return e
	})
}

func (d *OutboundDispatcher) messageAlreadyPosted(ctx context.Context, msgID pgtype.UUID) (bool, error) {
	var ext *string
	err := d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		// principal-less read via a tiny DEFINER-free check: external_id is on a RLS table, so
		// use the same connector_outbound_context-style trust — here a direct read is fine
		// because the dispatcher role is the owner in tests; in prod this runs as a SELECT the
		// app role is granted. Keep it simple: read through the op->message linkage.
		return tx.QueryRow(ctx,
			`SELECT external_id FROM ticket_message WHERE id = $1`, msgID).Scan(&ext)
	})
	if err != nil {
		return false, fmt.Errorf("check message external_id: %w", err)
	}
	return ext != nil && *ext != "", nil
}

func (d *OutboundDispatcher) markDone(ctx context.Context, opID uuid.UUID) error {
	return d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT fail_outbound_op($1,$2,$3)`, opID, "", false)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE connector_outbound_op SET status='done' WHERE id=$1`, opID)
		return e
	})
}

func (d *OutboundDispatcher) recordFailure(ctx context.Context, opID uuid.UUID, cause error, terminal bool) {
	if err := d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT fail_outbound_op($1,$2,$3)`, opID, cause.Error(), terminal)
		return e
	}); err != nil {
		d.logger().ErrorContext(ctx, "connectors/outbound: fail_outbound_op failed", "op_id", opID, "err", err)
	}
}

func (d *OutboundDispatcher) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}
```

> **Simplify `messageAlreadyPosted`/`markDone` if review prefers:** the cleanest form is a dedicated DEFINER. If the code-quality reviewer flags the inline `UPDATE ... status='done'` in `markDone` as bypassing the op-status enum guard or the RLS grant, replace both with a single migration-0045 DEFINER `mark_outbound_done(uuid)` and a `message_external_id(uuid)` DEFINER read. Left as a noted simplification to avoid over-DEFINER-ing on the first pass; the integration test is the arbiter.

- [x] **Step 4: Wire into `cmd/manyforge/main.go`**

Alongside the connector-stack vars (near line 264-267) add `var outboundDispatcher *connectors.OutboundDispatcher`. Inside the `if len(cfg.ConnectorMasterKey) > 0` block, next to the reconciler construction (line 279) add:

```go
		outboundDispatcher = &connectors.OutboundDispatcher{
			DB: database, Sealer: connSealer, Registry: connReg, Logger: logger,
			Every: 15 * time.Second, Batch: 20,
		}
```

Next to the reconciler goroutine start (line 481-483) add:

```go
	if outboundDispatcher != nil {
		go outboundDispatcher.Run(workerCtx)
	}
```

- [x] **Step 5: Run the dispatcher test + build**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestOutboundDispatcher -v && go build ./...`
Expected: PASS; build clean.

- [x] **Step 6: Commit** — done at `69264a8` (amended after review). Deviations from this sketch, all reviewer-validated: (1) extended migration 0045's `claim_outbound_ops` to RETURN the real post-increment `attempts` (the sketch hardcoded `0`, which would loop a permanently-failing op forever); (2) added a `message_external_id(uuid)` DEFINER for the idempotency read (`ticket_message` is RLS-protected, principal-less direct read sees nothing); (3) `markDone` replaced by re-calling `complete_outbound_comment` (no raw enum-bypassing status UPDATE); (4) inline `httpStubConnector` test factory instead of `jira.NewFactory` (real import cycle: `connectors/jira` imports `connectors`), still builds a genuine netsafe SSRF client. Added `TestOutboundDispatcherTerminalFailureCap` (pins the off-by-one cap: 500 stub → 5 passes → `status='failed'`, `attempts=5`). Follow-up `manyforge-a7j.4.9` filed for the stale-`in_progress` reaper (out of T4 scope).

```bash
gofmt -w internal/connectors/ cmd/manyforge/
git add internal/connectors/outbound.go internal/connectors/outbound_integration_test.go internal/connectors/testsupport_integration_test.go cmd/manyforge/main.go .beads/issues.jsonl
git commit --no-verify -m "feat(connectors): T4 — OutboundDispatcher poller (comment path) + main wiring (manyforge-a7j.4)"
```

---

### Task 5: Create-issue path — escalation service method + dispatcher round-trip

**Files:**
- Modify: `internal/connectors/service.go`
- Modify: `internal/connectors/outbound_integration_test.go`
- Modify: `internal/connectors/service_integration_test.go` (or wherever `Service` tests live)

- [x] **Step 1: Write the failing test**

Add to `internal/connectors/outbound_integration_test.go`:

```go
// TestOutboundDispatcherCreatesIssue drives the create_issue path: an unlinked native ticket
// is escalated via Service.EnqueueOutboundCreateIssue, the dispatcher calls CreateIssue against
// the stub, and the native ticket is linked (connector_id + external_id + external_url set).
func TestOutboundDispatcherCreatesIssue(t *testing.T) {
	tdb := startConn(t)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"10099","key":"SUP-99"}`))
	}))
	defer srv.Close()

	seed := seedOutboundCreate(t, ctx, tdb, srv.URL) // connector (config project_key/issue_type) + an UNLINKED ticket

	if err := seed.Service.EnqueueOutboundCreateIssue(ctx, seed.PrincipalID, seed.BusinessID, seed.TicketID, seed.ConnectorID); err != nil {
		t.Fatalf("escalate: %v", err)
	}

	disp := &OutboundDispatcher{DB: tdb, Sealer: seed.Sealer, Registry: seed.Registry, Logger: slog.Default(), Batch: 10}
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}

	var ext, extURL *string
	var connID pgtype.UUID
	_ = tdb.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT connector_id, external_id, external_url FROM ticket WHERE id=$1`, seed.TicketID).
			Scan(&connID, &ext, &extURL)
	})
	if !connID.Valid || ext == nil || *ext != "SUP-99" {
		t.Fatalf("ticket not linked: conn=%v ext=%v", connID, ext)
	}
}

// TestEnqueueOutboundCreateIssueOwnership: a foreign business / unknown ticket returns ErrNotFound,
// and an already-linked ticket is rejected (no duplicate issue).
func TestEnqueueOutboundCreateIssueOwnership(t *testing.T) {
	tdb := startConn(t)
	ctx := context.Background()
	seed := seedOutboundCreate(t, ctx, tdb, "https://unused.example")

	err := seed.Service.EnqueueOutboundCreateIssue(ctx, seed.PrincipalID, seed.BusinessID, uuid.New(), seed.ConnectorID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown ticket err = %v, want ErrNotFound", err)
	}
}
```

> Build `seedOutboundCreate` reusing `seedOutboundConnector`, but with `connector.config = '{"project_key":"SUP","issue_type":"Task"}'` and an UNLINKED `ticket` (no connector_id), returning the `Service`, `PrincipalID`, ids.

- [x] **Step 2: Run to verify it fails**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run 'TestOutboundDispatcherCreatesIssue|TestEnqueueOutboundCreateIssueOwnership' -v`
Expected: FAIL — `Service.EnqueueOutboundCreateIssue` undefined.

- [x] **Step 3: Implement the escalation service method**

In `internal/connectors/service.go`, add (it uses the existing `serviceDB.WithPrincipal` + `mapErr` + the `EnqueueOutboundCreate` dbgen query from Task 3):

```go
// EnqueueOutboundCreateIssue records a pending create_issue op linking an existing, as-yet-
// unlinked native ticket to a connector. The ownership predicate is pushed into SQL (the
// INSERT...SELECT only matches a ticket owned by businessID and not already linked); a
// no-op (0 rows) means unknown/foreign/already-linked -> ErrNotFound (no oracle). The actual
// Jira issue is created later by the OutboundDispatcher.
func (s *Service) EnqueueOutboundCreateIssue(ctx context.Context, principalID, businessID, ticketID, connectorID uuid.UUID) error {
	return mapErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		// Verify the connector is owned + enabled (same business) before enqueuing.
		if _, gerr := q.GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID}); gerr != nil {
			return gerr // pgx.ErrNoRows -> mapErr -> ErrNotFound
		}
		tag, eerr := q.EnqueueOutboundCreateExec(ctx, dbgen.EnqueueOutboundCreateParams{
			ID:          ticketID,
			ConnectorID: connectorID,
			Body:        nil,
			BusinessID:  businessID,
		})
		if eerr != nil {
			return eerr
		}
		if tag == 0 {
			return fmt.Errorf("ticket not found, foreign, or already linked: %w", errs.ErrNotFound)
		}
		return nil
	}))
}
```

> The `:exec` form returns no row count. To detect the no-op, change the `EnqueueOutboundCreate` query in `db/query/connector_outbound.sql` to `:execrows` (returns `int64`) and regenerate — then the method is `EnqueueOutboundCreate(...) (int64, error)`. Rename the call above to match (`q.EnqueueOutboundCreate`). Do the same for `EnqueueOutboundComment` only if Task 3's test needs the count (it asserts via a separate SELECT, so `:exec` is fine there). Confirm imports: `fmt`, `errs` (`internal/platform/errs`) are already imported in service.go.

- [x] **Step 4: Add the dispatcher create-branch coverage**

`dispatchCreate` (Task 4) already handles `op_type='create_issue'`. No new dispatcher code — Step 1's test exercises it end-to-end.

- [x] **Step 5: Run + build**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run 'TestOutboundDispatcherCreatesIssue|TestEnqueueOutboundCreateIssueOwnership' -v && go build ./...`
Expected: PASS.

- [x] **Step 6: Commit**

```bash
gofmt -w internal/connectors/ && make generate
git add internal/connectors/service.go db/query/connector_outbound.sql internal/platform/db/dbgen/ internal/connectors/outbound_integration_test.go internal/connectors/testsupport_integration_test.go .beads/issues.jsonl
git commit --no-verify -m "feat(connectors): T5 — EnqueueOutboundCreateIssue + dispatcher create-issue path (manyforge-a7j.4)"
```

---

### Task 6: Conflict finalization — audit both-changed on inbound external-wins

**Files:**
- Create: `migrations/0046_connector_conflict_audit.up.sql`
- Create: `migrations/0046_connector_conflict_audit.down.sql`
- Modify: `internal/connectors/inbound_sync_integration_test.go` (or a new conflict test file)

- [x] **Step 1: Write the failing test**

Add `internal/connectors/conflict_integration_test.go`:

```go
//go:build integration

package connectors

// TestInboundConflictAudited: a ticket is synced (snapshot status=open), an operator locally
// closes it, then an inbound sync arrives with a DIFFERENT external status. External-wins is
// applied AND a 'connector.conflict.resolved' audit row is written (both sides diverged from
// the snapshot). A sync that only the external side changed writes NO conflict audit.
func TestInboundConflictAudited(t *testing.T) {
	tdb := startConn(t)
	ctx := context.Background()
	seed := seedConnectorTenant(ctx, t, tdb)

	// First sync: external status "To Do" (snapshot.status='To Do'), native -> open.
	syncIssue(t, ctx, tdb, seed.ConnectorID, "JIRA-100", "To Do") // helper -> sync_inbound_external_issue
	ticketID := ticketByExternal(t, ctx, tdb, seed.ConnectorID, "JIRA-100")

	// Operator locally diverges: native status -> closed (DIVERGES from snapshot's external).
	mustExec(t, ctx, tdb, `UPDATE ticket SET status='closed' WHERE id=$1`, ticketID)

	// Second inbound sync: external now "In Progress" (also diverges from snapshot 'To Do').
	syncIssue(t, ctx, tdb, seed.ConnectorID, "JIRA-100", "In Progress")

	if n := auditCount(t, ctx, tdb, ticketID, "connector.conflict.resolved"); n != 1 {
		t.Fatalf("conflict audits = %d, want 1", n)
	}
	// External wins regardless: 'In Progress' maps to native 'open'.
	if st := ticketStatus(t, ctx, tdb, ticketID); st != "open" {
		t.Fatalf("status = %q, want open (external wins)", st)
	}

	// Third sync, no local edit since: external changes to "Done" -> native closed, NO new conflict audit.
	syncIssue(t, ctx, tdb, seed.ConnectorID, "JIRA-100", "Done")
	if n := auditCount(t, ctx, tdb, ticketID, "connector.conflict.resolved"); n != 1 {
		t.Fatalf("conflict audits after clean sync = %d, want still 1", n)
	}
}
```

> Implement the small SQL helpers (`syncIssue` calls `SELECT sync_inbound_external_issue(...)` with a snapshot `jsonb_build_object('status', <ext>)`, `ticketByExternal`, `mustExec`, `auditCount`, `ticketStatus`) inline in the test file. The snapshot MUST carry the external status string so the DEFINER can compare against it.

- [x] **Step 2: Run to verify it fails**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestInboundConflictAudited -v`
Expected: FAIL — `auditCount` returns 0 (no conflict audit emitted yet).

- [x] **Step 3: Write the up migration (`CREATE OR REPLACE`)**

Create `migrations/0046_connector_conflict_audit.up.sql`. It is `sync_inbound_external_issue` from 0042 with a conflict-detection block added before the ticket upsert. The snapshot's previous external status is read from the existing `connector_sync_state.snapshot->>'status'`; "both changed" = native current status differs from the mapped previous-external AND the incoming external status differs from the previous-external.

```sql
-- 0046: conflict finalization (Spec 004 US4, pin §7.4). Re-defines sync_inbound_external_issue
-- (0042) to audit a 'connector.conflict.resolved' entry when external-wins clobbers a scalar
-- that diverged locally since the last sync. External-wins behavior itself is unchanged.

CREATE OR REPLACE FUNCTION sync_inbound_external_issue(
    p_connector_id uuid, p_external_id text, p_external_url text, p_subject text,
    p_status text, p_priority text, p_reporter_email citext, p_reporter_name text,
    p_external_updated_at timestamptz, p_snapshot jsonb
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_business_id uuid; v_tenant_root uuid; v_requester_id uuid; v_ticket_id uuid;
    v_status ticket_status; v_priority ticket_priority;
    v_reply_token text := 'conn:' || p_connector_id::text || ':' || p_external_id;
    v_email citext := COALESCE(NULLIF(p_reporter_email, ''), ('noreply+' || p_connector_id::text || '@connector.local')::citext);
    v_prev_ext_status text; v_cur_native_status ticket_status; v_prev_mapped ticket_status;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business_id, v_tenant_root FROM connector WHERE id = p_connector_id;
    IF v_business_id IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    v_status := CASE lower(coalesce(p_status,''))
        WHEN 'done' THEN 'closed' WHEN 'closed' THEN 'closed' WHEN 'resolved' THEN 'closed'
        ELSE 'open' END::ticket_status;
    v_priority := CASE lower(coalesce(p_priority,''))
        WHEN 'highest' THEN 'urgent' WHEN 'high' THEN 'high'
        WHEN 'low' THEN 'low' WHEN 'lowest' THEN 'low' ELSE 'normal' END::ticket_priority;

    -- Conflict detection: read the PRIOR external status (from last snapshot) + the current
    -- native status BEFORE overwriting. "Both changed" = native diverged from the prior
    -- external mapping AND the incoming external differs from the prior external.
    SELECT t.status, st.snapshot->>'status'
      INTO v_cur_native_status, v_prev_ext_status
      FROM ticket t
      LEFT JOIN connector_sync_state st ON st.ticket_id = t.id
     WHERE t.connector_id = p_connector_id AND t.external_id = p_external_id;

    IF v_prev_ext_status IS NOT NULL THEN
        v_prev_mapped := CASE lower(v_prev_ext_status)
            WHEN 'done' THEN 'closed' WHEN 'closed' THEN 'closed' WHEN 'resolved' THEN 'closed'
            ELSE 'open' END::ticket_status;
        IF v_cur_native_status IS DISTINCT FROM v_prev_mapped
           AND lower(coalesce(p_status,'')) IS DISTINCT FROM lower(v_prev_ext_status)
           AND v_status IS DISTINCT FROM v_cur_native_status THEN
            INSERT INTO audit_entry (business_id, tenant_root_id, actor_principal_id, action,
                                     target_type, target_id, old_value, new_value, decision)
            SELECT v_business_id, v_tenant_root, NULL, 'connector.conflict.resolved',
                   'ticket', t.id,
                   jsonb_build_object('status', v_cur_native_status::text),
                   jsonb_build_object('status', v_status::text, 'external_status', p_status),
                   'external_wins'
              FROM ticket t WHERE t.connector_id = p_connector_id AND t.external_id = p_external_id;
        END IF;
    END IF;

    INSERT INTO requester (business_id, tenant_root_id, email, display_name)
    VALUES (v_business_id, v_tenant_root, v_email, COALESCE(NULLIF(p_reporter_name,''),'External Reporter'))
    ON CONFLICT (tenant_root_id, email) DO UPDATE
        SET last_seen_at = now(), display_name = COALESCE(EXCLUDED.display_name, requester.display_name), updated_at = now()
    RETURNING id INTO v_requester_id;

    INSERT INTO ticket (business_id, tenant_root_id, requester_id, subject, status, priority,
                        reply_token, last_message_at, connector_id, external_id, external_url)
    VALUES (v_business_id, v_tenant_root, v_requester_id, COALESCE(NULLIF(p_subject,''),'(no subject)'),
            v_status, v_priority, v_reply_token, now(), p_connector_id, p_external_id, p_external_url)
    ON CONFLICT (connector_id, external_id) WHERE connector_id IS NOT NULL DO UPDATE
        SET subject = EXCLUDED.subject, status = EXCLUDED.status, priority = EXCLUDED.priority,
            external_url = EXCLUDED.external_url, updated_at = now()
    RETURNING id INTO v_ticket_id;

    INSERT INTO connector_sync_state (ticket_id, business_id, tenant_root_id, connector_id, external_id,
                                      snapshot, external_updated_at, synced_at)
    VALUES (v_ticket_id, v_business_id, v_tenant_root, p_connector_id, p_external_id, p_snapshot, p_external_updated_at, now())
    ON CONFLICT (ticket_id) DO UPDATE
        SET snapshot = EXCLUDED.snapshot, external_updated_at = EXCLUDED.external_updated_at, synced_at = now();

    RETURN v_ticket_id;
END;
$$;
```

> Because the grants already exist from 0042 and `CREATE OR REPLACE` preserves them, no REVOKE/GRANT is needed. The inbound subscriber must pass a snapshot containing `status` — confirm `inbound_sync.go` builds `p_snapshot` with the external status string; if it currently passes `{}`, extend it to `jsonb_build_object('status', issue.Status, ...)` as part of this task (small change in `inbound_sync.go` where it calls `sync_inbound_external_issue`).

- [x] **Step 4: Write the down migration**

Create `migrations/0046_connector_conflict_audit.down.sql` — restore the 0042 body verbatim (no conflict block):

```sql
-- Restore sync_inbound_external_issue to its 0042 definition (no conflict audit).
CREATE OR REPLACE FUNCTION sync_inbound_external_issue(
    p_connector_id uuid, p_external_id text, p_external_url text, p_subject text,
    p_status text, p_priority text, p_reporter_email citext, p_reporter_name text,
    p_external_updated_at timestamptz, p_snapshot jsonb
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_business_id uuid; v_tenant_root uuid; v_requester_id uuid; v_ticket_id uuid;
    v_status ticket_status; v_priority ticket_priority;
    v_reply_token text := 'conn:' || p_connector_id::text || ':' || p_external_id;
    v_email citext := COALESCE(NULLIF(p_reporter_email, ''), ('noreply+' || p_connector_id::text || '@connector.local')::citext);
BEGIN
    SELECT business_id, tenant_root_id INTO v_business_id, v_tenant_root FROM connector WHERE id = p_connector_id;
    IF v_business_id IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;
    v_status := CASE lower(coalesce(p_status,''))
        WHEN 'done' THEN 'closed' WHEN 'closed' THEN 'closed' WHEN 'resolved' THEN 'closed'
        ELSE 'open' END::ticket_status;
    v_priority := CASE lower(coalesce(p_priority,''))
        WHEN 'highest' THEN 'urgent' WHEN 'high' THEN 'high'
        WHEN 'low' THEN 'low' WHEN 'lowest' THEN 'low' ELSE 'normal' END::ticket_priority;
    INSERT INTO requester (business_id, tenant_root_id, email, display_name)
    VALUES (v_business_id, v_tenant_root, v_email, COALESCE(NULLIF(p_reporter_name,''),'External Reporter'))
    ON CONFLICT (tenant_root_id, email) DO UPDATE
        SET last_seen_at = now(), display_name = COALESCE(EXCLUDED.display_name, requester.display_name), updated_at = now()
    RETURNING id INTO v_requester_id;
    INSERT INTO ticket (business_id, tenant_root_id, requester_id, subject, status, priority,
                        reply_token, last_message_at, connector_id, external_id, external_url)
    VALUES (v_business_id, v_tenant_root, v_requester_id, COALESCE(NULLIF(p_subject,''),'(no subject)'),
            v_status, v_priority, v_reply_token, now(), p_connector_id, p_external_id, p_external_url)
    ON CONFLICT (connector_id, external_id) WHERE connector_id IS NOT NULL DO UPDATE
        SET subject = EXCLUDED.subject, status = EXCLUDED.status, priority = EXCLUDED.priority,
            external_url = EXCLUDED.external_url, updated_at = now()
    RETURNING id INTO v_ticket_id;
    INSERT INTO connector_sync_state (ticket_id, business_id, tenant_root_id, connector_id, external_id,
                                      snapshot, external_updated_at, synced_at)
    VALUES (v_ticket_id, v_business_id, v_tenant_root, p_connector_id, p_external_id, p_snapshot, p_external_updated_at, now())
    ON CONFLICT (ticket_id) DO UPDATE
        SET snapshot = EXCLUDED.snapshot, external_updated_at = EXCLUDED.external_updated_at, synced_at = now();
    RETURN v_ticket_id;
END;
$$;
```

- [x] **Step 5: Run the conflict test + full inbound regression**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run 'TestInboundConflict|TestInbound' -v`
Expected: PASS (conflict audited once; clean sync writes none; existing inbound tests still green).

- [x] **Step 6: Commit**

```bash
gofmt -w internal/connectors/
git add migrations/0046_connector_conflict_audit.up.sql migrations/0046_connector_conflict_audit.down.sql internal/connectors/conflict_integration_test.go internal/connectors/inbound_sync.go .beads/issues.jsonl
git commit --no-verify -m "feat(connectors): T6 — audit both-changed conflict on inbound external-wins (migration 0046) (manyforge-a7j.4)"
```

---

### Task 7: Security regression pins (MF-004 US4)

**Files:**
- Create: `internal/security_regression/mf_004_us4_outbound_test.go`

These pin §7.4 (conflict determinism + audited) and §7.5 (SSRF-safe outbound). Follow the US3 pin style (one file per finding, finding id in the header, `//go:build integration` for behavioral pins, plus source-level `strings.Contains` pins so a refactor that drops the fix fails CI even without infra).

- [x] **Step 1: Write the pins**

Create `internal/security_regression/mf_004_us4_outbound_test.go`:

```go
//go:build integration

// MF-004-US4: Jira outbound + conflict finalization security pins.
//   - §7.4 conflict determinism: external-wins is applied AND a both-changed clobber is audited.
//   - §7.5 SSRF-safe outbound: the outbound dispatcher's HTTP goes through the netsafe client;
//     a connector whose base_url resolves to cloud metadata is refused at dial (no allow flag).
//   - outbound idempotency: a re-claimed op whose message is already stamped does NOT re-post.

package security_regression

import (
	"context"
	"strings"
	"testing"
	// ... project imports (connectors, crypto, db test harness)
)

// Source-level pin: the outbound dispatcher must build its client through the registry's
// netsafe-backed factory and must NOT construct a raw http.Client. Cheap, infra-free.
func TestMF004US4_OutboundUsesNetsafeFactory_SourcePin(t *testing.T) {
	src := readFile(t, "../connectors/outbound.go")
	if strings.Contains(src, "http.Client{") || strings.Contains(src, "http.DefaultClient") {
		t.Fatalf("outbound.go must not build a raw HTTP client; SSRF safety comes from Registry.BuildSystem -> netsafe")
	}
	if !strings.Contains(src, "Registry.BuildSystem") {
		t.Fatalf("outbound.go must build connector clients via Registry.BuildSystem (netsafe-backed)")
	}
}

// Source-level pin: complete_* DEFINERs write an audit_entry (so every external write is audited).
func TestMF004US4_OutboundWritesAreAudited_SourcePin(t *testing.T) {
	up := readFile(t, "../../migrations/0045_connector_outbound.up.sql")
	for _, want := range []string{"connector.outbound.commented", "connector.outbound.created"} {
		if !strings.Contains(up, want) {
			t.Fatalf("migration 0045 must audit outbound writes (%s)", want)
		}
	}
	conf := readFile(t, "../../migrations/0046_connector_conflict_audit.up.sql")
	if !strings.Contains(conf, "connector.conflict.resolved") || !strings.Contains(conf, "external_wins") {
		t.Fatalf("migration 0046 must audit both-changed conflicts as external_wins")
	}
}

// Behavioral pin: an outbound dispatch against a connector whose base_url is the cloud-metadata
// IP (allow_private=false) must FAIL to dial — the op ends up failed/retryable, NO write-back.
func TestMF004US4_OutboundRefusesMetadataDial(t *testing.T) {
	tdb := startConnSec(t) // security_regression's connector harness (reuse connectors test helpers)
	ctx := context.Background()
	seed := seedOutboundConnectorAt(t, ctx, tdb, "http://169.254.169.254", /*allowPrivate=*/ false)

	disp := newOutboundDispatcher(tdb, seed) // helper builds OutboundDispatcher with real jira factory
	if err := disp.DispatchOnceForTest(ctx); err != nil {
		t.Fatalf("dispatchOnce returned fatal err (should swallow per-op): %v", err)
	}
	// The message must NOT be stamped (dial refused upstream of any write-back).
	if ext := messageExternalID(t, ctx, tdb, seed.MessageID); ext != nil {
		t.Fatalf("metadata dial was NOT refused: message external_id = %q", *ext)
	}
}
```

> Notes: `dispatchOnce` is unexported. Either (a) place this behavioral pin in `package connectors` under `internal/connectors/` with the rest, or (b) add a thin exported `DispatchOnceForTest(ctx) error` wrapper guarded by a comment. Prefer (a) — keep the SSRF behavioral pin as `mf_004_us4_outbound_integration_test.go` inside `internal/connectors/` (same package, calls `dispatchOnce` directly) and keep ONLY the infra-free `strings.Contains` source pins in `internal/security_regression/`. This matches US3's split (source pins in `security_regression`, behavioral pins co-located). Adjust file placement accordingly when implementing.

- [x] **Step 2: Run to verify the source pins pass and behavioral pin fails first if the fix were absent**

Run: `go test ./internal/security_regression/ -run TestMF004US4 -v` (source pins; no infra) and `go test -tags integration -p 1 ./internal/connectors/ -run TestMF004US4_OutboundRefusesMetadataDial -v`.
Expected: source pins PASS (the code from T2/T4/T6 satisfies them); behavioral SSRF pin PASS (netsafe refuses 169.254.169.254).

- [x] **Step 3: Confirm `make sec-test` picks the file up**

Run: `export PATH="$PATH:$HOME/go/bin" && make sec-test`
Expected: green, including the new MF-004-US4 source pins.

- [x] **Step 4: Commit** — done at `c66c05c` (amended once after the code-quality review). **Deviations from this sketch, both reviewer-validated:** (1) **File split** — only the infra-free `strings.Contains` source pins live in `internal/security_regression/mf_004_us4_outbound_test.go` (5 pins: SSRF-via-`BuildSystem`, outbound writes audited in 0045, conflict `external_wins` audited in 0046, create-issue ownership predicate in SQL, service→`ErrNotFound` no-oracle); the behavioral SSRF dial-refusal pin lives co-located in `internal/connectors/mf_004_us4_outbound_pin_integration_test.go` (same package — it calls the unexported `dispatchOnce` + reuses `seedOutboundConnector`/`registerStubJira`). (2) **Metadata seed** — the behavioral pin can't seed `http://169.254.169.254` straight through `seedOutboundConnector`, because `Service.Create → validateBaseURL → netsafe.IsBlocked` rejects it at **create-time** (SSRF defense layer #1). To isolate and exercise the **dial-time** guard (layer #2, the one `dispatchOnce` hits), the test seeds a loopback connector then `UPDATE`s the stored `base_url` to the metadata IP via the Super conn before dispatch; `connector_outbound_context` reads `base_url` straight off the row, so the dispatcher genuinely attempts the dial and netsafe refuses it (`AllowPrivateBaseURL=true` makes the pin *stronger* — metadata blocked even with the private-IP hatch open). Spec-compliance ✅ + code-quality ✅ APPROVED (1 Minor — OWNERSHIP `business_id` literal was shared with the comment query — fixed by adding the create-unique INSERT-column-tuple anchor; amended into `c66c05c`).

```bash
gofmt -w internal/security_regression/ internal/connectors/
git add internal/security_regression/mf_004_us4_outbound_test.go internal/connectors/mf_004_us4_outbound_pin_integration_test.go .beads/issues.jsonl
git commit --no-verify -m "test(sec): T7 — MF-004-US4 outbound pins (conflict audited, SSRF dial-refusal, ownership no-oracle) (manyforge-a7j.4)"
```

---

### Task 8: End-to-end bidirectional round-trip integration test

**Files:**
- Create: `internal/connectors/bidirectional_integration_test.go`

Proves the full loop the spec demo (§10) requires, behind the SSRF httptest stub: inbound webhook → native ticket → native reply → outbound dispatcher → Jira comment, plus the create-issue direction.

- [ ] **Step 1: Write the round-trip test**

Create `internal/connectors/bidirectional_integration_test.go`:

```go
//go:build integration

package connectors

// TestBidirectionalRoundTrip: (1) an inbound webhook creates a native ticket, (2) an operator
// reply enqueues an outbound comment op, (3) the dispatcher posts it to the Jira stub and
// writes the comment external_id back. All HTTP flows through the SSRF-safe client.
func TestBidirectionalRoundTrip(t *testing.T) {
	tdb := startConn(t)
	ctx := context.Background()

	var commentPosts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issue/"):
			w.Write(loadIssueGolden(t)) // FetchIssue
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comment"):
			commentPosts++
			w.Write([]byte(`{"id":"jc-rt","author":{"displayName":"ops"},"created":"2026-06-07T00:00:00.000+0000"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	env := seedFullConnectorEnv(t, ctx, tdb, srv.URL) // connector + inbound subscriber + dispatcher + a principal

	// (1) Inbound: deliver a webhook -> inbound subscriber upserts the native ticket.
	env.deliverWebhook(t, "JIRA-RT", "issue.updated")
	env.runInboundOnce(t)
	ticketID := ticketByExternal(t, ctx, tdb, env.ConnectorID, "JIRA-RT")

	// (2) Outbound: an operator reply on the now-linked ticket enqueues a comment op.
	env.replyAsOperator(t, ticketID, "we pushed a fix")

	// (3) Dispatch -> stub receives the comment POST; message external_id written back.
	if err := env.Dispatcher.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}
	if commentPosts != 1 {
		t.Fatalf("comment posts = %d, want 1", commentPosts)
	}
	if !operatorMessageHasExternalID(t, ctx, tdb, ticketID) {
		t.Fatalf("operator reply was not linked back to a Jira comment id")
	}
}
```

> Compose `seedFullConnectorEnv` from the helpers built in Tasks 2/4 plus the US3 inbound webhook + subscriber helpers (`webhook_integration_test.go`, `inbound_sync_integration_test.go`). `replyAsOperator` calls `ticketing.Service.Reply` (wire a `ticketing.Service` into the env) — this is the genuine producer path from Task 3, not a raw insert.

- [ ] **Step 2: Run**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestBidirectionalRoundTrip -v`
Expected: PASS.

- [ ] **Step 3: Full connectors integration sweep + build + gofmt**

Run: `go build ./... && gofmt -l internal/ cmd/ db/ && go test -tags integration -p 1 ./internal/connectors/ -v`
Expected: build clean, gofmt empty, all connector integration tests green.

- [ ] **Step 4: Commit**

```bash
git add internal/connectors/bidirectional_integration_test.go internal/connectors/testsupport_integration_test.go .beads/issues.jsonl
git commit --no-verify -m "test(connectors): T8 — bidirectional round-trip integration (inbound->reply->Jira comment) (manyforge-a7j.4)"
```

---

## Self-Review

**1. Spec coverage (§8 US4 = "native reply→Jira comment, new linked ticket→issue, conflict finalized, round-trip integration test"):**
- native reply → Jira comment → Tasks 3 (producer) + 4 (dispatcher). ✓
- new linked ticket → issue → Tasks 1 (`CreateIssue`) + 5 (escalation + dispatcher create branch). ✓
- conflict finalized (external-wins, both-changed audited, §5.1/§7.4) → Task 6. ✓
- round-trip integration test (§9) → Tasks 4, 5, 8. ✓
- SSRF-safe outbound (§7.5) → Task 7 (reuses US3 netsafe factory). ✓
- Append-only comments / idempotency (§5.2) → external_id-null idempotency anchor in Task 4. ✓

**2. Placeholder scan:** No "TBD"/"add error handling"/"similar to Task N". Each implementation step carries complete code. Test helper bodies (`seed*`, `env.*`) are explicitly flagged as "build this small helper" with their exact responsibilities — they are test scaffolding whose shape depends on the real US3 helper names, which the implementer confirms at the file. Code-quality review must verify these were implemented, not stubbed.

**3. Type consistency:** `ExternalIssueDraft` (Task 1) is consumed identically in `dispatchCreate` (Task 4) and `CreateIssue` (Task 1). `claim_outbound_ops` columns (Task 2) match the `claimedOp` scan (Task 4: `op_id, op_type, connector_id, ticket_id, message_id, ticket_external_id, ticket_subject, body`). `complete_outbound_comment(uuid,uuid,uuid,text)` / `complete_outbound_create(uuid,uuid,uuid,text,text)` signatures match their `tx.Exec` call sites. The `EnqueueOutboundCreate` `:exec`→`:execrows` change (Task 5) is called out so the dbgen method name + return type line up.

**Known soft spots for reviewers to scrutinize (not blockers):**
- `OutboundDispatcher.markDone`/`messageAlreadyPosted` use inline SQL on RLS tables from a principal-less tx; if the app role lacks the needed grant in prod (vs the owner role in tests), convert them to DEFINERs (`mark_outbound_done`, `message_external_id_read`) in migration 0045. Decide via the behavioral test, which runs as the app role under testcontainers.
- The conflict pin only covers the `status` scalar. `priority`/`subject` divergence is out of scope for US4's pin (status is the demonstrative scalar); a follow-up can generalize.

---

## bd children

Create these under `manyforge-a7j.4` so progress is trackable (one per task). Run after the plan is committed:

```bash
bd create --parent manyforge-a7j.4 --type task --priority 2 --title "US4 T1 — CreateIssue on connector interface + Jira client"
bd create --parent manyforge-a7j.4 --type task --priority 2 --title "US4 T2 — outbound op table + claim/complete DEFINERs (0045)"
bd create --parent manyforge-a7j.4 --type task --priority 2 --title "US4 T3 — producer hook: enqueue comment op on connector-linked reply"
bd create --parent manyforge-a7j.4 --type task --priority 2 --title "US4 T4 — OutboundDispatcher poller (comment) + main wiring"
bd create --parent manyforge-a7j.4 --type task --priority 2 --title "US4 T5 — EnqueueOutboundCreateIssue + dispatcher create-issue path"
bd create --parent manyforge-a7j.4 --type task --priority 2 --title "US4 T6 — conflict finalization audit (0046)"
bd create --parent manyforge-a7j.4 --type task --priority 2 --title "US4 T7 — MF-004-US4 security pins"
bd create --parent manyforge-a7j.4 --type task --priority 2 --title "US4 T8 — bidirectional round-trip integration test"
```

## Follow-ups (file as bd issues, do NOT implement in US4)

- `manyforge-a7j.8` — Decide native-email suppression for connector-linked ticket replies (currently additive/both-channel). Product call.
- Generalize conflict audit beyond `status` to `priority`/`subject`.
- Remove the now-unused `TopicConnectorOutboundSync` const (or repurpose as a low-latency dispatcher wakeup).
- `EnqueueOutboundCreateIssue` is the plumbing US6's gated `create`/`transition` tools will drive — ensure US6 routes through the autonomy gate before calling it.

## Final gate (run at story close, NOT per task — separate increment)

```bash
export PATH="$PATH:$HOME/go/bin"
gofmt -l internal/ cmd/ db/         # must be empty
make test && make contract-test && make lint && make sec-test && make int-test   # int-test ~7min, background it
```
Green → final whole-story opus review → `bd close manyforge-a7j.4` → file follow-ups → commit `.beads/issues.jsonl` → `git push`.
