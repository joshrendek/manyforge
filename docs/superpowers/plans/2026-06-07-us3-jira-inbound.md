# US3 — Jira Inbound Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Sync Jira issues + comments INTO native tickets, one-way (external→native), through a signed public webhook + a reconcile poller, with external-wins scalars and idempotent upserts — proving the US2 framework against a real connector.

**Architecture:** A Jira REST client (`internal/connectors/jira`) implements `TicketingConnector` behind an SSRF-safe `netsafe` client (HTTP Basic auth), tested with golden JSON fixtures. A public webhook handler verifies the per-connector HMAC secret, dedupes the delivery, and enqueues a `connector.inbound.sync` outbox event — all principal-less, so the lookup/dedupe/enqueue run through SECURITY DEFINER functions. The inbound-sync outbox subscriber fetches the issue via the Jira client and upserts requester+ticket+message+`connector_sync_state` through a DEFINER upsert function (the worker tx is principal-less). A reconcile poller periodically pulls issues updated since `connector.last_reconciled_at`. US3 also wires the first production `Service`/`Registry`/Jira-`Factory` in main.go.

**Tech Stack:** Go, pgx/v5, sqlc, PostgreSQL (RLS + SECURITY DEFINER fns), `net/http`+`netsafe`, chi, `httptest` golden fixtures, the `events` outbox, the US1/US2 `connectors` package.

**Spec:** `docs/superpowers/specs/2026-06-06-external-connectors-design.md` §5 (sync engine), §7 (pins). **Issue:** `manyforge-a7j.3`. **Branch:** `004-external-connectors`.

---

## CRITICAL design decisions (locked; baked into tasks)

1. **Principal-less writes → SECURITY DEFINER fns.** The outbox worker tx AND the public webhook handler have NO `manyforge.principal_id` GUC, so RLS-protected writes (`ticket`/`requester`/`ticket_message`/`connector_sync_state`/`connector_webhook_delivery`/`outbox`) must go through DEFINER functions (precedent: `ingest_inbound_message()` in `migrations/0024`, and `notify.SendSubscriber` calling `get_send_context()` DEFINER). Plain US2 `RecordWebhookDelivery` (RLS sqlc) is NOT usable principal-less — US3 adds DEFINER equivalents.
2. **`ticket.reply_token`** (NOT NULL, `UNIQUE(tenant_root_id, reply_token)`): connector tickets use synthetic `'conn:'||connector_id||':'||external_id`.
3. **Reconcile cursor:** new `connector.last_reconciled_at timestamptz NULL` (NULL = never → full initial pull). Stamp `now()` after a successful reconcile pass.
4. **Jira auth:** HTTP Basic `SetBasicAuth(cred.Email, cred.APIToken)`. Build query strings via `net/url.Values{}.Encode()` (never `fmt.Sprintf` user input into a URL).
5. **Webhook secret:** extend the sealed `Credential` struct with `WebhookSecret string` (`json:"webhook_secret,omitempty"`) + extend `CreateConnectorInput`/`Create` (US1 same package). The public handler gets the sealed blob via the `connector_webhook_context()` DEFINER and unseals it with the vault Sealer in Go to verify the signature.
6. **External-wins** scalars in the upsert DEFINER; **comments append-only** union deduped by `(connector_id, external_id)`. Status/priority best-effort mapping (refine later).

## File Structure

| File | Responsibility |
|------|----------------|
| `migrations/0042_connector_inbound.{up,down}.sql` | `connector.last_reconciled_at`; DEFINER fns `sync_inbound_external_issue`, `sync_inbound_external_comment`, `connector_webhook_context`, `ingest_connector_webhook`; reconcile queries |
| `db/schema.sql` + `db/query/connector.sql` (mod) | mirror column; sqlc wrappers calling the DEFINER fns + `ListConnectorsDueForReconcile`/`StampConnectorReconciled` |
| `internal/connectors/jira/client.go` + `jira/factory.go` | Jira REST client (TicketingConnector impl) + `NewFactory` |
| `internal/connectors/jira/testdata/*.json` + `jira/client_test.go` | golden fixtures + replay tests |
| `internal/connectors/types.go` + `credential.go`/`service.go` (mod) | `Credential.WebhookSecret` + `CreateConnectorInput.WebhookSecret` |
| `internal/connectors/webhook.go` + `webhook_handler_integration_test.go` | public signed webhook handler (DEFINER-backed verify+dedupe+enqueue) |
| `internal/connectors/inbound_sync.go` + `*_integration_test.go` | `InboundSyncSubscriber.Handle` (fetch + DEFINER upsert) |
| `internal/connectors/reconcile.go` + `*_integration_test.go` | reconcile poller |
| `cmd/manyforge/main.go` + `internal/platform/config/config.go` (mod) | `ConnectorMasterKey`, sealer→vault→Service→Registry→Jira factory→webhook route→subscriber→poller |
| `internal/security_regression/us3_*_pin_test.go` | webhook-sig, no-secret-in-logs, idempotency, external-wins, SSRF, tenant-isolation pins |

