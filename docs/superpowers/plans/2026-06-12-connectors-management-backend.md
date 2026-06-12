# Connectors Management API (Backend) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `connectors.manage`-gated HTTP CRUD API (list, create, get, edit, rotate-credential, test, delete) over the existing Spec-004 connectors engine, with sealed credentials never returned and hard-delete that detaches synced tickets to native.

**Architecture:** Mirror the existing `agents` CRUD slice end-to-end — chi `ProtectedRoutes` behind a `RequirePermission` gate, thin handlers with wire DTOs separate from domain structs, service methods that wrap every DB call in `DB.WithPrincipal` with an explicit `business_id` predicate, and typed `errs` sentinels mapped to HTTP by `httpx.WriteError`. New service methods live on the existing `connectors.Service`. No new tables — only a permission-catalog migration (0048) and new sqlc queries.

**Tech Stack:** Go, chi, pgx/v5, sqlc (`dbgen`), PostgreSQL with RLS, testcontainers (integration tests), golang-migrate.

**Spec:** `docs/superpowers/specs/2026-06-12-connectors-management-design.md`. **Issue:** `manyforge-4zs.3`.

**Conventions you must follow (verbatim from the codebase):**
- `errs` sentinels: `ErrNotFound` (also used for unauthorized — no oracle), `ErrForbidden`, `ErrValidation` (message safe to surface), `ErrConflict`, `ErrRateLimited`.
- `httpx.WriteError(w, r, err)` maps those to 404/404/400/409/429 (else 500). `httpx.WriteJSON(w, status, v)`. `httpx.DecodeJSON(w, r, &v) bool` (writes 400 on bad input, returns false). `httpx.PrincipalFromContext(ctx) (uuid.UUID, bool)`.
- `db.DB.WithPrincipal(ctx, principalID uuid.UUID, fn func(pgx.Tx) error) error` — runs `fn` in an RLS-scoped tx.
- `dbgen.New(tx).<Query>(ctx, params)` — sqlc queries. Run `make generate` after editing any `db/query/*.sql`. **Never hand-edit `dbgen`.**
- `Vault.Put(ctx, tx, businessID, scope, plaintext) (uuid.UUID, error)`, `Vault.Open(ctx, tx, businessID, secretID) ([]byte, error)`, `Vault.Delete(ctx, tx, businessID, secretID) error`.
- `audit.Write(ctx, tx, audit.Entry{...}) error` — call inside the same tx as the mutation.
- Integration tests use `//go:build integration` and run under `make sec-test` / `make int-test` (Docker required). Unit tests run under `make test`.

---

## File Structure

| File | Responsibility | Create/Modify |
|---|---|---|
| `migrations/0048_connector_manage_permission.up.sql` / `.down.sql` | `connectors.manage` permission catalog + owner/admin grant | Create |
| `db/query/connector_manage.sql` | New sqlc queries: list, health, update, secret-ref swap, detach + cascade-delete | Create |
| `internal/connectors/manage.go` | Management service methods (`List/Get/Update/RotateCredential/Test/Delete`) + their domain types/validation on the existing `*Service` | Create |
| `internal/connectors/handler.go` | HTTP `Handler`: `ProtectedRoutes`, wire DTOs, thin handlers | Create |
| `cmd/manyforge/main.go` | Construct the handler + `connectors.manage` middleware, mount under RequireAuth | Modify |
| `specs/004-external-connectors/contracts/openapi.yaml` | OpenAPI contract for the connector-management endpoints (establishes the 004 contract dir) | Create |
| `internal/connectors/handler_test.go` | Unit tests (fake service): validation, no-credential-in-response, bad-UUID → 404 | Create |
| `internal/connectors/manage_integration_test.go` | Integration: full CRUD, RLS isolation, permission matrix, delete-detach correctness | Create |
| `internal/security_regression/connectors_manage_pin_test.go` | Merge-gate security pins (write-only creds, delete preserves external_id, source-level detach pin) | Create |

---

## Task 1: Permission migration `0048` + middleware wiring

**Files:**
- Create: `migrations/0048_connector_manage_permission.up.sql`
- Create: `migrations/0048_connector_manage_permission.down.sql`

- [ ] **Step 1: Write the up migration**

`migrations/0048_connector_manage_permission.up.sql`:
```sql
-- 0048: connectors-management permission (Spec 004 / manyforge-4zs.3). connectors.manage
-- gates the human-facing connector CRUD API (list/create/edit/rotate/test/delete). It is
-- DISTINCT from connectors.read / connectors.write (migration 0047), which gate the agent
-- tools. Granted to the owner + admin presets (connecting an external system + holding its
-- credentials is an administrative action). Key/module are authoritative and shared verbatim
-- with the OpenAPI contract — do not rename.

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('connectors.manage', 'connectors', 'Create, configure, rotate credentials for, and delete external connectors');

-- owner + admin ⇒ connectors.manage.
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'connectors.manage'
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin');
```

- [ ] **Step 2: Write the down migration**

`migrations/0048_connector_manage_permission.down.sql`:
```sql
-- Reverse 0048_connector_manage_permission.
DELETE FROM role_permission WHERE permission_key = 'connectors.manage';
DELETE FROM permission WHERE key = 'connectors.manage';
```

- [ ] **Step 3: Apply the migration against the dev DB**

Run: `set -a; . ./.air.env; set +a; make migrate`
Expected: `48/u connector_manage_permission` (no error). Verify:
`PGPASSWORD=devpassword psql -h localhost -p 55432 -U manyforge -d manyforge -tAc "select key from permission where key='connectors.manage';"`
Expected output: `connectors.manage`

- [ ] **Step 4: Commit**

```bash
git add migrations/0048_connector_manage_permission.up.sql migrations/0048_connector_manage_permission.down.sql
git commit -m "feat(connectors): connectors.manage permission (migration 0048)"
```

---

## Task 2: sqlc queries for management

**Files:**
- Create: `db/query/connector_manage.sql`
- Modify (generated): `internal/platform/db/dbgen/*` via `make generate`

- [ ] **Step 1: Write the new query file**

`db/query/connector_manage.sql`:
```sql
-- ListConnectors returns all connectors for a business, newest-stable order. RLS + the
-- business_id predicate scope this to the caller's tenant. Credentials are NOT joined —
-- only the connector row (which holds no plaintext secret, just secret_ref).
-- name: ListConnectors :many
SELECT * FROM connector WHERE business_id = $1 ORDER BY display_name, created_at;

-- ListConnectorHealth returns per-connector sync-health aggregates for a business in one
-- round-trip (avoids N+1). Counts run under the caller's RLS context. last_error is the
-- most-recent failed outbound op's stored reason (already redacted at write time).
-- name: ListConnectorHealth :many
SELECT
    c.id AS connector_id,
    (SELECT count(*) FROM ticket t WHERE t.connector_id = c.id)::bigint AS linked_ticket_count,
    (SELECT count(*) FROM connector_outbound_op o WHERE o.connector_id = c.id AND o.status IN ('pending','in_progress'))::bigint AS pending_ops,
    (SELECT count(*) FROM connector_outbound_op o WHERE o.connector_id = c.id AND o.status = 'failed')::bigint AS failed_ops,
    (SELECT o.last_error FROM connector_outbound_op o WHERE o.connector_id = c.id AND o.status = 'failed' ORDER BY o.updated_at DESC LIMIT 1) AS last_error
FROM connector c
WHERE c.business_id = $1;

-- GetConnectorHealth returns the same aggregates for a single connector (used by Get). The
-- caller has already confirmed ownership via GetConnector; RLS still scopes the subqueries.
-- name: GetConnectorHealth :one
SELECT
    (SELECT count(*) FROM ticket t WHERE t.connector_id = sqlc.arg('connector_id'))::bigint AS linked_ticket_count,
    (SELECT count(*) FROM connector_outbound_op o WHERE o.connector_id = sqlc.arg('connector_id') AND o.status IN ('pending','in_progress'))::bigint AS pending_ops,
    (SELECT count(*) FROM connector_outbound_op o WHERE o.connector_id = sqlc.arg('connector_id') AND o.status = 'failed')::bigint AS failed_ops,
    (SELECT o.last_error FROM connector_outbound_op o WHERE o.connector_id = sqlc.arg('connector_id') AND o.status = 'failed' ORDER BY o.updated_at DESC LIMIT 1) AS last_error;

-- UpdateConnector applies a partial (PATCH) change scoped to (id, business_id). Omitted
-- fields (NULL narg) are preserved via COALESCE. base_url and type are intentionally NOT
-- updatable (they are part of the connector's identity). No matching row → no row returned
-- → pgx.ErrNoRows → 404 (no oracle). status is written as text exactly like InsertConnector.
-- name: UpdateConnector :one
UPDATE connector SET
    display_name = COALESCE(sqlc.narg('display_name'), display_name),
    config       = COALESCE(sqlc.narg('config'), config),
    status       = COALESCE(sqlc.narg('status'), status),
    updated_at   = now()
WHERE id = sqlc.arg('id') AND business_id = sqlc.arg('business_id')
RETURNING *;

-- UpdateConnectorSecretRef swaps the sealed-credential pointer during rotation, scoped to
-- (id, business_id). :execrows lets the caller detect a no-op (unknown/foreign id → 0 rows).
-- name: UpdateConnectorSecretRef :execrows
UPDATE connector SET secret_ref = sqlc.arg('secret_ref'), updated_at = now()
WHERE id = sqlc.arg('id') AND business_id = sqlc.arg('business_id');

-- DetachTicketsFromConnector severs linked tickets on hard-delete: NULL connector_id only,
-- PRESERVING external_id/external_url as read-only history. Permitted by
-- CHECK(connector_id IS NULL OR external_id IS NOT NULL) — the NULL-connector clause passes.
-- Scoped by connector_id (a globally-unique uuid the caller already confirmed it owns).
-- name: DetachTicketsFromConnector :execrows
UPDATE ticket SET connector_id = NULL, updated_at = now() WHERE connector_id = $1;

-- DetachTicketMessagesFromConnector — same sever for message-level external linkage.
-- name: DetachTicketMessagesFromConnector :execrows
UPDATE ticket_message SET connector_id = NULL WHERE connector_id = $1;

-- DeleteConnectorSyncState cascades the per-ticket sync bookkeeping for a connector.
-- name: DeleteConnectorSyncState :execrows
DELETE FROM connector_sync_state WHERE connector_id = $1;

-- DeleteConnectorWebhookDeliveries cascades the inbound webhook-dedup rows for a connector.
-- name: DeleteConnectorWebhookDeliveries :execrows
DELETE FROM connector_webhook_delivery WHERE connector_id = $1;

-- DeleteConnectorOutboundOps cascades the outbound op queue for a connector.
-- name: DeleteConnectorOutboundOps :execrows
DELETE FROM connector_outbound_op WHERE connector_id = $1;

-- DeleteConnectorRow removes the connector row, scoped to (id, business_id). Run AFTER the
-- detach + cascade (those clear FKs into connector) and BEFORE Vault.Delete (the connector
-- still references secret_ref until this runs).
-- name: DeleteConnectorRow :execrows
DELETE FROM connector WHERE id = sqlc.arg('id') AND business_id = sqlc.arg('business_id');
```

