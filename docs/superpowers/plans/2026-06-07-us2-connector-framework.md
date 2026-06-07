# US2 — Connector Framework Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the SL-B connector framework seams that US3/US4 fill — the `TicketingConnector` capability interface, a `Registry` that resolves a `connector` row into a live credential-bound client, external-id mapping columns on `ticket`/`ticket_message`, the sync-state + webhook-delivery tables, and the outbox sync topics — all proven against a fake in-memory connector (no real API yet).

**Architecture:** `TicketingConnector` is the contract every external system implements (US3 Jira, US5 Zendesk). The `Registry` maps `connector_type → Factory` (mirroring `ai.New`'s fail-closed dispatch) and composes US1's `connectors.Service.Resolve` (RLS-scoped credential unseal) with the registered factory to hand back a live client. Migration 0041 adds nullable `connector_id`/`external_id` columns to ticket/ticket_message (composite-FK to connector) plus `connector_sync_state` + `connector_webhook_delivery` tables. US2 ships the seams + the webhook-dedupe idempotency primitive; the inbound/outbound sync handlers and the real Jira factory are US3/US4.

**Tech Stack:** Go, pgx/v5, sqlc (`dbgen`), PostgreSQL + RLS, testcontainers (`testdb`), the US1 `connectors` package (`Service.Resolve`, `ResolvedConnector`), the `events` outbox.

**Spec:** `docs/superpowers/specs/2026-06-06-external-connectors-design.md` (§2 framework, §3 data model, §5 sync transport, §8 US2 row). **Issue:** `manyforge-a7j.2`. **Branch:** `004-external-connectors` (US1 already merged into it).

---

## Conventions locked from the codebase (read before starting)

- **Next migration: `0041`.** Pair `migrations/0041_<name>.{up,down}.sql`. Reuse `support_tenant_root_immutable()` (from 0013; do NOT redefine). Mirror every column/table into `db/schema.sql` (sqlc reads only that). Business-scoped RLS: `USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))`.
- **Composite-FK pattern is mandatory.** Every new child table carries `business_id` + `tenant_root_id`, a composite FK `(business_id, tenant_root_id) → business(id, tenant_root_id)`, a `*_troot_immutable` trigger, RLS, and GRANT. References to `connector`/`ticket` use the composite `(x_id, tenant_root_id) → parent(id, tenant_root_id)` form (both parents already have the backing `UNIQUE(id, tenant_root_id)`).
- **Adding nullable columns to ticket/ticket_message is SAFE** — no positional `dbgen.Ticket{}`/`dbgen.TicketMessage{}` literals exist; production inserts (`ingest_inbound_message()` fn + `db/query/ticketing.sql`) use explicit column lists; `RETURNING *`/`SELECT *` regenerate in lockstep after `make generate`. Still run the existing ticketing+inbox integration tests to confirm.
- **dbgen nullable mapping (IMPORTANT):** with the `uuid`/`timestamptz` go_type overrides, `emit_pointers_for_null_types` does NOT apply to those. So `connector_id uuid NULL` → `pgtype.UUID` (NOT `*uuid.UUID`); `external_id text NULL`/`external_url text NULL` → `*string`. US2 writes almost no Go against the ticket columns (US3/US4 do), but the generated `dbgen.Ticket` gains `ConnectorID pgtype.UUID`, `ExternalID *string`, `ExternalUrl *string`.
- **events outbox:** `events.Enqueue(ctx, tx pgx.Tx, tenantRootID uuid.UUID, topic string, payload any) error`; `(*events.Bus).Subscribe(topic string, h events.Handler)`; `events.Handler = func(ctx, tx pgx.Tx, e events.Event) error`. Existing topic constants live in `events/bus.go`, but module-local topics (e.g. `agents.TopicAgentApproved`) are the precedent for a package owning its own topics — so US2's connector topics live in the `connectors` package.
- **US1 surface (reuse):** `connectors.Service{DB, Vault, Verify}` with `Resolve(ctx, principalID, businessID, connectorID uuid.UUID) (ResolvedConnector, error)` (cross-tenant/unknown → `errs.ErrNotFound`). `ResolvedConnector{ID, Type string, BaseURL string, AllowPrivateBaseURL bool, Config map[string]any, Credential{Email, APIToken}}`. Test helpers in `internal/connectors/*_test.go`: `startConn(t)`, `newConnService(t, tdb, v)`, `jiraInput()`, `seedConnectorTenant(ctx, t, tdb)`.
- **dbgen enum:** `dbgen.ConnectorTypeJira = "jira"`, `dbgen.ConnectorTypeZendesk = "zendesk"`. `ResolvedConnector.Type` is a plain `string`.
- **gopls LIES** in this repo (`dbgen.*` "undefined", `//go:build integration` "No packages found") — trust `go build ./...`/`go test`, NEVER the IDE panel.
- **Tests:** `make test` = unit (no tag). DB-backed = `//go:build integration`, `testdb.Start(ctx)` auto-applies `migrations/`. Commit `--no-verify`; `git add` only the intended files (no stray `CLAUDE.md`).

---

## File Structure

| File | Responsibility |
|------|----------------|
| `migrations/0041_connector_sync.{up,down}.sql` | ext-id columns on ticket/ticket_message; `connector_sync_state` + `connector_webhook_delivery` tables |
| `db/schema.sql` (modify) | mirror the new columns + tables for sqlc |
| `db/query/connector.sql` (append) | `RecordWebhookDelivery` query |
| `internal/platform/db/dbgen/*` (generated) | sqlc output |
| `internal/connectors/connector.go` (create) | `TicketingConnector` interface + `ExternalIssue`/`ExternalComment`/`WebhookEvent` types + sync topic constants |
| `internal/connectors/connector_test.go` (create) | `fakeConnector` + interface-satisfaction unit test |
| `internal/connectors/registry.go` (create) | `Factory`, `Registry{Register,Resolve}` |
| `internal/connectors/registry_integration_test.go` (create) | registry resolve / unregistered-type / cross-tenant |
| `internal/connectors/sync_store.go` (create) | `Service.RecordWebhookDelivery` dedupe helper |
| `internal/connectors/sync_store_integration_test.go` (create) | webhook-delivery idempotency + cross-business |

**Deferred to US3** (do NOT build here): the `UpsertSyncState` helper + `connector_sync_state` queries (US3's inbound flow seeds a real ticket and populates it); the inbound/outbound outbox subscribers; the real Jira `Factory`; the webhook HTTP handler.

---

## Task 1: Migration 0041 — ext-id columns + sync tables + codegen

**Files:**
- Create: `migrations/0041_connector_sync.up.sql`, `migrations/0041_connector_sync.down.sql`
- Modify: `db/schema.sql`
- Modify: `db/query/connector.sql` (append one query)
- Generated: `internal/platform/db/dbgen/*`

- [ ] **Step 1: up migration** — `migrations/0041_connector_sync.up.sql`:

```sql
-- 0041: connector framework schema (Spec 004 US2). External-id mapping columns on
-- ticket/ticket_message (composite-FK to connector) + the sync-state snapshot table
-- and the webhook-delivery replay-dedupe table. All nullable on ticket/ticket_message
-- so existing native tickets are unaffected.

ALTER TABLE ticket
    ADD COLUMN connector_id uuid NULL,
    ADD COLUMN external_id  text NULL,
    ADD COLUMN external_url text NULL,
    ADD CONSTRAINT ticket_connector_fk
        FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id);
CREATE UNIQUE INDEX ticket_external_idx ON ticket (connector_id, external_id)
    WHERE connector_id IS NOT NULL;

ALTER TABLE ticket_message
    ADD COLUMN connector_id uuid NULL,
    ADD COLUMN external_id  text NULL,
    ADD CONSTRAINT ticket_message_connector_fk
        FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id);
CREATE UNIQUE INDEX ticket_message_external_idx ON ticket_message (connector_id, external_id)
    WHERE connector_id IS NOT NULL;

-- Per-ticket external sync snapshot (external-wins reconcile cursor + last-synced scalars).
CREATE TABLE connector_sync_state (
    ticket_id           uuid PRIMARY KEY,
    business_id         uuid NOT NULL,
    tenant_root_id      uuid NOT NULL,
    connector_id        uuid NOT NULL,
    external_id         text NOT NULL,
    snapshot            jsonb NOT NULL DEFAULT '{}',
    external_updated_at timestamptz NOT NULL,
    synced_at           timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (ticket_id, tenant_root_id) REFERENCES ticket (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id)
);
CREATE INDEX connector_sync_state_business_idx ON connector_sync_state (business_id, tenant_root_id);
CREATE TRIGGER connector_sync_state_troot_immutable
    BEFORE UPDATE ON connector_sync_state
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
GRANT SELECT, INSERT, UPDATE, DELETE ON connector_sync_state TO manyforge_app;
ALTER TABLE connector_sync_state ENABLE ROW LEVEL SECURITY;
CREATE POLICY connector_sync_state_rls ON connector_sync_state FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

-- Inbound webhook delivery dedupe (replay protection): one row per (connector, delivery id).
CREATE TABLE connector_webhook_delivery (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    connector_id         uuid NOT NULL,
    external_delivery_id text NOT NULL,
    received_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (connector_id, external_delivery_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id)
);
CREATE INDEX connector_webhook_delivery_business_idx ON connector_webhook_delivery (business_id, tenant_root_id);
CREATE TRIGGER connector_webhook_delivery_troot_immutable
    BEFORE UPDATE ON connector_webhook_delivery
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
GRANT SELECT, INSERT, UPDATE, DELETE ON connector_webhook_delivery TO manyforge_app;
ALTER TABLE connector_webhook_delivery ENABLE ROW LEVEL SECURITY;
CREATE POLICY connector_webhook_delivery_rls ON connector_webhook_delivery FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
```

- [ ] **Step 2: down migration** — `migrations/0041_connector_sync.down.sql`:

```sql
-- Reverse 0041_connector_sync.
DROP POLICY IF EXISTS connector_webhook_delivery_rls ON connector_webhook_delivery;
ALTER TABLE connector_webhook_delivery DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON connector_webhook_delivery FROM manyforge_app;
DROP TRIGGER IF EXISTS connector_webhook_delivery_troot_immutable ON connector_webhook_delivery;
DROP INDEX IF EXISTS connector_webhook_delivery_business_idx;
DROP TABLE IF EXISTS connector_webhook_delivery;

DROP POLICY IF EXISTS connector_sync_state_rls ON connector_sync_state;
ALTER TABLE connector_sync_state DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON connector_sync_state FROM manyforge_app;
DROP TRIGGER IF EXISTS connector_sync_state_troot_immutable ON connector_sync_state;
DROP INDEX IF EXISTS connector_sync_state_business_idx;
DROP TABLE IF EXISTS connector_sync_state;

DROP INDEX IF EXISTS ticket_message_external_idx;
ALTER TABLE ticket_message DROP CONSTRAINT IF EXISTS ticket_message_connector_fk;
ALTER TABLE ticket_message DROP COLUMN IF EXISTS external_id;
ALTER TABLE ticket_message DROP COLUMN IF EXISTS connector_id;

DROP INDEX IF EXISTS ticket_external_idx;
ALTER TABLE ticket DROP CONSTRAINT IF EXISTS ticket_connector_fk;
ALTER TABLE ticket DROP COLUMN IF EXISTS external_url;
ALTER TABLE ticket DROP COLUMN IF EXISTS external_id;
ALTER TABLE ticket DROP COLUMN IF EXISTS connector_id;
```

- [ ] **Step 3: mirror into `db/schema.sql`.** (a) In the `ticket` CREATE TABLE, add after `updated_at timestamptz NOT NULL,`:

```sql
    connector_id          uuid,
    external_id           text,
    external_url          text,
```

(b) In the `ticket_message` CREATE TABLE, add after `source_approval_item_id uuid,`:

```sql
    connector_id        uuid,
    external_id         text,
```

(c) After the `ticket`/`ticket_message` indexes already in schema.sql (or near them), add:

```sql
CREATE UNIQUE INDEX ticket_external_idx ON ticket (connector_id, external_id) WHERE connector_id IS NOT NULL;
CREATE UNIQUE INDEX ticket_message_external_idx ON ticket_message (connector_id, external_id) WHERE connector_id IS NOT NULL;
```

(d) Append the two new tables (sqlc-only form, DEFAULT/RLS/trigger stripped) after the `connector` block:

```sql
CREATE TABLE connector_sync_state (
    ticket_id           uuid PRIMARY KEY,
    business_id         uuid NOT NULL,
    tenant_root_id      uuid NOT NULL,
    connector_id        uuid NOT NULL,
    external_id         text NOT NULL,
    snapshot            jsonb NOT NULL,
    external_updated_at timestamptz NOT NULL,
    synced_at           timestamptz NOT NULL,
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (ticket_id, tenant_root_id) REFERENCES ticket (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id)
);
CREATE INDEX connector_sync_state_business_idx ON connector_sync_state (business_id, tenant_root_id);

CREATE TABLE connector_webhook_delivery (
    id                   uuid PRIMARY KEY,
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    connector_id         uuid NOT NULL,
    external_delivery_id text NOT NULL,
    received_at          timestamptz NOT NULL,
    UNIQUE (connector_id, external_delivery_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id)
);
CREATE INDEX connector_webhook_delivery_business_idx ON connector_webhook_delivery (business_id, tenant_root_id);
```

- [ ] **Step 4: append the dedupe query** to `db/query/connector.sql`:

```sql
-- RecordWebhookDelivery dedupes inbound webhook deliveries per connector: ON CONFLICT
-- DO NOTHING means a replayed external_delivery_id inserts zero rows, which the caller
-- reads as "already seen". tenant_root derived from the RLS-visible business; the EXISTS
-- guard requires connector_id to belong to the SAME business (defense-in-depth).
-- name: RecordWebhookDelivery :execrows
INSERT INTO connector_webhook_delivery (id, business_id, tenant_root_id, connector_id, external_delivery_id, received_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('connector_id'), sqlc.arg('external_delivery_id'), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
  AND EXISTS (SELECT 1 FROM connector c WHERE c.id = sqlc.arg('connector_id') AND c.business_id = b.id)
ON CONFLICT (connector_id, external_delivery_id) DO NOTHING;
```

- [ ] **Step 5: generate** — Run `make generate`. Expected exit 0; `dbgen/models.go` gains `ConnectorSyncState` + `ConnectorWebhookDelivery` structs, `Ticket` gains `ConnectorID pgtype.UUID`/`ExternalID *string`/`ExternalUrl *string`, `TicketMessage` gains `ConnectorID pgtype.UUID`/`ExternalID *string`; `RecordWebhookDeliveryParams{ID, ConnectorID, ExternalDeliveryID, BusinessID}` is generated. Report the exact `RecordWebhookDeliveryParams` field names + types.

- [ ] **Step 6: verify migration up/down** (skip if dev DB on :55432 unreachable — testdb is authoritative):
```bash
export DSN="postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable"
migrate -path migrations -database "$DSN" up && migrate -path migrations -database "$DSN" down 1 && migrate -path migrations -database "$DSN" up
```
Expected: each exits 0.

- [ ] **Step 7: build + prove existing tables still work** — Run:
```bash
go build ./...
go test -tags integration -p 1 ./internal/ticketing/ ./internal/inbox/ -count=1 2>&1 | tail -8
```
Expected: build 0; ticketing + inbox integration tests PASS (confirms the nullable columns don't break native ticket flows).

- [ ] **Step 8: commit**
```bash
git add migrations/0041_connector_sync.up.sql migrations/0041_connector_sync.down.sql db/schema.sql db/query/connector.sql internal/platform/db/dbgen/
git commit -m "feat(connectors): ext-id columns + sync-state/webhook-delivery tables (mig 0041) (manyforge-a7j.2)" --no-verify
```

---

## Task 2: `TicketingConnector` interface + types + sync topics

**Files:**
- Create: `internal/connectors/connector.go`
- Test: `internal/connectors/connector_test.go`

- [ ] **Step 1: write the interface + types + topics** — `internal/connectors/connector.go`:

```go
package connectors

import (
	"context"
	"time"
)

// Outbox topics for the sync engine. The connectors package owns them (mirroring how
// agents owns TopicAgentApproved). US3 subscribes a connector-sync handler to these.
const (
	TopicConnectorInboundSync  = "connector.inbound.sync"
	TopicConnectorOutboundSync = "connector.outbound.sync"
)

// ExternalComment is one comment on an external issue.
type ExternalComment struct {
	ExternalID string
	Author     string
	Body       string
	CreatedAt  time.Time
}

// ExternalIssue is the external system's view of a ticket (Jira issue / Zendesk ticket).
type ExternalIssue struct {
	ExternalID string
	URL        string
	Title      string
	Status     string
	Priority   string
	Comments   []ExternalComment
	UpdatedAt  time.Time
}

// WebhookEvent is the routing info decoded from an inbound webhook payload.
type WebhookEvent struct {
	DeliveryID string // unique per delivery, for replay dedupe
	ExternalID string // the external issue this event concerns
	Kind       string // e.g. "issue.updated", "comment.created"
}

// TicketingConnector is the capability contract every external ticketing system
// implements. A live instance is bound (by the Registry) to one business's resolved
// credential + an SSRF-safe HTTP client. US3 implements Jira; US5 implements Zendesk.
// US3 may refine these signatures against the real Jira API.
type TicketingConnector interface {
	// FetchIssue returns the external issue + its comments by external id.
	FetchIssue(ctx context.Context, externalID string) (ExternalIssue, error)
	// PostComment appends a comment, returning the created comment's metadata.
	PostComment(ctx context.Context, externalID, body string) (ExternalComment, error)
	// TransitionStatus moves the external issue to the target status.
	TransitionStatus(ctx context.Context, externalID, status string) error
	// ListUpdatedSince returns external issue ids updated at/after the cursor (reconcile).
	ListUpdatedSince(ctx context.Context, since time.Time) ([]string, error)
	// VerifyWebhook checks the inbound payload's signature (per-connector secret).
	VerifyWebhook(headers map[string]string, body []byte) error
	// DecodeWebhook extracts routing info from a verified inbound payload.
	DecodeWebhook(body []byte) (WebhookEvent, error)
}
```

- [ ] **Step 2: write the fake + interface-satisfaction test** — `internal/connectors/connector_test.go` (NO build tag — `fakeConnector` is reused by the Task 3 integration test, and untagged test files compile in both unit and integration builds):

```go
package connectors

import (
	"context"
	"testing"
	"time"
)

// fakeConnector is a canned in-memory TicketingConnector: it proves the interface is
// implementable and lets the Registry test run without a real external API.
type fakeConnector struct {
	issue ExternalIssue
}

var _ TicketingConnector = (*fakeConnector)(nil)

func (f *fakeConnector) FetchIssue(ctx context.Context, externalID string) (ExternalIssue, error) {
	return f.issue, nil
}
func (f *fakeConnector) PostComment(ctx context.Context, externalID, body string) (ExternalComment, error) {
	return ExternalComment{ExternalID: "c-1", Body: body, CreatedAt: time.Unix(0, 0).UTC()}, nil
}
func (f *fakeConnector) TransitionStatus(ctx context.Context, externalID, status string) error {
	return nil
}
func (f *fakeConnector) ListUpdatedSince(ctx context.Context, since time.Time) ([]string, error) {
	return []string{f.issue.ExternalID}, nil
}
func (f *fakeConnector) VerifyWebhook(headers map[string]string, body []byte) error { return nil }
func (f *fakeConnector) DecodeWebhook(body []byte) (WebhookEvent, error) {
	return WebhookEvent{DeliveryID: "d-1", ExternalID: f.issue.ExternalID, Kind: "issue.updated"}, nil
}

func TestFakeConnectorSatisfiesInterface(t *testing.T) {
	var c TicketingConnector = &fakeConnector{issue: ExternalIssue{ExternalID: "JIRA-1", Title: "x"}}
	iss, err := c.FetchIssue(context.Background(), "JIRA-1")
	if err != nil || iss.ExternalID != "JIRA-1" {
		t.Fatalf("fake fetch: err=%v issue=%+v", err, iss)
	}
}
```

- [ ] **Step 3: run** — `go test ./internal/connectors/ -run 'TestFakeConnector|TestValidate' -v`
Expected: PASS. Also `go build ./...` (0) and `go vet ./internal/connectors/` (clean).

- [ ] **Step 4: commit**
```bash
git add internal/connectors/connector.go internal/connectors/connector_test.go
git commit -m "feat(connectors): TicketingConnector interface + external types + sync topics (manyforge-a7j.2)" --no-verify
```

---

## Task 3: `Registry` — type→factory dispatch + credential-bound resolve

**Files:**
- Create: `internal/connectors/registry.go`
- Test: `internal/connectors/registry_integration_test.go`

- [ ] **Step 1: write the failing registry tests** — `internal/connectors/registry_integration_test.go`:

```go
//go:build integration

package connectors

import (
	"context"
	"errors"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestRegistryResolveBindsCredential(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		// The factory receives the resolved, unsealed credential + base_url.
		if rc.Credential.APIToken != "tok-abc-123" {
			t.Fatalf("factory did not receive unsealed credential: %+v", rc.Credential)
		}
		return &fakeConnector{issue: ExternalIssue{ExternalID: "JIRA-1", URL: rc.BaseURL}}, nil
	})

	c, err := reg.Resolve(ctx, seed.principalID, seed.businessID, connID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	iss, err := c.FetchIssue(ctx, "JIRA-1")
	if err != nil || iss.URL != "https://acme.atlassian.net" {
		t.Fatalf("expected fake bound to base_url, got %+v err %v", iss, err)
	}
}

func TestRegistryUnregisteredTypeErrors(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	reg := NewRegistry(svc) // no factories registered
	if _, err := reg.Resolve(ctx, seed.principalID, seed.businessID, connID); err == nil {
		t.Fatalf("expected error for unregistered connector type")
	}
}

func TestRegistryResolveCrossTenantNotFound(t *testing.T) {
	ctx, tdb, a := startConn(t)
	b := seedConnectorTenant(ctx, t, tdb)
	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, a.principalID, a.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		return &fakeConnector{}, nil
	})
	_, err = reg.Resolve(ctx, b.principalID, b.businessID, connID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("want ErrNotFound for cross-tenant resolve, got %v", err)
	}
}
```

- [ ] **Step 2: run, verify FAIL** — `go test -tags integration -p 1 ./internal/connectors/ -run TestRegistry -v`
Expected: FAIL — `undefined: NewRegistry`.

- [ ] **Step 3: implement** — `internal/connectors/registry.go`:

```go
package connectors

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Factory builds a live TicketingConnector from a resolved connector (unsealed
// credential + base_url + trust flag). US3/US4 register the real Jira/Zendesk factories;
// each builds its own SSRF-safe HTTP client honoring rc.AllowPrivateBaseURL.
type Factory func(rc ResolvedConnector) (TicketingConnector, error)

// Registry maps a connector type to its Factory and resolves a connector row (via the
// US1 credential Service, RLS-scoped) into a live, credential-bound client.
type Registry struct {
	svc       *Service
	factories map[string]Factory
}

// NewRegistry builds an empty registry over the credential service.
func NewRegistry(svc *Service) *Registry {
	return &Registry{svc: svc, factories: map[string]Factory{}}
}

// Register binds a connector type (e.g. "jira") to its factory.
func (r *Registry) Register(connectorType string, f Factory) {
	r.factories[connectorType] = f
}

// Resolve loads the connector (RLS-scoped to business) and builds its live client.
// Cross-tenant / unknown connector → ErrNotFound (from the Service). An enabled
// connector whose type has no registered factory is a server-config error (not a
// client fault), returned as a plain wrapped error (→ 500 at the handler).
func (r *Registry) Resolve(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (TicketingConnector, error) {
	rc, err := r.svc.Resolve(ctx, principalID, businessID, connectorID)
	if err != nil {
		return nil, err
	}
	f, ok := r.factories[rc.Type]
	if !ok {
		return nil, fmt.Errorf("connectors: no factory registered for type %q", rc.Type)
	}
	return f(rc)
}
```

- [ ] **Step 4: run, verify PASS** — `go test -tags integration -p 1 ./internal/connectors/ -run TestRegistry -v`
Expected: PASS (all three). Also `go build ./...` (0).

- [ ] **Step 5: commit**
```bash
git add internal/connectors/registry.go internal/connectors/registry_integration_test.go
git commit -m "feat(connectors): Registry — type->factory dispatch over RLS-scoped Resolve (manyforge-a7j.2)" --no-verify
```

---

## Task 4: Webhook-delivery dedupe helper (sync idempotency primitive)

**Files:**
- Create: `internal/connectors/sync_store.go`
- Test: `internal/connectors/sync_store_integration_test.go`

- [ ] **Step 1: write the failing idempotency tests** — `internal/connectors/sync_store_integration_test.go`:

```go
//go:build integration

package connectors

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func recordDelivery(t *testing.T, ctx context.Context, tdb *testdb.TestDB, svc *Service, principalID, businessID, connectorID uuid.UUID, deliveryID string) bool {
	t.Helper()
	var fresh bool
	if err := tdb.App.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		ok, e := svc.RecordWebhookDelivery(ctx, tx, businessID, connectorID, deliveryID)
		fresh = ok
		return e
	}); err != nil {
		t.Fatalf("record delivery: %v", err)
	}
	return fresh
}

func TestRecordWebhookDeliveryIdempotent(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	if !recordDelivery(t, ctx, tdb, svc, seed.principalID, seed.businessID, connID, "delivery-1") {
		t.Fatalf("first delivery should be newly recorded")
	}
	if recordDelivery(t, ctx, tdb, svc, seed.principalID, seed.businessID, connID, "delivery-1") {
		t.Fatalf("replayed delivery-1 should be a duplicate (false)")
	}
	if !recordDelivery(t, ctx, tdb, svc, seed.principalID, seed.businessID, connID, "delivery-2") {
		t.Fatalf("new delivery-2 should be recorded")
	}
}

func TestRecordWebhookDeliveryCrossBusiness(t *testing.T) {
	ctx, tdb, a := startConn(t)
	b := seedConnectorTenant(ctx, t, tdb)
	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, a.principalID, a.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Tenant B cannot record a delivery against tenant A's connector (EXISTS guard +
	// RLS → zero rows → false, no row written).
	if recordDelivery(t, ctx, tdb, svc, b.principalID, b.businessID, connID, "x") {
		t.Fatalf("cross-business record must not succeed")
	}
}
```

- [ ] **Step 2: run, verify FAIL** — `go test -tags integration -p 1 ./internal/connectors/ -run TestRecordWebhook -v`
Expected: FAIL — `svc.RecordWebhookDelivery undefined`.

- [ ] **Step 3: implement** — `internal/connectors/sync_store.go`:

```go
package connectors

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// RecordWebhookDelivery records an inbound webhook delivery for replay protection, in
// the caller's tx. Returns true if newly recorded, false if this (connector, delivery_id)
// was already seen — or if the connector is not visible to the business (RLS/guard) — in
// which case the caller should no-op. The query's ON CONFLICT DO NOTHING + same-business
// EXISTS guard make replays and cross-business attempts both yield zero rows.
func (s *Service) RecordWebhookDelivery(ctx context.Context, tx pgx.Tx, businessID, connectorID uuid.UUID, deliveryID string) (bool, error) {
	n, err := dbgen.New(tx).RecordWebhookDelivery(ctx, dbgen.RecordWebhookDeliveryParams{
		ID:                 uuid.New(),
		BusinessID:         businessID,
		ConnectorID:        connectorID,
		ExternalDeliveryID: deliveryID,
	})
	if err != nil {
		return false, fmt.Errorf("connectors: record webhook delivery: %w", err)
	}
	return n > 0, nil
}
```

> If Task 1's `make generate` produced different `RecordWebhookDeliveryParams` field names than `{ID, BusinessID, ConnectorID, ExternalDeliveryID}`, match the generated names (check Task 1's Step 5 report).

- [ ] **Step 4: run, verify PASS** — `go test -tags integration -p 1 ./internal/connectors/ -run TestRecordWebhook -v`
Expected: PASS (both). Then the full connectors suite + build:
```bash
go build ./...
go test -tags integration -p 1 ./internal/connectors/ -v 2>&1 | tail -20
```
Expected: all connectors tests (US1 + US2) PASS.

- [ ] **Step 5: commit**
```bash
git add internal/connectors/sync_store.go internal/connectors/sync_store_integration_test.go
git commit -m "feat(connectors): RecordWebhookDelivery dedupe — webhook replay idempotency (manyforge-a7j.2)" --no-verify
```

- [ ] **Step 6: full gate + close issue** (controller runs this after reviews):
```bash
export PATH="$PATH:$HOME/go/bin"
gofmt -l internal/ cmd/ db/    # must be empty
make test && make contract-test && make lint && make sec-test && make int-test
bd close manyforge-a7j.2
```

---

## Deferred to US3 (do NOT build here)

- `UpsertSyncState` helper + `connector_sync_state` queries — US3's inbound sync seeds a real ticket and populates the snapshot.
- Inbound/outbound outbox **subscribers** (`Handle` methods) + `eventBus.Subscribe(...)` wiring in `main.go` — US3/US4.
- The real Jira `Factory` (builds a `netsafe.NewClientWithOptions` client + Jira REST calls) — US3.
- The public webhook HTTP handler — US3.
- main.go construction of `Service`/`Registry` (config master key + `secrets.NewVault`) — first production consumer is US3's handler/subscriber wiring; US2 ships tested library code.

## Self-Review

- **Spec coverage (US2 slice):** `TicketingConnector` interface ✅ (Task 2); `Registry` ✅ (Task 3); ext-id/`connector_id` columns on ticket+ticket_message ✅ (Task 1); `connector_sync_state` + `connector_webhook_delivery` tables ✅ (Task 1); outbox topic constants ✅ (Task 2); proven against a fake connector ✅ (Tasks 2–3); sync-idempotency primitive (webhook dedupe) ✅ (Task 4, supports §7 pin 3). Deferred items explicitly scoped to US3.
- **Placeholder scan:** none — every step has full code + commands.
- **Type consistency:** `TicketingConnector`/`Factory`/`Registry`/`ResolvedConnector`/`ExternalIssue`/`ExternalComment`/`WebhookEvent` names consistent across Tasks 2–4; `fakeConnector` (Task 2) reused by Task 3; `RecordWebhookDelivery` Go method ↔ `RecordWebhookDelivery` sqlc query ↔ `RecordWebhookDeliveryParams` consistent. dbgen nullable nuance (`pgtype.UUID`/`*string`) noted but not Go-touched in US2.