---

## Task 1: Migration 0042 — DEFINER sync functions + reconcile cursor + sqlc wrappers

**Files:** `migrations/0042_connector_inbound.{up,down}.sql`, `db/schema.sql`, `db/query/connector.sql`, generated `dbgen`.

> **Implementer MUST first verify, from the live schema:** the `outbox` table columns (`internal/platform/events/outbox.go` + its migration — likely `0016`), the `ticket_status` + `ticket_priority` enum values (`migrations/0013`), and the `ticket`/`ticket_message`/`requester` column lists. Adjust the DEFINER SQL below to match exactly.

- [ ] **Step 1: up migration** — `migrations/0042_connector_inbound.up.sql`:

```sql
-- 0042: Jira inbound (Spec 004 US3). Reconcile cursor + SECURITY DEFINER sync functions
-- (worker tx + public webhook are principal-less, so RLS-table writes go through DEFINER
-- fns, mirroring ingest_inbound_message). All fns SET search_path = public.

ALTER TABLE connector ADD COLUMN last_reconciled_at timestamptz NULL;

-- Public webhook lookup: returns the connector's tenancy + the SEALED credential blob
-- (ciphertext only) so the principal-less handler can unseal the webhook secret in Go.
CREATE FUNCTION connector_webhook_context(p_connector_id uuid)
RETURNS TABLE(business_id uuid, tenant_root_id uuid, ctype connector_type, sealed_secret text)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.business_id, c.tenant_root_id, c.type, s.sealed_value
    FROM connector c JOIN secret s ON s.id = c.secret_ref
    WHERE c.id = p_connector_id AND c.status = 'enabled';
$$;

-- Dedupe a verified webhook delivery AND enqueue the inbound-sync event atomically.
-- Returns true if newly accepted (enqueued), false on replay.
CREATE FUNCTION ingest_connector_webhook(
    p_connector_id uuid, p_business_id uuid, p_tenant_root uuid,
    p_delivery_id text, p_external_id text
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_rows int;
BEGIN
    INSERT INTO connector_webhook_delivery (business_id, tenant_root_id, connector_id, external_delivery_id)
    VALUES (p_business_id, p_tenant_root, p_connector_id, p_delivery_id)
    ON CONFLICT (connector_id, external_delivery_id) DO NOTHING;
    GET DIAGNOSTICS v_rows = ROW_COUNT;
    IF v_rows = 0 THEN
        RETURN false;  -- replay
    END IF;
    INSERT INTO outbox (tenant_root_id, topic, payload)
    VALUES (p_tenant_root, 'connector.inbound.sync',
            jsonb_build_object('connector_id', p_connector_id, 'external_id', p_external_id, 'business_id', p_business_id));
    RETURN true;
END;
$$;

-- External-wins upsert of requester+ticket+sync_state for one external issue. Returns ticket_id.
CREATE FUNCTION sync_inbound_external_issue(
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

-- Append-only inbound comment upsert (deduped by connector_id+external_id). Returns message id or NULL on dup.
CREATE FUNCTION sync_inbound_external_comment(
    p_ticket_id uuid, p_connector_id uuid, p_external_id text, p_body text
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_business_id uuid; v_tenant_root uuid; v_message_id uuid;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business_id, v_tenant_root FROM connector WHERE id = p_connector_id;
    IF v_business_id IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    INSERT INTO ticket_message (ticket_id, business_id, tenant_root_id, direction, author_principal_id,
                               message_id, body_text, connector_id, external_id)
    VALUES (p_ticket_id, v_business_id, v_tenant_root, 'inbound', NULL,
            'conn:' || p_connector_id::text || ':' || p_external_id,
            COALESCE(NULLIF(p_body,''),'(empty)'), p_connector_id, p_external_id)
    ON CONFLICT (connector_id, external_id) WHERE connector_id IS NOT NULL DO NOTHING
    RETURNING id INTO v_message_id;

    IF v_message_id IS NOT NULL THEN
        UPDATE ticket SET last_message_at = now(), updated_at = now() WHERE id = p_ticket_id;
    END IF;
    RETURN v_message_id;
END;
$$;

GRANT EXECUTE ON FUNCTION connector_webhook_context(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION ingest_connector_webhook(uuid,uuid,uuid,text,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION sync_inbound_external_issue(uuid,text,text,text,text,text,citext,text,timestamptz,jsonb) TO manyforge_app;
GRANT EXECUTE ON FUNCTION sync_inbound_external_comment(uuid,uuid,text,text) TO manyforge_app;
```