- [ ] **Step 2: Regenerate sqlc and build**

Run: `export PATH="$HOME/go/bin:$PATH" && make generate && go build ./...`
Expected: no errors. `make generate` adds `ListConnectors`, `ListConnectorHealth`, `GetConnectorHealth`, `UpdateConnector`, `UpdateConnectorSecretRef`, `DetachTicketsFromConnector`, `DetachTicketMessagesFromConnector`, `DeleteConnectorSyncState`, `DeleteConnectorWebhookDeliveries`, `DeleteConnectorOutboundOps`, `DeleteConnectorRow` to `dbgen`.

> If `make generate` errors that a column/enum/table doesn't exist (e.g. `connector_outbound_op.last_error`, `ticket_message.connector_id`, the `connector_status`/outbound-status enum names), open `db/schema.sql` and adjust the query to the real column/enum name. These are the only lookups that can fail here; everything else is mechanical.

- [ ] **Step 3: Commit**

```bash
git add db/query/connector_manage.sql internal/platform/db/dbgen
git commit -m "feat(connectors): sqlc queries for management CRUD + health + delete-detach"
```

---

## Task 3: Management service — types, `List`, `Get`

**Files:**
- Create: `internal/connectors/manage.go`
- Test: `internal/connectors/manage_integration_test.go` (started here)

- [ ] **Step 1: Write the failing integration test for List/Get**

`internal/connectors/manage_integration_test.go`:
```go
//go:build integration

package connectors

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// startConn / newConnService / seed are the shared connectors integration helpers
// (see internal/connectors/inbound_definer_integration_test.go). jiraInput() builds a
// valid CreateConnectorInput. If a helper name differs, align to the existing harness.

func TestManageListAndGet(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil) // nil Verifier = skip live verify

	id, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// List returns the connector with health, no credential.
	views, err := svc.List(ctx, seed.principalID, seed.businessID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 1 || views[0].ID != id.String() {
		t.Fatalf("list: want [%s], got %+v", id, views)
	}
	if views[0].Health.State != "healthy" {
		t.Fatalf("list: want health=healthy, got %q", views[0].Health.State)
	}

	// Get returns the same view.
	v, err := svc.Get(ctx, seed.principalID, seed.businessID, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.ID != id.String() || v.Type != "jira" {
		t.Fatalf("get: unexpected view %+v", v)
	}

	// Get with an unknown id → ErrNotFound (no oracle).
	if _, err := svc.Get(ctx, seed.principalID, seed.businessID, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("get unknown: want ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails (compile error: List/Get undefined)**

Run: `export PATH="$HOME/go/bin:$PATH" && go vet -tags integration ./internal/connectors/`
Expected: FAIL — `svc.List undefined`, `svc.Get undefined`, `ConnectorView` / `.Health` undefined.

- [ ] **Step 3: Implement types + List + Get in `manage.go`**

`internal/connectors/manage.go`:
```go
package connectors

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// ConnectorHealth is the moderate sync-health summary surfaced per connector.
type ConnectorHealth struct {
	State             string  `json:"state"` // healthy | degraded | disabled
	LinkedTicketCount int64   `json:"linked_ticket_count"`
	PendingOutboundOps int64  `json:"pending_outbound_ops"`
	FailedOutboundOps  int64  `json:"failed_outbound_ops"`
	LastError         *string `json:"last_error"`
}

// ConnectorView is a connector as returned to management callers. It deliberately carries
// NO credential fields (email/api_token/webhook_secret) — credentials are write-only.
type ConnectorView struct {
	ID                  string
	BusinessID          string
	Type                string
	DisplayName         string
	BaseURL             string
	AllowPrivateBaseURL bool
	Config              map[string]any
	Status              string
	LastReconciledAt    *string // RFC3339, nil if never reconciled
	CreatedAt           string
	UpdatedAt           string
	Health              ConnectorHealth
}

// healthState derives the rollup pill from status + failure counts. A disabled connector is
// "disabled" regardless of counts; any failed outbound op makes it "degraded"; else "healthy".
// Pending ops alone are normal queue depth, not degradation.
func healthState(status string, failedOps int64) string {
	switch {
	case status == "disabled":
		return "disabled"
	case failedOps > 0:
		return "degraded"
	default:
		return "healthy"
	}
}

// connectorToView maps a dbgen.Connector + its health aggregates into a ConnectorView.
func connectorToView(row dbgen.Connector, h ConnectorHealth) ConnectorView {
	var cfg map[string]any
	if len(row.Config) > 0 {
		_ = json.Unmarshal(row.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	var lastRec *string
	if row.LastReconciledAt.Valid {
		s := row.LastReconciledAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		lastRec = &s
	}
	h.State = healthState(row.Status, h.FailedOutboundOps)
	return ConnectorView{
		ID:                  row.ID.String(),
		BusinessID:          row.BusinessID.String(),
		Type:                string(row.Type),
		DisplayName:         row.DisplayName,
		BaseURL:             row.BaseUrl,
		AllowPrivateBaseURL: row.AllowPrivateBaseUrl,
		Config:              cfg,
		Status:              row.Status,
		LastReconciledAt:    lastRec,
		CreatedAt:           row.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:           row.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Health:              h,
	}
}

// List returns all connectors for a business with health, ordered by display name. RLS +
// the business_id predicate scope this to the caller's tenant.
func (s *Service) List(ctx context.Context, principalID, businessID uuid.UUID) ([]ConnectorView, error) {
	var rows []dbgen.Connector
	health := map[uuid.UUID]ConnectorHealth{}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		r, qerr := q.ListConnectors(ctx, businessID)
		if qerr != nil {
			return qerr
		}
		rows = r
		hr, herr := q.ListConnectorHealth(ctx, businessID)
		if herr != nil {
			return herr
		}
		for _, h := range hr {
			health[h.ConnectorID] = ConnectorHealth{
				LinkedTicketCount:  h.LinkedTicketCount,
				PendingOutboundOps: h.PendingOps,
				FailedOutboundOps:  h.FailedOps,
				LastError:          h.LastError,
			}
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]ConnectorView, 0, len(rows))
	for _, row := range rows {
		out = append(out, connectorToView(row, health[row.ID]))
	}
	return out, nil
}

// Get loads one connector with health by (id, business_id). Unknown/foreign id → ErrNotFound.
func (s *Service) Get(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (ConnectorView, error) {
	var row dbgen.Connector
	var h ConnectorHealth
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		r, qerr := q.GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID})
		if qerr != nil {
			return qerr
		}
		row = r
		hr, herr := q.GetConnectorHealth(ctx, connectorID)
		if herr != nil {
			return herr
		}
		h = ConnectorHealth{
			LinkedTicketCount:  hr.LinkedTicketCount,
			PendingOutboundOps: hr.PendingOps,
			FailedOutboundOps:  hr.FailedOps,
			LastError:          hr.LastError,
		}
		return nil
	})
	if err != nil {
		return ConnectorView{}, mapErr(err)
	}
	return connectorToView(row, h), nil
}