- [ ] **Step 2: down migration** — drop the 4 functions (with full arg signatures) + `ALTER TABLE connector DROP COLUMN last_reconciled_at`.
- [ ] **Step 3: schema.sql** — add `last_reconciled_at timestamptz` to the `connector` block (sqlc needs the column for codegen; the DEFINER fns are not in schema.sql).
- [ ] **Step 4: sqlc wrappers** in `db/query/connector.sql` — thin queries calling the DEFINER fns + the reconcile cursor queries:

```sql
-- name: ConnectorWebhookContext :one
SELECT * FROM connector_webhook_context($1);

-- name: IngestConnectorWebhook :one
SELECT ingest_connector_webhook($1, $2, $3, $4, $5);

-- name: SyncInboundExternalIssue :one
SELECT sync_inbound_external_issue($1,$2,$3,$4,$5,$6,$7,$8,$9,$10);

-- name: SyncInboundExternalComment :one
SELECT sync_inbound_external_comment($1,$2,$3,$4);

-- name: ListConnectorsDueForReconcile :many
SELECT id, business_id, tenant_root_id, type, last_reconciled_at
FROM connector WHERE status = 'enabled'
  AND (last_reconciled_at IS NULL OR last_reconciled_at < now() - $1::interval);

-- name: StampConnectorReconciled :exec
UPDATE connector SET last_reconciled_at = now(), updated_at = now() WHERE id = $1;
```

- [ ] **Step 5:** `make generate`; report the generated param/return types for the 6 queries. **Step 6:** migrate up/down round-trip (dev DB or skip). **Step 7:** `go build ./...`. **Step 8:** integration test `migrations_0042_integration_test.go` (or in connectors): call `SyncInboundExternalIssue` twice (insert then external-wins update) via `tdb.App.WithTx` (principal-less) asserting one ticket, status updated; `SyncInboundExternalComment` twice asserting one message (append-only dedupe). **Step 9:** commit `--no-verify`.

---

## Task 2: Jira REST client + Factory + golden fixtures

**Files:** `internal/connectors/jira/client.go`, `jira/factory.go`, `jira/testdata/{issue,comments,search_updated}.json`, `jira/client_test.go`.

Implement the full `connectors.TicketingConnector` (US3 tests the inbound methods; US4 tests `PostComment`/`TransitionStatus`). Model request/response on `internal/platform/ai/openaicompat.go:171-232` (build req → `SetBasicAuth(email, token)` → `httpClient.Do` → `io.LimitReader` 8MiB → non-2xx → sentinel that NEVER leaks the body). Jira Cloud endpoints: `GET /rest/api/3/issue/{key}?fields=summary,status,priority,reporter,updated`, `GET /rest/api/3/issue/{key}/comment`, `GET /rest/api/3/search?jql=...&fields=updated` (JQL `updated >= "<ts>" ORDER BY updated ASC`). Webhook: `VerifyHmac` over the body using the per-connector secret injected by the Factory; `DecodeWebhook` parses Jira's webhook JSON → `{DeliveryID, ExternalID, Kind}`.

- [ ] **Steps:** golden fixtures (record real Jira JSON shapes); `client_test.go` replays via `httptest.NewServer` + `os.ReadFile` (mirror `internal/platform/ai/openaicompat_test.go:160-198`), asserting path + Basic auth header + parsed `ExternalIssue`/`[]string`; a no-secret-in-error pin (`!strings.Contains(err.Error(), token)`). `factory.go`: `NewFactory(timeout) connectors.Factory` building `netsafe.NewClientWithOptions(timeout, netsafe.Options{AllowLoopback: rc.AllowPrivateBaseURL, AllowPrivate: rc.AllowPrivateBaseURL})` + a `*client` bound to `rc.BaseURL`/`rc.Credential`. `go test ./internal/connectors/jira/`, `go build`, commit.