// auditConnector is a small helper for the management mutations (update/rotate/delete) to
// write a same-tx audit row with non-secret metadata only.
func auditConnector(ctx context.Context, tx pgx.Tx, businessID, principalID, connectorID uuid.UUID, action string, inputs map[string]any) error {
	tt := "connector"
	return audit.Write(ctx, tx, audit.Entry{
		BusinessID:       &businessID,
		ActorPrincipalID: &principalID,
		Action:           action,
		TargetType:       &tt,
		TargetID:         &connectorID,
		Inputs:           inputs,
	})
}
```

> The dbgen row field names `LinkedTicketCount`, `PendingOps`, `FailedOps`, `LastError`, `ConnectorID` come from the `ListConnectorHealth`/`GetConnectorHealth` result structs generated in Task 2; if sqlc named a column differently, match it. (`fmt` and `errs` are added to `manage.go`'s imports in Task 4, where `validateUpdate` first uses them.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test -tags integration -run TestManageListAndGet ./internal/connectors/`
Expected: PASS (Docker must be running for testcontainers).

- [ ] **Step 5: Commit**

```bash
git add internal/connectors/manage.go internal/connectors/manage_integration_test.go
git commit -m "feat(connectors): management service List + Get with sync health"
```

---

## Task 4: Management service — `Update` (PATCH)

**Files:**
- Modify: `internal/connectors/manage.go`
- Test: `internal/connectors/manage_integration_test.go`

- [ ] **Step 1: Write the failing test**

Append to `manage_integration_test.go`:
```go
func TestManageUpdate(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Rename + disable; config omitted (preserved).
	newName := "Acme Jira (prod)"
	disabled := "disabled"
	v, err := svc.Update(ctx, seed.principalID, seed.businessID, id, UpdateConnectorInput{
		DisplayName: &newName, Status: &disabled,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if v.DisplayName != newName || v.Status != "disabled" {
		t.Fatalf("update: got name=%q status=%q", v.DisplayName, v.Status)
	}
	if v.Health.State != "disabled" {
		t.Fatalf("update: want health=disabled, got %q", v.Health.State)
	}

	// Empty display_name → validation error, nothing persisted.
	empty := ""
	if _, err := svc.Update(ctx, seed.principalID, seed.businessID, id, UpdateConnectorInput{DisplayName: &empty}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("update empty name: want ErrValidation, got %v", err)
	}

	// Bad status value → validation error.
	bad := "paused"
	if _, err := svc.Update(ctx, seed.principalID, seed.businessID, id, UpdateConnectorInput{Status: &bad}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("update bad status: want ErrValidation, got %v", err)
	}

	// Unknown id → ErrNotFound.
	if _, err := svc.Update(ctx, seed.principalID, seed.businessID, uuid.New(), UpdateConnectorInput{DisplayName: &newName}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("update unknown: want ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH="$HOME/go/bin:$PATH" && go vet -tags integration ./internal/connectors/`
Expected: FAIL — `UpdateConnectorInput` / `svc.Update` undefined.

- [ ] **Step 3: Implement `UpdateConnectorInput` + `Update`**

Append to `internal/connectors/manage.go`:
```go
// UpdateConnectorInput is a partial (PATCH) update. nil fields are preserved. base_url and
// type are intentionally absent — they are immutable (identity). An empty non-nil config
// pointer replaces config with {}.
type UpdateConnectorInput struct {
	DisplayName *string
	Config      *map[string]any
	Status      *string // "enabled" | "disabled"
}

func validateUpdate(in UpdateConnectorInput) error {
	if in.DisplayName != nil && *in.DisplayName == "" {
		return fmt.Errorf("connectors: display_name cannot be empty: %w", errs.ErrValidation)
	}
	if in.Status != nil && *in.Status != "enabled" && *in.Status != "disabled" {
		return fmt.Errorf("connectors: status must be 'enabled' or 'disabled': %w", errs.ErrValidation)
	}
	return nil
}

// Update applies a partial change scoped to (id, business_id). Omitted fields preserved via
// COALESCE in SQL. No matching row → ErrNotFound (no oracle). Audited in the same tx.
func (s *Service) Update(ctx context.Context, principalID, businessID, connectorID uuid.UUID, in UpdateConnectorInput) (ConnectorView, error) {
	if err := validateUpdate(in); err != nil {
		return ConnectorView{}, err
	}
	params := dbgen.UpdateConnectorParams{ID: connectorID, BusinessID: businessID}
	params.DisplayName = in.DisplayName
	params.Status = in.Status
	if in.Config != nil {
		cfg := *in.Config
		if cfg == nil {
			cfg = map[string]any{}
		}
		b, merr := json.Marshal(cfg)
		if merr != nil {
			return ConnectorView{}, fmt.Errorf("connectors: marshal config: %w", errs.ErrValidation)
		}
		params.Config = b
	}
	var row dbgen.Connector
	var h ConnectorHealth
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		r, qerr := q.UpdateConnector(ctx, params)
		if qerr != nil {
			return qerr
		}
		row = r
		hr, herr := q.GetConnectorHealth(ctx, connectorID)
		if herr != nil {
			return herr
		}
		h = ConnectorHealth{LinkedTicketCount: hr.LinkedTicketCount, PendingOutboundOps: hr.PendingOps, FailedOutboundOps: hr.FailedOps, LastError: hr.LastError}
		return auditConnector(ctx, tx, businessID, principalID, connectorID, "connector.updated",
			map[string]any{"display_name_changed": in.DisplayName != nil, "config_changed": in.Config != nil, "status": in.Status})
	})
	if err != nil {
		return ConnectorView{}, mapErr(err)
	}
	return connectorToView(row, h), nil
}
```

> The dbgen `UpdateConnectorParams` field types for the COALESCE narg columns are `*string` (`DisplayName`, `Status`) and `[]byte` (`Config`). If sqlc generated different nullable wrappers (e.g. `pgtype.Text`), adapt the assignments — `go build` will tell you. Add `fmt` and `errs` to `manage.go`'s import block in this task — `validateUpdate` is the first code to use them.

- [ ] **Step 4: Run to verify it passes**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test -tags integration -run TestManageUpdate ./internal/connectors/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/connectors/manage.go internal/connectors/manage_integration_test.go
git commit -m "feat(connectors): management service Update (PATCH display_name/config/status)"
```

---

## Task 5: Management service — `RotateCredential`

**Files:**
- Modify: `internal/connectors/manage.go`
- Test: `internal/connectors/manage_integration_test.go`

- [ ] **Step 1: Write the failing test**

Append to `manage_integration_test.go`:
```go
func TestManageRotateCredential(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil) // nil Verifier: rotation skips live verify (mirrors Create)
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Capture the original secret_ref to prove it is swapped + the old secret deleted.
	oldRef := connectorSecretRef(t, ctx, tdb, seed.businessID, id)

	if err := svc.RotateCredential(ctx, seed.principalID, seed.businessID, id, RotateCredentialInput{
		Email: "rotated@acme.test", APIToken: "new-token-xyz", WebhookSecret: "new-webhook-secret",
	}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	newRef := connectorSecretRef(t, ctx, tdb, seed.businessID, id)
	if newRef == oldRef {
		t.Fatal("rotate: secret_ref was not swapped")
	}
	// Old secret row must be gone.
	if secretExists(t, ctx, tdb, oldRef) {
		t.Fatal("rotate: old secret was not deleted")
	}
	// The resolved credential must be the new one.
	rc, err := svc.Resolve(ctx, seed.principalID, seed.businessID, id)
	if err != nil {
		t.Fatalf("resolve after rotate: %v", err)
	}
	if rc.Credential.APIToken != "new-token-xyz" {
		t.Fatalf("rotate: resolved token = %q, want new-token-xyz", rc.Credential.APIToken)
	}

	// Empty api_token → validation, nothing changed.
	if err := svc.RotateCredential(ctx, seed.principalID, seed.businessID, id, RotateCredentialInput{Email: "x@y.z", APIToken: ""}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("rotate empty token: want ErrValidation, got %v", err)
	}

	// Unknown id → ErrNotFound.
	if err := svc.RotateCredential(ctx, seed.principalID, seed.businessID, uuid.New(), RotateCredentialInput{Email: "x@y.z", APIToken: "t"}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("rotate unknown: want ErrNotFound, got %v", err)
	}
}
```

Add these test helpers (use the integration harness's superuser handle `tdb.Super` for raw assertions; adjust to the real field name in the connectors harness):
```go
func connectorSecretRef(t *testing.T, ctx context.Context, tdb *testdb.DB, businessID, connectorID uuid.UUID) uuid.UUID {
	t.Helper()
	var ref uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT secret_ref FROM connector WHERE id=$1 AND business_id=$2", connectorID, businessID).Scan(&ref); err != nil {
		t.Fatalf("read secret_ref: %v", err)
	}
	return ref
}

func secretExists(t *testing.T, ctx context.Context, tdb *testdb.DB, secretID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM secret WHERE id=$1", secretID).Scan(&n); err != nil {
		t.Fatalf("count secret: %v", err)
	}
	return n > 0
}
```
> The integration test file will need `import ("context"; "github.com/manyforge/manyforge/internal/platform/db/testdb")`. Match `tdb`'s actual type + superuser-pool field to the existing connectors harness (the inbound DEFINER test uses `tdb.Super` / `tdb.App`).

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH="$HOME/go/bin:$PATH" && go vet -tags integration ./internal/connectors/`
Expected: FAIL — `RotateCredentialInput` / `svc.RotateCredential` undefined.

- [ ] **Step 3: Implement `RotateCredentialInput` + `RotateCredential`**

Append to `internal/connectors/manage.go` (add `"github.com/manyforge/manyforge/internal/platform/db/dbgen"` is already imported; ensure `errs` import remains):
```go
// RotateCredentialInput replaces the full sealed credential bundle. Partial (webhook-secret-only)
// rotation is intentionally unsupported (YAGNI) — callers always supply the complete bundle.
type RotateCredentialInput struct {
	Email         string
	APIToken      string
	WebhookSecret string
}

func validateRotate(in RotateCredentialInput) error {
	if in.Email == "" {
		return fmt.Errorf("connectors: email required: %w", errs.ErrValidation)
	}
	if in.APIToken == "" {
		return fmt.Errorf("connectors: api_token required: %w", errs.ErrValidation)
	}
	return nil
}

// RotateCredential seals a new credential bundle and atomically swaps the connector's
// secret_ref, deleting the old sealed secret — mirroring Create's seal/audit discipline.
// When a Verifier is wired, the NEW credential is live-verified BEFORE the tx; a credential
// that fails to authenticate is refused (400) and nothing is persisted. base_url/type are
// read from the existing connector (unchanged).
func (s *Service) RotateCredential(ctx context.Context, principalID, businessID, connectorID uuid.UUID, in RotateCredentialInput) error {
	if err := validateRotate(in); err != nil {
		return err
	}
	// Read connector metadata (and prove ownership) in a short tx; need base_url/type/flag for
	// verification and the old secret_ref for deletion.
	var meta dbgen.Connector
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID})
		meta = r
		return qerr
	}); err != nil {
		return mapErr(err)
	}
	// Live-verify the NEW credential before sealing (only when a Verifier is configured).
	if s.Verify != nil {
		if err := s.Verify.Verify(ctx, VerifyTarget{
			Type: string(meta.Type), BaseURL: meta.BaseUrl, AllowPrivateBaseURL: meta.AllowPrivateBaseUrl,
			Credential: Credential{Email: in.Email, APIToken: in.APIToken, WebhookSecret: in.WebhookSecret},
		}); err != nil {
			return fmt.Errorf("connectors: credential verification failed: %w", errs.ErrValidation)
		}
	}
	credBytes, err := json.Marshal(Credential{Email: in.Email, APIToken: in.APIToken, WebhookSecret: in.WebhookSecret})
	if err != nil {
		return fmt.Errorf("connectors: marshal credential: %w", err)
	}
	return mapErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		newRef, perr := s.Vault.Put(ctx, tx, businessID, "connector", credBytes)
		if perr != nil {
			return perr
		}
		n, uerr := dbgen.New(tx).UpdateConnectorSecretRef(ctx, dbgen.UpdateConnectorSecretRefParams{
			SecretRef: newRef, ID: connectorID, BusinessID: businessID,
		})
		if uerr != nil {
			return uerr
		}
		if n == 0 {
			return fmt.Errorf("connectors: not found: %w", errs.ErrNotFound)
		}
		if derr := s.Vault.Delete(ctx, tx, businessID, meta.SecretRef); derr != nil {
			return derr
		}
		return auditConnector(ctx, tx, businessID, principalID, connectorID, "connector.credential_rotated", map[string]any{})
	}))
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test -tags integration -run TestManageRotateCredential ./internal/connectors/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/connectors/manage.go internal/connectors/manage_integration_test.go
git commit -m "feat(connectors): management service RotateCredential (re-verify + atomic swap)"
```

---

## Task 6: Management service — `Test` connection

**Files:**
- Modify: `internal/connectors/manage.go`
- Test: `internal/connectors/manage_integration_test.go`

- [ ] **Step 1: Write the failing test**

Append to `manage_integration_test.go`:
```go
func TestManageTest(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	// With a stub Verifier that returns nil → ok=true.
	okSvc := newConnService(t, tdb, stubVerifier{})
	id, err := okSvc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := okSvc.Test(ctx, seed.principalID, seed.businessID, id)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if !res.OK {
		t.Fatalf("test: want ok=true, got %+v", res)
	}

	// Unknown id → ErrNotFound (the connector must resolve first).
	if _, err := okSvc.Test(ctx, seed.principalID, seed.businessID, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("test unknown: want ErrNotFound, got %v", err)
	}
}

type stubVerifier struct{}

func (stubVerifier) Verify(ctx context.Context, t VerifyTarget) error { return nil }
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH="$HOME/go/bin:$PATH" && go vet -tags integration ./internal/connectors/`
Expected: FAIL — `svc.Test` / `TestResult` undefined.

- [ ] **Step 3: Implement `TestResult` + `Test`**

Append to `internal/connectors/manage.go`:
```go
// TestResult reports a live connection test. Detail is a short, non-leaking status string
// (never the credential or an upstream response body).
type TestResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Test resolves the stored credential and runs a live verify against the external system.
// Unknown/foreign id → ErrNotFound. A configured-but-failing credential returns {ok:false}
// with a safe detail (HTTP 200 — a test result is not an API error). If no Verifier is wired
// (dev without the connector master key), returns {ok:false, detail:"verification unavailable"}.
func (s *Service) Test(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (TestResult, error) {
	rc, err := s.Resolve(ctx, principalID, businessID, connectorID)
	if err != nil {
		return TestResult{}, err // already mapped (ErrNotFound on unknown)
	}
	if s.Verify == nil {
		return TestResult{OK: false, Detail: "verification unavailable"}, nil
	}
	if verr := s.Verify.Verify(ctx, VerifyTarget{
		Type: rc.Type, BaseURL: rc.BaseURL, AllowPrivateBaseURL: rc.AllowPrivateBaseURL, Credential: rc.Credential,
	}); verr != nil {
		return TestResult{OK: false, Detail: "credential verification failed"}, nil
	}
	return TestResult{OK: true, Detail: "ok"}, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test -tags integration -run TestManageTest ./internal/connectors/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/connectors/manage.go internal/connectors/manage_integration_test.go
git commit -m "feat(connectors): management service Test (live connection check)"
```

---

## Task 7: Management service — `Delete` (detach + cascade)

**Files:**
- Modify: `internal/connectors/manage.go`
- Test: `internal/connectors/manage_integration_test.go`

- [ ] **Step 1: Write the failing test (delete preserves external_id; bookkeeping + secret gone)**