---

## Task 3: Public signed webhook handler (principal-less, DEFINER-backed)

**Files:** `internal/connectors/webhook.go`, `webhook_handler_integration_test.go`; modify `types.go`/`credential.go`/`service.go` for `Credential.WebhookSecret` + `CreateConnectorInput.WebhookSecret`.

Flow (mirror `internal/inbox/handler.go:81-170` ORDER): route `POST /connectors/jira/{connectorID}/webhook` (public group, `h.ingestLimit`). (1) body cap `http.MaxBytesReader`→413; (2) parse `connectorID`; (3) `ConnectorWebhookContext(connectorID)` DEFINER → business/tenant/type/sealedSecret (unknown/disabled → still return uniform **202** no-oracle, do nothing); (4) unseal sealedSecret via the vault Sealer → `Credential.WebhookSecret`; (5) build the typed connector via Registry/Factory (or just verify with the secret) → `VerifyWebhook(r.Header, body)` → on fail return **202** (no oracle — do NOT 401-distinguish a real vs bogus connector; OR 401 only on a present-but-bad sig — match spec §5.1: forged/missing→401 but unknown connector→202; pick: verify only when connector resolved, else 202); (6) `DecodeWebhook(body)` → `{deliveryID, externalID}`; (7) `IngestConnectorWebhook(connectorID, business, tenant, deliveryID, externalID)` DEFINER (dedupe+enqueue) in one `database.WithTx`; (8) uniform **202**.

- [ ] **Steps:** extend `Credential`+`CreateConnectorInput`+`Create` (seal WebhookSecret in the credential JSON; existing creds without it unseal to empty — fine). Tests: valid sig → 202 + delivery row + outbox event; forged/missing sig → no delivery row/no event; replay (same delivery) → 202 + single event; unknown connectorID → 202, no rows; body over cap → 413. `go build`, commit.

---

## Task 4: Inbound-sync subscriber (fetch + DEFINER upsert)

**Files:** `internal/connectors/inbound_sync.go`, `inbound_sync_integration_test.go`.

`InboundSyncSubscriber{DB *db.DB; Registry *Registry; Logger}` with `Handle(ctx, tx pgx.Tx, e events.Event) error` (mirror `internal/agents/trigger.go:45`). Decode payload `{connector_id, external_id, business_id}`. Build the Jira client for the connector (the Registry needs a credential resolve — but the worker is principal-less; use a **system resolve** path: a DEFINER/`ConnectorWebhookContext`-style lookup of the sealed credential + Go unseal, then `Factory(rc)`, since `Service.Resolve` needs a principal). Call `client.FetchIssue(ctx, externalID)` → `ExternalIssue`. Apply via DEFINER fns using the worker `tx`: `tx.QueryRow("SELECT sync_inbound_external_issue(...)")` → ticket_id; then per comment `tx.QueryRow("SELECT sync_inbound_external_comment(...)")`. Snapshot = the external scalars (JSON). Errors → return (reschedule); poison payload → log + return nil.

- [ ] **Steps:** test (integration): seed a connector (via `Service.Create` with a fake-registered factory returning a canned `ExternalIssue`), enqueue an inbound-sync event, run the subscriber's `Handle` in a principal-less `tx`, assert a native ticket+message+sync_state created with external-wins scalars; re-run → idempotent (no dup ticket/message). `go build`, commit.

> Resolving the Jira client principal-less is the subtlety: add a `Registry.ResolveSystem(ctx, tx, connectorID)` that uses `ConnectorWebhookContext` + Go-unseal (no principal) rather than `Service.Resolve`. Implement it in this task.

---

## Task 5: Reconcile poller

**Files:** `internal/connectors/reconcile.go`, `reconcile_integration_test.go`.