Append to `manage_integration_test.go`:
```go
func TestManageDeleteDetaches(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	oldRef := connectorSecretRef(t, ctx, tdb, seed.businessID, id)

	// Link a ticket to the connector (simulate a synced ticket) via the inbound DEFINER fn.
	externalID := "JIRA-77"
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			id, externalID, "https://acme.atlassian.net/browse/JIRA-77", "Linked issue",
			"open", "high", "reporter@example.com", "Reporter", timeMinus5(), []byte(`{"key":"JIRA-77"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("seed linked ticket: %v", err)
	}

	// Delete the connector.
	if err := svc.Delete(ctx, seed.principalID, seed.businessID, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Connector row gone.
	var connCount int
	tdb.Super.QueryRow(ctx, "SELECT count(*) FROM connector WHERE id=$1", id).Scan(&connCount)
	if connCount != 0 {
		t.Fatalf("delete: connector row still present")
	}
	// Secret deleted.
	if secretExists(t, ctx, tdb, oldRef) {
		t.Fatalf("delete: sealed secret not removed")
	}
	// Ticket SURVIVES, connector_id NULL, external_id/external_url PRESERVED.
	var connID *uuid.UUID
	var extID, extURL *string
	if err := tdb.Super.QueryRow(ctx, "SELECT connector_id, external_id, external_url FROM ticket WHERE id=$1", ticketID).Scan(&connID, &extID, &extURL); err != nil {
		t.Fatalf("read detached ticket: %v", err)
	}
	if connID != nil {
		t.Fatalf("delete: ticket.connector_id not nulled")
	}
	if extID == nil || *extID != externalID {
		t.Fatalf("delete: ticket.external_id not preserved, got %v", extID)
	}
	if extURL == nil || *extURL == "" {
		t.Fatalf("delete: ticket.external_url not preserved")
	}
	// Sync-state bookkeeping gone.
	var ssCount int
	tdb.Super.QueryRow(ctx, "SELECT count(*) FROM connector_sync_state WHERE ticket_id=$1", ticketID).Scan(&ssCount)
	if ssCount != 0 {
		t.Fatalf("delete: connector_sync_state not cascaded")
	}

	// Delete unknown id → ErrNotFound.
	if err := svc.Delete(ctx, seed.principalID, seed.businessID, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("delete unknown: want ErrNotFound, got %v", err)
	}
}

func timeMinus5() time.Time { return time.Now().UTC().Add(-5 * time.Minute) }
```
> Add `import "time"` to the integration test file. `syncIssueSQL` is declared in `inbound_definer_integration_test.go` (same package) — reuse it; do not redeclare.

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH="$HOME/go/bin:$PATH" && go vet -tags integration ./internal/connectors/`
Expected: FAIL — `svc.Delete` undefined.

- [ ] **Step 3: Implement `Delete`**

Append to `internal/connectors/manage.go`:
```go
// Delete is the terminal connector removal. In one tx it: confirms ownership (and reads
// secret_ref), detaches linked tickets + messages to native (NULL connector_id, PRESERVING
// external_id/external_url), cascade-deletes the sync/webhook/outbound bookkeeping, deletes
// the connector row, then deletes the sealed secret — and audits. Order matters: tickets and
// bookkeeping clear their FKs into connector BEFORE the connector row is deleted; the secret
// is deleted LAST (the connector references it until then). Unknown/foreign id → ErrNotFound.
func (s *Service) Delete(ctx context.Context, principalID, businessID, connectorID uuid.UUID) error {
	return mapErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, gerr := q.GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID})
		if gerr != nil {
			return gerr // pgx.ErrNoRows → ErrNotFound
		}
		// Capture the linked-ticket count for the audit before detaching.
		linked, herr := q.GetConnectorHealth(ctx, connectorID)
		if herr != nil {
			return herr
		}
		if _, derr := q.DetachTicketsFromConnector(ctx, connectorID); derr != nil {
			return derr
		}
		if _, derr := q.DetachTicketMessagesFromConnector(ctx, connectorID); derr != nil {
			return derr
		}
		if _, derr := q.DeleteConnectorSyncState(ctx, connectorID); derr != nil {
			return derr
		}
		if _, derr := q.DeleteConnectorWebhookDeliveries(ctx, connectorID); derr != nil {
			return derr
		}
		if _, derr := q.DeleteConnectorOutboundOps(ctx, connectorID); derr != nil {
			return derr
		}
		n, derr := q.DeleteConnectorRow(ctx, dbgen.DeleteConnectorRowParams{ID: connectorID, BusinessID: businessID})
		if derr != nil {
			return derr
		}
		if n == 0 {
			return fmt.Errorf("connectors: not found: %w", errs.ErrNotFound)
		}
		if verr := s.Vault.Delete(ctx, tx, businessID, row.SecretRef); verr != nil {
			return verr
		}
		return auditConnector(ctx, tx, businessID, principalID, connectorID, "connector.deleted",
			map[string]any{"type": string(row.Type), "base_url": row.BaseUrl, "detached_tickets": linked.LinkedTicketCount})
	}))
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test -tags integration -run TestManageDeleteDetaches ./internal/connectors/`
Expected: PASS.

- [ ] **Step 5: Run the full management integration suite**

Run: `export PATH="$HOME/go/bin:$PATH" && go test -tags integration -run TestManage ./internal/connectors/`
Expected: PASS (all `TestManage*`).

- [ ] **Step 6: Commit**

```bash
git add internal/connectors/manage.go internal/connectors/manage_integration_test.go
git commit -m "feat(connectors): management service Delete (detach tickets + cascade + drop secret)"
```

---

## Task 8: HTTP handler + DTOs + routes

**Files:**
- Create: `internal/connectors/handler.go`
- Test: `internal/connectors/handler_test.go`

- [ ] **Step 1: Write the failing unit test (credentials never echoed; bad UUID → 404)**

`internal/connectors/handler_test.go`:
```go
package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// fakeManageSvc implements manageCRUD for handler unit tests (no DB).
type fakeManageSvc struct {
	created    uuid.UUID
	createErr  error
	gotCreate  CreateConnectorInput
	listOut    []ConnectorView
	getOut     ConnectorView
	getErr     error
	updateOut  ConnectorView
	rotateErr  error
	testOut    TestResult
	deleteErr  error
}

func (f *fakeManageSvc) Create(_ context.Context, _, _ uuid.UUID, in CreateConnectorInput) (uuid.UUID, error) {
	f.gotCreate = in
	return f.created, f.createErr
}
func (f *fakeManageSvc) List(_ context.Context, _, _ uuid.UUID) ([]ConnectorView, error) {
	return f.listOut, nil
}
func (f *fakeManageSvc) Get(_ context.Context, _, _, _ uuid.UUID) (ConnectorView, error) {
	return f.getOut, f.getErr
}
func (f *fakeManageSvc) Update(_ context.Context, _, _, _ uuid.UUID, _ UpdateConnectorInput) (ConnectorView, error) {
	return f.updateOut, nil
}
func (f *fakeManageSvc) RotateCredential(_ context.Context, _, _, _ uuid.UUID, _ RotateCredentialInput) error {
	return f.rotateErr
}
func (f *fakeManageSvc) Test(_ context.Context, _, _, _ uuid.UUID) (TestResult, error) {
	return f.testOut, nil
}
func (f *fakeManageSvc) Delete(_ context.Context, _, _, _ uuid.UUID) error { return f.deleteErr }

// serveConn copies the auth-ring + serve harness from internal/agents/agent_handler_test.go
// (newAgentTestRing / serveAgent), adapted agent→connector. It mounts ProtectedRoutes behind
// httpx.RequireAuth with a valid bearer for `pid`, and injects the principal into context.
// (See that file for the ~40-line ring/bearer helper to copy verbatim.)

func TestGetConnectorNeverReturnsCredential(t *testing.T) {
	bid := uuid.New()
	view := ConnectorView{ID: uuid.New().String(), BusinessID: bid.String(), Type: "jira",
		DisplayName: "Acme", BaseURL: "https://acme.atlassian.net", Status: "enabled",
		Health: ConnectorHealth{State: "healthy"}}
	h := NewHandler(&fakeManageSvc{getOut: view})

	rr := serveConn(t, h, http.MethodGet, "/businesses/"+bid.String()+"/connectors/"+view.ID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, leak := range []string{"api_token", "webhook_secret", "secret_ref", "\"email\""} {
		if strings.Contains(body, leak) {
			t.Fatalf("response leaked %q: %s", leak, body)
		}
	}
}

func TestGetConnectorBadUUIDIs404(t *testing.T) {
	bid := uuid.New()
	h := NewHandler(&fakeManageSvc{getErr: errs.ErrNotFound})
	rr := serveConn(t, h, http.MethodGet, "/businesses/"+bid.String()+"/connectors/not-a-uuid", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestCreateConnectorReturns201(t *testing.T) {
	bid := uuid.New()
	newID := uuid.New()
	f := &fakeManageSvc{created: newID}
	h := NewHandler(f)
	body := `{"type":"jira","display_name":"Acme","base_url":"https://acme.atlassian.net","email":"a@b.c","api_token":"tok","webhook_secret":"whs"}`
	rr := serveConnBody(t, h, http.MethodPost, "/businesses/"+bid.String()+"/connectors", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	if f.gotCreate.APIToken != "tok" || f.gotCreate.Type != "jira" {
		t.Fatalf("service did not receive create input: %+v", f.gotCreate)
	}
	// Response body must not echo the token back.
	if strings.Contains(rr.Body.String(), "tok") {
		t.Fatalf("create response leaked api_token: %s", rr.Body.String())
	}
	_ = json.RawMessage(rr.Body.Bytes())
}
```
> Copy the `serveConn` / `serveConnBody` helpers and the ed25519 key-ring/bearer plumbing **verbatim** from `internal/agents/agent_handler_test.go` (`newAgentTestRing`, `serveAgent`), renaming `agent`→`connector`. They mount `h.ProtectedRoutes` behind `httpx.RequireAuth` and issue a valid bearer.

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH="$HOME/go/bin:$PATH" && go vet ./internal/connectors/`
Expected: FAIL — `NewHandler`, `Handler`, `serveConn`, `manageCRUD` undefined.

- [ ] **Step 3: Implement the handler**

`internal/connectors/handler.go`:
```go
package connectors

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// manageCRUD is the subset of *Service the handler needs (an interface so handler tests can
// inject a fake). *Service satisfies it.
type manageCRUD interface {
	Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateConnectorInput) (uuid.UUID, error)
	List(ctx context.Context, principalID, businessID uuid.UUID) ([]ConnectorView, error)
	Get(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (ConnectorView, error)
	Update(ctx context.Context, principalID, businessID, connectorID uuid.UUID, in UpdateConnectorInput) (ConnectorView, error)
	RotateCredential(ctx context.Context, principalID, businessID, connectorID uuid.UUID, in RotateCredentialInput) error
	Test(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (TestResult, error)
	Delete(ctx context.Context, principalID, businessID, connectorID uuid.UUID) error
}

var _ manageCRUD = (*Service)(nil)

// Handler exposes connector-management CRUD over HTTP, mounted behind the connectors.manage
// RequirePermission gate (so a lacking perm / invisible business is a no-oracle 404).
type Handler struct{ svc manageCRUD }

// NewHandler builds a connectors management HTTP handler.
func NewHandler(svc manageCRUD) *Handler { return &Handler{svc: svc} }

// ProtectedRoutes mounts authenticated connector endpoints under a business.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/connectors", func(r chi.Router) {
		r.Get("/", h.list)
		r.Post("/", h.create)
		r.Get("/{cid}", h.get)
		r.Patch("/{cid}", h.update)
		r.Put("/{cid}/credential", h.rotate)
		r.Post("/{cid}/test", h.test)
		r.Delete("/{cid}", h.delete)
	})
}

func connBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func connPathID(r *http.Request) (uuid.UUID, error)     { return uuid.Parse(chi.URLParam(r, "cid")) }

// healthResp / connectorResp are the OpenAPI wire shapes. connectorResp has NO credential
// fields — credentials are write-only by construction.
type healthResp struct {
	State              string  `json:"state"`
	LinkedTicketCount  int64   `json:"linked_ticket_count"`
	PendingOutboundOps int64   `json:"pending_outbound_ops"`
	FailedOutboundOps  int64   `json:"failed_outbound_ops"`
	LastError          *string `json:"last_error"`
}

type connectorResp struct {
	ID                  string         `json:"id"`
	BusinessID          string         `json:"business_id"`
	Type                string         `json:"type"`
	DisplayName         string         `json:"display_name"`
	BaseURL             string         `json:"base_url"`
	AllowPrivateBaseURL bool           `json:"allow_private_base_url"`
	Config              map[string]any `json:"config"`
	Status              string         `json:"status"`
	LastReconciledAt    *string        `json:"last_reconciled_at"`
	CreatedAt           string         `json:"created_at"`
	UpdatedAt           string         `json:"updated_at"`
	Health              healthResp     `json:"health"`
}

func toConnectorResp(v ConnectorView) connectorResp {
	cfg := v.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	return connectorResp{
		ID: v.ID, BusinessID: v.BusinessID, Type: v.Type, DisplayName: v.DisplayName,
		BaseURL: v.BaseURL, AllowPrivateBaseURL: v.AllowPrivateBaseURL, Config: cfg, Status: v.Status,
		LastReconciledAt: v.LastReconciledAt, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
		Health: healthResp{
			State: v.Health.State, LinkedTicketCount: v.Health.LinkedTicketCount,
			PendingOutboundOps: v.Health.PendingOutboundOps, FailedOutboundOps: v.Health.FailedOutboundOps,
			LastError: v.Health.LastError,
		},
	}
}