`Reconciler{DB, Registry, Logger, Every time.Duration}` with `Run(ctx)` (mirror the approval-sweep ticker `cmd/manyforge/main.go:448-467`). Each tick: `ListConnectorsDueForReconcile(interval)` → for each, system-resolve the client, `client.ListUpdatedSince(last_reconciled_at)` → enqueue a `connector.inbound.sync` event per changed external id (via `ingest`/Enqueue in a tx) → `StampConnectorReconciled(id)`. NULL `last_reconciled_at` → full pull (pass zero time).

- [ ] **Steps:** test the single-pass method (factor `reconcileOnce(ctx)` out of the ticker) against a fake connector returning 2 updated ids → asserts 2 inbound-sync outbox events enqueued + `last_reconciled_at` stamped. `go build`, commit.

---

## Task 6: main.go wiring (first production Service/Registry/Factory)

**Files:** `internal/platform/config/config.go`, `cmd/manyforge/main.go`.

Add `ConnectorMasterKey []byte` (config.go, via `envKey32("MANYFORGE_CONNECTOR_MASTER_KEY")` — mirror `MCPMasterKey` at config.go:66/175). In main.go (mirror the MCP sealer block at :227-238): build `connSealer`, `connSvc := &connectors.Service{DB: database, Vault: secrets.NewVault(connSealer)}`, `connReg := connectors.NewRegistry(connSvc)`, `connReg.Register("jira", jira.NewFactory(60*time.Second))`, the webhook handler (mount in the public ingress group at :618-624 with `h.ingestLimit`), `eventBus.Subscribe(connectors.TopicConnectorInboundSync, inboundSyncSub.Handle)` (before `go outboxWorker.Run` at :441), and `go reconciler.Run(workerCtx)` (after the worker). Warn-if-unset for the master key (don't fatal).

- [ ] **Steps:** `go build ./...`; a `cmd/manyforge` smoke/contract test that the routes mount + subscriber registers (mirror existing main wiring tests if present); `make contract-test`. commit.

---

## Task 7: Security-regression pins

**Files:** `internal/security_regression/us3_jira_inbound_pin_test.go` (+ split files per finding if large).

Pins (spec §7): (1) **webhook signature** — forged/missing sig writes no delivery + no event; (2) **no-secret-in-logs** — Jira client error + webhook handler never contain the api_token/webhook_secret; (3) **sync idempotency** — re-delivered webhook → single ticket/message; (4) **external-wins determinism** — external scalar change reflected, snapshot updated; (5) **SSRF** — Jira client refuses a metadata-IP base_url (and on-prem only via trust flag); (6) **tenant isolation** — a webhook for connector A cannot write into business B (the DEFINER fns derive business from the connector row, not the caller).

- [ ] **Steps:** TDD each pin; reuse `seedAgentTenant`/`seedConnectorTenant` patterns; `go test -tags integration ./internal/security_regression/ -run TestUS3`; commit.

---

## Task 8: Full gate + close

- [ ] `export PATH=$PATH:$HOME/go/bin`; `gofmt -l internal/ cmd/ db/` empty; `make test && make contract-test && make lint && make sec-test && make int-test` (int-test backgrounded). Fix any failure. Final whole-US3 review subagent (opus). `bd close manyforge-a7j.3`; file follow-ups (e.g. status/priority mapping refinement; webhook-secret rotation); commit `.beads/issues.jsonl`; `git push`; update HANDOFF.md.

## Deferred to US4 (do NOT build here)
Outbound: native reply→Jira comment, `PostComment`/`TransitionStatus` exercised, the `connector.outbound.sync` subscriber, conflict finalization. (The Jira client's `PostComment`/`TransitionStatus` are implemented in T2 but tested + wired in US4.)

## Self-Review
- **Spec coverage (US3):** Jira client+fixtures ✅ T2; signed webhook ✅ T3; inbound external-wins upsert + snapshot ✅ T1(fns)+T4; reconcile poll ✅ T5; pins ✅ T7; first wiring ✅ T6. Principal-less constraint handled via DEFINER fns (T1) everywhere a principal-less context writes (T3 webhook, T4 subscriber).
- **Placeholders:** the DEFINER SQL + Jira endpoints are concrete; T1's implementer verifies outbox/enum/column specifics against the live schema before finalizing (flagged).
- **Type consistency:** `sync_inbound_external_issue`/`_comment`/`ingest_connector_webhook`/`connector_webhook_context` names consistent across T1 SQL ↔ T3/T4 Go callers; `Credential.WebhookSecret` consistent T3; `ResolveSystem` introduced T4 used by T4/T5.