// ctxIDs extracts principal + business id, writing a 404 and returning ok=false on any miss
// (missing principal, malformed business UUID) — no oracle.
func (h *Handler) ctxIDs(w http.ResponseWriter, r *http.Request) (pid, bid uuid.UUID, ok bool) {
	pid, has := httpx.PrincipalFromContext(r.Context())
	if !has {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, uuid.Nil, false
	}
	bid, err := connBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, uuid.Nil, false
	}
	return pid, bid, true
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	views, err := h.svc.List(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]connectorResp, 0, len(views))
	for _, v := range views {
		out = append(out, toConnectorResp(v))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	var in struct {
		Type                string         `json:"type"`
		DisplayName         string         `json:"display_name"`
		BaseURL             string         `json:"base_url"`
		AllowPrivateBaseURL bool           `json:"allow_private_base_url"`
		Email               string         `json:"email"`
		APIToken            string         `json:"api_token"`
		WebhookSecret       string         `json:"webhook_secret"`
		Config              map[string]any `json:"config"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	id, err := h.svc.Create(r.Context(), pid, bid, CreateConnectorInput{
		Type: in.Type, DisplayName: in.DisplayName, BaseURL: in.BaseURL,
		AllowPrivateBaseURL: in.AllowPrivateBaseURL, Email: in.Email, APIToken: in.APIToken,
		WebhookSecret: in.WebhookSecret, Config: in.Config,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// Return the freshly-created connector view (no credential).
	v, gerr := h.svc.Get(r.Context(), pid, bid, id)
	if gerr != nil {
		httpx.WriteError(w, r, gerr)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toConnectorResp(v))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	v, err := h.svc.Get(r.Context(), pid, bid, cid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toConnectorResp(v))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		DisplayName *string         `json:"display_name"`
		Config      *map[string]any `json:"config"`
		Status      *string         `json:"status"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Update(r.Context(), pid, bid, cid, UpdateConnectorInput{
		DisplayName: in.DisplayName, Config: in.Config, Status: in.Status,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toConnectorResp(v))
}

func (h *Handler) rotate(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		Email         string `json:"email"`
		APIToken      string `json:"api_token"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.RotateCredential(r.Context(), pid, bid, cid, RotateCredentialInput{
		Email: in.Email, APIToken: in.APIToken, WebhookSecret: in.WebhookSecret,
	}); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	v, gerr := h.svc.Get(r.Context(), pid, bid, cid)
	if gerr != nil {
		httpx.WriteError(w, r, gerr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toConnectorResp(v))
}

func (h *Handler) test(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	res, err := h.svc.Test(r.Context(), pid, bid, cid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.Delete(r.Context(), pid, bid, cid); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `export PATH="$HOME/go/bin:$PATH" && go test ./internal/connectors/ -run 'TestGetConnector|TestCreateConnector'`
Expected: PASS (unit tests; no Docker needed).

- [ ] **Step 5: Commit**

```bash
git add internal/connectors/handler.go internal/connectors/handler_test.go
git commit -m "feat(connectors): HTTP handler (CRUD/rotate/test/delete), credentials never echoed"
```

---

## Task 9: Wire the handler into `main.go`

**Files:**
- Modify: `cmd/manyforge/main.go`

- [ ] **Step 1: Locate the existing connectors + agents wiring**

Run: `grep -n 'connectors\.\|agentsConfigure\|businessIDFromPath\|permResolve\|ProtectedRoutes' cmd/manyforge/main.go`
Note the existing `*connectors.Service` variable (built for the webhook handler / agent tools / registry — it already has `DB`, `Vault`, and possibly `Verify` wired). Reuse that exact variable; do **not** construct a second Service.

- [ ] **Step 2: Add the permission middleware (next to `agentsConfigure`)**

Find the struct/area where `agentsConfigure: httpx.RequirePermission(database, permResolve, "agents.configure", businessIDFromPath),` is defined and add alongside it:
```go
connectorsManage: httpx.RequirePermission(database, permResolve, "connectors.manage", businessIDFromPath),
```
Add the matching field to that handler-deps struct (mirror the `agentsConfigure` field declaration):
```go
connectorsManage func(http.Handler) http.Handler
```
And add the connectors HTTP handler next to `h.agents` (reusing the existing connectors Service variable — assume it is named `connectorSvc`; match the real name from Step 1):
```go
connectors *connectors.Handler
```
…initialized where the other handlers are built:
```go
connectors: connectors.NewHandler(connectorSvc),
```

- [ ] **Step 3: Mount the routes (mirror the agents group)**

Next to the agents `pr.Group(...)` block inside the `RequireAuth` group, add:
```go
// Connectors management slice: CRUD external connectors under a business, gated on
// connectors.manage (migration-0048 catalog). Same RLS-bound 404-on-lacking-perm semantics.
pr.Group(func(cg chi.Router) {
	cg.Use(h.connectorsManage)
	h.connectors.ProtectedRoutes(cg)
})
```

- [ ] **Step 4: Build and run the backend**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && set -a; . ./.air.env; set +a; ./tmp/manyforge & sleep 3; curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8081/healthz; pkill -f tmp/manyforge`
Expected: `200`. (If `air` is running, it auto-rebuilds; this is just a direct smoke check.)

- [ ] **Step 5: Commit**

```bash
git add cmd/manyforge/main.go
git commit -m "feat(connectors): mount management API behind connectors.manage"
```

---

## Task 10: OpenAPI contract

**Files:**
- Create: `specs/004-external-connectors/contracts/openapi.yaml`

- [ ] **Step 1: Write the contract**

`specs/004-external-connectors/contracts/openapi.yaml`:
```yaml
openapi: 3.1.0
info:
  title: ManyForge External Connectors API
  version: 0.1.0
  description: >
    Backend contract for connector management (Spec 004 / manyforge-4zs.3), layered on the
    tenant foundation (spec 001). Inherits every foundation convention: Bearer JWT auth, RLS
    tenant scoping, 404-for-unauthorized (no existence oracle), 400 VALIDATION with safe
    messages. Credentials (email/api_token/webhook_secret) are WRITE-ONLY: accepted on
    create/rotate, never returned. This file describes ONLY the connector-management endpoints;
    the inbound webhook endpoint and auth/account endpoints live in their own contracts.
servers:
  - url: /api/v1
security:
  - bearerAuth: []
tags:
  - name: Connectors
    description: Manage external ticketing connectors (Jira/Zendesk) for a business.
paths:
  /businesses/{id}/connectors:
    parameters:
      - $ref: '#/components/parameters/BusinessID'
    get:
      tags: [Connectors]
      operationId: listConnectors
      summary: List connectors for a business (with sync health)
      responses:
        '200':
          description: Connector list
          content:
            application/json:
              schema:
                type: object
                properties:
                  items:
                    type: array
                    items: { $ref: '#/components/schemas/Connector' }
        '404': { $ref: '#/components/responses/NotFound' }
    post:
      tags: [Connectors]
      operationId: createConnector
      summary: Connect a new external system (validates + live-verifies the credential)
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/CreateConnector' }
      responses:
        '201':
          description: Created connector (no credential)
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Connector' }
        '400': { $ref: '#/components/responses/ValidationError' }
        '404': { $ref: '#/components/responses/NotFound' }
        '409': { $ref: '#/components/responses/Conflict' }
  /businesses/{id}/connectors/{cid}:
    parameters:
      - $ref: '#/components/parameters/BusinessID'
      - $ref: '#/components/parameters/ConnectorID'
    get:
      tags: [Connectors]
      operationId: getConnector
      responses:
        '200':
          description: Connector
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Connector' }
        '404': { $ref: '#/components/responses/NotFound' }
    patch:
      tags: [Connectors]
      operationId: updateConnector
      summary: Edit display_name / config / status (base_url + type immutable)
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/UpdateConnector' }
      responses:
        '200':
          description: Updated connector
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Connector' }
        '400': { $ref: '#/components/responses/ValidationError' }
        '404': { $ref: '#/components/responses/NotFound' }
    delete:
      tags: [Connectors]
      operationId: deleteConnector
      summary: Terminal delete — detaches synced tickets to native, drops bookkeeping + secret
      responses:
        '204': { description: Deleted }
        '404': { $ref: '#/components/responses/NotFound' }
  /businesses/{id}/connectors/{cid}/credential:
    parameters:
      - $ref: '#/components/parameters/BusinessID'
      - $ref: '#/components/parameters/ConnectorID'
    put:
      tags: [Connectors]
      operationId: rotateConnectorCredential
      summary: Rotate the sealed credential (re-verified before sealing)
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/RotateCredential' }
      responses:
        '200':
          description: Connector after rotation (no credential)
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Connector' }
        '400': { $ref: '#/components/responses/ValidationError' }
        '404': { $ref: '#/components/responses/NotFound' }
  /businesses/{id}/connectors/{cid}/test:
    parameters:
      - $ref: '#/components/parameters/BusinessID'
      - $ref: '#/components/parameters/ConnectorID'
    post:
      tags: [Connectors]
      operationId: testConnector
      summary: Live-verify the stored credential
      responses:
        '200':
          description: Test result
          content:
            application/json:
              schema: { $ref: '#/components/schemas/TestResult' }
        '404': { $ref: '#/components/responses/NotFound' }
components:
  securitySchemes:
    bearerAuth: { type: http, scheme: bearer, bearerFormat: JWT }
  parameters:
    BusinessID:
      name: id
      in: path
      required: true
      schema: { type: string, format: uuid }
    ConnectorID:
      name: cid
      in: path
      required: true
      schema: { type: string, format: uuid }
  responses:
    NotFound:
      description: Not found (also returned for unauthorized — no existence oracle)
      content:
        application/json:
          schema: { $ref: '#/components/schemas/Error' }
    ValidationError:
      description: Invalid input (message is safe to surface)
      content:
        application/json:
          schema: { $ref: '#/components/schemas/Error' }
    Conflict:
      description: Duplicate (business, type, base_url) or concurrent mutation
      content:
        application/json:
          schema: { $ref: '#/components/schemas/Error' }
  schemas:
    Error:
      type: object
      properties:
        code: { type: string }
        message: { type: string }
      required: [code, message]
    Health:
      type: object
      properties:
        state: { type: string, enum: [healthy, degraded, disabled] }
        linked_ticket_count: { type: integer, format: int64 }
        pending_outbound_ops: { type: integer, format: int64 }
        failed_outbound_ops: { type: integer, format: int64 }
        last_error: { type: [string, 'null'] }
      required: [state, linked_ticket_count, pending_outbound_ops, failed_outbound_ops]
    Connector:
      type: object
      description: A connector. NEVER contains credential fields (write-only).
      properties:
        id: { type: string, format: uuid }
        business_id: { type: string, format: uuid }
        type: { type: string, enum: [jira, zendesk] }
        display_name: { type: string }
        base_url: { type: string, format: uri }
        allow_private_base_url: { type: boolean }
        config: { type: object, additionalProperties: true }
        status: { type: string, enum: [enabled, disabled] }
        last_reconciled_at: { type: [string, 'null'], format: date-time }
        created_at: { type: string, format: date-time }
        updated_at: { type: string, format: date-time }
        health: { $ref: '#/components/schemas/Health' }
      required: [id, business_id, type, display_name, base_url, status, health]
    CreateConnector:
      type: object
      properties:
        type: { type: string, enum: [jira, zendesk] }
        display_name: { type: string }
        base_url: { type: string, format: uri }
        allow_private_base_url: { type: boolean, default: false }
        email: { type: string }
        api_token: { type: string, description: Write-only; never returned. }
        webhook_secret: { type: string, description: Write-only; never returned. }
        config: { type: object, additionalProperties: true }
      required: [type, display_name, base_url, email, api_token]
    UpdateConnector:
      type: object
      description: Partial update. Omitted fields preserved. base_url + type are immutable.
      properties:
        display_name: { type: string }
        config: { type: object, additionalProperties: true }
        status: { type: string, enum: [enabled, disabled] }
    RotateCredential:
      type: object
      properties:
        email: { type: string }
        api_token: { type: string }
        webhook_secret: { type: string }
      required: [email, api_token]
    TestResult:
      type: object
      properties:
        ok: { type: boolean }
        detail: { type: string }
      required: [ok, detail]
```

- [ ] **Step 2: Validate the YAML parses**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('specs/004-external-connectors/contracts/openapi.yaml')); print('ok')"`
Expected: `ok`

- [ ] **Step 3: Commit**

```bash
git add specs/004-external-connectors/contracts/openapi.yaml
git commit -m "docs(connectors): OpenAPI contract for management API (establishes specs/004 contracts)"
```

---

## Task 11: Integration — permission matrix + RLS isolation

**Files:**
- Modify: `internal/connectors/manage_integration_test.go`

- [ ] **Step 1: Write the failing test (cross-tenant + perm-gate)**

Append to `manage_integration_test.go`:
```go
// TestManageCrossTenantIsolation: a connector created by tenant A is invisible to tenant B —
// Get/Update/Delete by B's principal all return ErrNotFound (RLS + business predicate).
func TestManageCrossTenantIsolation(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	idA, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create A: %v", err)
	}

	// seedOther builds a second tenant (principal + business). If the connectors harness lacks
	// it, mirror the multi-tenant seed used by the ticketing/agents integration tests.
	other := seedOther(t, ctx, tdb)

	if _, err := svc.Get(ctx, other.principalID, other.businessID, idA); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Get: want ErrNotFound, got %v", err)
	}
	name := "hijack"
	if _, err := svc.Update(ctx, other.principalID, other.businessID, idA, UpdateConnectorInput{DisplayName: &name}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Update: want ErrNotFound, got %v", err)
	}
	if err := svc.Delete(ctx, other.principalID, other.businessID, idA); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Delete: want ErrNotFound, got %v", err)
	}
}
```
> `seedOther` returns a struct with `principalID`/`businessID` for a different tenant. If the connectors harness doesn't expose it, add a small helper that inserts a second account+business+owner principal (copy the pattern from `internal/ticketing/permissions_integration_test.go` setup). The HTTP-level permission-matrix (owner/admin allowed; member/viewer → 404) is covered end-to-end in Task 12's `make sec-test` run via the `RequirePermission` gate; this task pins the service-layer RLS boundary.

- [ ] **Step 2: Run to verify it fails (then passes once `seedOther` exists)**

Run: `export PATH="$HOME/go/bin:$PATH" && go test -tags integration -run TestManageCrossTenantIsolation ./internal/connectors/`
Expected: PASS once `seedOther` is wired. If it fails because cross-tenant access is NOT blocked, that's a real RLS bug — stop and investigate (do not weaken the test).

- [ ] **Step 3: Commit**

```bash
git add internal/connectors/manage_integration_test.go
git commit -m "test(connectors): cross-tenant RLS isolation for management CRUD"
```

---

## Task 12: Security-regression pins

**Files:**
- Create: `internal/security_regression/connectors_manage_pin_test.go`

- [ ] **Step 1: Write the source-level + behavioral pins**

`internal/security_regression/connectors_manage_pin_test.go`:
```go
//go:build integration

// Package security_regression — connectors-management (manyforge-4zs.3 / Spec 004) merge-gate
// pins. Each test pins a security property of the connector-management API:
//
//   MF-4zs3-NO-CRED-IN-RESP   credentials are never present in any management response DTO.
//   MF-4zs3-DELETE-PRESERVES  hard-delete NULLs connector_id but PRESERVES external_id/url.
//   MF-4zs3-DETACH-SQL-PIN    source-level: the detach query NULLs connector_id, not external_id.
package security_regression

import (
	"os"
	"strings"
	"testing"
)

const (
	FindingNoCredInResp     = "MF-4zs3-NO-CRED-IN-RESP"
	FindingDeletePreserves  = "MF-4zs3-DELETE-PRESERVES"
	FindingDetachSQLPin     = "MF-4zs3-DETACH-SQL-PIN"
)

// TestDetachSQLPin is a source-level guard: a future refactor must not change the delete-detach
// to also null external_id/external_url (which would destroy reconnect history). We assert the
// DetachTicketsFromConnector query sets connector_id = NULL and does NOT mention external_id.
func TestDetachSQLPin(t *testing.T) {
	b, err := os.ReadFile("../../db/query/connector_manage.sql")
	if err != nil {
		t.Fatalf("%s: read query file: %v", FindingDetachSQLPin, err)
	}
	src := string(b)
	idx := strings.Index(src, "DetachTicketsFromConnector")
	if idx < 0 {
		t.Fatalf("%s: DetachTicketsFromConnector query not found", FindingDetachSQLPin)
	}
	// Inspect just the statement following the -- name: line.
	stmt := src[idx:]
	if end := strings.Index(stmt, ";"); end >= 0 {
		stmt = stmt[:end]
	}
	if !strings.Contains(stmt, "connector_id = NULL") {
		t.Fatalf("%s: detach must set connector_id = NULL; got:\n%s", FindingDetachSQLPin, stmt)
	}
	if strings.Contains(stmt, "external_id") || strings.Contains(stmt, "external_url") {
		t.Fatalf("%s: detach must NOT touch external_id/external_url (reconnect history); got:\n%s", FindingDetachSQLPin, stmt)
	}
}
```

- [ ] **Step 2: Add the behavioral no-credential-in-response pin**

Append to the same file (uses the connectors HTTP handler + a fake-free real service is heavy; pin at the DTO/JSON level by serializing a populated ConnectorView through the handler's response mapper). The simplest robust pin asserts the `connectorResp` JSON tags contain no credential fields via reflection:
```go
import (
	"reflect"

	"github.com/manyforge/manyforge/internal/connectors"
)

// TestNoCredentialFieldsInResponseType pins that the wire response type exposes NO credential
// field. This is a structural guard: even if a future edit adds email/api_token to ConnectorView,
// the response must not carry it. We reflect over the JSON-tag set of the exported view type.
func TestNoCredentialFieldsInResponseType(t *testing.T) {
	// ConnectorView is the service-layer view the handler serializes. Assert it has no
	// credential-bearing field name.
	forbidden := []string{"APIToken", "Email", "WebhookSecret", "Credential", "SecretRef"}
	tp := reflect.TypeOf(connectors.ConnectorView{})
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		for _, f := range forbidden {
			if name == f {
				t.Fatalf("%s: ConnectorView exposes credential field %q", FindingNoCredInResp, name)
			}
		}
	}
}
```
> The end-to-end "no token in HTTP body" assertion is already pinned at the unit level in `internal/connectors/handler_test.go` (`TestGetConnectorNeverReturnsCredential`); this structural pin is the merge-gate backstop. The `MF-4zs3-DELETE-PRESERVES` behavior is pinned by `TestManageDeleteDetaches` in the connectors package; reference it in the finding comment rather than duplicating the DB harness here.

- [ ] **Step 3: Run the security-regression suite**

Run: `export PATH="$HOME/go/bin:$PATH" && go test -tags integration -run 'TestDetachSQLPin|TestNoCredentialFieldsInResponseType' ./internal/security_regression/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/security_regression/connectors_manage_pin_test.go
git commit -m "test(security): connectors-management merge-gate pins (no-cred-in-resp, detach SQL)"
```

---

## Task 13: Full gate + close out

**Files:** none (verification only)

- [ ] **Step 1: Run the unit suite**

Run: `export PATH="$HOME/go/bin:$PATH" && make test`
Expected: PASS (all packages, including `internal/connectors` unit tests).

- [ ] **Step 2: Run the security/integration suite**

Run: `export PATH="$HOME/go/bin:$PATH" && make sec-test`
Expected: PASS (Docker running). Includes the new connectors pins.

- [ ] **Step 3: Run the connectors integration suite**

Run: `export PATH="$HOME/go/bin:$PATH" && go test -tags integration ./internal/connectors/`
Expected: PASS (all `TestManage*` + existing connector tests).

- [ ] **Step 4: Lint**

Run: `export PATH="$HOME/go/bin:$PATH" && make lint` (or `make check` if that is the combined target)
Expected: no findings.

- [ ] **Step 5: Update the bd issue + note follow-ups**

Run:
```bash
bd update manyforge-4zs.3 --notes "Backend management API landed (CRUD + rotate + test + delete-detach, connectors.manage perm 0048, OpenAPI specs/004). Frontend UI pending — see frontend plan."
```
File the deferred follow-up from the spec:
```bash
bd create --title="Connectors: auto-re-adopt detached tickets on reconnect" --type=feature --priority=3 --description="On creating a connector for a (business,type,base_url) that has orphaned tickets (connector_id NULL, external_id preserved), offer to relink by matching external_url host + external_id. Deferred from manyforge-4zs.3."
```

- [ ] **Step 6: Final commit (if any uncommitted gate fixes) + push**

```bash
git status   # expect clean apart from .beads/issues.jsonl
git push -u origin 4zs.3-connectors-management
```

---

## Notes for the implementer

- **Dev backend & DB:** start with `set -a; . ./.air.env; set +a; air`. DB is Colima Postgres on `:55432` (already migrated to 47; this plan adds 48). Backend serves `:8081`; frontend proxies `/api → :8081`.
- **`MANYFORGE_CONNECTOR_MASTER_KEY` is unset in dev**, so the real Jira/Zendesk verifier may be nil — `Create`/`Rotate`/`Test` then skip live verification gracefully (exactly as the existing `Create` does). Integration tests use a `nil` or `stubVerifier`, so they don't need the key. Manual end-to-end testing of a real Jira connect WILL need the key added to `.air.env`.
- **bd-journal gotcha:** `.beads/issues.jsonl` is auto-staged by bd tooling. Commit code with explicit `git add <paths>`; never `git add -A`. Commit bd-state changes deliberately.
- **Don't weaken a failing security/RLS test to make it green.** A red cross-tenant or no-credential pin is a real finding — investigate the code, not the test.
```
