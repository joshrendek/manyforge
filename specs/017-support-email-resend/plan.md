# Support Email via Resend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A business with a verified Resend sending profile receives support mail on a subdomain it owns (`support@support.garden.gg`), its agents' replies leave through that same Resend profile with a plus-addressed reply token, and the customer's reply threads back onto the same ticket.

**Architecture:** The existing per-profile Resend webhook (spec 013) gains the `email.received` event. On a signed `email.received`, `mailing.WebhookHandler` fetches the message metadata and the raw RFC 822 bytes from Resend's Receiving API with the profile's own credential, then hands the raw bytes to `inbox.Service.ReceiveResend`. That function picks a recipient from the raw `To`/`Cc` headers, accepts only custom addresses on verified domains owned by the profile's business, and runs the existing ingest pipeline with source `inbox:resend`. The webhook event id is recorded only after ingest succeeds. On the outbound side, `notify.SendSubscriber` asks a new DEFINER for the business's Resend support identity. When one exists, the reply goes through `mailing.Service.SendSupportReply` with no fallback to the shared transport, and the final failed attempt marks the reply `failed`. Hardening rides along: production refuses to boot without a reply-token secret, a disallowed attachment is dropped and recorded instead of failing the whole message, and the Helm chart wires the reply-token secret and `MANYFORGE_BLOB_URL`.

**Tech Stack:** Go 1.x (chi, pgx v5, sqlc v1.27.0, enmime v2, testcontainers Postgres), PostgreSQL SECURITY DEFINER functions, Helm 3, Angular (signals, vitest unit tests via `@angular/build:unit-test`), Playwright, the SDK generator in `tools/sdk`.

**Spec:** `specs/017-support-email-resend/spec.md` and `specs/017-support-email-resend/data-model.md`. Read both before starting any task.

## Global Constraints

- One branch only: all work lands on `feat/support-email-resend`. Do not create, switch, or stack branches. Do not open a second PR.
- Migration number is `0139` (`migrations/0139_support_email_resend.{up,down}.sql`). No other migration is added by this plan.
- Never hand-edit generated sqlc output in `internal/platform/db/dbgen/`. Regenerate with sqlc **v1.27.0 exactly**. Check `sqlc version` first. If it does not print `v1.27.0`, run `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0 generate` instead of `make generate` (any other version rewrites every generated file).
- Service-layer errors use the typed sentinels in `internal/platform/errs` (`ErrNotFound`, `ErrValidation`, `ErrConflict`). Handlers never echo `err.Error()` to a client.
- Ownership and same-business predicates live in SQL (`WHERE business_id = $1 ...`), never only in Go.
- Resend response bodies and signed raw-download URLs are never returned to a client and never logged above debug. `provider.HTTPError` carries only the status and error name.
- Every new SECURITY DEFINER function has a pinned `search_path`, `REVOKE ALL ... FROM PUBLIC`, and `GRANT EXECUTE ... TO manyforge_app`. Support-side functions use `SET search_path = public` so `citext` comparisons stay case-insensitive (matching migrations 0014 and 0023). Mailing-side functions use `SET search_path = pg_catalog` with `public.`-qualified names (matching 0134 and its pins in `internal/security_regression/mailing_pins_test.go`).
- Shell safety: no `rm -rf`. Use targeted deletes, `>|` to overwrite logs (the user's zsh has `noclobber`), and kill only PIDs you started.
- Commits use conventional-commit subjects and carry **no `Co-Authored-By` trailer** (the user's global instructions override the harness default).
- Integration tests need Docker. The default colima profile is broken. Run `colima start --profile mftest` once, then prefix integration commands with `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock`.
- Run Playwright with `E2E_PORT=4311` so it never collides with the user's own dev server on `:4300`.
- A PR that touches `sdk/` must carry a `feat(sdk):` line in its body.

## Review Focus

1. **HTML-only customer mail** (Outlook and many phone clients send no `text/plain` part): the ticket must show a readable body, not "(no text body)". Test: `TestReceiveResendHTMLOnlyMessageHasReadableBody` (Task 5).
2. **A `To` header with display names and several recipients across owned and foreign domains, and a Bcc'd support address** (no owned address in `To`/`Cc` at all): the message must ingest exactly once into the profile's business, and a recipient owned by a different business must never route. Tests: `TestReceiveResendRoutesOwnedRecipientAmongDisplayNamesAndForeignAddresses` and `TestReceiveResendFallsBackToEnvelopeForBcc` (Task 5).
3. **Resend redelivering after a partial failure**, where ingest committed but the delivery row was never recorded (crash, or a record error after ingest): the redelivery must not create a second ticket or a second message. Test: `TestResendLoopRedeliveryAfterPartialFailureCreatesNoDuplicate` (Task 9).
4. **A huge raw message, or a huge JSON metadata response, including a chunked body with no `Content-Length`**: it must be acknowledged without ingesting so Resend stops retrying, and memory stays bounded by the cap. Tests: `TestResendDownloadReceivedRawCapsAndNeverSendsAPIKey` and `TestResendGetReceivedEmailClassifiesErrors` (Task 4), plus `TestResendReceivedOversizedRawIsAcknowledged` (Task 6).
5. **The profile credential rotated mid-flight** (API key replaced, or the webhook re-provisioned with a new signing secret): inbound fetches and outbound replies must use the current key, and events signed with the retired webhook secret must be rejected. Tests: `TestResendReceivedUsesCurrentCredentialAfterRotation` (Task 6) and `TestSendSupportReplyUsesRotatedCredential` (Task 7).

## Decisions This Plan Makes Beyond the Data Model

The controller should accept or reject these before execution starts:

- **Extra DEFINERs in 0139.** The data model says the `email.received` delivery row is recorded "through its existing DEFINER". That DEFINER (`mailing_process_provider_webhook`, 0134) only accepts `delivered`/`bounce`/`complaint` events with 1-50 normalized recipients, and `mailing_record_webhook` was dropped in 0134. So 0139 adds `mailing_received_webhook_state` and `mailing_record_received_webhook`. It also adds `set_message_dropped_attachments` (a principal-less ingest cannot `UPDATE` the RLS-protected `ticket_message`), `support_audit_inbound_dropped` (the R4 audit entry), and `mailing_list_verified_resend_profiles` (the R1 upgrade sweep).
- **`resolve_inbound_address_for_business` returns `(inbound_address_id, business_id, tenant_root_id, email_domain_id)`** instead of the bare id, so it plugs into the existing `route` struct.
- **`get_resend_support_identity(p_business_id, p_tenant_root_id)` also returns `profile_status`, and it returns a row for a `resend` profile in any status.** If it required a verified profile, as the data model says, then a profile that drops to `error` would return no row, the reply would silently fall back to the shared transport, and R11 would be violated. The extra tenant argument matches `get_send_identity`.
- **R1 "no operator action".** The reconcile path in `EnsureWebhook` only runs when someone clicks Verify on the sending profile. This plan adds a boot-time sweep (`WebhookHandler.UpgradeReceivingEvents`) that adds `email.received` to each verified profile's webhook with `PATCH /webhooks/{id}`, without rotating anything.
- **R3 Bcc fallback.** When no `To`/`Cc` header address is owned by the business, the recipient is taken from the authenticated Receiving API's `received_for`/`to` (not from the webhook payload), still restricted by `resolve_inbound_address_for_business`. Without this, a customer who Bcc's support is silently dropped.
- **Attachments over the size cap** are dropped and recorded the same way as disallowed types. They used to fail the whole message the same way, and the spec's motivation for R15 applies equally.

## Resend API Facts (checked 2026-10-10)

Confirmed in Resend's docs:
- `GET /emails/receiving/{id}` returns `id`, `to[]`, `cc[]`, `received_for[]`, `message_id`, `html`, `text`, `headers`, `attachments[]`, and `raw: {download_url, expires_at}`. `download_url` is a pre-signed CloudFront URL for the original message, attachments included. An optional `html_format` query (`data_uri` default, or `cid`) controls inline images.
- The `email.received` webhook `data` carries `email_id`, `created_at`, `from`, `to[]`, `cc[]`, `bcc[]`, `received_for[]`, `message_id`, `subject`, and `attachments[]` metadata. There is no body.
- `PATCH /webhooks/{webhook_id}` takes optional `endpoint`, `events[]`, and `status`, and returns `{object, id}`.
- `GET /domains/{id}` returns `capabilities.{sending,receiving}` and a `records[]` array (`record`, `name`, `type`, `ttl`, `status`, `value`, `priority` for MX).
- Resend's receiving MX must be the lowest-priority MX on its name. Resend recommends a subdomain when the apex already has MX.

Assumed, and verified by tests against the fake plus the live hub check in the runbook:
- `PATCH /webhooks/{id}` keeps the signing secret. The code re-reads the webhook after a PATCH and refuses to proceed if the secret changed.
- A `PATCH` with only `events` leaves `endpoint` and `status` unchanged.
- Resend's DKIM record name is `resend._domainkey.<domain>`, and its SPF TXT on `send.<domain>` is `v=spf1 include:amazonses.com ~all`. The UI says "copy from Resend" for every value it cannot know.
- Whether the webhook `to` keeps `+token` tags. The design does not depend on it. The runbook records what was observed.

---
### Task 1: Migration 0139, sqlc regeneration, and the global domain conflict

**Files:**
- Create: `migrations/0139_support_email_resend.up.sql`
- Create: `migrations/0139_support_email_resend.down.sql`
- Create: `migrations/support_email_resend_test.go` (integration)
- Modify: `db/schema.sql` (header comment line 4; `ticket_message` table ~:237-261; add two partial unique indexes)
- Regenerate: `internal/platform/db/dbgen/models.go` and any query file whose rows are `ticket_message` (sqlc v1.27.0)
- Modify: `internal/ticketing/identity.go` (`CreateEmailDomain` return ~:238, `dkimStateForList` ~:298, `VerifyEmailDomain` ~:367)
- Modify: `internal/ticketing/identity_integration_test.go` (append two tests)
- Modify: `api/openapi.yaml` (`/api/v1/businesses/{id}/email-domains/{did}/verify` post responses ~:1353)
- Modify: `specs/002-support-desk/contracts/openapi.yaml` (verify op responses)
- Modify: `cmd/manyforge/drift_002_test.go:85` (require 409 on verify)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces (SQL, all callable by `manyforge_app`):
  - `resolve_inbound_address_for_business(p_business_id uuid, p_address citext) RETURNS TABLE(inbound_address_id uuid, business_id uuid, tenant_root_id uuid, email_domain_id uuid)`
  - `get_resend_support_identity(p_business_id uuid, p_tenant_root_id uuid) RETURNS TABLE(profile_id uuid, from_address text, profile_status text)`
  - `set_message_dropped_attachments(p_message_id uuid, p_tenant_root_id uuid, p_dropped jsonb) RETURNS boolean`
  - `support_audit_inbound_dropped(p_business_id uuid, p_source text, p_reason text, p_event_id text) RETURNS boolean`. `p_reason` must be `no_owned_recipient` or `route_lost`.
  - `mailing_received_webhook_state(p_profile_id uuid, p_event_id text) RETURNS text`, one of `new`, `seen`, `ignored`
  - `mailing_record_received_webhook(p_profile_id uuid, p_event_id text) RETURNS boolean`
  - `mailing_list_verified_resend_profiles(p_limit integer) RETURNS TABLE(profile_id uuid)`
  - Column `ticket_message.dropped_attachments jsonb NOT NULL DEFAULT '[]'`; Go field `dbgen.TicketMessage.DroppedAttachments []byte`
  - `IdentityService.VerifyEmailDomain` returns an error wrapping `errs.ErrConflict` (HTTP 409) when another business already verified the domain.

- [ ] **Step 1: Write the failing migration test**

Create `migrations/support_email_resend_test.go`:

```go
//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func readMigration(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func seedResendBusiness(ctx context.Context, t *testing.T, database *testdb.TestDB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	for _, q := range []string{
		`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'Biz','active',now(),now())`,
		`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
	} {
		if _, err := database.Super.Exec(ctx, q, id); err != nil {
			t.Fatalf("seed business: %v", err)
		}
	}
	return id
}

func seedResendDomain(ctx context.Context, t *testing.T, database *testdb.TestDB, businessID uuid.UUID, domain, mode string, verified bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var verifiedAt any
	if verified {
		verifiedAt = time.Now().UTC()
	}
	if _, err := database.Super.Exec(ctx, `INSERT INTO email_domain
		(id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,dkim_selector,dkim_public_key,dkim_private_key_ref,spf_state,created_at,updated_at)
		VALUES ($1,$2,$2,$3,$4::email_domain_mode,'mf-verify=seed',$5,'mfseed','cHVi','cmVm','unknown',now(),now())`,
		id, businessID, domain, mode, verifiedAt); err != nil {
		t.Fatalf("seed email_domain: %v", err)
	}
	return id
}

func seedResendAddress(ctx context.Context, t *testing.T, database *testdb.TestDB, businessID, domainID uuid.UUID, address string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := database.Super.Exec(ctx, `INSERT INTO inbound_address
		(id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		VALUES ($1,$2,$2,$3,'custom',$4,now(),now())`, id, businessID, address, domainID); err != nil {
		t.Fatalf("seed inbound_address: %v", err)
	}
	return id
}

func seedResendProfile(ctx context.Context, t *testing.T, database *testdb.TestDB, businessID uuid.UUID, fromEmail, status string) uuid.UUID {
	t.Helper()
	secretID, profileID := uuid.New(), uuid.New()
	if _, err := database.Super.Exec(ctx, `INSERT INTO secret (id,business_id,tenant_root_id,scope,sealed_value,created_at,updated_at)
		VALUES ($1,$2,$2,'mailing','sealed',now(),now())`, secretID, businessID); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := database.Super.Exec(ctx, `INSERT INTO mailing_sending_profile
		(id,business_id,tenant_root_id,mode,from_email,from_name,secret_ref,status,feedback_status)
		VALUES ($1,$2,$2,'resend',$3,'Support',$4,$5,'ready')`,
		profileID, businessID, fromEmail, secretID, status); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	return profileID
}

func TestSupportEmailResendMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	if _, err := database.Super.Exec(ctx, readMigration(t, "0139_support_email_resend.down.sql")); err != nil {
		t.Fatalf("0139 down: %v", err)
	}
	assertSupportEmailResendObjects(t, ctx, database, false)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0139_support_email_resend.up.sql")); err != nil {
		t.Fatalf("0139 up: %v", err)
	}
	assertSupportEmailResendObjects(t, ctx, database, true)
}

func assertSupportEmailResendObjects(t *testing.T, ctx context.Context, database *testdb.TestDB, want bool) {
	t.Helper()
	checks := map[string]string{
		"verified domain index": `SELECT to_regclass('email_domain_verified_global_uq') IS NOT NULL`,
		"custom address index":  `SELECT to_regclass('inbound_address_custom_global_uq') IS NOT NULL`,
		"dropped column": `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_name='ticket_message' AND column_name='dropped_attachments')`,
		"resolve for business": `SELECT to_regprocedure('resolve_inbound_address_for_business(uuid,citext)') IS NOT NULL`,
		"resend identity":      `SELECT to_regprocedure('get_resend_support_identity(uuid,uuid)') IS NOT NULL`,
		"dropped setter":       `SELECT to_regprocedure('set_message_dropped_attachments(uuid,uuid,jsonb)') IS NOT NULL`,
		"drop audit":           `SELECT to_regprocedure('support_audit_inbound_dropped(uuid,text,text,text)') IS NOT NULL`,
		"received state":       `SELECT to_regprocedure('mailing_received_webhook_state(uuid,text)') IS NOT NULL`,
		"received record":      `SELECT to_regprocedure('mailing_record_received_webhook(uuid,text)') IS NOT NULL`,
		"profile sweep list":   `SELECT to_regprocedure('mailing_list_verified_resend_profiles(integer)') IS NOT NULL`,
	}
	for name, q := range checks {
		var got bool
		if err := database.Super.QueryRow(ctx, q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s present = %v, want %v", name, got, want)
		}
	}
}

func TestSupportEmailResendMigrationRefusesSharedVerifiedDomain(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0139_support_email_resend.down.sql")); err != nil {
		t.Fatalf("0139 down: %v", err)
	}
	a, b := seedResendBusiness(ctx, t, database), seedResendBusiness(ctx, t, database)
	seedResendDomain(ctx, t, database, a, "support.shared.example", "provider_route", true)
	seedResendDomain(ctx, t, database, b, "support.shared.example", "provider_route", true)
	_, err = database.Super.Exec(ctx, readMigration(t, "0139_support_email_resend.up.sql"))
	if err == nil || !strings.Contains(err.Error(), "0139: a verified email domain is shared") {
		t.Fatalf("0139 up over a shared verified domain error = %v, want the loud precheck failure", err)
	}
}

func TestSupportEmailResendDefiners(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	own, other := seedResendBusiness(ctx, t, database), seedResendBusiness(ctx, t, database)
	ownDomain := seedResendDomain(ctx, t, database, own, "support.own.example", "provider_route", true)
	ownAddr := seedResendAddress(ctx, t, database, own, ownDomain, "support@support.own.example")
	otherDomain := seedResendDomain(ctx, t, database, other, "support.other.example", "provider_route", true)
	seedResendAddress(ctx, t, database, other, otherDomain, "support@support.other.example")
	pendingDomain := seedResendDomain(ctx, t, database, own, "pending.own.example", "provider_route", false)
	seedResendAddress(ctx, t, database, own, pendingDomain, "support@pending.own.example")

	resolve := func(business uuid.UUID, address string) (uuid.UUID, bool) {
		t.Helper()
		var id, biz, root uuid.UUID
		var domain *uuid.UUID
		err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT inbound_address_id, business_id, tenant_root_id, email_domain_id
				FROM resolve_inbound_address_for_business($1, $2)`, business, address).Scan(&id, &biz, &root, &domain)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false
		}
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return id, true
	}
	if id, ok := resolve(own, "SUPPORT@Support.Own.Example"); !ok || id != ownAddr {
		t.Fatalf("own mixed-case address resolved = %v/%v, want %v", id, ok, ownAddr)
	}
	if _, ok := resolve(own, "support@support.other.example"); ok {
		t.Fatal("another business's address resolved for the receiving business")
	}
	if _, ok := resolve(own, "support@pending.own.example"); ok {
		t.Fatal("an address on an unverified domain resolved")
	}

	// Global uniqueness: a second verified row for the same domain fails with 23505.
	_, err = database.Super.Exec(ctx, `INSERT INTO email_domain
		(id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,spf_state,created_at,updated_at)
		VALUES ($1,$2,$2,'support.own.example','provider_route','mf-verify=x',now(),'unknown',now(),now())`,
		uuid.New(), other)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("second verified domain insert error = %v, want unique violation", err)
	}

	// get_resend_support_identity: no profile -> no row; error-status profile -> row with its status.
	identity := func() (uuid.UUID, string, string, bool) {
		t.Helper()
		var profile uuid.UUID
		var from, status string
		err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT profile_id, from_address, profile_status
				FROM get_resend_support_identity($1, $1)`, own).Scan(&profile, &from, &status)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, "", "", false
		}
		if err != nil {
			t.Fatalf("identity: %v", err)
		}
		return profile, from, status, true
	}
	if _, _, _, ok := identity(); ok {
		t.Fatal("identity returned a row without any Resend profile")
	}
	profileID := seedResendProfile(ctx, t, database, own, "support@support.own.example", "error")
	if got, from, status, ok := identity(); !ok || got != profileID || from != "support@support.own.example" || status != "error" {
		t.Fatalf("identity = %v %q %q %v, want the error-status profile so the sender refuses instead of falling back", got, from, status, ok)
	}

	// Received-webhook state machine.
	state := func() string {
		t.Helper()
		var s string
		if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mailing_received_webhook_state($1, 'evt-1')`, profileID).Scan(&s)
		}); err != nil {
			t.Fatalf("state: %v", err)
		}
		return s
	}
	if got := state(); got != "ignored" {
		t.Fatalf("state for an error-status profile = %q, want ignored", got)
	}
	if _, err := database.Super.Exec(ctx, `UPDATE mailing_sending_profile SET status='verified' WHERE id=$1`, profileID); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "new" {
		t.Fatalf("state before record = %q, want new", got)
	}
	for i := 0; i < 2; i++ {
		if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
			var ok bool
			return tx.QueryRow(ctx, `SELECT mailing_record_received_webhook($1, 'evt-1')`, profileID).Scan(&ok)
		}); err != nil {
			t.Fatalf("record #%d: %v", i, err)
		}
	}
	if got := state(); got != "seen" {
		t.Fatalf("state after record = %q, want seen", got)
	}
	var rows int
	if err := database.Super.QueryRow(ctx, `SELECT count(*) FROM mailing_provider_webhook_delivery
		WHERE profile_id=$1 AND external_event_id='evt-1' AND processing_status='applied'`, profileID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("recorded rows = %d, want exactly 1", rows)
	}

	// Sweep listing returns the verified Resend profile.
	var listed int
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM mailing_list_verified_resend_profiles(10) WHERE profile_id=$1`, profileID).Scan(&listed)
	}); err != nil {
		t.Fatal(err)
	}
	if listed != 1 {
		t.Fatalf("sweep listed %d, want 1", listed)
	}

	// Drop audit writes one business-scoped audit_entry.
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
		var ok bool
		return tx.QueryRow(ctx, `SELECT support_audit_inbound_dropped($1,'inbox:resend','no_owned_recipient','evt-9')`, own).Scan(&ok)
	}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	var audits int
	if err := database.Super.QueryRow(ctx, `SELECT count(*) FROM audit_entry
		WHERE business_id=$1 AND action='ticket.inbound_dropped' AND inputs->>'event_id'='evt-9'`, own).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("drop audits = %d, want 1", audits)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `colima start --profile mftest` (once per machine boot), then
`DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestSupportEmailResend' ./migrations/`
Expected: FAIL with `open 0139_support_email_resend.down.sql: no such file or directory`.

- [ ] **Step 3: Write the up migration**

Create `migrations/0139_support_email_resend.up.sql`:

```sql
-- 0139: Support email via Resend (spec 017).
--
-- Verified email domains and custom inbound addresses become globally unique,
-- inbound messages record dropped attachments, and the Resend inbound/outbound
-- path gets the SECURITY DEFINER functions it needs from principal-less
-- contexts (webhook ingress, outbox worker).

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM email_domain
        WHERE verified_at IS NOT NULL
        GROUP BY domain HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION '0139: a verified email domain is shared by more than one business; resolve it manually before migrating';
    END IF;
    IF EXISTS (
        SELECT 1 FROM inbound_address
        WHERE kind = 'custom'
        GROUP BY address HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION '0139: a custom inbound address exists in more than one tenant; resolve it manually before migrating';
    END IF;
END;
$$;

CREATE UNIQUE INDEX email_domain_verified_global_uq
    ON email_domain (domain)
    WHERE verified_at IS NOT NULL;

CREATE UNIQUE INDEX inbound_address_custom_global_uq
    ON inbound_address (address)
    WHERE kind = 'custom';

ALTER TABLE ticket_message
    ADD COLUMN dropped_attachments jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD CONSTRAINT ticket_message_dropped_attachments_chk CHECK (
        jsonb_typeof(dropped_attachments) = 'array'
        AND jsonb_array_length(dropped_attachments) <= 50
    );

-- Same-business routing for Resend-received mail (R4): only a custom address on a
-- verified domain owned by p_business_id matches. citext keeps it case-insensitive.
CREATE FUNCTION resolve_inbound_address_for_business(p_business_id uuid, p_address citext)
RETURNS TABLE (inbound_address_id uuid, business_id uuid, tenant_root_id uuid, email_domain_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT ia.id, ia.business_id, ia.tenant_root_id, ia.email_domain_id
    FROM inbound_address ia
    JOIN email_domain ed
      ON ed.id = ia.email_domain_id
     AND ed.tenant_root_id = ia.tenant_root_id
    WHERE ia.business_id = p_business_id
      AND ed.business_id = p_business_id
      AND ia.kind = 'custom'
      AND ia.address = p_address
      AND ed.verified_at IS NOT NULL
    LIMIT 1;
$$;

-- Outbound identity for support replies (R10-R12). Returns a row whenever the
-- business has a verified provider_route domain with a custom address AND a
-- resend-mode profile in ANY status, so an unverified profile makes the caller
-- refuse to send rather than fall back to the shared transport (R11).
CREATE FUNCTION get_resend_support_identity(p_business_id uuid, p_tenant_root_id uuid)
RETURNS TABLE (profile_id uuid, from_address text, profile_status text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT p.id, ia.address::text, p.status
    FROM mailing_sending_profile p
    JOIN email_domain ed
      ON ed.business_id = p.business_id
     AND ed.tenant_root_id = p.tenant_root_id
    JOIN inbound_address ia
      ON ia.email_domain_id = ed.id
     AND ia.tenant_root_id = ed.tenant_root_id
     AND ia.business_id = p.business_id
     AND ia.kind = 'custom'
    WHERE p.business_id = p_business_id
      AND p.tenant_root_id = p_tenant_root_id
      AND p.mode = 'resend'
      AND ed.mode = 'provider_route'
      AND ed.verified_at IS NOT NULL
    ORDER BY ia.created_at ASC, ia.id ASC
    LIMIT 1;
$$;

-- R15: record attachments that were not stored on the inbound message. Called in
-- the principal-less ingest transaction right after ingest_inbound_message.
CREATE FUNCTION set_message_dropped_attachments(p_message_id uuid, p_tenant_root_id uuid, p_dropped jsonb)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    IF p_dropped IS NULL OR jsonb_typeof(p_dropped) <> 'array'
       OR jsonb_array_length(p_dropped) > 50 THEN
        RAISE EXCEPTION 'set_message_dropped_attachments: invalid dropped list';
    END IF;
    UPDATE ticket_message
       SET dropped_attachments = p_dropped
     WHERE id = p_message_id
       AND tenant_root_id = p_tenant_root_id
       AND direction = 'inbound'
       AND tenant_merge_root_write_allowed(tenant_root_id);
    RETURN FOUND;
END;
$$;

-- R4: audit a Resend-received message dropped because no recipient is owned by
-- the receiving business. No recipient address is stored (no PII in audit).
CREATE FUNCTION support_audit_inbound_dropped(p_business_id uuid, p_source text, p_reason text, p_event_id text)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_tenant uuid;
BEGIN
    IF p_reason NOT IN ('no_owned_recipient', 'route_lost')
       OR p_source IS NULL OR length(p_source) > 64
       OR length(COALESCE(p_event_id, '')) > 500 THEN
        RAISE EXCEPTION 'support_audit_inbound_dropped: invalid arguments';
    END IF;
    SELECT tenant_root_id INTO v_tenant FROM business WHERE id = p_business_id;
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    INSERT INTO audit_entry (id, business_id, tenant_root_id, actor_principal_id, action,
                             target_type, target_id, inputs)
    VALUES (gen_random_uuid(), p_business_id, v_tenant, NULL, 'ticket.inbound_dropped',
            'business', p_business_id,
            jsonb_build_object('source', p_source, 'reason', p_reason, 'event_id', p_event_id));
    RETURN true;
END;
$$;

-- R5: email.received idempotency. 'ignored' for an unknown/unverified/fenced
-- profile or a non-operational business; 'seen' once recorded; else 'new'.
CREATE FUNCTION mailing_received_webhook_state(p_profile_id uuid, p_event_id text)
RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    v_profile public.mailing_sending_profile%ROWTYPE;
BEGIN
    IF p_event_id IS NULL OR btrim(p_event_id) = '' OR length(p_event_id) > 500 THEN
        RETURN 'ignored';
    END IF;
    SELECT * INTO v_profile
    FROM public.mailing_sending_profile p
    WHERE p.id = p_profile_id
      AND p.mode::text = 'resend'
      AND p.status = 'verified'
      AND public.tenant_merge_root_write_allowed(p.tenant_root_id);
    IF NOT FOUND
       OR NOT public.mailing_business_operational(v_profile.business_id, v_profile.tenant_root_id) THEN
        RETURN 'ignored';
    END IF;
    IF EXISTS (
        SELECT 1 FROM public.mailing_provider_webhook_delivery w
        WHERE w.profile_id = p_profile_id
          AND w.external_event_id = p_event_id
          AND w.expires_at > clock_timestamp()
    ) THEN
        RETURN 'seen';
    END IF;
    RETURN 'new';
END;
$$;

-- R5: record an email.received event as processed. Called only after the fetch
-- and ingest succeeded (or after a deliberate acknowledge-without-ingest).
CREATE FUNCTION mailing_record_received_webhook(p_profile_id uuid, p_event_id text)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    v_profile public.mailing_sending_profile%ROWTYPE;
    v_now timestamptz := clock_timestamp();
BEGIN
    IF p_event_id IS NULL OR btrim(p_event_id) = '' OR length(p_event_id) > 500 THEN
        RETURN false;
    END IF;
    SELECT * INTO v_profile
    FROM public.mailing_sending_profile p
    WHERE p.id = p_profile_id
      AND p.mode::text = 'resend'
      AND public.tenant_merge_root_write_allowed(p.tenant_root_id);
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    DELETE FROM public.mailing_provider_webhook_delivery
    WHERE profile_id = p_profile_id
      AND external_event_id = p_event_id
      AND expires_at <= v_now;
    INSERT INTO public.mailing_provider_webhook_delivery (
        business_id, tenant_root_id, profile_id, provider, external_event_id,
        envelope_payload, normalized_events, processing_status, expires_at, applied_at
    ) VALUES (
        v_profile.business_id, v_profile.tenant_root_id, v_profile.id, 'resend', p_event_id,
        '{}'::jsonb, '[]'::jsonb, 'applied', v_now + interval '7 days', v_now
    ) ON CONFLICT (profile_id, external_event_id) DO NOTHING;
    RETURN true;
END;
$$;

-- R1: bounded list of verified Resend profiles for the boot-time webhook sweep.
CREATE FUNCTION mailing_list_verified_resend_profiles(p_limit integer)
RETURNS TABLE (profile_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT p.id
    FROM public.mailing_sending_profile p
    WHERE p.mode::text = 'resend'
      AND p.status = 'verified'
      AND public.tenant_merge_root_write_allowed(p.tenant_root_id)
    ORDER BY p.id
    LIMIT GREATEST(0, LEAST(COALESCE(p_limit, 0), 1000));
$$;

REVOKE ALL ON FUNCTION resolve_inbound_address_for_business(uuid, citext) FROM PUBLIC;
REVOKE ALL ON FUNCTION get_resend_support_identity(uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION set_message_dropped_attachments(uuid, uuid, jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION support_audit_inbound_dropped(uuid, text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_received_webhook_state(uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_record_received_webhook(uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_list_verified_resend_profiles(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION resolve_inbound_address_for_business(uuid, citext) TO manyforge_app;
GRANT EXECUTE ON FUNCTION get_resend_support_identity(uuid, uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION set_message_dropped_attachments(uuid, uuid, jsonb) TO manyforge_app;
GRANT EXECUTE ON FUNCTION support_audit_inbound_dropped(uuid, text, text, text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_received_webhook_state(uuid, text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_record_received_webhook(uuid, text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_list_verified_resend_profiles(integer) TO manyforge_app;
```

- [ ] **Step 4: Write the down migration**

Create `migrations/0139_support_email_resend.down.sql`:

```sql
DROP FUNCTION IF EXISTS mailing_list_verified_resend_profiles(integer);
DROP FUNCTION IF EXISTS mailing_record_received_webhook(uuid, text);
DROP FUNCTION IF EXISTS mailing_received_webhook_state(uuid, text);
DROP FUNCTION IF EXISTS support_audit_inbound_dropped(uuid, text, text, text);
DROP FUNCTION IF EXISTS set_message_dropped_attachments(uuid, uuid, jsonb);
DROP FUNCTION IF EXISTS get_resend_support_identity(uuid, uuid);
DROP FUNCTION IF EXISTS resolve_inbound_address_for_business(uuid, citext);
ALTER TABLE ticket_message
    DROP CONSTRAINT IF EXISTS ticket_message_dropped_attachments_chk,
    DROP COLUMN IF EXISTS dropped_attachments;
DROP INDEX IF EXISTS inbound_address_custom_global_uq;
DROP INDEX IF EXISTS email_domain_verified_global_uq;
```

- [ ] **Step 5: Run the migration tests to verify they pass**

Run: `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestSupportEmailResend' ./migrations/`
Expected: PASS (3 tests). If `resolve(own, "SUPPORT@Support.Own.Example")` fails, check that the function uses `SET search_path = public`. Under `pg_catalog` the `citext` operator is not found, and the comparison silently falls back to case-sensitive `text =`.

- [ ] **Step 6: Mirror the column into the sqlc schema and regenerate**

In `db/schema.sql`, change line 4 to `-- Keep this in sync with migrations/0001..0139 when table shapes change.`, then add the column as the last column line of `CREATE TABLE ticket_message` (immediately after `external_id         text,`):

```sql
    dropped_attachments jsonb NOT NULL DEFAULT '[]'::jsonb,
```

After the existing `CREATE UNIQUE INDEX ticket_message_external_idx ...` line, add:

```sql
CREATE UNIQUE INDEX email_domain_verified_global_uq ON email_domain (domain) WHERE verified_at IS NOT NULL;
CREATE UNIQUE INDEX inbound_address_custom_global_uq ON inbound_address (address) WHERE kind = 'custom';
```

Run: `sqlc version` and confirm `v1.27.0`, then `make generate`. If the version differs, use `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0 generate`.
Run: `git diff --stat internal/platform/db/dbgen/`
Expected: only `models.go` plus the query files whose results are `TicketMessage` rows change. Every generated header still reads `sqlc v1.27.0`. `models.go` now has `DroppedAttachments []byte \`json:"dropped_attachments"\`` on `TicketMessage`.
Run: `go build ./...`
Expected: success.

- [ ] **Step 7: Write the failing identity tests**

Append to `internal/ticketing/identity_integration_test.go`:

```go
// Spec 017 R7: a verified email domain is unique across all businesses. Two
// tenants may both start verification (UNIQUE is per-tenant for unverified rows),
// but once one verifies, the other's verify is a 409 conflict and its row stays
// unverified.
func TestIdentityVerifyEmailDomainGlobalConflict(t *testing.T) {
	ctx, tdb := startReadDB(t)
	first := seedReadTenant(ctx, t, tdb)
	second := seedReadTenant(ctx, t, tdb)
	resolver := newStubResolver()
	svc := newIdentityService(tdb, resolver)

	domain := uniqueDomain("shared")
	edFirst, err := svc.CreateEmailDomain(ctx, first.owner, first.master, CreateEmailDomainInput{Domain: domain, Mode: "provider_route"})
	if err != nil {
		t.Fatalf("first CreateEmailDomain: %v", err)
	}
	edSecond, err := svc.CreateEmailDomain(ctx, second.owner, second.master, CreateEmailDomainInput{Domain: domain, Mode: "provider_route"})
	if err != nil {
		t.Fatalf("second CreateEmailDomain (unverified rows may collide across tenants): %v", err)
	}
	resolver.records["_manyforge."+domain] = []string{
		edFirst.DNSChallenge.VerificationTXT.Value,
		edSecond.DNSChallenge.VerificationTXT.Value,
	}

	if got, err := svc.VerifyEmailDomain(ctx, first.owner, first.master, edFirst.ID); err != nil || got.Verification != "verified" {
		t.Fatalf("first verify = %+v, %v; want verified", got, err)
	}
	_, err = svc.VerifyEmailDomain(ctx, second.owner, second.master, edSecond.ID)
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("second verify error = %v, want ErrConflict (409)", err)
	}
	if va, _, _ := emailDomainRow(ctx, t, tdb, edSecond.ID); va != nil {
		t.Fatalf("conflicting domain persisted verified_at = %v, want NULL", va)
	}
}

// Spec 017 R8: Resend signs mail for a provider_route domain, so ManyForge's own
// DKIM selector is informational and is reported as "unknown", never "pending".
func TestIdentityProviderRouteDKIMIsInformational(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	resolver := newStubResolver()
	svc := newIdentityService(tdb, resolver)

	domain := uniqueDomain("resend")
	ed, err := svc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: domain, Mode: "provider_route"})
	if err != nil {
		t.Fatalf("CreateEmailDomain: %v", err)
	}
	if ed.DKIMState != "unknown" {
		t.Fatalf("create dkim_state = %q, want unknown", ed.DKIMState)
	}
	resolver.records["_manyforge."+domain] = []string{ed.DNSChallenge.VerificationTXT.Value}
	verified, err := svc.VerifyEmailDomain(ctx, rt.owner, rt.master, ed.ID)
	if err != nil || verified.Verification != "verified" || verified.DKIMState != "unknown" {
		t.Fatalf("verify = %+v, %v; want verified with dkim_state unknown", verified, err)
	}
	list, _, err := svc.ListEmailDomains(ctx, rt.owner, rt.master, Pagination{})
	if err != nil || len(list) != 1 || list[0].DKIMState != "unknown" {
		t.Fatalf("list = %+v, %v; want one domain with dkim_state unknown", list, err)
	}
}
```

- [ ] **Step 8: Run the identity tests to verify the DKIM one fails**

Run: `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestIdentityVerifyEmailDomainGlobalConflict|TestIdentityProviderRouteDKIMIsInformational' ./internal/ticketing/`
Expected: `TestIdentityVerifyEmailDomainGlobalConflict` PASSES already, because `MarkEmailDomainVerified` hits the new index and `mapIdentityErr` maps 23505 to `ErrConflict`. Keep it as the regression pin. `TestIdentityProviderRouteDKIMIsInformational` FAILS with `create dkim_state = "pending", want unknown`.

- [ ] **Step 9: Implement the provider_route DKIM state**

In `internal/ticketing/identity.go`, add below `dkimStateForList`:

```go
// dkimStateForMode reports ManyForge's DKIM probe as informational ("unknown")
// for a Resend-received provider_route domain: Resend signs that mail, so
// ManyForge's own selector is never published there (spec 017 R8).
func dkimStateForMode(mode dbgen.EmailDomainMode, computed string) string {
	if mode == dbgen.EmailDomainModeProviderRoute {
		return "unknown"
	}
	return computed
}
```

Then make three edits:
- In `CreateEmailDomain`, replace `return s.toEmailDomain(row, "pending"), nil` with `return s.toEmailDomain(row, dkimStateForMode(row.Mode, "pending")), nil`.
- Replace the body of `dkimStateForList` with:

```go
func (s *IdentityService) dkimStateForList(r dbgen.EmailDomain) string {
	if r.VerifiedAt.Valid {
		return dkimStateForMode(r.Mode, "pass")
	}
	return dkimStateForMode(r.Mode, "pending")
}
```

- In `VerifyEmailDomain`, replace `out = s.toEmailDomain(row, dkimState)` with `out = s.toEmailDomain(row, dkimStateForMode(row.Mode, dkimState))`.

- [ ] **Step 10: Run the identity suite**

Run: `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestIdentity' ./internal/ticketing/`
Expected: PASS.

- [ ] **Step 11: Document 409 on verify and pin it**

In `cmd/manyforge/drift_002_test.go`, in the `POST /businesses/{id}/email-domains/{did}/verify response codes` subtest, change `[]string{"200", "404"}` to `[]string{"200", "404", "409"}`.

Run: `go test -tags contract -count=1 -run TestEmailDomainEndpointContract ./cmd/manyforge/`
Expected: FAIL with `must document response 409`.

In `api/openapi.yaml`, under `/api/v1/businesses/{id}/email-domains/{did}/verify` → `post` → `responses`, insert after the `'404'` entry:

```yaml
        '409':
          $ref: '#/components/responses/SupportCRMConflict'
```

In `specs/002-support-desk/contracts/openapi.yaml`, add the same response under the verify operation, in that file's flow style:

```yaml
        '409': { $ref: '#/components/responses/Conflict' }
```

Run: `go test -tags contract -count=1 ./cmd/...`
Expected: PASS.

- [ ] **Step 12: Run the unit gates and regenerate the SDK if the contract change touched it**

Run: `go vet ./... && make lint && go test ./...`
Expected: PASS.
Run: `make sdk-check`
Expected: PASS, or a drift report for the new 409. If it reports drift, run `JAVA_HOME=/Library/Java/JavaVirtualMachines/temurin-21.jdk/Contents/Home make sdk-generate`, then `make sdk-check` again and expect PASS.

- [ ] **Step 13: Commit**

```bash
git add migrations/0139_support_email_resend.up.sql migrations/0139_support_email_resend.down.sql \
  migrations/support_email_resend_test.go db/schema.sql internal/platform/db/dbgen \
  internal/ticketing/identity.go internal/ticketing/identity_integration_test.go \
  api/openapi.yaml specs/002-support-desk/contracts/openapi.yaml cmd/manyforge/drift_002_test.go
git add sdk
git commit -m "feat(support): add migration 0139 for Resend support email

Globally unique verified domains and custom addresses, dropped-attachment
column, and the DEFINERs the Resend inbound/outbound path needs. Verify
maps the global conflict to 409; provider_route DKIM is informational."
```

---
### Task 2: Reply-token secret guard and Helm wiring (R14, R16)

**Files:**
- Modify: `internal/platform/config/config.go` (add `ValidateServe` after `Load`, ~:556)
- Create: `internal/platform/config/serve_config_test.go`
- Modify: `cmd/manyforge/main.go:101-109` (call `cfg.ValidateServe()` after the `migrate` branch)
- Modify: `charts/manyforge/values.yaml` (`secrets.masterKeys` ~:245, new `blob` block, new `secrets.blob` block)
- Modify: `charts/manyforge/templates/deployment.yaml` (env list ~:117)
- Modify: `charts/manyforge/templates/configmap.yaml` (append `MANYFORGE_BLOB_URL`)
- Modify: `charts/manyforge/chart_test.go` (append a test)
- Modify: `.env.example` (document the key)

**Interfaces:**
- Consumes: nothing.
- Produces: `func (cfg Config) ValidateServe() error`. It returns an error naming `MANYFORGE_INBOUND_REPLY_TOKEN_SECRET` when `Environment == "production"` and the secret is empty. It is called only on the serve path, so the Helm `migrate` Job (which does not receive the secret) keeps working.
- Produces (Helm): `secrets.masterKeys.inboundReplyTokenKey` (default `inbound-reply-token`), `blob.url`, and `secrets.blob.{secretName,accessKeyIDKey,secretAccessKeyKey}`.

- [ ] **Step 1: Write the failing config test**

Create `internal/platform/config/serve_config_test.go`:

```go
package config

import (
	"strings"
	"testing"
)

// MF-017-REPLY-TOKEN-SECRET: an empty reply-token HMAC key lets anyone forge a
// reply token and inject mail into any ticket of a business (spec 017 R14).
func TestValidateServeRequiresReplyTokenSecretInProduction(t *testing.T) {
	cfg := Config{Environment: "production"}
	err := cfg.ValidateServe()
	if err == nil || !strings.Contains(err.Error(), "MANYFORGE_INBOUND_REPLY_TOKEN_SECRET") {
		t.Fatalf("ValidateServe error = %v, want a MANYFORGE_INBOUND_REPLY_TOKEN_SECRET error", err)
	}
	cfg.InboundReplyTokenSecret = []byte("a-production-reply-token-secret-0123")
	if err := cfg.ValidateServe(); err != nil {
		t.Fatalf("ValidateServe with secret: %v", err)
	}
}

func TestValidateServeToleratesEmptySecretInDevelopment(t *testing.T) {
	if err := (Config{Environment: "development"}).ValidateServe(); err != nil {
		t.Fatalf("development ValidateServe: %v", err)
	}
}

// The migrate Job loads config in production without the reply-token secret, so
// Load itself must keep accepting an empty secret; only the serve path refuses.
func TestLoadInProductionStillAllowsMigrateWithoutReplyTokenSecret(t *testing.T) {
	setOutboundTestEnv(t, "smtp")
	t.Setenv("MANYFORGE_INBOUND_REPLY_TOKEN_SECRET", "")
	if _, err := Load(); err != nil {
		t.Fatalf("Load in production without reply-token secret: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 -run 'TestValidateServe|TestLoadInProductionStillAllowsMigrate' ./internal/platform/config/`
Expected: FAIL to compile with `cfg.ValidateServe undefined`.

- [ ] **Step 3: Implement ValidateServe**

In `internal/platform/config/config.go`, add `"errors"` to the import block, then add after `Load` (immediately before `// ValidateOutbound checks ...`):

```go
// ValidateServe checks settings the long-running server requires but the
// `manyforge migrate` Job does not (the Job loads the same config without
// inbound secrets). Spec 017 R14: an empty reply-token HMAC key lets anyone forge
// a reply token and inject a message into any ticket of a business.
func (cfg Config) ValidateServe() error {
	if cfg.Environment == "production" && len(cfg.InboundReplyTokenSecret) == 0 {
		return errors.New("MANYFORGE_INBOUND_REPLY_TOKEN_SECRET: required in production")
	}
	return nil
}
```

In `cmd/manyforge/main.go`, insert right after the `migrate` branch's closing `}` (before `ctx := context.Background()`):

```go
	// Serve-only requirements (the migrate Job above does not carry them).
	if err := cfg.ValidateServe(); err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/platform/config/ && go build ./cmd/manyforge/`
Expected: PASS and a clean build.

- [ ] **Step 5: Write the failing chart test**

Append to `charts/manyforge/chart_test.go`:

```go
// Spec 017 R16: the reply-token secret comes from the master-keys Secret (never
// the ConfigMap), MANYFORGE_BLOB_URL is plain config, and optional S3 credentials
// come from a named Secret only when one is configured.
func TestSupportEmailSecretAndBlobWiring(t *testing.T) {
	for _, tc := range []struct {
		name       string
		blobSecret bool
	}{
		{"with-blob-credentials", true},
		{"without-blob-credentials", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{"blob.url": "s3://support-attachments"}
			if tc.blobSecret {
				settings["secrets.blob.secretName"] = "blob-credentials"
			}
			output := renderChart(t, settings)
			var configMap struct {
				Data map[string]string `yaml:"data"`
			}
			if err := yaml.Unmarshal([]byte(renderedSource(t, output, "templates/configmap.yaml")), &configMap); err != nil {
				t.Fatal(err)
			}
			if got := configMap.Data["MANYFORGE_BLOB_URL"]; got != "s3://support-attachments" {
				t.Fatalf("MANYFORGE_BLOB_URL = %q, want s3://support-attachments", got)
			}
			for _, key := range []string{"MANYFORGE_INBOUND_REPLY_TOKEN_SECRET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
				if _, exists := configMap.Data[key]; exists {
					t.Fatalf("credential %s must never appear in the ConfigMap", key)
				}
			}
			app := workloadEnv(t, renderedSource(t, output, "templates/deployment.yaml"), configMap.Data)
			reply := app["MANYFORGE_INBOUND_REPLY_TOKEN_SECRET"]
			if reply.Value != "" || reply.ValueFrom.SecretKeyRef == nil ||
				*reply.ValueFrom.SecretKeyRef != (chartSecretRef{"manyforge-masterkeys", "inbound-reply-token"}) {
				t.Fatalf("reply-token env = %+v, want secretKeyRef manyforge-masterkeys/inbound-reply-token", reply)
			}
			for key, want := range map[string]chartSecretRef{
				"AWS_ACCESS_KEY_ID":     {"blob-credentials", "access-key-id"},
				"AWS_SECRET_ACCESS_KEY": {"blob-credentials", "secret-access-key"},
			} {
				got, exists := app[key]
				if exists != tc.blobSecret {
					t.Fatalf("%s present = %v, want %v", key, exists, tc.blobSecret)
				}
				if exists && (got.ValueFrom.SecretKeyRef == nil || *got.ValueFrom.SecretKeyRef != want) {
					t.Fatalf("%s = %+v, want secretKeyRef %+v", key, got, want)
				}
			}
		})
	}
}
```

- [ ] **Step 6: Run the chart test to verify it fails**

Run: `go test -count=1 -run TestSupportEmailSecretAndBlobWiring ./charts/manyforge/`
Expected: FAIL with `MANYFORGE_BLOB_URL = "", want s3://support-attachments` (requires `helm` on PATH, the same as the existing chart test).

- [ ] **Step 7: Implement the chart wiring**

In `charts/manyforge/values.yaml`, under `secrets.masterKeys`, after `mailingKey: mailing`, add:

```yaml
    # HMAC key minting/verifying support reply tokens (spec 017 R14). Production
    # refuses to start without it. Add the key to this Secret BEFORE upgrading.
    inboundReplyTokenKey: inbound-reply-token
```

Under `secrets:`, after the `inbound:` block, add:

```yaml
  # Optional S3 credentials for blob.url (read by the AWS SDK default chain).
  # Leave secretName empty for file:// storage or for IRSA/instance credentials.
  blob:
    secretName: ""
    accessKeyIDKey: access-key-id
    secretAccessKeyKey: secret-access-key
```

At top level, directly before the `mailing:` block, add:

```yaml
# Attachment object storage -> MANYFORGE_BLOB_URL (file:///… or s3://bucket?region=…).
# Empty disables attachment storage (inbound attachments are then not stored).
blob:
  url: ""
```

In `charts/manyforge/templates/deployment.yaml`, insert after the `MANYFORGE_MAILING_MASTER_KEY` env entry (before `{{- include "manyforge.outboundMailSecretEnv" . | nindent 12 }}`):

```yaml
            # HMAC key for support reply tokens (spec 017 R14). Optional at the pod
            # level so a missing key surfaces as the app's explicit startup error
            # rather than CreateContainerConfigError.
            - name: MANYFORGE_INBOUND_REPLY_TOKEN_SECRET
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.secrets.masterKeys.secretName }}
                  key: {{ .Values.secrets.masterKeys.inboundReplyTokenKey }}
                  optional: true
            {{- if .Values.secrets.blob.secretName }}
            - name: AWS_ACCESS_KEY_ID
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.secrets.blob.secretName }}
                  key: {{ .Values.secrets.blob.accessKeyIDKey }}
            - name: AWS_SECRET_ACCESS_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.secrets.blob.secretName }}
                  key: {{ .Values.secrets.blob.secretAccessKeyKey }}
            {{- end }}
```

In `charts/manyforge/templates/configmap.yaml`, append after the `MANYFORGE_MAILING_MESSAGE_DOMAIN` line:

```yaml
  # Attachment object storage (spec 017 R16). Empty disables attachment storage.
  MANYFORGE_BLOB_URL: {{ .Values.blob.url | quote }}
```

In `.env.example`, find the `MANYFORGE_INBOUND_REPLY_TOKEN_SECRET` line and put this comment directly above it:

```
# Required when MANYFORGE_ENVIRONMENT=production (server refuses to start without it).
```

- [ ] **Step 8: Run the chart tests and the CI chart smoke**

Run: `go test -count=1 ./charts/manyforge/`
Expected: PASS (the new test and `TestOutboundProviderEnvironmentParity`).
Run: `helm template mf charts/manyforge --set jwt.activeKid=k --set trustedProxyCidr=10.0.0.0/8 >| /tmp/mf-render.yaml && grep -c MANYFORGE_INBOUND_REPLY_TOKEN_SECRET /tmp/mf-render.yaml`
Expected: `1`.

- [ ] **Step 9: Commit**

```bash
git add internal/platform/config/config.go internal/platform/config/serve_config_test.go cmd/manyforge/main.go \
  charts/manyforge/values.yaml charts/manyforge/templates/deployment.yaml charts/manyforge/templates/configmap.yaml \
  charts/manyforge/chart_test.go .env.example
git commit -m "feat(config): require reply-token secret in production; wire it and blob URL in Helm

Closes manyforge-g97y. The guard runs on the serve path only so the migrate
Job is unaffected."
```

---
### Task 3: Drop and record disallowed attachments (R15)

**Files:**
- Modify: `internal/platform/blob/blob.go:94-105` (add `Detect`; `Sniff` uses it)
- Modify: `internal/inbox/service.go` (Ingest doc comment ~:110-120; sniff call ~:129-134; record after the non-duplicate branch ~:280; `sniffAttachments` ~:333-363)
- Create: `internal/inbox/attachment_drop.go`
- Create: `internal/inbox/attachment_drop_test.go` (unit)
- Create: `internal/inbox/attachment_drop_integration_test.go` (integration)
- Modify: `internal/security_regression/mime_sniff_test.go` (assert the message now persists, with the drop recorded)
- Modify: `internal/ticketing/service.go` (`Message` ~:120-138, `toMessage` ~:1162)
- Modify: `internal/ticketing/handler.go` (`messageResp` ~:150, `toMessageResp` ~:209)
- Create: `internal/ticketing/dropped_attachments_test.go` (unit)
- Modify: `api/openapi.yaml` (`TicketMessage` ~:21216, new `DroppedAttachment` schema next to `Attachment` ~:14805)
- Modify: `specs/002-support-desk/contracts/openapi.yaml` (`TicketMessage` ~:452)
- Regenerate: `sdk/**` via `make sdk-generate`

**Interfaces:**
- Consumes: `set_message_dropped_attachments(uuid, uuid, jsonb)` and `ticket_message.dropped_attachments` (Task 1).
- Produces:
  - `func blob.Detect(content []byte) string`: the base sniffed type, parameters stripped, allowlist not applied.
  - `ticketing.DroppedAttachment{Filename string; DetectedType string; Size int64}` and `ticketing.Message.DroppedAttachments []DroppedAttachment`.
  - JSON field `dropped_attachments: [{filename, detected_type, size}]` on every TicketMessage, always an array.

- [ ] **Step 1: Write the failing unit tests**

Create `internal/inbox/attachment_drop_test.go`:

```go
package inbox

import (
	"strings"
	"testing"
)

func TestSanitizeAttachmentFilenameStripsControlsAndBounds(t *testing.T) {
	if got := sanitizeAttachmentFilename("bad\x00na\x07me\x7f.exe"); got != "badname.exe" {
		t.Fatalf("sanitize = %q, want badname.exe (jsonb rejects \\u0000)", got)
	}
	long := strings.Repeat("é", 400)
	if got := sanitizeAttachmentFilename(long); len([]rune(got)) != maxDroppedFilenameRunes {
		t.Fatalf("sanitize kept %d runes, want %d", len([]rune(got)), maxDroppedFilenameRunes)
	}
	if got := sanitizeAttachmentFilename("\xff\xfeok.bin"); got != "ok.bin" {
		t.Fatalf("sanitize invalid UTF-8 = %q, want ok.bin", got)
	}
}

func TestSniffAttachmentsDropsDisallowedAndOversized(t *testing.T) {
	svc := &Service{cfg: Config{AttachmentMaxBytes: 64}}
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R'}
	elf := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	big := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, make([]byte, 100)...)
	staged, dropped := svc.sniffAttachments([]ParsedAttachment{
		{FileName: "ok.png", Content: png},
		{FileName: "run.exe", Content: elf},
		{FileName: "huge.png", Content: big},
		{FileName: "empty.txt", Content: nil},
	})
	if len(staged) != 1 || staged[0].filename != "ok.png" || staged[0].contentType != "image/png" {
		t.Fatalf("staged = %+v, want only ok.png", staged)
	}
	if len(dropped) != 2 ||
		dropped[0] != (droppedAttachment{Filename: "run.exe", DetectedType: "application/octet-stream", Size: int64(len(elf))}) ||
		dropped[1] != (droppedAttachment{Filename: "huge.png", DetectedType: "image/png", Size: int64(len(big))}) {
		t.Fatalf("dropped = %+v", dropped)
	}
}

func TestSniffAttachmentsBoundsDroppedList(t *testing.T) {
	svc := &Service{}
	elf := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}
	parts := make([]ParsedAttachment, maxDroppedAttachments+10)
	for i := range parts {
		parts[i] = ParsedAttachment{FileName: "x.exe", Content: elf}
	}
	_, dropped := svc.sniffAttachments(parts)
	if len(dropped) != maxDroppedAttachments {
		t.Fatalf("dropped len = %d, want %d (CHECK caps the column at 50)", len(dropped), maxDroppedAttachments)
	}
}
```

Create `internal/ticketing/dropped_attachments_test.go`:

```go
package ticketing

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

func TestToMessageProjectsDroppedAttachments(t *testing.T) {
	m := dbgen.TicketMessage{
		ID: uuid.New(), TicketID: uuid.New(),
		DroppedAttachments: []byte(`[{"filename":"a.exe","detected_type":"application/octet-stream","size":12}]`),
	}
	got := toMessage(m, nil)
	want := DroppedAttachment{Filename: "a.exe", DetectedType: "application/octet-stream", Size: 12}
	if len(got.DroppedAttachments) != 1 || got.DroppedAttachments[0] != want {
		t.Fatalf("DroppedAttachments = %+v, want [%+v]", got.DroppedAttachments, want)
	}
	raw, err := json.Marshal(toMessageResp(got))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"dropped_attachments":[{"filename":"a.exe","detected_type":"application/octet-stream","size":12}]`) {
		t.Fatalf("response JSON = %s", raw)
	}
}

func TestToMessageDroppedAttachmentsAlwaysAnArray(t *testing.T) {
	for _, stored := range [][]byte{nil, []byte(`[]`), []byte(`not json`)} {
		raw, err := json.Marshal(toMessageResp(toMessage(dbgen.TicketMessage{ID: uuid.New(), DroppedAttachments: stored}, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"dropped_attachments":[]`) {
			t.Fatalf("stored %q -> JSON %s, want dropped_attachments: []", stored, raw)
		}
	}
}
```

- [ ] **Step 2: Run the unit tests to verify they fail**

Run: `go test -count=1 -run 'TestSanitizeAttachmentFilename|TestSniffAttachments' ./internal/inbox/ ; go test -count=1 -run TestToMessage ./internal/ticketing/`
Expected: compile failures: `undefined: sanitizeAttachmentFilename`, `undefined: droppedAttachment`, `DroppedAttachment` undefined, and `sniffAttachments` returning the wrong number of values.

- [ ] **Step 3: Add blob.Detect**

In `internal/platform/blob/blob.go`, replace `Sniff` with:

```go
// Detect returns the base sniffed content type of content (parameters stripped)
// WITHOUT applying the allowlist. It describes a rejected attachment; Sniff is the
// gate that decides what may be stored.
func Detect(content []byte) string {
	ct := http.DetectContentType(content) // e.g. "image/png" or "text/plain; charset=utf-8"
	return strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
}

// Sniff determines the content type from the first 512 bytes and validates it
// against the allowlist (FR-007). A spoofed declared type is irrelevant: a file
// whose bytes sniff outside the allowlist is rejected with ErrUnsupportedType
// before any row or object is written.
func Sniff(content []byte) (string, error) {
	base := Detect(content)
	if !allowedTypes[base] {
		return "", fmt.Errorf("%q: %w", base, ErrUnsupportedType)
	}
	return base, nil
}
```

- [ ] **Step 4: Add the dropped-attachment helpers**

Create `internal/inbox/attachment_drop.go`:

```go
package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// maxDroppedAttachments matches the ticket_message.dropped_attachments CHECK.
const maxDroppedAttachments = 50

// maxDroppedFilenameRunes bounds an attacker-controlled filename kept for display.
const maxDroppedFilenameRunes = 255

// droppedAttachment is one inbound attachment that was NOT stored because its
// sniffed type is outside the allowlist or it exceeds the size cap (spec 017 R15).
// No bytes are retained; the record is shown on the message in the ticket view.
type droppedAttachment struct {
	Filename     string `json:"filename"`
	DetectedType string `json:"detected_type"`
	Size         int64  `json:"size"`
}

// sanitizeAttachmentFilename drops invalid UTF-8 and control characters (Postgres
// jsonb rejects \u0000, which would otherwise fail the whole ingest) and bounds
// the length.
func sanitizeAttachmentFilename(name string) string {
	var b strings.Builder
	runes := 0
	for len(name) > 0 && runes < maxDroppedFilenameRunes {
		r, size := utf8.DecodeRuneInString(name)
		name = name[size:]
		if r == utf8.RuneError && size <= 1 {
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		runes++
	}
	return b.String()
}

// recordDroppedAttachments stores the dropped list on the just-inserted inbound
// message through the SECURITY DEFINER (the ingest tx is principal-less, so a
// plain UPDATE would match zero rows under RLS).
func recordDroppedAttachments(ctx context.Context, tx pgx.Tx, messageID, tenantRootID uuid.UUID, dropped []droppedAttachment) error {
	raw, err := json.Marshal(dropped)
	if err != nil {
		return fmt.Errorf("inbox: marshal dropped attachments: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_message_dropped_attachments($1, $2, $3)",
		messageID, tenantRootID, raw); err != nil {
		return fmt.Errorf("inbox: record dropped attachments: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Change sniffAttachments and Ingest**

In `internal/inbox/service.go`, replace the whole `sniffAttachments` function (and its doc comment) with:

```go
// sniffAttachments MIME-sniffs every parsed attachment against the blob allowlist
// (FR-007) and enforces the per-attachment size cap, BEFORE any DB/blob write. A
// disallowed or lying part (declared type ignored) or an over-cap part is DROPPED
// and described in the returned dropped list; the rest of the message still
// ingests (spec 017 R15). Zero-byte parts are skipped (the attachment table CHECK
// requires size > 0).
func (s *Service) sniffAttachments(parts []ParsedAttachment) ([]sniffedAttachment, []droppedAttachment) {
	if len(parts) == 0 {
		return nil, nil
	}
	out := make([]sniffedAttachment, 0, len(parts))
	var dropped []droppedAttachment
	for _, p := range parts {
		if len(p.Content) == 0 {
			continue // nothing to store; CHECK (size > 0) would reject anyway
		}
		size := int64(len(p.Content))
		ct, err := blob.Sniff(p.Content) // declared type ignored; bytes decide
		if err != nil || (s.cfg.AttachmentMaxBytes > 0 && size > s.cfg.AttachmentMaxBytes) {
			if len(dropped) < maxDroppedAttachments {
				dropped = append(dropped, droppedAttachment{
					Filename:     sanitizeAttachmentFilename(p.FileName),
					DetectedType: blob.Detect(p.Content),
					Size:         size,
				})
			}
			continue
		}
		out = append(out, sniffedAttachment{
			id:          uuid.New(),
			filename:    p.FileName,
			contentType: ct,
			size:        size,
			content:     p.Content,
		})
	}
	return out, dropped
}
```

In `Ingest`, replace:

```go
	staged, err := s.sniffAttachments(parsed.Attachments)
	if err != nil {
		return IngestResult{}, err
	}
```

with:

```go
	staged, dropped := s.sniffAttachments(parsed.Attachments)
```

and, immediately after the `if out.Duplicate { ... return nil }` block, insert:

```go
		// Spec 017 R15: describe attachments that were not stored on the message.
		if len(dropped) > 0 && out.MessageID != uuid.Nil {
			if err := recordDroppedAttachments(ctx, tx, out.MessageID, r.tenantRootID, dropped); err != nil {
				return err
			}
			s.logger.InfoContext(ctx, "inbox: attachments dropped", "count", len(dropped), "provider", msg.Provider)
		}
```

In Ingest's doc comment, replace step 3 with:

```go
//  3. MIME-SNIFF every attachment BEFORE any DB/blob write; a disallowed, lying,
//     or over-cap part is DROPPED (never stored) and recorded on the message, and
//     the rest of the message ingests (FR-007, spec 017 R15).
```

If `fmt` becomes unused in `service.go`, the compiler says so. Leave it if it is still used elsewhere in the file (it is, in error wraps).

- [ ] **Step 6: Project the field through the ticket API**

In `internal/ticketing/service.go`, add above `type Message struct`:

```go
// DroppedAttachment describes an inbound attachment that was not stored (spec
// 017 R15). No bytes exist for it.
type DroppedAttachment struct {
	Filename     string
	DetectedType string
	Size         int64
}
```

Add a field to `Message` after `Attachments []Attachment`:

```go
	DroppedAttachments []DroppedAttachment
```

In `toMessage`, before the `return Message{`, add:

```go
	dropped := []DroppedAttachment{}
	if len(m.DroppedAttachments) > 0 {
		var stored []struct {
			Filename     string `json:"filename"`
			DetectedType string `json:"detected_type"`
			Size         int64  `json:"size"`
		}
		if err := json.Unmarshal(m.DroppedAttachments, &stored); err == nil {
			for _, d := range stored {
				dropped = append(dropped, DroppedAttachment{Filename: d.Filename, DetectedType: d.DetectedType, Size: d.Size})
			}
		}
	}
```

and set `DroppedAttachments: dropped,` in the returned literal (after `Attachments: atts,`). `encoding/json` is already imported by `service.go` (used by `projectAuthResults`).

In `internal/ticketing/handler.go`, add next to `attachmentResp`:

```go
type droppedAttachmentResp struct {
	Filename     string `json:"filename"`
	DetectedType string `json:"detected_type"`
	Size         int64  `json:"size"`
}
```

Add to `messageResp` after `Attachments`:

```go
	DroppedAttachments []droppedAttachmentResp `json:"dropped_attachments"`
```

In `toMessageResp`, before the return, add:

```go
	dropped := make([]droppedAttachmentResp, 0, len(m.DroppedAttachments))
	for _, d := range m.DroppedAttachments {
		dropped = append(dropped, droppedAttachmentResp{Filename: d.Filename, DetectedType: d.DetectedType, Size: d.Size})
	}
```

and set `DroppedAttachments: dropped,` after `Attachments: atts,`.

- [ ] **Step 7: Run the unit tests to verify they pass**

Run: `go test -count=1 ./internal/inbox/ ./internal/ticketing/ ./internal/platform/blob/`
Expected: PASS.

- [ ] **Step 8: Write the failing integration test**

Create `internal/inbox/attachment_drop_integration_test.go`:

```go
//go:build integration

package inbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

var dropELF = []byte{
	0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x02, 0x00, 0x3e, 0x00, 0x01, 0x00, 0x00, 0x00,
}

// mixedAttachmentMessage: a text part, an allowlisted PNG, and an ELF executable
// that declares itself application/pdf with the given (RFC 2231-encoded) filename.
func mixedAttachmentMessage(to, from, subject, messageID, encodedFilename string) []byte {
	return []byte("From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
		"Message-ID: <" + messageID + ">\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
		"--BOUND\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nsee attached\r\n" +
		"--BOUND\r\nContent-Type: image/png; name=\"ok.png\"\r\n" +
		"Content-Disposition: attachment; filename=\"ok.png\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(replayPNG) + "\r\n" +
		"--BOUND\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename*=" + encodedFilename + "\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(dropELF) + "\r\n" +
		"--BOUND--\r\n")
}

type storedDrop struct {
	Filename     string `json:"filename"`
	DetectedType string `json:"detected_type"`
	Size         int64  `json:"size"`
}

// Spec 017 R15 / MF-002-MIME-SNIFF: a disallowed attachment no longer fails the
// message (which made the sender retry until it bounced); it is dropped and
// recorded, and the allowlisted attachment is still stored.
func TestIngestDropsDisallowedAttachmentAndKeepsMessage(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	msg := RawMessage{
		Provider: "webhook:test", EnvelopeRecipient: ten.address, EnvelopeSender: "mallory@example.com",
		ReceivedAt: time.Now(),
		Raw:        mixedAttachmentMessage(ten.address, "Mallory <mallory@example.com>", "invoice", "drop-1@example.com", "utf-8''invoice.exe"),
	}
	res, err := svc.Ingest(ctx, msg)
	if err != nil {
		t.Fatalf("ingest with a disallowed attachment must succeed: %v", err)
	}
	if !res.Created {
		t.Fatalf("result = %+v, want a new ticket", res)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM attachment WHERE ticket_message_id=$1", res.MessageID); n != 1 {
		t.Fatalf("stored attachments = %d, want 1 (the PNG)", n)
	}
	var raw []byte
	if err := tdb.Super.QueryRow(ctx, "SELECT dropped_attachments FROM ticket_message WHERE id=$1", res.MessageID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var dropped []storedDrop
	if err := json.Unmarshal(raw, &dropped); err != nil {
		t.Fatal(err)
	}
	want := storedDrop{Filename: "invoice.exe", DetectedType: "application/octet-stream", Size: int64(len(dropELF))}
	if len(dropped) != 1 || dropped[0] != want {
		t.Fatalf("dropped = %+v, want [%+v]", dropped, want)
	}

	again, err := svc.Ingest(ctx, msg)
	if err != nil || !again.Duplicate {
		t.Fatalf("replay = %+v, %v; want an idempotent duplicate", again, err)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1", res.TicketID); n != 1 {
		t.Fatalf("messages after replay = %d, want 1", n)
	}
}

// A NUL or control byte in an attachment filename must not fail the ingest
// (jsonb rejects \u0000); it is stripped before recording.
func TestIngestDroppedAttachmentFilenameWithNULStillIngests(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	res, err := svc.Ingest(ctx, RawMessage{
		Provider: "webhook:test", EnvelopeRecipient: ten.address, EnvelopeSender: "mallory@example.com",
		ReceivedAt: time.Now(),
		Raw:        mixedAttachmentMessage(ten.address, "mallory@example.com", "nul", "drop-nul@example.com", "utf-8''bad%00na%07me.exe"),
	})
	if err != nil {
		t.Fatalf("ingest with a NUL filename must succeed: %v", err)
	}
	var raw []byte
	if err := tdb.Super.QueryRow(ctx, "SELECT dropped_attachments FROM ticket_message WHERE id=$1", res.MessageID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var dropped []storedDrop
	if err := json.Unmarshal(raw, &dropped); err != nil || len(dropped) != 1 || dropped[0].Filename != "badname.exe" {
		t.Fatalf("dropped = %+v (%v), want filename badname.exe", dropped, err)
	}
}
```

- [ ] **Step 9: Run the integration tests**

Run: `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestIngestDrop|TestIngestReplayDoesNotLeakBlobs' ./internal/inbox/`
Expected: PASS. (`replayPNG` comes from `attachment_replay_integration_test.go` in the same package and build tag.)

- [ ] **Step 10: Tighten the MIME-sniff security regression**

In `internal/security_regression/mime_sniff_test.go`, update the header comment's last sentence to read: "... is never persisted as an attachment; since spec 017 R15 the message itself still ingests and records the drop." Then replace the line `_, _ = svc.Ingest(ctx, ingestAttachment(ctx, ten, "Mallory <mallory@example.com>", "gift", "mime-bad@example.com", elfBytes))` with:

```go
	resBad, err := svc.Ingest(ctx, ingestAttachment(ctx, ten, "Mallory <mallory@example.com>", "gift", "mime-bad@example.com", elfBytes))
	if err != nil {
		t.Fatalf("spec 017 R15: a lying attachment must not fail the whole message: %v", err)
	}
	if n := countSuperInt(ctx, t, tdb.Super,
		"SELECT jsonb_array_length(dropped_attachments) FROM ticket_message WHERE id=$1", resBad.MessageID); n != 1 {
		t.Errorf("dropped_attachments length = %d, want 1 (the lying part is recorded, never stored)", n)
	}
```

Run: `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run TestLyingAttachmentRejectedBeforePersistence ./internal/security_regression/`
Expected: PASS. In the same file, the positive-control PNG's attachment count stays 1.

- [ ] **Step 11: Document the field in OpenAPI and regenerate the SDK**

In `api/openapi.yaml`, under `components.schemas.TicketMessage.properties`, insert after `attachments`:

```yaml
        dropped_attachments:
          type: array
          description: Inbound attachments that were not stored because their sniffed type is not allowed or they exceed the size cap (spec 017 R15). No bytes are retained.
          items:
            $ref: '#/components/schemas/DroppedAttachment'
```

Add `- dropped_attachments` to `TicketMessage.required` right after `- attachments`. Add a new schema directly after the `Attachment:` schema block:

```yaml
    DroppedAttachment:
      type: object
      description: An inbound attachment that was dropped instead of stored (spec 017 R15).
      properties:
        filename:
          type: string
        detected_type:
          type: string
          description: content type sniffed from the bytes (the declared type is never trusted)
        size:
          type: integer
          format: int64
      required:
      - filename
      - detected_type
      - size
```

In `specs/002-support-desk/contracts/openapi.yaml`, add to `TicketMessage.properties` after `attachments`:

```yaml
        dropped_attachments: { type: array, items: { type: object, properties: { filename: { type: string }, detected_type: { type: string }, size: { type: integer } } }, description: inbound attachments dropped instead of stored (spec 017 R15) }
```

Run: `go test -tags contract -count=1 ./cmd/...`
Expected: PASS.
Run: `JAVA_HOME=/Library/Java/JavaVirtualMachines/temurin-21.jdk/Contents/Home make sdk-generate && make sdk-check`
Expected: generation rewrites the TicketMessage model in each language under `sdk/`, and `sdk-check` PASSES. If `tools/sdk/compatibility.py` flags the new required response field, follow its message. A new response field is additive for SDK consumers.

- [ ] **Step 12: Run the unit gates**

Run: `go vet ./... && make lint && go test ./...`
Expected: PASS.

- [ ] **Step 13: Commit**

```bash
git add internal/platform/blob/blob.go internal/inbox/attachment_drop.go internal/inbox/attachment_drop_test.go \
  internal/inbox/attachment_drop_integration_test.go internal/inbox/service.go \
  internal/security_regression/mime_sniff_test.go internal/ticketing/service.go internal/ticketing/handler.go \
  internal/ticketing/dropped_attachments_test.go api/openapi.yaml specs/002-support-desk/contracts/openapi.yaml sdk
git commit -m "fix(inbox): drop and record disallowed attachments instead of failing the message

feat(sdk): TicketMessage gains dropped_attachments."
```

---
### Task 4: Resend client for Receiving API and webhook events (R1, R2, R6)

**Files:**
- Modify: `internal/mailing/provider/resend.go` (struct fields ~:24-29; `EnsureWebhook` create payload ~:133-139; `reconcileWebhooks` candidate selection ~:189-203; `do` ~:286-344)
- Create: `internal/mailing/provider/resend_receiving.go`
- Create: `internal/mailing/provider/resend_receiving_test.go`
- Modify: `internal/mailing/provider/provider_test.go` (existing webhook fixtures gain `email.received`; create test asserts it)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces (package `provider`, imported in `mailing` as `mailprovider`):
  - New `Resend` fields: `RawClient *http.Client` (falls back to `Client`) and `AllowInsecureRawURL bool` (tests only).
  - `var ResendWebhookEvents = []string{"email.bounced", "email.complained", "email.received"}`
  - `type ReceivedEmail struct { ID string; To []string; ReceivedFor []string; MessageID string; RawURL string }`
  - `func (r *Resend) GetReceivedEmail(ctx context.Context, id string, maxRawBytes int64) (ReceivedEmail, error)`
  - `func (r *Resend) DownloadReceivedRaw(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error)`
  - `func (r *Resend) EnsureReceivingEvents(ctx context.Context, webhookID string) (signingSecret string, patched bool, err error)`
  - Sentinels: `ErrReceivedEmailTooLarge` (acknowledge, never retry), `ErrReceivedEmailGone` (Resend 404: acknowledge), `ErrReceivedRawUnavailable` (retry).
  - `EnsureWebhook` now creates webhooks with all three events, and upgrades an otherwise-valid legacy webhook in place with `PATCH /webhooks/{id}`.

- [ ] **Step 1: Write the failing tests**

Create `internal/mailing/provider/resend_receiving_test.go`:

```go
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResendGetReceivedEmailParsesRawURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.URL.Path != "/emails/receiving/em_1" {
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
		if req.URL.Query().Get("html_format") != "cid" {
			t.Fatalf("html_format = %q, want cid (keeps inline images out of the metadata)", req.URL.Query().Get("html_format"))
		}
		if req.Header.Get("Authorization") != "Bearer re_test" {
			t.Fatalf("authorization = %q", req.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"object":"email","id":"em_1","to":["support+Tok@support.example.test"],
			"received_for":["support@support.example.test"],"message_id":"<m-1@customer.test>",
			"html":"<p>hi</p>","text":null,"raw":{"download_url":"https://cdn.example.test/raw/em_1?sig=s","expires_at":"2026-10-10T01:00:00Z"}}`)
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	got, err := r.GetReceivedEmail(context.Background(), "em_1", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "em_1" || got.RawURL != "https://cdn.example.test/raw/em_1?sig=s" || got.MessageID != "<m-1@customer.test>" ||
		len(got.To) != 1 || got.To[0] != "support+Tok@support.example.test" ||
		len(got.ReceivedFor) != 1 || got.ReceivedFor[0] != "support@support.example.test" {
		t.Fatalf("received email = %+v", got)
	}
}

func TestResendGetReceivedEmailClassifiesErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		want error
	}{
		{"gone", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, ErrReceivedEmailGone},
		{"no raw", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"id":"em_1","raw":null}`) }, ErrReceivedRawUnavailable},
		{"huge metadata", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"id":"em_1","html":"`+strings.Repeat("a", 2<<20)+`"}`)
		}, ErrReceivedEmailTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.h)
			defer server.Close()
			r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
			if _, err := r.GetReceivedEmail(context.Background(), "em_1", 1024); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"name":"internal_server_error","message":"upstream secret detail"}`)
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	_, err := r.GetReceivedEmail(context.Background(), "em_1", 1024)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadGateway ||
		errors.Is(err, ErrReceivedEmailGone) || strings.Contains(err.Error(), "upstream secret detail") {
		t.Fatalf("5xx error = %v, want a retryable HTTPError without the upstream body", err)
	}
}

func TestResendDownloadReceivedRawCapsAndNeverSendsAPIKey(t *testing.T) {
	raw := []byte("From: a@b.test\r\nTo: s@x.test\r\nSubject: hi\r\n\r\nbody\r\n")
	var chunked atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" {
			t.Fatalf("raw download sent Authorization %q; the URL is pre-signed", req.Header.Get("Authorization"))
		}
		if chunked.Load() {
			// No Content-Length: the cap must hold while streaming.
			_, _ = w.Write(raw[:10])
			w.(http.Flusher).Flush()
			_, _ = w.Write(raw[10:])
			return
		}
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", Client: server.Client(), AllowInsecureRawURL: true}
	got, err := r.DownloadReceivedRaw(context.Background(), server.URL+"/raw/em_1", int64(len(raw)))
	if err != nil || string(got) != string(raw) {
		t.Fatalf("download = %q, %v", got, err)
	}
	if _, err := r.DownloadReceivedRaw(context.Background(), server.URL+"/raw/em_1", int64(len(raw)-1)); !errors.Is(err, ErrReceivedEmailTooLarge) {
		t.Fatalf("over-cap error = %v, want ErrReceivedEmailTooLarge", err)
	}
	chunked.Store(true)
	if _, err := r.DownloadReceivedRaw(context.Background(), server.URL+"/raw/em_1", int64(len(raw)-1)); !errors.Is(err, ErrReceivedEmailTooLarge) {
		t.Fatalf("chunked over-cap error = %v, want ErrReceivedEmailTooLarge", err)
	}
}

func TestResendDownloadReceivedRawRejectsNonHTTPS(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	r := &Resend{Client: server.Client()}
	for _, u := range []string{server.URL + "/raw", "file:///etc/passwd", "://bad", ""} {
		if _, err := r.DownloadReceivedRaw(context.Background(), u, 1024); !errors.Is(err, ErrReceivedRawUnavailable) {
			t.Fatalf("url %q error = %v, want ErrReceivedRawUnavailable", u, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("non-https URLs reached the network %d times", hits.Load())
	}
}

func TestResendDownloadTransportErrorDoesNotLeakSignedURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := server.URL
	server.Close() // connection refused from here on
	r := &Resend{AllowInsecureRawURL: true}
	_, err := r.DownloadReceivedRaw(context.Background(), base+"/raw?X-Amz-Signature=topsecret", 1024)
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("transport error = %v, must not contain the signed URL", err)
	}
}

func TestResendEnsureWebhookUpgradesLegacyEventsWithPatch(t *testing.T) {
	const endpoint = "https://hub.example.test/api/v1/inbound/mailing/profile-a/resend"
	events := []string{"email.bounced", "email.complained"}
	var patchBody struct {
		Events []string `json:"events"`
	}
	var mutations, patches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks":
			_ = json.NewEncoder(w).Encode(map[string]any{"has_more": false, "data": []any{
				map[string]any{"id": "wh_legacy", "status": "enabled", "endpoint": endpoint, "events": events},
			}})
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks/wh_legacy":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "wh_legacy", "status": "enabled", "endpoint": endpoint,
				"events": events, "signing_secret": "whsec_MDEyMzQ1Njc4OWFiY2RlZg=="})
		case req.Method == http.MethodPatch && req.URL.Path == "/webhooks/wh_legacy":
			patches++
			if err := json.NewDecoder(req.Body).Decode(&patchBody); err != nil {
				t.Fatal(err)
			}
			events = patchBody.Events
			_, _ = io.WriteString(w, `{"object":"webhook","id":"wh_legacy"}`)
		default:
			t.Fatalf("request = %s %s (an upgrade must not delete or recreate)", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	got, created, err := r.EnsureWebhook(context.Background(), endpoint, "wh_legacy", func(context.Context) error {
		mutations++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created || got.ID != "wh_legacy" || got.SigningSecret != "whsec_MDEyMzQ1Njc4OWFiY2RlZg==" {
		t.Fatalf("webhook = %+v created=%v, want the same webhook and secret", got, created)
	}
	if patches != 1 || mutations != 1 || !containsResendEvents(patchBody.Events, ResendWebhookEvents...) {
		t.Fatalf("patches=%d mutations=%d events=%v", patches, mutations, patchBody.Events)
	}
}

func TestResendEnsureReceivingEvents(t *testing.T) {
	events := []string{"email.bounced", "email.complained", "email.delivered"}
	var patches int
	var patched []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks/wh_1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "wh_1", "status": "enabled",
				"events": events, "signing_secret": "whsec_MDEyMzQ1Njc4OWFiY2RlZg=="})
		case req.Method == http.MethodPatch && req.URL.Path == "/webhooks/wh_1":
			patches++
			var body struct {
				Events []string `json:"events"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			patched, events = body.Events, body.Events
			_, _ = io.WriteString(w, `{"object":"webhook","id":"wh_1"}`)
		default:
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	secret, didPatch, err := r.EnsureReceivingEvents(context.Background(), "wh_1")
	if err != nil || !didPatch || secret != "whsec_MDEyMzQ1Njc4OWFiY2RlZg==" {
		t.Fatalf("first = %q %v %v", secret, didPatch, err)
	}
	if !containsResendEvents(patched, "email.bounced", "email.complained", "email.delivered", "email.received") {
		t.Fatalf("patched events = %v, want the union (existing events kept)", patched)
	}
	if _, didPatch, err = r.EnsureReceivingEvents(context.Background(), "wh_1"); err != nil || didPatch || patches != 1 {
		t.Fatalf("second call patched=%v patches=%d err=%v, want read-only", didPatch, patches, err)
	}
}
```

- [ ] **Step 2: Update the existing webhook fixtures and run all tests to watch them fail**

Webhooks created by the new code carry all three events, so the read-only reconcile fixtures must too. Run:

```bash
sed -i '' \
  -e 's/\[\]string{"email.bounced", "email.complained"}/[]string{"email.bounced", "email.complained", "email.received"}/g' \
  -e 's/\["email.bounced","email.complained"\]/["email.bounced","email.complained","email.received"]/g' \
  internal/mailing/provider/provider_test.go
```

Then in `TestResendEnsureWebhookCreatesExactFeedbackRoute`, change the assertion `!containsStrings(gotEvents, "email.bounced", "email.complained")` to `!containsStrings(gotEvents, "email.bounced", "email.complained", "email.received")`.

Run: `go test -count=1 ./internal/mailing/provider/`
Expected: compile FAIL (`undefined: ErrReceivedEmailGone`, `r.GetReceivedEmail undefined`, `unknown field AllowInsecureRawURL`, `undefined: ResendWebhookEvents`).

- [ ] **Step 3: Add the receiving client**

Create `internal/mailing/provider/resend_receiving.go`:

```go
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var (
	// ErrReceivedEmailTooLarge marks a received message (or its metadata) larger
	// than MANYFORGE_INBOUND_MAX_BYTES allows. Callers acknowledge, never retry (R6).
	ErrReceivedEmailTooLarge = errors.New("provider: received email exceeds the size cap")
	// ErrReceivedEmailGone marks a received email Resend no longer has (HTTP 404).
	ErrReceivedEmailGone = errors.New("provider: received email not found")
	// ErrReceivedRawUnavailable marks a missing or unusable raw download URL.
	// Callers treat it as transient and let Resend retry.
	ErrReceivedRawUnavailable = errors.New("provider: received email raw message unavailable")
)

// ReceivedEmail is the subset of Resend's GET /emails/receiving/{id} that
// ManyForge uses. Shape confirmed against
// https://resend.com/docs/api-reference/emails/retrieve-received-email (2026-10-10).
type ReceivedEmail struct {
	ID          string
	To          []string
	ReceivedFor []string
	MessageID   string
	RawURL      string
}

type resendReceivedEmailResponse struct {
	ID          string   `json:"id"`
	To          []string `json:"to"`
	ReceivedFor []string `json:"received_for"`
	MessageID   string   `json:"message_id"`
	Raw         *struct {
		DownloadURL string `json:"download_url"`
	} `json:"raw"`
}

// GetReceivedEmail loads a received email's metadata with the profile's API key.
// html_format=cid keeps inline images out of the JSON. The response is bounded
// by 1 MiB + 2*maxRawBytes (text and html are each at most the raw size); a
// larger response returns ErrReceivedEmailTooLarge.
func (r *Resend) GetReceivedEmail(ctx context.Context, id string, maxRawBytes int64) (ReceivedEmail, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 200 {
		return ReceivedEmail{}, ErrReceivedEmailGone
	}
	limit := int64(1 << 20)
	if maxRawBytes > 0 {
		limit += 2 * maxRawBytes
	}
	var resp resendReceivedEmailResponse
	err := r.doLimited(ctx, http.MethodGet, "/emails/receiving/"+url.PathEscape(id)+"?html_format=cid", nil, "", &resp, limit)
	var httpErr *HTTPError
	switch {
	case errors.Is(err, errResendResponseTooLarge):
		return ReceivedEmail{}, ErrReceivedEmailTooLarge
	case errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound:
		return ReceivedEmail{}, ErrReceivedEmailGone
	case err != nil:
		return ReceivedEmail{}, err
	}
	out := ReceivedEmail{ID: resp.ID, To: resp.To, ReceivedFor: resp.ReceivedFor, MessageID: resp.MessageID}
	if resp.Raw != nil {
		out.RawURL = strings.TrimSpace(resp.Raw.DownloadURL)
	}
	if out.RawURL == "" {
		return ReceivedEmail{}, ErrReceivedRawUnavailable
	}
	return out, nil
}

// DownloadReceivedRaw fetches the original RFC 822 message from Resend's
// pre-signed raw.download_url. The API key is never sent. Only https URLs are
// fetched (http only when AllowInsecureRawURL is set, for tests). The body is
// capped at maxBytes while streaming, so a missing or lying Content-Length cannot
// exhaust memory. Transport errors never include the signed URL.
func (r *Resend) DownloadReceivedRaw(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || !(u.Scheme == "https" || (r.AllowInsecureRawURL && u.Scheme == "http")) {
		return nil, ErrReceivedRawUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrReceivedRawUnavailable
	}
	client := r.RawClient
	if client == nil {
		client = r.Client
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, fmt.Errorf("provider: resend raw transport: %w", urlErr.Err)
		}
		return nil, errors.New("provider: resend raw transport failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode}
	}
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		return nil, ErrReceivedEmailTooLarge
	}
	reader := io.Reader(resp.Body)
	if maxBytes > 0 {
		reader = io.LimitReader(resp.Body, maxBytes+1)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, errors.New("provider: resend raw read failed")
	}
	if maxBytes > 0 && int64(len(raw)) > maxBytes {
		return nil, ErrReceivedEmailTooLarge
	}
	return raw, nil
}

// EnsureReceivingEvents makes an existing webhook subscribe to every event in
// ResendWebhookEvents, keeping any other events it already has, without touching
// its endpoint or identity. It is read-only when nothing is missing. It returns
// the signing secret as read after any change so the caller can confirm the
// stored secret still matches.
func (r *Resend) EnsureReceivingEvents(ctx context.Context, webhookID string) (string, bool, error) {
	webhookID = strings.TrimSpace(webhookID)
	if webhookID == "" {
		return "", false, ErrResendWebhook
	}
	detail, err := r.webhookDetail(ctx, webhookID)
	if err != nil {
		return "", false, err
	}
	if containsResendEvents(detail.Events, ResendWebhookEvents...) {
		return detail.SigningSecret, false, nil
	}
	if err := r.patchWebhookEvents(ctx, webhookID, unionEvents(detail.Events, ResendWebhookEvents)); err != nil {
		return "", false, err
	}
	detail, err = r.webhookDetail(ctx, webhookID)
	if err != nil {
		return "", false, err
	}
	if !containsResendEvents(detail.Events, ResendWebhookEvents...) || !validResendSigningSecret(detail.SigningSecret) {
		return "", false, ErrResendWebhook
	}
	return detail.SigningSecret, true, nil
}

func (r *Resend) webhookDetail(ctx context.Context, webhookID string) (resendWebhookResponse, error) {
	var detail resendWebhookResponse
	err := r.do(ctx, http.MethodGet, "/webhooks/"+url.PathEscape(webhookID), nil, "", &detail)
	return detail, err
}

func (r *Resend) patchWebhookEvents(ctx context.Context, webhookID string, events []string) error {
	payload := struct {
		Events []string `json:"events"`
	}{Events: events}
	return r.do(ctx, http.MethodPatch, "/webhooks/"+url.PathEscape(webhookID), payload, "", nil)
}

func unionEvents(existing, required []string) []string {
	seen := make(map[string]bool, len(existing)+len(required))
	out := make([]string, 0, len(existing)+len(required))
	for _, list := range [][]string{existing, required} {
		for _, event := range list {
			if event != "" && !seen[event] {
				seen[event] = true
				out = append(out, event)
			}
		}
	}
	return out
}
```

- [ ] **Step 4: Wire the struct fields, the event list, the PATCH upgrade, and the bounded read**

In `internal/mailing/provider/resend.go`:

1. Extend the struct:

```go
type Resend struct {
	APIKey    string
	FromEmail string
	BaseURL   string
	Client    *http.Client
	// RawClient fetches pre-signed raw-message URLs (spec 017). Production wires an
	// SSRF-guarded netsafe client; nil falls back to Client.
	RawClient *http.Client
	// AllowInsecureRawURL permits http raw URLs. Tests only (fake Resend server).
	AllowInsecureRawURL bool
}
```

2. Below `const defaultResendBaseURL`, add:

```go
// ResendWebhookEvents is the event set every ManyForge-provisioned Resend webhook
// subscribes to: delivery feedback (spec 013) plus inbound mail (spec 017 R1).
var ResendWebhookEvents = []string{"email.bounced", "email.complained", "email.received"}

// errResendResponseTooLarge marks a Resend API response over the read bound.
var errResendResponseTooLarge = errors.New("provider: resend response exceeds the read bound")
```

3. In `EnsureWebhook`, replace `Events:   []string{"email.bounced", "email.complained"},` with `Events:   ResendWebhookEvents,`.

4. In `reconcileWebhooks`, replace the final line `return ResendWebhook{ID: candidates[0].ID, SigningSecret: candidates[0].SigningSecret}, true, nil` with:

```go
	chosen := candidates[0]
	if !containsResendEvents(chosen.Events, ResendWebhookEvents...) {
		// Spec 017 R1: a webhook provisioned before email.received existed is
		// upgraded in place, so its id (and signing secret) survive.
		if err := beforeMutation(ctx); err != nil {
			return ResendWebhook{}, false, err
		}
		if err := r.patchWebhookEvents(ctx, chosen.ID, unionEvents(chosen.Events, ResendWebhookEvents)); err != nil {
			return ResendWebhook{}, false, err
		}
		refreshed, err := r.webhookDetail(ctx, chosen.ID)
		if err != nil {
			return ResendWebhook{}, false, err
		}
		if refreshed.ID != chosen.ID || refreshed.Endpoint != endpoint ||
			!containsResendEvents(refreshed.Events, ResendWebhookEvents...) ||
			!validResendSigningSecret(refreshed.SigningSecret) {
			return ResendWebhook{}, false, ErrResendWebhook
		}
		chosen = refreshed
	}
	return ResendWebhook{ID: chosen.ID, SigningSecret: chosen.SigningSecret}, true, nil
```

5. Split `do` so receiving reads can use a larger bound. Rename the existing `do` to `doLimited`, add a `limit int64` parameter, and change its body read to:

```go
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("provider: resend response: %w", err)
	}
	if int64(len(raw)) > limit {
		return errResendResponseTooLarge
	}
```

Keep the rest of the body unchanged. Then add the wrapper so every existing call site is untouched:

```go
func (r *Resend) do(ctx context.Context, method, path string, body any, idempotencyKey string, out any) error {
	return r.doLimited(ctx, method, path, body, idempotencyKey, out, 1<<20)
}
```

- [ ] **Step 5: Run the provider tests**

Run: `go test -count=1 ./internal/mailing/provider/`
Expected: PASS, including every pre-existing `TestResendEnsureWebhook*` test.

- [ ] **Step 6: Run the mailing unit and security pins**

Run: `go test -count=1 ./internal/mailing/... ./internal/security_regression/`
Expected: PASS. `TestPin_*` in `mailing_automation_audit_pins_test.go` still finds `"email.bounced"`/`"email.complained"` in `resend.go`, because the new `ResendWebhookEvents` literal contains them.

- [ ] **Step 7: Commit**

```bash
git add internal/mailing/provider/resend.go internal/mailing/provider/resend_receiving.go \
  internal/mailing/provider/resend_receiving_test.go internal/mailing/provider/provider_test.go
git commit -m "feat(mailing): Resend receiving client and email.received webhook events

GET /emails/receiving/{id}, capped raw download from the pre-signed URL
(API key never sent, signed URL never in errors), and in-place PATCH
upgrade of legacy webhooks."
```

---
### Task 5: Inbox Resend source with the same-business rule (R2, R3, R4)

**Files:**
- Modify: `internal/inbox/source.go` (`RawMessage` struct ~:29-58: add `RestrictToBusiness`)
- Modify: `internal/inbox/resolve.go` (add `resolveRecipientForBusiness`; switch the `err == pgx.ErrNoRows` comparisons to `errors.Is`)
- Modify: `internal/inbox/service.go` (recipient resolution inside the Ingest tx ~:150-153)
- Create: `internal/inbox/resend.go`
- Create: `internal/inbox/resend_test.go` (unit)
- Create: `internal/inbox/resend_integration_test.go` (integration)

**Interfaces:**
- Consumes: `resolve_inbound_address_for_business`, `support_audit_inbound_dropped` (Task 1), and the Task 3 `Ingest` signature (unchanged).
- Produces:
  - `RawMessage.RestrictToBusiness uuid.UUID`. When non-nil, Ingest resolves the recipient only through `resolve_inbound_address_for_business`.
  - `func (s *Service) ReceiveResend(ctx context.Context, businessID uuid.UUID, eventID string, envelopeRecipients []string, raw []byte) error`. It returns nil for ingested, duplicate, loop-suppressed, and dropped messages. Any error means "ask Resend to retry". It satisfies `mailing.ResendInboundReceiver` (Task 6).
  - Ingest source label `inbox:resend` (from `Provider: "resend"`).

- [ ] **Step 1: Write the failing unit test**

Create `internal/inbox/resend_test.go`:

```go
package inbox

import (
	"fmt"
	"testing"
)

func TestResendCandidatesDedupesAndBounds(t *testing.T) {
	got := resendCandidates([]string{
		" Support+Tok@Example.com ", "support+Tok@example.com", "", "not-an-address", "other@example.com",
	})
	want := []string{"Support+Tok@Example.com", "other@example.com"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("candidates = %v, want %v (deduped on routing key + token, junk dropped, order kept)", got, want)
	}
	if got := resendCandidates([]string{"support+A@x.test", "support+B@x.test"}); len(got) != 2 {
		t.Fatalf("distinct tokens on one address collapsed: %v", got)
	}
	many := make([]string, maxResendCandidates+30)
	for i := range many {
		many[i] = fmt.Sprintf("u%d@x.test", i)
	}
	if got := resendCandidates(many); len(got) != maxResendCandidates {
		t.Fatalf("len = %d, want the %d cap", len(got), maxResendCandidates)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 -run TestResendCandidates ./internal/inbox/`
Expected: compile FAIL, `undefined: resendCandidates`.

- [ ] **Step 3: Add business-restricted resolution to Ingest**

In `internal/inbox/source.go`, add to `RawMessage` after `Raw []byte`:

```go
	// RestrictToBusiness, when non-nil, routes ONLY to a custom inbound address on
	// a verified domain owned by this business (spec 017 R4, Resend-received mail).
	// A recipient owned by any other business resolves as no-route.
	RestrictToBusiness uuid.UUID
```

and add `"github.com/google/uuid"` to its imports.

In `internal/inbox/resolve.go`, add `"errors"` to imports, change both `err == pgx.ErrNoRows` checks to `errors.Is(err, pgx.ErrNoRows)`, and append:

```go
// resolveRecipientForBusiness maps a normalized recipient to its route only when
// it is a custom address on a verified domain owned by businessID, via the
// resolve_inbound_address_for_business SECURITY DEFINER (0139). A miss — unknown,
// unverified, or owned by another business — is errNoRoute, never a distinguishable
// error (no oracle, and the same-business rule of spec 017 R4).
func resolveRecipientForBusiness(ctx context.Context, tx pgx.Tx, businessID uuid.UUID, address string) (route, error) {
	var (
		r           route
		addressID   uuid.UUID
		emailDomain *uuid.UUID
	)
	err := tx.QueryRow(ctx,
		"SELECT inbound_address_id, business_id, tenant_root_id, email_domain_id FROM resolve_inbound_address_for_business($1, $2)",
		businessID, address,
	).Scan(&addressID, &r.businessID, &r.tenantRootID, &emailDomain)
	if errors.Is(err, pgx.ErrNoRows) {
		return route{}, errNoRoute
	}
	if err != nil {
		return route{}, err
	}
	if emailDomain != nil {
		r.emailDomainID = *emailDomain
	}
	return r, nil
}
```

In `internal/inbox/service.go`, inside the `s.db.WithTx` closure, replace:

```go
		r, err := resolveRecipient(ctx, tx, normalizedAddr)
		if err != nil {
			return err // errNoRoute (no rows written) or a real DB error
		}
```

with:

```go
		var r route
		var err error
		if msg.RestrictToBusiness != uuid.Nil {
			r, err = resolveRecipientForBusiness(ctx, tx, msg.RestrictToBusiness, normalizedAddr)
		} else {
			r, err = resolveRecipient(ctx, tx, normalizedAddr)
		}
		if err != nil {
			return err // errNoRoute (no rows written) or a real DB error
		}
```

The later `attachmentsJSON, err := ...`, `authResults, err := ...`, and `ticketID, err := ...` lines stay as they are, because each introduces a new name. Go allows them to reuse `err`.

- [ ] **Step 4: Add the Resend source**

Create `internal/inbox/resend.go`:

```go
package inbox

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// maxResendCandidates bounds per-message recipient resolution work.
const maxResendCandidates = 50

// resendSource is the ingest source label suffix (p_source = "inbox:resend").
const resendSource = "resend"

// ReceiveResend ingests one raw message that Resend received for the business
// owning the receiving sending profile (spec 017 R2-R4).
//
// Recipients come from the raw message's To and Cc headers (R3). Only when none
// of them is owned does it consult envelopeRecipients — Resend's authenticated
// Receiving API received_for/to, never the webhook payload — so a Bcc'd support
// address still routes. Every candidate is resolved through
// resolve_inbound_address_for_business, so only custom addresses on verified
// domains owned by businessID can match (R4). The +token reply tag is read from
// the chosen address.
//
// It returns nil for ingested, duplicate, loop-suppressed, and dropped messages
// (a drop writes an audit entry). A non-nil error means the caller should ask
// Resend to retry.
func (s *Service) ReceiveResend(ctx context.Context, businessID uuid.UUID, eventID string, envelopeRecipients []string, raw []byte) error {
	parsed, perr := Parse(raw)
	if perr != nil {
		s.logger.WarnContext(ctx, "inbox: resend parse degraded", "event_id", eventID, "err", perr)
	}
	recipient, err := s.pickOwnedRecipient(ctx, businessID, resendCandidates(parsed.Recipients))
	if err != nil {
		return err
	}
	if recipient == "" {
		recipient, err = s.pickOwnedRecipient(ctx, businessID, resendCandidates(envelopeRecipients))
		if err != nil {
			return err
		}
	}
	if recipient == "" {
		return s.auditResendDrop(ctx, businessID, eventID, "no_owned_recipient")
	}
	_, err = s.Ingest(ctx, RawMessage{
		Provider:           resendSource,
		EnvelopeRecipient:  recipient,
		EnvelopeSender:     parsed.From.Address,
		ReceivedAt:         time.Now().UTC(),
		Raw:                raw,
		RestrictToBusiness: businessID,
	})
	if IsNoRoute(err) {
		// The address or its verification disappeared between pick and ingest.
		return s.auditResendDrop(ctx, businessID, eventID, "route_lost")
	}
	return err
}

// resendCandidates trims, drops non-addresses, de-duplicates on (routing key,
// token) keeping first-seen order and the original token case, and bounds the
// list.
func resendCandidates(addrs []string) []string {
	seen := make(map[string]bool, len(addrs))
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" || !strings.Contains(a, "@") {
			continue
		}
		normalized, token := normalizeRecipient(a)
		key := normalized + "\x00" + token
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
		if len(out) == maxResendCandidates {
			break
		}
	}
	return out
}

// pickOwnedRecipient returns the first candidate that resolves for businessID,
// or "" when none does. One short principal-less read transaction.
func (s *Service) pickOwnedRecipient(ctx context.Context, businessID uuid.UUID, candidates []string) (string, error) {
	if len(candidates) == 0 {
		return "", nil
	}
	var picked string
	err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
		for _, c := range candidates {
			normalized, _ := normalizeRecipient(c)
			_, rerr := resolveRecipientForBusiness(ctx, tx, businessID, normalized)
			if rerr == nil {
				picked = c
				return nil
			}
			if !errors.Is(rerr, errNoRoute) {
				return rerr
			}
		}
		return nil
	})
	return picked, err
}

// auditResendDrop records a dropped Resend-received message (R4). No recipient
// address is logged or audited.
func (s *Service) auditResendDrop(ctx context.Context, businessID uuid.UUID, eventID, reason string) error {
	s.logger.InfoContext(ctx, "inbox: resend message dropped", "business_id", businessID, "event_id", eventID, "reason", reason)
	return s.db.WithTx(ctx, func(tx pgx.Tx) error {
		var ok bool
		return tx.QueryRow(ctx, "SELECT support_audit_inbound_dropped($1, $2, $3, $4)",
			businessID, "inbox:"+resendSource, reason, eventID).Scan(&ok)
	})
}
```

- [ ] **Step 5: Run the unit test**

Run: `go test -count=1 ./internal/inbox/`
Expected: PASS.

- [ ] **Step 6: Write the failing integration tests**

Create `internal/inbox/resend_integration_test.go`:

```go
//go:build integration

package inbox

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

type resendTenant struct {
	business uuid.UUID
	domain   string
	address  string
}

// seedResendTenant: a business (with its system address) plus a provider_route
// domain and a custom support@ address on it.
func seedResendTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB, verified bool) resendTenant {
	t.Helper()
	ten := seedIngestTenant(ctx, t, tdb)
	domain := "support-" + ten.master.String()[:8] + ".example.com"
	domainID := uuid.New()
	var verifiedAt any
	if verified {
		verifiedAt = time.Now().UTC()
	}
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO email_domain
		(id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,dkim_selector,dkim_public_key,dkim_private_key_ref,spf_state,created_at,updated_at)
		VALUES ($1,$2,$2,$3,'provider_route','mf-verify=seed',$4,'mfseed','cHVi','cmVm','unknown',now(),now())`,
		domainID, ten.master, domain, verifiedAt); err != nil {
		t.Fatalf("seed email_domain: %v", err)
	}
	addr := "support@" + domain
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO inbound_address
		(id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		VALUES ($1,$2,$2,$3,'custom',$4,now(),now())`, uuid.New(), ten.master, addr, domainID); err != nil {
		t.Fatalf("seed inbound_address: %v", err)
	}
	return resendTenant{business: ten.master, domain: domain, address: addr}
}

func resendRaw(from, to, cc, subject, messageID, inReplyTo, contentType, body string) []byte {
	msg := "From: " + from + "\r\nTo: " + to + "\r\n"
	if cc != "" {
		msg += "Cc: " + cc + "\r\n"
	}
	msg += "Subject: " + subject + "\r\nDate: Mon, 02 Jan 2006 15:04:05 -0700\r\nMessage-ID: <" + messageID + ">\r\n"
	if inReplyTo != "" {
		msg += "In-Reply-To: <" + inReplyTo + ">\r\nReferences: <" + inReplyTo + ">\r\n"
	}
	msg += "MIME-Version: 1.0\r\nContent-Type: " + contentType + "\r\n\r\n" + body + "\r\n"
	return []byte(msg)
}

func ticketCount(ctx context.Context, t *testing.T, tdb *testdb.TestDB, business uuid.UUID) int {
	t.Helper()
	return countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", business)
}

func startResendDB(t *testing.T) (context.Context, *testdb.TestDB, *Service) {
	t.Helper()
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb, newIngestService(ctx, t, tdb)
}

// Review Focus 2: display names, several recipients, foreign and other-business
// addresses in To/Cc — routes once, into the receiving business only.
func TestReceiveResendRoutesOwnedRecipientAmongDisplayNamesAndForeignAddresses(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	other := seedResendTenant(ctx, t, tdb, true)
	raw := resendRaw("Ada Customer <ada@customer.test>",
		`"Someone Else" <help@foreign.example>, "Garden Support" <SUPPORT@`+own.domain+`>`,
		`"Other Biz" <`+other.address+`>`, "Need help", "rs-1@customer.test", "", "text/plain; charset=utf-8", "hello")
	if err := svc.ReceiveResend(ctx, own.business, "evt-1", nil, raw); err != nil {
		t.Fatalf("ReceiveResend: %v", err)
	}
	if n := ticketCount(ctx, t, tdb, own.business); n != 1 {
		t.Fatalf("own tickets = %d, want 1", n)
	}
	if n := ticketCount(ctx, t, tdb, other.business); n != 0 {
		t.Fatalf("other business tickets = %d, want 0 (same-business rule)", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM audit_entry WHERE business_id=$1 AND inputs->>'source'='inbox:resend'", own.business); n == 0 {
		t.Fatalf("no ingest audit with source inbox:resend")
	}
}

// R4: mail addressed only to another business is dropped with an audit entry
// and the uniform nil (same response as an unknown recipient).
func TestReceiveResendDropsMailForAnotherBusiness(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	other := seedResendTenant(ctx, t, tdb, true)
	raw := resendRaw("ada@customer.test", other.address, "", "wrong", "rs-2@customer.test", "", "text/plain", "x")
	if err := svc.ReceiveResend(ctx, own.business, "evt-cross", []string{other.address}, raw); err != nil {
		t.Fatalf("ReceiveResend: %v", err)
	}
	if ticketCount(ctx, t, tdb, own.business) != 0 || ticketCount(ctx, t, tdb, other.business) != 0 {
		t.Fatal("cross-business mail created a ticket")
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM audit_entry
		WHERE business_id=$1 AND action='ticket.inbound_dropped' AND inputs->>'event_id'='evt-cross'`, own.business); n != 1 {
		t.Fatalf("drop audits = %d, want 1", n)
	}
}

func TestReceiveResendDropsUnverifiedDomain(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, false)
	raw := resendRaw("ada@customer.test", own.address, "", "early", "rs-3@customer.test", "", "text/plain", "x")
	if err := svc.ReceiveResend(ctx, own.business, "evt-unverified", nil, raw); err != nil {
		t.Fatalf("ReceiveResend: %v", err)
	}
	if n := ticketCount(ctx, t, tdb, own.business); n != 0 {
		t.Fatalf("tickets = %d, want 0 for an unverified domain", n)
	}
}

// Scenario 4: a reply that drops In-Reply-To/References still threads by the
// +token on the chosen To address.
func TestReceiveResendThreadsByReplyTokenWithoutHeaders(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	first := resendRaw("ada@customer.test", own.address, "", "Order 12", "rs-4@customer.test", "", "text/plain", "first")
	if err := svc.ReceiveResend(ctx, own.business, "evt-4a", nil, first); err != nil {
		t.Fatal(err)
	}
	var ticketID uuid.UUID
	var token string
	if err := tdb.Super.QueryRow(ctx, "SELECT id, reply_token FROM ticket WHERE business_id=$1", own.business).Scan(&ticketID, &token); err != nil {
		t.Fatal(err)
	}
	reply := resendRaw("ada@customer.test", "Garden Support <support+"+token+"@"+own.domain+">", "",
		"totally different subject", "rs-5@customer.test", "", "text/plain", "second")
	if err := svc.ReceiveResend(ctx, own.business, "evt-4b", nil, reply); err != nil {
		t.Fatal(err)
	}
	if n := ticketCount(ctx, t, tdb, own.business); n != 1 {
		t.Fatalf("tickets = %d, want 1 (token threading)", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1", ticketID); n != 2 {
		t.Fatalf("messages = %d, want 2", n)
	}
}

// Review Focus 1: HTML-only mail must produce a readable body.
func TestReceiveResendHTMLOnlyMessageHasReadableBody(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	raw := resendRaw("ada@customer.test", own.address, "", "html", "rs-6@customer.test", "",
		"text/html; charset=utf-8", "<html><body><p>Hello <b>there</b>, my order is late.</p></body></html>")
	if err := svc.ReceiveResend(ctx, own.business, "evt-html", nil, raw); err != nil {
		t.Fatal(err)
	}
	var bodyText string
	var bodyHTML *string
	if err := tdb.Super.QueryRow(ctx, `SELECT COALESCE(m.body_text,''), m.body_html FROM ticket_message m
		JOIN ticket t ON t.id = m.ticket_id WHERE t.business_id=$1`, own.business).Scan(&bodyText, &bodyHTML); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodyText, "my order is late") || bodyHTML == nil {
		t.Fatalf("body_text = %q, body_html nil = %v; want readable text and the html kept", bodyText, bodyHTML == nil)
	}
}

// Review Focus 2: a Bcc'd support address (not in To/Cc) routes through the
// authenticated envelope list; another business's envelope address does not.
func TestReceiveResendFallsBackToEnvelopeForBcc(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	other := seedResendTenant(ctx, t, tdb, true)
	raw := resendRaw("ada@customer.test", "friend@foreign.example", "", "bcc", "rs-7@customer.test", "", "text/plain", "x")
	if err := svc.ReceiveResend(ctx, own.business, "evt-bcc", []string{own.address}, raw); err != nil {
		t.Fatal(err)
	}
	if n := ticketCount(ctx, t, tdb, own.business); n != 1 {
		t.Fatalf("own tickets = %d, want 1 via envelope fallback", n)
	}
	raw2 := resendRaw("ada@customer.test", "friend@foreign.example", "", "bcc2", "rs-8@customer.test", "", "text/plain", "x")
	if err := svc.ReceiveResend(ctx, own.business, "evt-bcc2", []string{other.address}, raw2); err != nil {
		t.Fatal(err)
	}
	if ticketCount(ctx, t, tdb, own.business) != 1 || ticketCount(ctx, t, tdb, other.business) != 0 {
		t.Fatal("envelope fallback routed to another business")
	}
}

func TestReceiveResendDuplicateRawIsIdempotent(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	raw := resendRaw("ada@customer.test", own.address, "", "dup", "rs-9@customer.test", "", "text/plain", "x")
	for i := 0; i < 2; i++ {
		if err := svc.ReceiveResend(ctx, own.business, "evt-dup", nil, raw); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM ticket_message m JOIN ticket t ON t.id=m.ticket_id
		WHERE t.business_id=$1`, own.business); n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
}
```

Add `"strings"` to that file's imports (used by the HTML test).

- [ ] **Step 7: Run the integration tests**

Run: `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestReceiveResend|TestIngest' ./internal/inbox/`
Expected: PASS. The HTML-only case passes because enmime v2.4.0 (`envelope.go:216`) fills `Envelope.Text` from the HTML part with `html2text` when there is no `text/plain` part, and `Parse` copies `env.Text` into `TextBody`. The test pins that behavior so a future `DisableTextConversion` option cannot silently regress it.

- [ ] **Step 8: Run the whole inbox and security suites**

Run: `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -p 1 ./internal/inbox/ ./internal/security_regression/`
Expected: PASS. In particular `MF-002-INGEST-SCOPE` and the threading/idempotency matrices are unchanged for non-Resend sources.

- [ ] **Step 9: Commit**

```bash
git add internal/inbox/source.go internal/inbox/resolve.go internal/inbox/service.go \
  internal/inbox/resend.go internal/inbox/resend_test.go internal/inbox/resend_integration_test.go
git commit -m "feat(inbox): ingest Resend-received mail with the same-business rule

Recipients come from raw To/Cc (envelope fallback for Bcc), restricted to
verified custom addresses owned by the profile's business; drops are audited."
```

---
### Task 6: Dispatch email.received with record-after-ingest (R1, R2, R5, R6)

**Files:**
- Modify: `internal/mailing/webhook.go` (`WebhookHandler` fields ~:30-38; `webhookContext` ~:56; `loadContext` SELECT ~:109-117)
- Modify: `internal/mailing/webhook_resend.go` (extract credential opening; branch on `email.received` ~:63)
- Create: `internal/mailing/resend_received.go`
- Modify: `internal/mailing/webhook_resend_test.go` (unit test for `hasPlusTag`)
- Create: `internal/mailing/resend_received_integration_test.go`
- Modify: `cmd/manyforge/main.go` (after `inboxSvc` is built ~:675, and next to the other background goroutines)

**Interfaces:**
- Consumes: `mailing_received_webhook_state`, `mailing_record_received_webhook`, `mailing_list_verified_resend_profiles` (Task 1). `mailprovider.Resend.{GetReceivedEmail, DownloadReceivedRaw, EnsureReceivingEvents}`, `ErrReceivedEmailTooLarge`, `ErrReceivedEmailGone` (Task 4). `inbox.Service.ReceiveResend` (Task 5), through the interface below.
- Produces:
  - `type ResendInboundReceiver interface { ReceiveResend(ctx context.Context, businessID uuid.UUID, eventID string, envelopeRecipients []string, raw []byte) error }`
  - New `WebhookHandler` fields: `Inbound ResendInboundReceiver`, `ResendBaseURL string`, `ResendHTTPClient *http.Client`, `RawDownloadClient *http.Client`, `AllowInsecureRawURL bool`, `MaxReceivedBytes int64`.
  - `func (h *WebhookHandler) UpgradeReceivingEvents(ctx context.Context) error`
  - HTTP behavior of `POST /api/v1/inbound/mailing/{profileID}/resend` for `email.received`:
    - bad signature → 401
    - already processed or ignored profile → 200 with no fetch
    - fetch/download/ingest/record failure → 503 with no delivery row
    - too large or gone → 200 with the row recorded
    - success → 200 with the row recorded after ingest

- [ ] **Step 1: Write the failing integration tests**

Create `internal/mailing/resend_received_integration_test.go`:

```go
//go:build integration

package mailing_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

type fakeInboundCall struct {
	businessID uuid.UUID
	eventID    string
	envelope   []string
	raw        []byte
}

type fakeInbound struct {
	mu    sync.Mutex
	calls []fakeInboundCall
	err   error
}

func (f *fakeInbound) ReceiveResend(_ context.Context, businessID uuid.UUID, eventID string, envelope []string, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeInboundCall{businessID, eventID, envelope, raw})
	return f.err
}

func (f *fakeInbound) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// fakeResend serves the Receiving API (metadata + pre-signed raw) and the
// webhook detail/patch endpoints the boot sweep uses.
type fakeResend struct {
	mu         sync.Mutex
	server     *httptest.Server
	raw        []byte
	metaStatus int
	metaHits   int
	auth       []string
	events     []string
	secret     string
	patches    int
}

func newFakeResend(t *testing.T, raw []byte) *fakeResend {
	t.Helper()
	f := &fakeResend{raw: raw, events: []string{"email.bounced", "email.complained"}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/emails/receiving/"):
			f.metaHits++
			f.auth = append(f.auth, r.Header.Get("Authorization"))
			if f.metaStatus != 0 {
				w.WriteHeader(f.metaStatus)
				return
			}
			id := strings.TrimPrefix(r.URL.Path, "/emails/receiving/")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": id, "to": []string{"support@support.example.test"},
				"received_for": []string{"support@support.example.test"}, "message_id": "<m@customer.test>",
				"raw": map[string]any{"download_url": f.server.URL + "/raw/" + id, "expires_at": "2099-01-01T00:00:00Z"},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/raw/"):
			if r.Header.Get("Authorization") != "" {
				t.Errorf("raw download carried Authorization")
			}
			_, _ = w.Write(f.raw)
		case r.Method == http.MethodGet && r.URL.Path == "/webhooks/wh_fixture":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "wh_fixture", "status": "enabled",
				"events": f.events, "signing_secret": f.secret})
		case r.Method == http.MethodPatch && r.URL.Path == "/webhooks/wh_fixture":
			f.patches++
			var body struct {
				Events []string `json:"events"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.events = body.Events
			_, _ = io.WriteString(w, `{"object":"webhook","id":"wh_fixture"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

type receivedHarness struct {
	ctx     context.Context
	tdb     *testdb.TestDB
	seed    mailingSeed
	fx      webhookFixture
	h       *mailing.WebhookHandler
	router  chi.Router
	inbound *fakeInbound
	fake    *fakeResend
	key     []byte
}

var testRaw = []byte("From: Ada <ada@customer.test>\r\nTo: Support <support@support.example.test>\r\nSubject: Help\r\nMessage-ID: <m@customer.test>\r\n\r\nhello\r\n")

func newReceivedHarness(t *testing.T) *receivedHarness {
	t.Helper()
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedMailingTenant(ctx, t, tdb)
	fx := seedWebhookFixture(ctx, t, tdb, seed)
	inbound := &fakeInbound{}
	fake := newFakeResend(t, testRaw)
	fake.secret = "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 32))
	h := mailing.NewWebhookHandler(tdb.App, fx.svc.Sealer, nil)
	h.Now = func() time.Time { return time.Unix(1_800_000_000, 0).UTC() }
	h.Inbound = inbound
	h.ResendBaseURL = fake.server.URL
	h.ResendHTTPClient = fake.server.Client()
	h.RawDownloadClient = fake.server.Client()
	h.AllowInsecureRawURL = true
	h.MaxReceivedBytes = 1 << 20
	router := chi.NewRouter()
	h.PublicRoutes(router)
	return &receivedHarness{ctx: ctx, tdb: tdb, seed: seed, fx: fx, h: h, router: router,
		inbound: inbound, fake: fake, key: bytes.Repeat([]byte{0x51}, 32)}
}

func (rh *receivedHarness) post(t *testing.T, eventID string, key []byte) int {
	t.Helper()
	body := []byte(`{"type":"email.received","created_at":"2026-10-10T12:00:00Z","data":{"email_id":"em_1","to":["support@support.example.test"]}}`)
	ts := fmt.Sprint(rh.h.Now().Unix())
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(eventID + "." + ts + "."))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+rh.fx.profileID.String()+"/resend", bytes.NewReader(body))
	req.Header.Set("svix-id", eventID)
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	rh.router.ServeHTTP(w, req)
	return w.Code
}

func (rh *receivedHarness) recorded(t *testing.T, eventID string) int {
	t.Helper()
	var n int
	if err := rh.tdb.Super.QueryRow(rh.ctx, `SELECT count(*) FROM mailing_provider_webhook_delivery
		WHERE profile_id=$1 AND external_event_id=$2 AND processing_status='applied'`, rh.fx.profileID, eventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (rh *receivedHarness) setCredential(t *testing.T, apiKey string, webhookKey []byte) {
	t.Helper()
	secret := "whsec_" + base64.StdEncoding.EncodeToString(webhookKey)
	sealed, err := rh.fx.svc.Sealer.Seal([]byte(fmt.Sprintf(
		`{"version":2,"api_key":%q,"webhook_id":"wh_fixture","webhook_secret":%q}`, apiKey, secret)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rh.tdb.Super.Exec(rh.ctx, `UPDATE secret SET sealed_value=$1
		WHERE id=(SELECT secret_ref FROM mailing_sending_profile WHERE id=$2)`, sealed, rh.fx.profileID); err != nil {
		t.Fatal(err)
	}
}

func TestResendReceivedIngestsThenRecords(t *testing.T) {
	rh := newReceivedHarness(t)
	if code := rh.post(t, "evt-r1", rh.key); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if rh.inbound.count() != 1 {
		t.Fatalf("ingest calls = %d, want 1", rh.inbound.count())
	}
	call := rh.inbound.calls[0]
	if call.businessID != rh.seed.businessID || call.eventID != "evt-r1" || !bytes.Equal(call.raw, testRaw) ||
		len(call.envelope) == 0 || call.envelope[0] != "support@support.example.test" {
		t.Fatalf("ingest call = %+v", call)
	}
	if rh.recorded(t, "evt-r1") != 1 {
		t.Fatal("delivery row not recorded after a successful ingest")
	}
	if code := rh.post(t, "evt-r1", rh.key); code != http.StatusOK || rh.inbound.count() != 1 || rh.fake.metaHits != 1 {
		t.Fatalf("redelivery status=%d ingests=%d fetches=%d, want 200/1/1", code, rh.inbound.count(), rh.fake.metaHits)
	}
}

// R5: a fetch failure answers 5xx so Resend retries, and is NOT recorded, so the
// retry is processed instead of discarded as a duplicate.
func TestResendReceivedFetchFailureIsRetriedNotRecorded(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.fake.metaStatus = http.StatusInternalServerError
	if code := rh.post(t, "evt-r2", rh.key); code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if rh.recorded(t, "evt-r2") != 0 || rh.inbound.count() != 0 {
		t.Fatal("failed fetch was recorded or ingested")
	}
	rh.fake.metaStatus = 0
	if code := rh.post(t, "evt-r2", rh.key); code != http.StatusOK || rh.inbound.count() != 1 || rh.recorded(t, "evt-r2") != 1 {
		t.Fatalf("retry status=%d ingests=%d recorded=%d, want 200/1/1", code, rh.inbound.count(), rh.recorded(t, "evt-r2"))
	}
}

func TestResendReceivedIngestFailureIsRetried(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.inbound.err = errors.New("database unavailable")
	if code := rh.post(t, "evt-r3", rh.key); code != http.StatusServiceUnavailable || rh.recorded(t, "evt-r3") != 0 {
		t.Fatalf("status=%d recorded=%d, want 503/0", code, rh.recorded(t, "evt-r3"))
	}
	rh.inbound.err = nil
	if code := rh.post(t, "evt-r3", rh.key); code != http.StatusOK || rh.recorded(t, "evt-r3") != 1 {
		t.Fatalf("retry status=%d recorded=%d, want 200/1", code, rh.recorded(t, "evt-r3"))
	}
}

// R6 / Review Focus 4: an oversized raw message is acknowledged (and recorded so
// Resend stops retrying) without ingesting.
func TestResendReceivedOversizedRawIsAcknowledged(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.h.MaxReceivedBytes = 16
	if code := rh.post(t, "evt-r4", rh.key); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if rh.inbound.count() != 0 || rh.recorded(t, "evt-r4") != 1 {
		t.Fatalf("ingests=%d recorded=%d, want 0/1", rh.inbound.count(), rh.recorded(t, "evt-r4"))
	}
}

// Review Focus 5: the fetch uses the CURRENT API key after rotation, and an
// event signed with the retired webhook secret is rejected.
func TestResendReceivedUsesCurrentCredentialAfterRotation(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.setCredential(t, "re_rotated", rh.key)
	if code := rh.post(t, "evt-r5", rh.key); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got := rh.fake.auth[len(rh.fake.auth)-1]; got != "Bearer re_rotated" {
		t.Fatalf("fetch authorization = %q, want the rotated key", got)
	}
	newKey := bytes.Repeat([]byte{0x52}, 32)
	rh.setCredential(t, "re_rotated", newKey)
	if code := rh.post(t, "evt-r6", rh.key); code != http.StatusUnauthorized {
		t.Fatalf("retired-secret status = %d, want 401", code)
	}
	if code := rh.post(t, "evt-r6", newKey); code != http.StatusOK {
		t.Fatalf("new-secret status = %d, want 200", code)
	}
}

func TestResendReceivedIgnoredForUnverifiedProfile(t *testing.T) {
	rh := newReceivedHarness(t)
	if _, err := rh.tdb.Super.Exec(rh.ctx, `UPDATE mailing_sending_profile SET status='unverified' WHERE id=$1`, rh.fx.profileID); err != nil {
		t.Fatal(err)
	}
	if code := rh.post(t, "evt-r7", rh.key); code != http.StatusOK || rh.fake.metaHits != 0 || rh.inbound.count() != 0 {
		t.Fatalf("status=%d fetches=%d ingests=%d, want 200/0/0", code, rh.fake.metaHits, rh.inbound.count())
	}
}

// R1: the boot sweep adds email.received to a legacy webhook in place and keeps
// its signing secret; a second run is read-only.
func TestUpgradeReceivingEventsPatchesLegacyWebhook(t *testing.T) {
	rh := newReceivedHarness(t)
	if err := rh.h.UpgradeReceivingEvents(rh.ctx); err != nil {
		t.Fatal(err)
	}
	if rh.fake.patches != 1 || !strings.Contains(strings.Join(rh.fake.events, ","), "email.received") {
		t.Fatalf("patches=%d events=%v", rh.fake.patches, rh.fake.events)
	}
	if err := rh.h.UpgradeReceivingEvents(rh.ctx); err != nil || rh.fake.patches != 1 {
		t.Fatalf("second sweep patches=%d err=%v, want read-only", rh.fake.patches, err)
	}
}
```

Append to `internal/mailing/webhook_resend_test.go`:

```go
func TestHasPlusTag(t *testing.T) {
	if !hasPlusTag([]string{"a@x.test", "support+Tok@x.test"}) {
		t.Fatal("plus tag not detected")
	}
	if hasPlusTag([]string{"a@x.test", "+@", "no-at-sign+x"}) {
		t.Fatal("false positive")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run TestHasPlusTag ./internal/mailing/` and
`DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestResendReceived|TestUpgradeReceivingEvents' ./internal/mailing/`
Expected: compile FAIL: `h.Inbound undefined`, `undefined: hasPlusTag`, `h.UpgradeReceivingEvents undefined`.

- [ ] **Step 3: Extend the handler and context**

In `internal/mailing/webhook.go`, add the interface above `type WebhookHandler struct`:

```go
// ResendInboundReceiver ingests a raw message Resend received for one business
// (spec 017). *inbox.Service implements it. A nil error covers ingested,
// duplicate, and dropped messages; an error asks Resend to retry.
type ResendInboundReceiver interface {
	ReceiveResend(ctx context.Context, businessID uuid.UUID, eventID string, envelopeRecipients []string, raw []byte) error
}
```

Add to `WebhookHandler`, after `Now func() time.Time`:

```go
	// Spec 017 inbound support mail (email.received). Inbound nil ⇒ email.received
	// answers 503 so Resend retries until the deployment is wired.
	Inbound             ResendInboundReceiver
	ResendBaseURL       string       // empty ⇒ https://api.resend.com
	ResendHTTPClient    *http.Client // Receiving API + webhook calls
	RawDownloadClient   *http.Client // pre-signed raw URL fetch; production: netsafe client
	AllowInsecureRawURL bool         // tests only
	MaxReceivedBytes    int64        // MANYFORGE_INBOUND_MAX_BYTES
```

Add `businessID uuid.UUID` to `webhookContext` right after `profileID uuid.UUID`, and change `loadContext`'s query and scan to:

```go
		err := tx.QueryRow(ctx, `
			SELECT profile_id, business_id, provider, credential_sealed, sns_topic_arn,
			       ses_region, ses_configuration_set, feedback_status, updated_at
			FROM mailing_webhook_context($1)`, profileID).Scan(
			&out.profileID, &out.businessID, &out.provider, &out.credentialSealed, &out.snsTopicARN,
			&out.sesRegion, &out.sesConfigurationSet, &out.feedbackStatus, &out.updatedAt,
		)
```

- [ ] **Step 4: Extract credential opening and branch on email.received**

In `internal/mailing/webhook_resend.go`, add:

```go
// openResendCredentials unseals and validates a profile's v2 Resend bundle.
// Failures are logged without secret material and collapse to unauthorized.
func (h *WebhookHandler) openResendCredentials(ctx context.Context, wc webhookContext) (resendStoredCredentials, error) {
	if wc.provider != "resend" || wc.credentialSealed == nil || h.Sealer == nil {
		return resendStoredCredentials{}, errWebhookUnauthorized
	}
	credential, err := h.Sealer.Open(*wc.credentialSealed)
	if err != nil {
		h.logger().ErrorContext(ctx, "mailing Resend webhook credential unseal failed", "profile_id", wc.profileID)
		return resendStoredCredentials{}, errWebhookUnauthorized
	}
	defer clear(credential)
	var creds resendStoredCredentials
	if json.Unmarshal(credential, &creds) != nil || creds.Version != 2 ||
		strings.TrimSpace(creds.WebhookID) == "" || creds.WebhookSecret == "" {
		h.logger().ErrorContext(ctx, "mailing Resend webhook credential is invalid", "profile_id", wc.profileID)
		return resendStoredCredentials{}, errWebhookUnauthorized
	}
	return creds, nil
}
```

(add `"context"` to the imports). Then rewrite `handleResend` as:

```go
func (h *WebhookHandler) handleResend(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	profileID, err := h.profileID(r)
	if err != nil {
		h.unauthorized(w)
		return
	}
	wc, err := h.loadContext(r.Context(), profileID)
	if err != nil {
		h.unauthorized(w)
		return
	}
	creds, err := h.openResendCredentials(r.Context(), wc)
	if err != nil {
		h.unauthorized(w)
		return
	}
	eventID := r.Header.Get("svix-id")
	if err := verifySvix(creds.WebhookSecret, eventID, r.Header.Get("svix-timestamp"),
		r.Header.Get("svix-signature"), body, h.now()); err != nil {
		h.unauthorized(w)
		return
	}

	var payload resendWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logger().WarnContext(r.Context(), "mailing Resend webhook payload decode failed", "profile_id", profileID)
		h.authenticatedOK(w)
		return
	}
	if payload.Type == "email.received" {
		h.handleResendReceived(w, r, wc, creds.APIKey, eventID, payload)
		return
	}
	events := mapResendEvent(payload)
	if err := h.recordAndApply(r.Context(), wc, "resend", eventID, body, events); err != nil {
		h.logger().ErrorContext(r.Context(), "mailing Resend webhook apply failed", "profile_id", profileID, "event_type", payload.Type, "err", err)
		h.retryableFailure(w)
		return
	}
	h.authenticatedOK(w)
}
```

- [ ] **Step 5: Implement the email.received flow and the sweep**

Create `internal/mailing/resend_received.go`:

```go
package mailing

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
)

func (h *WebhookHandler) resendClient(apiKey string) *mailprovider.Resend {
	return &mailprovider.Resend{
		APIKey: apiKey, BaseURL: h.ResendBaseURL, Client: h.ResendHTTPClient,
		RawClient: h.RawDownloadClient, AllowInsecureRawURL: h.AllowInsecureRawURL,
	}
}

// handleResendReceived processes a signature-valid email.received (spec 017).
// Order matters for R5: fetch → download → ingest → record. Any failure before
// the record answers 503 and writes nothing, so Resend's retry is processed; a
// retry after ingest-but-before-record re-ingests and is absorbed by ingest's
// (tenant_root_id, message_id) dedupe.
func (h *WebhookHandler) handleResendReceived(w http.ResponseWriter, r *http.Request, wc webhookContext, apiKey, eventID string, payload resendWebhook) {
	ctx := r.Context()
	if h.Inbound == nil {
		h.logger().WarnContext(ctx, "mailing Resend email.received but support ingest is not wired", "profile_id", wc.profileID)
		h.retryableFailure(w)
		return
	}
	state, err := h.receivedState(ctx, wc.profileID, eventID)
	if err != nil {
		h.logger().ErrorContext(ctx, "mailing Resend received state lookup failed", "profile_id", wc.profileID, "err", err)
		h.retryableFailure(w)
		return
	}
	if state != "new" {
		h.authenticatedOK(w) // seen (already processed) or ignored (unverified/fenced profile)
		return
	}
	if strings.TrimSpace(payload.Data.EmailID) == "" {
		h.logger().WarnContext(ctx, "mailing Resend email.received without email_id", "profile_id", wc.profileID, "event_id", eventID)
		h.finishReceived(w, r, wc, eventID)
		return
	}
	client := h.resendClient(apiKey)
	email, err := client.GetReceivedEmail(ctx, payload.Data.EmailID, h.MaxReceivedBytes)
	if h.skipOrRetry(w, r, wc, eventID, "fetch", err) {
		return
	}
	raw, err := client.DownloadReceivedRaw(ctx, email.RawURL, h.MaxReceivedBytes)
	if h.skipOrRetry(w, r, wc, eventID, "download", err) {
		return
	}
	// Runbook observation (spec 017 open fact): does Resend keep +tags in `to`?
	h.logger().InfoContext(ctx, "mailing Resend inbound recipients observed",
		"profile_id", wc.profileID, "event_id", eventID,
		"webhook_to_has_plus_tag", hasPlusTag(payload.Data.To), "api_to_has_plus_tag", hasPlusTag(email.To))
	envelope := make([]string, 0, len(email.ReceivedFor)+len(email.To))
	envelope = append(append(envelope, email.ReceivedFor...), email.To...)
	if err := h.Inbound.ReceiveResend(ctx, wc.businessID, eventID, envelope, raw); err != nil {
		h.logger().ErrorContext(ctx, "mailing Resend inbound ingest failed", "profile_id", wc.profileID, "event_id", eventID, "err", err)
		h.retryableFailure(w)
		return
	}
	h.finishReceived(w, r, wc, eventID)
}

// skipOrRetry handles a fetch/download error. Too-large and gone are permanent:
// acknowledge and record (R6). Anything else is transient: 503, nothing recorded
// (R5). It reports whether the response has been written.
func (h *WebhookHandler) skipOrRetry(w http.ResponseWriter, r *http.Request, wc webhookContext, eventID, stage string, err error) bool {
	if err == nil {
		return false
	}
	ctx := r.Context()
	if errors.Is(err, mailprovider.ErrReceivedEmailTooLarge) || errors.Is(err, mailprovider.ErrReceivedEmailGone) {
		reason := "too_large"
		if errors.Is(err, mailprovider.ErrReceivedEmailGone) {
			reason = "gone"
		}
		h.logger().WarnContext(ctx, "mailing Resend received email not ingested",
			"profile_id", wc.profileID, "event_id", eventID, "stage", stage, "reason", reason)
		h.finishReceived(w, r, wc, eventID)
		return true
	}
	h.logger().WarnContext(ctx, "mailing Resend received email "+stage+" failed",
		"profile_id", wc.profileID, "event_id", eventID, "err", err)
	h.retryableFailure(w)
	return true
}

func (h *WebhookHandler) finishReceived(w http.ResponseWriter, r *http.Request, wc webhookContext, eventID string) {
	if err := h.recordReceived(r.Context(), wc.profileID, eventID); err != nil {
		h.logger().ErrorContext(r.Context(), "mailing Resend received record failed", "profile_id", wc.profileID, "event_id", eventID, "err", err)
		h.retryableFailure(w)
		return
	}
	h.authenticatedOK(w)
}

func (h *WebhookHandler) receivedState(ctx context.Context, profileID uuid.UUID, eventID string) (string, error) {
	var state string
	err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT mailing_received_webhook_state($1, $2)", profileID, eventID).Scan(&state)
	})
	return state, err
}

func (h *WebhookHandler) recordReceived(ctx context.Context, profileID uuid.UUID, eventID string) error {
	return h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var recorded bool
		return tx.QueryRow(ctx, "SELECT mailing_record_received_webhook($1, $2)", profileID, eventID).Scan(&recorded)
	})
}

// hasPlusTag reports whether any address's local part carries a +tag. Only the
// boolean is logged, never the address.
func hasPlusTag(addrs []string) bool {
	for _, a := range addrs {
		at := strings.LastIndexByte(a, '@')
		if at > 0 && strings.Contains(a[:at], "+") && at > strings.IndexByte(a, '+')+1 {
			return true
		}
	}
	return false
}

// UpgradeReceivingEvents adds email.received to every verified Resend profile's
// provisioned webhook in place (spec 017 R1: no operator action). Idempotent and
// safe on every boot from every replica; per-profile failures are logged and do
// not stop the sweep. A webhook whose signing secret changed is reported, because
// the stored secret would no longer verify its events.
func (h *WebhookHandler) UpgradeReceivingEvents(ctx context.Context) error {
	var ids []uuid.UUID
	if err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT profile_id FROM mailing_list_verified_resend_profiles($1)", 1000)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	for _, id := range ids {
		if err := h.upgradeProfileEvents(ctx, id); err != nil {
			h.logger().WarnContext(ctx, "mailing Resend receiving upgrade failed", "profile_id", id, "err", err)
		}
	}
	return nil
}

func (h *WebhookHandler) upgradeProfileEvents(ctx context.Context, profileID uuid.UUID) error {
	wc, err := h.loadContext(ctx, profileID)
	if err != nil {
		return err
	}
	creds, err := h.openResendCredentials(ctx, wc)
	if err != nil {
		return err
	}
	secret, patched, err := h.resendClient(creds.APIKey).EnsureReceivingEvents(ctx, creds.WebhookID)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(creds.WebhookSecret)) != 1 {
		return errors.New("mailing: Resend webhook signing secret changed; verify the sending profile again")
	}
	if patched {
		h.logger().InfoContext(ctx, "mailing Resend webhook subscribed to email.received", "profile_id", profileID)
	}
	return nil
}
```

- [ ] **Step 6: Run the tests**

Run: `go test -count=1 ./internal/mailing/...` and
`DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestResendReceived|TestUpgradeReceivingEvents|TestResendWebhook|TestLegacyResend|TestMFMailWebhook|TestAuthenticatedWebhook' ./internal/mailing/`
Expected: PASS, both the new email.received tests and the existing bounce/complaint webhook tests (the refactor kept their 401/503 behavior).

- [ ] **Step 7: Wire it in main**

In `cmd/manyforge/main.go`, directly after the `inboxWebhookH.Metrics = metrics` line, add:

```go
	// Spec 017: Resend-received support mail. The per-profile Resend webhook
	// (mounted in the public ingress group) fetches the raw message with the
	// profile's own credential and ingests it through inboxSvc. The raw URL is
	// pre-signed and caller-influenced, so it is fetched with an SSRF-guarded client.
	if mailingWebhookH != nil {
		mailingWebhookH.Inbound = inboxSvc
		mailingWebhookH.ResendHTTPClient = &http.Client{Timeout: 30 * time.Second}
		mailingWebhookH.RawDownloadClient = netsafe.NewClient(60 * time.Second)
		mailingWebhookH.MaxReceivedBytes = cfg.InboundMaxBytes
		go func() {
			if err := mailingWebhookH.UpgradeReceivingEvents(ctx); err != nil {
				logger.Warn("mailing Resend receiving upgrade sweep failed", "err", err)
			}
		}()
	}
```

Add `"github.com/manyforge/manyforge/internal/platform/netsafe"` to main's imports if it is not already there (`grep -n netsafe cmd/manyforge/main.go`).

Run: `go build ./... && go vet ./cmd/... && go test -tags contract -count=1 ./cmd/...`
Expected: PASS. No routes changed, so there is no OpenAPI drift.

- [ ] **Step 8: Commit**

```bash
git add internal/mailing/webhook.go internal/mailing/webhook_resend.go internal/mailing/resend_received.go \
  internal/mailing/webhook_resend_test.go internal/mailing/resend_received_integration_test.go cmd/manyforge/main.go
git commit -m "feat(mailing): ingest Resend email.received and record only after ingest

Fetch/download/ingest failures answer 503 with nothing recorded; oversized
or gone messages are acknowledged. A boot sweep adds email.received to
existing webhooks in place."
```

---
### Task 7: Send support replies through the business's Resend profile (R10-R13)

**Files:**
- Create: `internal/mailing/support_reply.go`
- Create: `internal/mailing/support_reply_integration_test.go`
- Modify: `internal/platform/notify/sender_subscriber.go` (interfaces + fields ~:52-66; `Handle` ~:110-170; new helpers at the end)
- Create: `internal/platform/notify/final_attempt_test.go` (unit)
- Create: `internal/platform/notify/resend_support_integration_test.go`
- Modify: `cmd/manyforge/main.go:746-750` (`sendSub` wiring)

**Interfaces:**
- Consumes: `get_resend_support_identity(uuid, uuid)` (Task 1). `events.Event.{Attempts, MaxAttempts}` (existing, stamped by `events.Worker`). `mailing.Service.resolveSystemProfile` and `mailing.Service.Providers` (existing).
- Produces:
  - `var mailing.ErrSupportProfileUnavailable`
  - `func (s *mailing.Service) SendSupportReply(ctx context.Context, profileID uuid.UUID, m notify.Mail) error`
  - `notify.SupportProfileSender` interface: `SendSupportReply(ctx context.Context, profileID uuid.UUID, m Mail) error`
  - `notify.RecipientSuppression` interface: `IsSuppressed(ctx context.Context, email string) (bool, error)` (`notify.DBSuppression` already satisfies it)
  - New `SendSubscriber` fields: `ProfileSender SupportProfileSender` and `Suppression RecipientSuppression`
  - Behavior: a business with a Resend support identity never uses `SendSubscriber.Sender`. `From` is the custom address and `Reply-To` is `local+<token>@domain` with the token's case preserved. A failure on the final attempt (`Attempts+1 >= MaxAttempts`) marks the reply `failed` and returns nil, on both the Resend path and the shared path. An earlier failure returns an error and stays `pending`.

- [ ] **Step 1: Write the failing notify unit test**

Create `internal/platform/notify/final_attempt_test.go`:

```go
package notify

import (
	"testing"

	"github.com/manyforge/manyforge/internal/platform/events"
)

func TestIsFinalAttempt(t *testing.T) {
	for _, tc := range []struct {
		attempts, max int32
		want          bool
	}{
		{0, 0, false}, // in-process bus: no retry budget, never short-circuit
		{0, 10, false},
		{8, 10, false},
		{9, 10, true},
		{12, 10, true},
	} {
		if got := isFinalAttempt(events.Event{Attempts: tc.attempts, MaxAttempts: tc.max}); got != tc.want {
			t.Errorf("isFinalAttempt(%d/%d) = %v, want %v", tc.attempts, tc.max, got, tc.want)
		}
	}
}
```

Run: `go test -count=1 -run TestIsFinalAttempt ./internal/platform/notify/`
Expected: compile FAIL, `undefined: isFinalAttempt`.

- [ ] **Step 2: Write the failing notify integration tests**

Create `internal/platform/notify/resend_support_integration_test.go`:

```go
//go:build integration

package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
)

type captureProfileSender struct {
	calls     int
	profileID uuid.UUID
	last      *Mail
	err       error
}

func (c *captureProfileSender) SendSupportReply(_ context.Context, profileID uuid.UUID, m Mail) error {
	c.calls++
	c.profileID = profileID
	c.last = &m
	return c.err
}

type staticSuppression struct{ suppressed bool }

func (s staticSuppression) IsSuppressed(context.Context, string) (bool, error) { return s.suppressed, nil }

// seedResendIdentity adds a verified provider_route domain, a custom support@
// address, and a resend-mode profile in the given status to a seeded sendTenant.
func seedResendIdentity(ctx context.Context, t *testing.T, tdb *testdb.TestDB, st sendTenant, profileStatus string) (customAddr string, profileID uuid.UUID) {
	t.Helper()
	domain := "support-" + st.master.String()[:8] + ".example.com"
	customAddr = "support@" + domain
	domainID, secretID := uuid.New(), uuid.New()
	profileID = uuid.New()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO email_domain (id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,spf_state,created_at,updated_at)
		  VALUES ($1,$2,$2,$3,'provider_route','mf-verify=seed',now(),'unknown',now(),now())`, []any{domainID, st.master, domain}},
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		  VALUES ($1,$2,$2,$3,'custom',$4,now(),now())`, []any{uuid.New(), st.master, customAddr, domainID}},
		{`INSERT INTO secret (id,business_id,tenant_root_id,scope,sealed_value,created_at,updated_at)
		  VALUES ($1,$2,$2,'mailing','sealed',now(),now())`, []any{secretID, st.master}},
		{`INSERT INTO mailing_sending_profile (id,business_id,tenant_root_id,mode,from_email,from_name,secret_ref,status,feedback_status)
		  VALUES ($1,$2,$2,'resend',$3,'Support',$4,$5,'ready')`, []any{profileID, st.master, customAddr, secretID, profileStatus}},
	}
	for _, s := range stmts {
		if _, err := tdb.Super.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed resend identity: %v\nSQL: %s", err, s.sql)
		}
	}
	return customAddr, profileID
}

func handleWith(ctx context.Context, t *testing.T, tdb *testdb.TestDB, sub SendSubscriber, ev events.Event) error {
	t.Helper()
	return tdb.App.WithTx(ctx, func(tx pgx.Tx) error { return sub.Handle(ctx, tx, ev) })
}

// R10: From is the custom address, Reply-To its plus-addressed token form, and the
// shared transport is never touched.
func TestSendViaResendSupportProfile(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	customAddr, profileID := seedResendIdentity(ctx, t, tdb, st, "verified")
	shared := &captureSender{}
	profile := &captureProfileSender{}
	sub := SendSubscriber{Sender: shared, ProfileSender: profile, Suppression: staticSuppression{}}

	if err := handleWith(ctx, t, tdb, sub, repliedEvent(t, st)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if shared.calls != 0 {
		t.Fatalf("shared transport used %d times, want 0", shared.calls)
	}
	if profile.calls != 1 || profile.profileID != profileID {
		t.Fatalf("profile sender calls=%d profile=%v, want 1/%v", profile.calls, profile.profileID, profileID)
	}
	wantReplyTo := "support+" + st.replyToken + "@" + customAddr[len("support@"):]
	if profile.last.From != customAddr || profile.last.ReplyTo != wantReplyTo || profile.last.DKIM != nil {
		t.Fatalf("mail From=%q ReplyTo=%q DKIM=%v, want %q / %q / nil", profile.last.From, profile.last.ReplyTo, profile.last.DKIM, customAddr, wantReplyTo)
	}
	if profile.last.MessageID != st.rfcMessageID || profile.last.InReplyTo != "parent-1@example.com" || profile.last.BodyText != st.replyBody {
		t.Fatalf("threading/body not carried: %+v", profile.last)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "sent" {
		t.Fatalf("delivery_state = %q, want sent", got)
	}
}

// R11 + R13: a failing profile retries (pending) and, on the final attempt, marks
// failed — never falling back to the shared transport.
func TestSendResendProfileFailureRetriesThenFails(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	seedResendIdentity(ctx, t, tdb, st, "verified")
	shared := &captureSender{}
	profile := &captureProfileSender{err: errors.New("provider: http 403 validation_error: ")}
	sub := SendSubscriber{Sender: shared, ProfileSender: profile}

	ev := repliedEvent(t, st)
	ev.Attempts, ev.MaxAttempts = 0, 3
	if err := handleWith(ctx, t, tdb, sub, ev); err == nil {
		t.Fatal("non-final failure returned nil, want an error so the worker retries")
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "pending" {
		t.Fatalf("after non-final failure delivery_state = %q, want pending", got)
	}
	ev.Attempts = 2
	if err := handleWith(ctx, t, tdb, sub, ev); err != nil {
		t.Fatalf("final failure returned %v, want nil (terminal outcome recorded)", err)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "failed" {
		t.Fatalf("after final failure delivery_state = %q, want failed", got)
	}
	if shared.calls != 0 {
		t.Fatalf("shared transport used %d times, want 0 (no fallback)", shared.calls)
	}
}

// R11: an unverified profile does not fall back either.
func TestSendResendProfileUnverifiedNoFallback(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	seedResendIdentity(ctx, t, tdb, st, "error")
	shared := &captureSender{}
	profile := &captureProfileSender{}
	sub := SendSubscriber{Sender: shared, ProfileSender: profile}
	ev := repliedEvent(t, st)
	ev.Attempts, ev.MaxAttempts = 9, 10
	if err := handleWith(ctx, t, tdb, sub, ev); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if shared.calls != 0 || profile.calls != 0 {
		t.Fatalf("shared=%d profile=%d, want 0/0 for an unverified profile", shared.calls, profile.calls)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "failed" {
		t.Fatalf("delivery_state = %q, want failed", got)
	}
}

func TestSendResendSuppressedRecipient(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	seedResendIdentity(ctx, t, tdb, st, "verified")
	profile := &captureProfileSender{}
	sub := SendSubscriber{Sender: &captureSender{}, ProfileSender: profile, Suppression: staticSuppression{suppressed: true}}
	if err := handleWith(ctx, t, tdb, sub, repliedEvent(t, st)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if profile.calls != 0 || deliveryState(ctx, t, tdb, st.messageRowID) != "failed" {
		t.Fatalf("suppressed recipient: profile calls=%d state=%q, want 0/failed", profile.calls, deliveryState(ctx, t, tdb, st.messageRowID))
	}
}

// R12 + R13 on the shared path: no Resend identity keeps the shared transport,
// and its final failed attempt is marked failed instead of dead-lettering pending.
func TestSendSharedTransportFinalAttemptMarksFailed(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	shared := &captureSender{err: errors.New("relay down")}
	profile := &captureProfileSender{}
	sub := SendSubscriber{Sender: shared, ProfileSender: profile}
	ev := repliedEvent(t, st)
	ev.Attempts, ev.MaxAttempts = 9, 10
	if err := handleWith(ctx, t, tdb, sub, ev); err != nil {
		t.Fatalf("final shared failure returned %v, want nil", err)
	}
	if shared.calls != 1 || profile.calls != 0 {
		t.Fatalf("shared=%d profile=%d, want 1/0", shared.calls, profile.calls)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "failed" {
		t.Fatalf("delivery_state = %q, want failed", got)
	}
}
```

Run: `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestSendViaResend|TestSendResend|TestSendSharedTransport' ./internal/platform/notify/`
Expected: compile FAIL, `unknown field ProfileSender in struct literal of type SendSubscriber`.

- [ ] **Step 3: Implement the subscriber changes**

In `internal/platform/notify/sender_subscriber.go`, add after the `KeySealer` interface:

```go
// SupportProfileSender sends one support reply through a business-owned provider
// profile (spec 017 R10). *mailing.Service implements it. Returned errors carry
// no provider response bodies.
type SupportProfileSender interface {
	SendSupportReply(ctx context.Context, profileID uuid.UUID, m Mail) error
}

// RecipientSuppression reports whether a recipient is hard-bounced/suppressed.
// DBSuppression implements it.
type RecipientSuppression interface {
	IsSuppressed(ctx context.Context, email string) (bool, error)
}
```

Add fields to `SendSubscriber` after `Metrics`:

```go
	// ProfileSender delivers replies for businesses with a Resend support identity
	// (spec 017). nil ⇒ such replies fail (they NEVER fall back to Sender, R11).
	ProfileSender SupportProfileSender

	// Suppression re-checks the recipient on the profile path (the shared Sender
	// enforces suppression itself). nil ⇒ no send-time re-check.
	Suppression RecipientSuppression
```

In `Handle`, immediately before `s.selectCustomIdentity(ctx, tx, &mail, p.BusinessID, e.TenantRootID)`, insert:

```go
	// Spec 017: a business with a Resend support identity replies through its own
	// profile from its custom address. That path never falls back to the shared
	// transport (R11): a different From domain would break threading and alignment.
	identity, hasIdentity, err := s.resendSupportIdentity(ctx, tx, p.BusinessID, e.TenantRootID)
	if err != nil {
		return fmt.Errorf("notify: send: load resend support identity: %w", err)
	}
	if hasIdentity {
		return s.sendViaSupportProfile(ctx, tx, e, p, mail, identity)
	}
```

Replace the transient branch at the end of the `if serr := s.Sender.Send(...)` block, which is currently:

```go
		s.Metrics.Inc(observability.MetricOutboundFailed)
		return fmt.Errorf("notify: send: dispatch: %w", serr)
```

with:

```go
		s.Metrics.Inc(observability.MetricOutboundFailed)
		if isFinalAttempt(e) {
			// R13: record the terminal outcome so the UI shows Delivery failed instead
			// of a reply stuck pending after the worker dead-letters the event.
			return s.mark(ctx, tx, p.MessageRowID, e.TenantRootID, "failed", "delivery failed after retries")
		}
		return fmt.Errorf("notify: send: dispatch: %w", serr)
```

Append to the file:

```go
// resendSupportIdentity is one get_resend_support_identity row.
type resendSupportIdentity struct {
	profileID     uuid.UUID
	fromAddress   string
	profileStatus string
}

// resendSupportIdentity reads the business's Resend support identity through the
// principal-less DEFINER (0139). No row ⇒ today's shared-transport behavior (R12).
func (s SendSubscriber) resendSupportIdentity(ctx context.Context, tx pgx.Tx, businessID, tenantRootID uuid.UUID) (resendSupportIdentity, bool, error) {
	var id resendSupportIdentity
	err := tx.QueryRow(ctx,
		"SELECT profile_id, from_address, profile_status FROM get_resend_support_identity($1, $2)",
		businessID, tenantRootID).Scan(&id.profileID, &id.fromAddress, &id.profileStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return resendSupportIdentity{}, false, nil
	}
	if err != nil {
		return resendSupportIdentity{}, false, err
	}
	return id, true, nil
}

// sendViaSupportProfile delivers through the business's Resend profile with
// From = the custom address and Reply-To = its plus-addressed token form (R10).
// Failures retry; the final attempt marks the reply failed (R13). Never falls back.
func (s SendSubscriber) sendViaSupportProfile(ctx context.Context, tx pgx.Tx, e events.Event, p repliedPayload, mail Mail, id resendSupportIdentity) error {
	mail.From = id.fromAddress
	mail.ReplyTo = verpAddress(id.fromAddress, p.ReplyToken)
	mail.EnvelopeFrom = id.fromAddress
	mail.DKIM = nil // Resend signs for the domain

	if s.Suppression != nil {
		suppressed, err := s.Suppression.IsSuppressed(ctx, mail.To)
		if err != nil {
			return fmt.Errorf("notify: send: suppression check: %w", err)
		}
		if suppressed {
			s.Metrics.Inc(observability.MetricOutboundSuppressed)
			return s.mark(ctx, tx, p.MessageRowID, e.TenantRootID, "failed", "recipient suppressed")
		}
	}

	var serr error
	switch {
	case s.ProfileSender == nil:
		serr = errors.New("support profile sender not configured")
	case id.profileStatus != "verified":
		serr = errors.New("support sending profile is not verified")
	default:
		serr = s.ProfileSender.SendSupportReply(ctx, id.profileID, mail)
	}
	if serr != nil {
		s.Metrics.Inc(observability.MetricOutboundFailed)
		if isFinalAttempt(e) {
			s.logger().WarnContext(ctx, "send: support profile delivery failed on final attempt",
				"message_row_id", p.MessageRowID, "business_id", p.BusinessID)
			return s.mark(ctx, tx, p.MessageRowID, e.TenantRootID, "failed", "support profile delivery failed")
		}
		return fmt.Errorf("notify: send: support profile dispatch: %w", serr)
	}
	s.Metrics.Inc(observability.MetricOutboundSent)
	s.logger().InfoContext(ctx, "notify: reply sent via support profile", "message_id", p.RFCMessageID)
	return s.mark(ctx, tx, p.MessageRowID, e.TenantRootID, "sent", "")
}

// isFinalAttempt reports whether this is the outbox worker's last try before it
// dead-letters the event. MaxAttempts is zero on the in-process Bus path.
func isFinalAttempt(e events.Event) bool {
	return e.MaxAttempts > 0 && e.Attempts+1 >= e.MaxAttempts
}
```

- [ ] **Step 4: Run the notify tests**

Run: `go test -count=1 ./internal/platform/notify/` and
`DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 ./internal/platform/notify/`
Expected: PASS, including the existing `TestSendHappyPath`, `TestSendSuppressed`, and `TestSendDKIMSignsReplyFromVerifiedDomain`. The DKIM test seeds a `provider_route` domain with no Resend profile, so it still takes the SMTP/DKIM path (R12).

- [ ] **Step 5: Write the failing mailing test**

Create `internal/mailing/support_reply_integration_test.go`:

```go
//go:build integration

package mailing_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

func supportProfileID(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM mailing_sending_profile WHERE business_id=$1", businessID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSendSupportReplyUsesProfileAndRefusesWhenUnverified(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, captured := campaignService(t, ctx, tdb, seed)
	profileID := supportProfileID(ctx, t, tdb, seed.businessID)

	mail := notify.Mail{From: "support@support.example.test", To: "ada@customer.test", Subject: "Re: Help",
		BodyText: "hi", MessageID: "m-1@inbound.localhost", ReplyTo: "support+Tok@support.example.test"}
	if err := svc.SendSupportReply(ctx, profileID, mail); err != nil {
		t.Fatalf("SendSupportReply: %v", err)
	}
	if captured.mail.From != mail.From || captured.mail.ReplyTo != mail.ReplyTo || captured.mail.To != mail.To {
		t.Fatalf("delivered mail = %+v", captured.mail)
	}

	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_sending_profile SET status='error' WHERE id=$1", profileID); err != nil {
		t.Fatal(err)
	}
	before := len(captured.mails)
	if err := svc.SendSupportReply(ctx, profileID, mail); !errors.Is(err, mailing.ErrSupportProfileUnavailable) {
		t.Fatalf("unverified profile error = %v, want ErrSupportProfileUnavailable", err)
	}
	if len(captured.mails) != before {
		t.Fatal("an unverified profile still sent")
	}
}

// Review Focus 5 (outbound): a rotated API key is picked up on the next send
// because the provider cache is keyed by the profile's updated_at.
func TestSendSupportReplyUsesRotatedCredential(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, captured := campaignService(t, ctx, tdb, seed)
	var mu sync.Mutex
	var keys []string
	svc.Providers = mailprovider.NewCache(func(_ context.Context, p mailprovider.Profile) (mailprovider.Deliverer, error) {
		mu.Lock()
		keys = append(keys, p.ResendAPIKey)
		mu.Unlock()
		return captured, nil
	}, time.Minute)
	profileID := supportProfileID(ctx, t, tdb, seed.businessID)
	mail := notify.Mail{From: "support@support.example.test", To: "ada@customer.test", Subject: "Re", BodyText: "x"}
	if err := svc.SendSupportReply(ctx, profileID, mail); err != nil {
		t.Fatal(err)
	}
	sealed, err := svc.Sealer.Seal([]byte(`{"api_key":"re_rotated"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, `UPDATE secret SET sealed_value=$1
		WHERE id=(SELECT secret_ref FROM mailing_sending_profile WHERE id=$2)`, sealed, profileID); err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_sending_profile SET updated_at=now() + interval '1 second' WHERE id=$1", profileID); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendSupportReply(ctx, profileID, mail); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keys) != "[re_test re_rotated]" {
		t.Fatalf("built with keys %v, want [re_test re_rotated]", keys)
	}
}
```

Run: `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run TestSendSupportReply ./internal/mailing/`
Expected: compile FAIL, `svc.SendSupportReply undefined`.

- [ ] **Step 6: Implement SendSupportReply**

Create `internal/mailing/support_reply.go`:

```go
package mailing

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/notify"
)

// ErrSupportProfileUnavailable means the business's Resend support profile cannot
// send right now: missing, not verified, not Resend, or its credential could not
// be opened. The caller retries and finally marks the reply failed (spec 017 R13).
var ErrSupportProfileUnavailable = errors.New("mailing: support sending profile unavailable")

// SendSupportReply sends one support-desk reply through the given business-owned
// Resend profile (spec 017 R10). It never falls back to another transport (R11).
// mailing_profile_context only returns verified profiles. The provider cache is
// keyed by the profile's updated_at, so a rotated credential is used on the next
// call. Returned errors never contain provider response bodies.
func (s *Service) SendSupportReply(ctx context.Context, profileID uuid.UUID, m notify.Mail) error {
	if s.Providers == nil {
		return ErrSupportProfileUnavailable
	}
	profile, err := s.resolveSystemProfile(ctx, profileID, false)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSupportProfileUnavailable, err)
	}
	if profile.provider.Mode != "resend" {
		return ErrSupportProfileUnavailable
	}
	deliverer, err := s.Providers.Resolve(ctx, profile.provider)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSupportProfileUnavailable, err)
	}
	if _, err := deliverer.Send(ctx, m); err != nil {
		return fmt.Errorf("mailing: support reply send: %w", err)
	}
	return nil
}
```

- [ ] **Step 7: Run the mailing tests**

Run: `go test -count=1 ./internal/mailing/...` and
`DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestSendSupportReply|TestResendReceived' ./internal/mailing/`
Expected: PASS.

- [ ] **Step 8: Wire main**

In `cmd/manyforge/main.go`, replace:

```go
	sendSub := notify.SendSubscriber{Sender: sharedMail.Sender, Logger: logger, Metrics: metrics}
```

with:

```go
	sendSub := notify.SendSubscriber{
		Sender: sharedMail.Sender, Logger: logger, Metrics: metrics,
		Suppression: notify.DBSuppression{DB: database},
	}
	// Spec 017: replies for a business with a Resend support identity go through
	// that business's profile. Assign only a non-nil service (no typed-nil interface).
	if mailingSvc != nil {
		sendSub.ProfileSender = mailingSvc
	}
```

Run: `go build ./... && make lint`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/mailing/support_reply.go internal/mailing/support_reply_integration_test.go \
  internal/platform/notify/sender_subscriber.go internal/platform/notify/final_attempt_test.go \
  internal/platform/notify/resend_support_integration_test.go cmd/manyforge/main.go
git commit -m "feat(support): send replies through the business Resend profile without fallback

From is the custom address, Reply-To its plus-addressed token form. The
final failed attempt marks the reply failed on both transports."
```

---
### Task 8: Web UI for the Resend DNS checklist and dropped attachments (R9, R15)

**Files:**
- Create: `web/src/app/pages/support/resend-dns.ts`
- Create: `web/src/app/pages/support/resend-dns.spec.ts`
- Modify: `web/src/app/core/ticket.service.ts` (`TicketMessage` ~:60-81; new `DroppedAttachment`)
- Modify: `web/src/app/pages/support/inbox-settings.ts` (imports ~:1-16; domain row template ~:136-206; class members)
- Modify: `web/src/app/pages/support/inbox-settings.spec.ts` (append tests)
- Modify: `web/src/app/pages/support/thread-view.ts` (after the attachments block ~:121-130; styles ~:553)
- Modify: `web/src/app/pages/support/thread-view.spec.ts` (append a describe block)
- Create: `web/e2e/support-resend.spec.ts`

**Interfaces:**
- Consumes: `EmailDomain.mode === 'provider_route'` and `dns_challenge.verification_txt` (existing API). `TicketMessage.dropped_attachments` (Task 3).
- Produces:
  - `resendDnsChecklist(d: EmailDomain): DnsChecklistRow[]`
  - Test ids: `received-via-resend`, `resend-dns-checklist`, `resend-dns-row`, `dropped-attachments`, `dropped-attachment`. The existing `verify-domain` and `verify-hint` stay usable on provider_route rows.

- [ ] **Step 1: Write the failing helper spec**

Create `web/src/app/pages/support/resend-dns.spec.ts`:

```ts
import { describe, expect, it } from 'vitest';
import { EmailDomain } from '../../core/ticket.service';
import { resendDnsChecklist } from './resend-dns';

function domain(over: Partial<EmailDomain> = {}): EmailDomain {
  return {
    id: 'ed1',
    business_id: 'b1',
    tenant_root_id: 'root',
    domain: 'support.garden.gg',
    mode: 'provider_route',
    verification: 'unverified',
    verified_at: null,
    dkim_state: 'unknown',
    spf_state: 'unknown',
    dns_challenge: {
      verification_txt: { name: '_manyforge.support.garden.gg', value: 'mf-verify=abc' },
      dkim_record: { name: 'mf1._domainkey.support.garden.gg', value: 'v=DKIM1; k=ed25519; p=x' },
      spf_hint: 'v=spf1 include:mail.manyforge.example ~all',
      mx_hint: null,
    },
    created_at: '2026-10-10T00:00:00Z',
    ...over,
  };
}

describe('resendDnsChecklist', () => {
  it('lists the ownership TXT plus every Resend record, all under the subdomain', () => {
    const rows = resendDnsChecklist(domain());
    expect(rows.map((r) => `${r.type} ${r.name}`)).toEqual([
      'TXT _manyforge.support.garden.gg',
      'MX support.garden.gg',
      'TXT resend._domainkey.support.garden.gg',
      'MX send.support.garden.gg',
      'TXT send.support.garden.gg',
    ]);
    expect(rows[0]).toMatchObject({ source: 'manyforge', value: 'mf-verify=abc' });
    expect(rows.slice(1).every((r) => r.source === 'resend')).toBe(true);
  });

  it('never proposes a record at the apex', () => {
    for (const r of resendDnsChecklist(domain())) {
      expect(r.name.endsWith('support.garden.gg')).toBe(true);
    }
  });
});
```

Run: `cd web && npx ng test --watch=false --include src/app/pages/support/resend-dns.spec.ts`
Expected: FAIL, `Cannot find module './resend-dns'`.

- [ ] **Step 2: Implement the helper**

Create `web/src/app/pages/support/resend-dns.ts`:

```ts
import { EmailDomain } from '../../core/ticket.service';

// One DNS record an administrator publishes for a Resend-received support domain
// (spec 017 R9). `source: 'resend'` rows show where to copy the value from,
// because Resend issues those values per account and region.
export interface DnsChecklistRow {
  purpose: string;
  type: 'TXT' | 'MX';
  name: string;
  value: string;
  source: 'manyforge' | 'resend';
  note?: string;
}

// resendDnsChecklist returns every record a provider_route domain needs. All of
// them sit at or under the support subdomain, so the apex MX and SPF are untouched.
export function resendDnsChecklist(d: EmailDomain): DnsChecklistRow[] {
  const name = d.domain;
  const copy = (what: string) => `Copy the ${what} value Resend shows under Domains → ${name}`;
  return [
    {
      purpose: 'Ownership (ManyForge)',
      type: 'TXT',
      name: d.dns_challenge.verification_txt.name,
      value: d.dns_challenge.verification_txt.value,
      source: 'manyforge',
    },
    {
      purpose: 'Receiving',
      type: 'MX',
      name,
      value: copy('receiving MX'),
      source: 'resend',
      note: 'Must be the lowest-priority MX on this name. Your apex MX is not changed.',
    },
    {
      purpose: 'DKIM',
      type: 'TXT',
      name: `resend._domainkey.${name}`,
      value: copy('DKIM'),
      source: 'resend',
    },
    {
      purpose: 'SPF return path',
      type: 'MX',
      name: `send.${name}`,
      value: copy('feedback MX'),
      source: 'resend',
    },
    {
      purpose: 'SPF return path',
      type: 'TXT',
      name: `send.${name}`,
      value: copy('SPF'),
      source: 'resend',
    },
  ];
}
```

Run: `cd web && npx ng test --watch=false --include src/app/pages/support/resend-dns.spec.ts`
Expected: PASS.

- [ ] **Step 3: Write the failing inbox-settings tests**

Append inside the `describe('InboxSettingsComponent (US4)', ...)` block of `web/src/app/pages/support/inbox-settings.spec.ts`:

```ts
  it('shows the Resend checklist and "Received via Resend" for a provider_route domain', () => {
    loadWith([mockEmailDomain({ mode: 'provider_route', dkim_state: 'unknown' })], []);
    expect(text('received-via-resend')).toBe('Received via Resend');
    expect(all('resend-dns-checklist').length).toBe(1);
    expect(all('resend-dns-row').length).toBe(5);
    // The ManyForge DKIM record is not offered: Resend signs this domain.
    expect(all('challenge-dkim-name').length).toBe(0);
    expect(all('verify-domain').length).toBe(1);
  });

  it('keeps the Resend checklist visible after the domain is verified', () => {
    loadWith(
      [mockEmailDomain({ mode: 'provider_route', verification: 'verified', verified_at: '2026-10-10T00:00:00Z' })],
      [],
    );
    expect(all('resend-dns-checklist').length).toBe(1);
    expect(all('verify-domain').length).toBe(0);
  });

  it('does not show the Resend checklist for other modes', () => {
    loadWith([mockEmailDomain({ mode: 'forward_in' })], []);
    expect(all('resend-dns-checklist').length).toBe(0);
    expect(all('received-via-resend').length).toBe(0);
  });
```

Run: `cd web && npx ng test --watch=false --include src/app/pages/support/inbox-settings.spec.ts`
Expected: FAIL, `expected '' to be 'Received via Resend'`.

- [ ] **Step 4: Implement the inbox-settings changes**

In `web/src/app/pages/support/inbox-settings.ts`:

1. Add the import: `import { DnsChecklistRow, resendDnsChecklist } from './resend-dns';`
2. In the domain row's `mf-row-main` div, after the `domain-mode` pill, add:

```html
                @if (d.mode === 'provider_route') {
                  <mf-status-pill
                    [tone]="'neutral'"
                    label="Received via Resend"
                    data-testid="received-via-resend"
                  />
                }
```

3. Change the existing `@if (d.verification !== 'verified') {` that wraps the `dns-challenge` block to `@if (d.verification !== 'verified' && d.mode !== 'provider_route') {`.
4. Directly after that block's closing `}`, add:

```html
              @if (d.mode === 'provider_route') {
                <div class="mf-dns-challenge" data-testid="resend-dns-checklist">
                  <p class="mf-dns-intro">
                    Publish these records under {{ d.domain }}, verify the domain in Resend, then
                    verify here. Nothing changes at your apex domain.
                  </p>
                  @for (r of checklist(d); track r.name + r.type) {
                    <div class="mf-dns-rec" data-testid="resend-dns-row">
                      <span class="mf-dns-kind">{{ r.purpose }} ({{ r.type }})</span>
                      <code>{{ r.name }}</code>
                      <code>{{ r.value }}</code>
                      @if (r.note) {
                        <span class="mf-hint">{{ r.note }}</span>
                      }
                    </div>
                  }
                  @if (d.verification !== 'verified') {
                    <div class="mf-challenge-actions">
                      <button
                        type="button"
                        class="mf-btn mf-btn-ghost mf-btn-sm"
                        data-testid="verify-domain"
                        [disabled]="verifyingId() !== null"
                        (click)="verify(d)"
                      >
                        {{ verifyingId() === d.id ? 'Checking DNS…' : 'Verify' }}
                      </button>
                      @if (verifyHintId() === d.id) {
                        <span class="mf-hint" data-testid="verify-hint">
                          Not verified yet — re-check that the records are published and try again.
                        </span>
                      }
                    </div>
                  }
                </div>
              }
```

5. Add a class member next to the existing `verificationTone`/`dkimSpfTone` members:

```ts
  protected checklist(d: EmailDomain): DnsChecklistRow[] {
    return resendDnsChecklist(d);
  }
```

Use only existing `--mf-*` tokens and classes. `npm run check:tokens` rejects raw colors.

Run: `cd web && npx ng test --watch=false --include src/app/pages/support/inbox-settings.spec.ts && npm run check:tokens`
Expected: PASS.

- [ ] **Step 5: Write the failing thread-view test**

In `web/src/app/core/ticket.service.ts`, add above `export interface TicketMessage`:

```ts
// An inbound attachment that was dropped instead of stored (spec 017 R15).
export interface DroppedAttachment {
  filename: string;
  detected_type: string;
  size: number;
}
```

and add to `TicketMessage` after `attachments: Attachment[];`:

```ts
  // Inbound attachments not stored (disallowed type or over the size cap).
  dropped_attachments?: DroppedAttachment[];
```

Append to `web/src/app/pages/support/thread-view.spec.ts`:

```ts
describe('ThreadViewComponent dropped attachments (spec 017)', () => {
  let fixture: ComponentFixture<ThreadViewComponent>;
  let mock: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: { snapshot: { paramMap: new Map([['businessId', biz], ['tid', tid]]) } },
        },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
  });

  afterEach(() => mock.verify());

  it('lists dropped attachments on the message without offering a download', () => {
    fixture = TestBed.createComponent(ThreadViewComponent);
    fixture.detectChanges();
    mock.expectOne('/api/v1/me').flush({ id: myPid, email: 'me@x.test', display_name: 'Me', email_verified: true, status: 'active' });
    mock.expectOne(`/api/v1/businesses/${biz}/assignable-members`).flush({ items: [], next_cursor: null });
    mock.expectOne(`/api/v1/businesses/${biz}/agents`).flush({ items: [] });
    mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`).flush(makeTicket());
    const message: TicketMessage = {
      id: 'm1', ticket_id: tid, direction: 'inbound', message_id: '<m1@x>', in_reply_to: null, references: [],
      author_principal_id: null, body_text: 'see attached', body_html: null, attachments: [],
      dropped_attachments: [{ filename: 'invoice.exe', detected_type: 'application/octet-stream', size: 24 }],
      spf_result: 'pass', dkim_result: 'pass', dmarc_result: 'pass', created_at: '2026-10-10T00:00:00Z',
    };
    mock
      .expectOne((r) => r.url === `/api/v1/businesses/${biz}/tickets/${tid}/messages`)
      .flush({ items: [message], next_cursor: null });
    fixture.detectChanges();
    const el = fixture.nativeElement as HTMLElement;
    const rows = el.querySelectorAll('[data-testid="dropped-attachment"]');
    expect(rows.length).toBe(1);
    expect(rows[0].textContent).toContain('invoice.exe');
    expect(rows[0].textContent).toContain('not stored');
    expect(rows[0].querySelector('a')).toBeNull();
  });
});
```

Run: `cd web && npx ng test --watch=false --include src/app/pages/support/thread-view.spec.ts`
Expected: FAIL, `expected 0 to be 1`.

- [ ] **Step 6: Render dropped attachments**

In `web/src/app/pages/support/thread-view.ts`, directly after the `@if (m.attachments.length) { ... }` block, add:

```html
              @if (m.dropped_attachments?.length) {
                <ul class="attachments dropped" data-testid="dropped-attachments">
                  @for (d of m.dropped_attachments; track $index) {
                    <li class="attachment" data-testid="dropped-attachment">
                      <span class="filename">{{ d.filename || '(unnamed)' }}</span>
                      <span class="muted">not stored · {{ d.detected_type }} · {{ d.size }} bytes</span>
                    </li>
                  }
                </ul>
              }
```

In the component styles, after the `.attachment .filename` rule, add:

```css
      .attachments.dropped .filename {
        text-decoration: line-through;
        color: var(--mf-text-muted);
      }
```

Run: `cd web && npx ng test --watch=false && npm run check:tokens`
Expected: PASS (whole unit suite).

- [ ] **Step 7: Write the Playwright regression**

Create `web/e2e/support-resend.spec.ts`:

```ts
import { Page, expect, test } from '@playwright/test';

// Spec 017 UI regression: the Resend DNS checklist on a provider_route domain and
// dropped attachments in the thread. The /api/v1 surface is mocked.
const BIZ_ID = 'biz-1';
const TICKET_ID = 'tkt-1';

const business = {
  id: BIZ_ID, parent_id: null, tenant_root_id: 'root-1', name: 'Garden', status: 'active', is_tenant_root: true,
};

const resendDomain = {
  id: 'ed-resend', business_id: BIZ_ID, tenant_root_id: 'root-1', domain: 'support.garden.example',
  mode: 'provider_route', verification: 'unverified', verified_at: null, dkim_state: 'unknown', spf_state: 'unknown',
  dns_challenge: {
    verification_txt: { name: '_manyforge.support.garden.example', value: 'mf-verify=TESTTOKEN' },
    dkim_record: { name: 'mf1._domainkey.support.garden.example', value: 'v=DKIM1; k=ed25519; p=AAAA' },
    spf_hint: 'v=spf1 include:mx.manyforge.test ~all', mx_hint: null,
  },
  created_at: '2026-10-10T00:00:00Z',
};

async function installBase(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  // Registered FIRST so it is the lowest-priority match: unmocked shell nav-badge
  // calls (approvals, connectors, …) get an empty 200 instead of a 401 that would
  // trigger refresh → /login mid-test.
  await page.route('**/api/**', (route) => route.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({ json: { id: 'u1', email: 'owner@manyforge.test', display_name: 'Owner', email_verified: true, status: 'active' } }),
  );
  await page.route('**/api/v1/businesses**', (route) => route.fulfill({ json: { items: [business], next_cursor: null } }));
}

test('provider_route domain shows the Resend DNS checklist and the Received via Resend label', async ({ page }) => {
  await installBase(page);
  await page.route(`**/api/v1/businesses/${BIZ_ID}/email-domains`, (route) =>
    route.fulfill({ json: { items: [resendDomain], next_cursor: null } }),
  );
  await page.route(`**/api/v1/businesses/${BIZ_ID}/inbound-addresses`, (route) =>
    route.fulfill({ json: { items: [], next_cursor: null } }),
  );

  await page.goto(`/support/${BIZ_ID}/settings/inbox`);
  const row = page.getByTestId('domain-row');
  await expect(row.getByTestId('received-via-resend')).toHaveText('Received via Resend');
  const checklist = row.getByTestId('resend-dns-checklist');
  await expect(checklist).toBeVisible();
  await expect(checklist.getByTestId('resend-dns-row')).toHaveCount(5);
  await expect(checklist).toContainText('_manyforge.support.garden.example');
  await expect(checklist).toContainText('mf-verify=TESTTOKEN');
  await expect(checklist).toContainText('resend._domainkey.support.garden.example');
  await expect(checklist).toContainText('send.support.garden.example');
  await expect(row.getByTestId('challenge-dkim-name')).toHaveCount(0);
  await expect(row.getByTestId('verify-domain')).toBeVisible();
});

test('a dropped attachment is listed on the inbound message as not stored', async ({ page }) => {
  await installBase(page);
  const ticket = {
    id: TICKET_ID, business_id: BIZ_ID, tenant_root_id: 'root-1', subject: 'Invoice', status: 'new', priority: 'normal',
    assignee_principal_id: null,
    requester: {
      id: 'req-1', tenant_root_id: 'root-1', email: 'ada@customer.test', display_name: 'Ada', contact_id: null,
      first_seen_at: '2026-10-10T00:00:00Z', last_seen_at: '2026-10-10T00:00:00Z',
    },
    tags: [], message_count: 1, last_message_at: '2026-10-10T00:00:00Z',
    created_at: '2026-10-10T00:00:00Z', updated_at: '2026-10-10T00:00:00Z',
  };
  const message = {
    id: 'msg-1', ticket_id: TICKET_ID, direction: 'inbound', message_id: '<m1@customer.test>', in_reply_to: null,
    references: [], author_principal_id: null, body_text: 'invoice attached', body_html: null, attachments: [],
    dropped_attachments: [{ filename: 'invoice.exe', detected_type: 'application/octet-stream', size: 24 }],
    spf_result: 'pass', dkim_result: 'pass', dmarc_result: 'pass', delivery_state: null,
    created_at: '2026-10-10T00:00:00Z',
  };
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}`, (route) => route.fulfill({ json: ticket }));
  await page.route(`**/api/v1/businesses/${BIZ_ID}/tickets/${TICKET_ID}/messages**`, (route) =>
    route.fulfill({ json: { items: [message], next_cursor: null } }),
  );

  await page.goto(`/support/${BIZ_ID}/${TICKET_ID}`);
  const dropped = page.getByTestId('dropped-attachment');
  await expect(dropped).toHaveCount(1);
  await expect(dropped).toContainText('invoice.exe');
  await expect(dropped).toContainText('not stored');
});
```

- [ ] **Step 8: Run Playwright and drive the real UI**

Run: `cd web && E2E_PORT=4311 npx playwright test e2e/support-resend.spec.ts e2e/support.spec.ts`
Expected: PASS. The existing `support.spec.ts` must stay green.

Then drive a real browser against the live dev stack (the global CLAUDE.md requires a real-browser check for visible UI changes). Use `Skill: gstack` / `$B`, or the Playwright MCP. Start the backend and SPA per the local full-app demo recipe (compose DB on `:55432`, `.air.env` on `:8081`, `npx ng serve --port 4311`). Log in as the seeded demo user, add a `provider_route` domain on Support → Email domains, and confirm the checklist, the label, and the Verify button render with no console errors. Write down what you saw in the Task 9 runbook PR notes.

- [ ] **Step 9: Commit**

```bash
git add web/src/app/pages/support/resend-dns.ts web/src/app/pages/support/resend-dns.spec.ts \
  web/src/app/core/ticket.service.ts web/src/app/pages/support/inbox-settings.ts \
  web/src/app/pages/support/inbox-settings.spec.ts web/src/app/pages/support/thread-view.ts \
  web/src/app/pages/support/thread-view.spec.ts web/e2e/support-resend.spec.ts
git commit -m "feat(web): Resend DNS checklist for support domains and dropped attachments in threads"
```

---
### Task 9: End-to-end loop test, security regression pins, and the runbook

**Files:**
- Create: `internal/inbox/resend_loop_integration_test.go` (package `inbox_test`)
- Create: `internal/security_regression/doc_support017.go`
- Create: `internal/security_regression/mf017_reply_token_secret_pin_test.go`
- Create: `internal/security_regression/mf017_same_business_pin_test.go`
- Create: `internal/security_regression/mf017_record_after_ingest_pin_test.go`
- Create: `internal/security_regression/mf017_no_fallback_pin_test.go`
- Create: `internal/security_regression/mf017_raw_download_pin_test.go`
- Create: `internal/security_regression/mf017_definers_pin_test.go`
- Create: `docs/runbooks/support-inbound-resend.md`

**Interfaces:**
- Consumes everything from Tasks 1-8: `mailing.NewWebhookHandler` and its new fields, `mailing.Service.SendSupportReply`, `inbox.Service.ReceiveResend`, `notify.SendSubscriber{ProfileSender, Suppression}`, `ticketing.Service.Reply`, and `mailprovider.Factory{ResendBaseURL, HTTPClient}`.
- Produces: no new code API. It adds the spec's success-criteria test and the CI pins.

- [ ] **Step 1: Write the end-to-end loop test**

Create `internal/inbox/resend_loop_integration_test.go`:

```go
//go:build integration

// Spec 017 success criteria: against a fake Resend API, a signed email.received
// creates a ticket; the agent reply leaves through the business's profile with the
// right From and Reply-To; the customer's reply threads onto the same ticket both
// by headers and by token alone.
package inbox_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	mfcrypto "github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/platform/notify"
	"github.com/manyforge/manyforge/internal/ticketing"
)

var loopReplyKey = []byte("loop-reply-token-key-0123456789abcdef")

type loopResend struct {
	mu     sync.Mutex
	server *httptest.Server
	raws   map[string][]byte
	sent   []map[string]any
}

func newLoopResend(t *testing.T) *loopResend {
	t.Helper()
	f := &loopResend{raws: map[string][]byte{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/emails/receiving/"):
			id := strings.TrimPrefix(r.URL.Path, "/emails/receiving/")
			if _, ok := f.raws[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "to": []string{}, "received_for": []string{},
				"raw": map[string]any{"download_url": f.server.URL + "/raw/" + id}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/raw/"):
			_, _ = w.Write(f.raws[strings.TrimPrefix(r.URL.Path, "/raw/")])
		case r.Method == http.MethodPost && r.URL.Path == "/emails":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.sent = append(f.sent, body)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"sent-%d"}`, len(f.sent)))
		default:
			t.Errorf("unexpected Resend request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

type loopFixture struct {
	ctx        context.Context
	tdb        *testdb.TestDB
	business   uuid.UUID
	owner      uuid.UUID
	profileID  uuid.UUID
	domain     string
	address    string
	webhookKey []byte
	resend     *loopResend
	router     chi.Router
	tickets    *ticketing.Service
	sendSub    notify.SendSubscriber
	shared     *countingSender
}

type countingSender struct{ calls int }

func (c *countingSender) Send(context.Context, notify.Mail) error { c.calls++; return nil }

func newLoopFixture(t *testing.T) *loopFixture {
	t.Helper()
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	sealer, err := mfcrypto.NewSealer(bytes.Repeat([]byte{0x72}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &loopFixture{ctx: ctx, tdb: tdb, business: uuid.New(), owner: uuid.New(), profileID: uuid.New(),
		webhookKey: bytes.Repeat([]byte{0x51}, 32), resend: newLoopResend(t), shared: &countingSender{}}
	f.domain = "support-" + f.business.String()[:8] + ".example.com"
	f.address = "support@" + f.domain
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatal(err)
	}
	accountID, domainID, secretID := uuid.New(), uuid.New(), uuid.New()
	sealed, err := sealer.Seal([]byte(fmt.Sprintf(`{"version":2,"api_key":"re_loop","webhook_id":"wh_loop","webhook_secret":%q}`,
		"whsec_"+base64.StdEncoding.EncodeToString(f.webhookKey))))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`, []any{accountID, "owner-" + f.owner.String() + "@x.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{f.owner, accountID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'Garden','active',now(),now())`, []any{f.business}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{f.business}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`, []any{f.owner, f.business, ownerRole}},
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at) VALUES ($1,$2,$2,$3,'system',NULL,now(),now())`, []any{uuid.New(), f.business, "b-" + f.business.String()[:8] + "@inbound.localhost"}},
		{`INSERT INTO email_domain (id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,spf_state,created_at,updated_at) VALUES ($1,$2,$2,$3,'provider_route','mf-verify=x',now(),'unknown',now(),now())`, []any{domainID, f.business, f.domain}},
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at) VALUES ($1,$2,$2,$3,'custom',$4,now(),now())`, []any{uuid.New(), f.business, f.address, domainID}},
		{`INSERT INTO secret (id,business_id,tenant_root_id,scope,sealed_value,created_at,updated_at) VALUES ($1,$2,$2,'mailing',$3,now(),now())`, []any{secretID, f.business, sealed}},
		{`INSERT INTO mailing_sending_profile (id,business_id,tenant_root_id,mode,from_email,from_name,secret_ref,status,feedback_status) VALUES ($1,$2,$2,'resend',$3,'Garden Support',$4,'verified','ready')`, []any{f.profileID, f.business, f.address, secretID}},
	} {
		if _, err := tdb.Super.Exec(ctx, s.q, s.args...); err != nil {
			t.Fatalf("seed: %v\nSQL: %s", err, s.q)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	inboxSvc := inbox.NewService(tdb.App, nil, inbox.Config{ReplyTokenKey: loopReplyKey, InboundSystemDomain: "inbound.localhost"}, logger)
	factory := &mailprovider.Factory{DB: tdb.App, ResendBaseURL: f.resend.server.URL, HTTPClient: f.resend.server.Client()}
	mailingSvc := &mailing.Service{DB: tdb.App, Sealer: sealer, Providers: mailprovider.NewCache(factory.Build, time.Minute)}
	wh := mailing.NewWebhookHandler(tdb.App, sealer, logger)
	wh.Inbound = inboxSvc
	wh.ResendBaseURL = f.resend.server.URL
	wh.ResendHTTPClient = f.resend.server.Client()
	wh.RawDownloadClient = f.resend.server.Client()
	wh.AllowInsecureRawURL = true
	wh.MaxReceivedBytes = 1 << 20
	f.router = chi.NewRouter()
	wh.PublicRoutes(f.router)
	f.tickets = &ticketing.Service{DB: tdb.App, ReplyTokenKey: loopReplyKey, SystemDomain: "inbound.localhost"}
	f.sendSub = notify.SendSubscriber{Sender: f.shared, ProfileSender: mailingSvc, Suppression: notify.DBSuppression{DB: tdb.App}}
	return f
}

func (f *loopFixture) receive(t *testing.T, eventID, emailID string, raw []byte) {
	t.Helper()
	f.resend.mu.Lock()
	f.resend.raws[emailID] = raw
	f.resend.mu.Unlock()
	body := []byte(fmt.Sprintf(`{"type":"email.received","created_at":"2026-10-10T12:00:00Z","data":{"email_id":%q,"to":[%q]}}`, emailID, f.address))
	ts := fmt.Sprint(time.Now().Unix())
	mac := hmac.New(sha256.New, f.webhookKey)
	mac.Write([]byte(eventID + "." + ts + "."))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+f.profileID.String()+"/resend", bytes.NewReader(body))
	req.Header.Set("svix-id", eventID)
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook %s status = %d", eventID, w.Code)
	}
}

func (f *loopFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.tdb.Super.QueryRow(f.ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func customerMail(from, to, messageID, inReplyTo, body string) []byte {
	m := "From: " + from + "\r\nTo: " + to + "\r\nSubject: Seeds never arrived\r\nDate: Mon, 02 Jan 2006 15:04:05 -0700\r\nMessage-ID: <" + messageID + ">\r\n"
	if inReplyTo != "" {
		m += "In-Reply-To: <" + inReplyTo + ">\r\nReferences: <" + inReplyTo + ">\r\n"
	}
	return []byte(m + "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
}

func TestResendSupportLoop(t *testing.T) {
	f := newLoopFixture(t)

	// 1. Customer writes in.
	f.receive(t, "evt-1", "em_1", customerMail("Ada <ada@customer.test>", "Garden Support <"+f.address+">", "c1@customer.test", "", "Where are my seeds?"))
	if n := f.count(t, "SELECT count(*) FROM ticket WHERE business_id=$1", f.business); n != 1 {
		t.Fatalf("tickets = %d, want 1", n)
	}
	var ticketID uuid.UUID
	var token string
	if err := f.tdb.Super.QueryRow(f.ctx, "SELECT id, reply_token FROM ticket WHERE business_id=$1", f.business).Scan(&ticketID, &token); err != nil {
		t.Fatal(err)
	}

	// 2. Agent replies; the outbox event drains through the subscriber.
	reply, err := f.tickets.Reply(f.ctx, f.owner, f.business, ticketID, ticketing.ReplyInput{BodyText: "Shipping today."})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	var ev events.Event
	if err := f.tdb.Super.QueryRow(f.ctx, `SELECT id, tenant_root_id, topic, payload, attempts FROM outbox
		WHERE tenant_root_id=$1 AND topic='ticket.replied' AND processed_at IS NULL
		ORDER BY created_at DESC LIMIT 1`, f.business).Scan(&ev.ID, &ev.TenantRootID, &ev.Topic, &ev.Payload, &ev.Attempts); err != nil {
		t.Fatal(err)
	}
	ev.MaxAttempts = 10
	if err := f.tdb.App.WithTx(f.ctx, func(tx pgx.Tx) error { return f.sendSub.Handle(f.ctx, tx, ev) }); err != nil {
		t.Fatalf("send: %v", err)
	}
	if f.shared.calls != 0 || len(f.resend.sent) != 1 {
		t.Fatalf("shared calls=%d resend sends=%d, want 0/1", f.shared.calls, len(f.resend.sent))
	}
	sent := f.resend.sent[0]
	wantReplyTo := "support+" + token + "@" + f.domain
	if sent["from"] != f.address || sent["reply_to"] != wantReplyTo {
		t.Fatalf("sent from=%v reply_to=%v, want %s / %s", sent["from"], sent["reply_to"], f.address, wantReplyTo)
	}
	var rfcID string
	if err := f.tdb.Super.QueryRow(f.ctx, "SELECT message_id, delivery_state::text FROM ticket_message WHERE id=$1", reply.ID).Scan(&rfcID, new(string)); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM ticket_message WHERE id=$1 AND delivery_state='sent'", reply.ID) != 1 {
		t.Fatal("reply not marked sent")
	}

	// 3a. Customer replies with threading headers.
	f.receive(t, "evt-2", "em_2", customerMail("ada@customer.test", f.address, "c2@customer.test", rfcID, "Thanks!"))
	// 3b. Customer replies to the token address with every header stripped.
	f.receive(t, "evt-3", "em_3", customerMail("ada@customer.test", "support+"+token+"@"+f.domain, "c3@customer.test", "", "One more thing"))

	if n := f.count(t, "SELECT count(*) FROM ticket WHERE business_id=$1", f.business); n != 1 {
		t.Fatalf("tickets after replies = %d, want 1", n)
	}
	if n := f.count(t, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1", ticketID); n != 4 {
		t.Fatalf("messages = %d, want 4 (in, out, in by headers, in by token)", n)
	}
}

// Review Focus 3: ingest committed but the delivery row was lost (crash between
// ingest and record) — Resend's redelivery must not duplicate the ticket/message.
func TestResendLoopRedeliveryAfterPartialFailureCreatesNoDuplicate(t *testing.T) {
	f := newLoopFixture(t)
	raw := customerMail("ada@customer.test", f.address, "p1@customer.test", "", "hello")
	f.receive(t, "evt-p", "em_p", raw)
	if _, err := f.tdb.Super.Exec(f.ctx, "DELETE FROM mailing_provider_webhook_delivery WHERE external_event_id='evt-p'"); err != nil {
		t.Fatal(err)
	}
	f.receive(t, "evt-p", "em_p", raw)
	if n := f.count(t, "SELECT count(*) FROM ticket WHERE business_id=$1", f.business); n != 1 {
		t.Fatalf("tickets = %d, want 1", n)
	}
	if n := f.count(t, "SELECT count(*) FROM ticket_message WHERE message_id='p1@customer.test'"); n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
	if n := f.count(t, "SELECT count(*) FROM mailing_provider_webhook_delivery WHERE external_event_id='evt-p'"); n != 1 {
		t.Fatalf("delivery rows = %d, want 1 (re-recorded)", n)
	}
}
```

- [ ] **Step 2: Run the loop tests**

Run: `DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -count=1 -run 'TestResendSupportLoop|TestResendLoopRedelivery' ./internal/inbox/`
Expected: PASS. Every piece is in place from Tasks 1-7, so a failure here is an integration bug in those tasks. Fix it in the owning task's file and re-run that task's tests.

- [ ] **Step 3: Add the finding IDs and the pins**

Create `internal/security_regression/doc_support017.go`:

```go
package security_regression

// Spec 017 (Support Email via Resend) security-regression finding IDs. Each pin
// file below carries one ID in its header comment.
const (
	// FindingReplyTokenSecret — production refuses to serve with an empty
	// reply-token HMAC key (R14); the migrate Job is unaffected.
	FindingReplyTokenSecret = "MF-017-REPLY-TOKEN-SECRET"
	// FindingSameBusiness — Resend-received mail routes only to verified custom
	// addresses owned by the receiving profile's business (R4).
	FindingSameBusiness = "MF-017-SAME-BUSINESS"
	// FindingRecordAfterIngest — the email.received event id is recorded only
	// after fetch and ingest succeed, so a retry is never discarded (R5).
	FindingRecordAfterIngest = "MF-017-RECORD-AFTER-INGEST"
	// FindingNoFallback — a business with a Resend support identity never sends
	// through the shared transport (R11).
	FindingNoFallback = "MF-017-NO-FALLBACK"
	// FindingRawDownload — the pre-signed raw URL is https-only, size-capped,
	// fetched through netsafe, never sent the API key, never logged.
	FindingRawDownload = "MF-017-RAW-DOWNLOAD"
	// FindingDefiners — 0139's functions are search-path-pinned DEFINERs with
	// PUBLIC execute revoked, and verified domains are globally unique (R7).
	FindingDefiners = "MF-017-DEFINERS"
)
```

Create `internal/security_regression/mf017_reply_token_secret_pin_test.go`:

```go
// Finding MF-017-REPLY-TOKEN-SECRET (doc_support017.go / spec 017 R14).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017ReplyTokenSecretRequiredInProduction(t *testing.T) {
	cfg := mustRead(t, "../platform/config/config.go")
	for _, pin := range []string{"func (cfg Config) ValidateServe() error",
		`cfg.Environment == "production" && len(cfg.InboundReplyTokenSecret) == 0`,
		"MANYFORGE_INBOUND_REPLY_TOKEN_SECRET: required in production"} {
		if !strings.Contains(cfg, pin) {
			t.Errorf("config.go missing %q", pin)
		}
	}
	main := mustRead(t, "../../cmd/manyforge/main.go")
	guard := strings.Index(main, "cfg.ValidateServe()")
	migrate := strings.Index(main, `os.Args[1] == "migrate"`)
	if guard < 0 || migrate < 0 || guard < migrate {
		t.Error("main.go must call cfg.ValidateServe() on the serve path, after the migrate branch")
	}
	chart := mustRead(t, "../../charts/manyforge/templates/deployment.yaml")
	if !strings.Contains(chart, "MANYFORGE_INBOUND_REPLY_TOKEN_SECRET") || !strings.Contains(chart, "inboundReplyTokenKey") {
		t.Error("Helm deployment must wire MANYFORGE_INBOUND_REPLY_TOKEN_SECRET from the master-keys Secret")
	}
}
```

Create `internal/security_regression/mf017_same_business_pin_test.go`:

```go
// Finding MF-017-SAME-BUSINESS (doc_support017.go / spec 017 R4).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017SameBusinessRouting(t *testing.T) {
	mig := mustRead(t, "../../migrations/0139_support_email_resend.up.sql")
	start := strings.Index(mig, "FUNCTION resolve_inbound_address_for_business(")
	if start < 0 {
		t.Fatal("missing resolve_inbound_address_for_business")
	}
	body := mig[start : start+strings.Index(mig[start:], "$$;")]
	for _, pin := range []string{"ia.business_id = p_business_id", "ed.business_id = p_business_id",
		"ia.kind = 'custom'", "ed.verified_at IS NOT NULL", "SET search_path = public"} {
		if !strings.Contains(body, pin) {
			t.Errorf("resolve_inbound_address_for_business missing %q", pin)
		}
	}
	src := mustRead(t, "../inbox/resend.go")
	for _, pin := range []string{"resolveRecipientForBusiness(ctx, tx, businessID, normalized)",
		"RestrictToBusiness: businessID", "support_audit_inbound_dropped"} {
		if !strings.Contains(src, pin) {
			t.Errorf("inbox/resend.go missing %q", pin)
		}
	}
	svc := mustRead(t, "../inbox/service.go")
	if !strings.Contains(svc, "if msg.RestrictToBusiness != uuid.Nil {") {
		t.Error("Ingest must honor RestrictToBusiness before resolving the recipient")
	}
	if !strings.Contains(mustRead(t, "../mailing/resend_received.go"), "h.Inbound.ReceiveResend(ctx, wc.businessID,") {
		t.Error("the Resend webhook must pass the PROFILE's business id, never one derived from the payload")
	}
}
```

Create `internal/security_regression/mf017_record_after_ingest_pin_test.go`:

```go
// Finding MF-017-RECORD-AFTER-INGEST (doc_support017.go / spec 017 R5).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017RecordOnlyAfterIngest(t *testing.T) {
	src := mustRead(t, "../mailing/resend_received.go")
	start := strings.Index(src, "func (h *WebhookHandler) handleResendReceived(")
	end := strings.Index(src, "func (h *WebhookHandler) skipOrRetry(")
	if start < 0 || end < start {
		t.Fatal("handleResendReceived/skipOrRetry not found")
	}
	body := src[start:end]
	ingest := strings.Index(body, "h.Inbound.ReceiveResend(")
	lastRecord := strings.LastIndex(body, "h.finishReceived(w, r, wc, eventID)")
	if ingest < 0 || lastRecord < ingest {
		t.Error("the success-path record must come after ReceiveResend")
	}
	if strings.Contains(body, "mailing_record_received_webhook") {
		t.Error("handleResendReceived must record only through finishReceived")
	}
	if !strings.Contains(src, `if state != "new" {`) {
		t.Error("a seen/ignored event must short-circuit before any fetch")
	}
}
```

Create `internal/security_regression/mf017_no_fallback_pin_test.go`:

```go
// Finding MF-017-NO-FALLBACK (doc_support017.go / spec 017 R11).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017NoSharedTransportFallback(t *testing.T) {
	src := mustRead(t, "../platform/notify/sender_subscriber.go")
	if !strings.Contains(src, "return s.sendViaSupportProfile(ctx, tx, e, p, mail, identity)") {
		t.Error("Handle must RETURN the support-profile path (no fall-through to Sender)")
	}
	start := strings.Index(src, "func (s SendSubscriber) sendViaSupportProfile(")
	end := strings.Index(src, "func isFinalAttempt(")
	if start < 0 || end < start {
		t.Fatal("sendViaSupportProfile not found")
	}
	if strings.Contains(src[start:end], "s.Sender") {
		t.Error("sendViaSupportProfile must never use the shared Sender")
	}
	mig := mustRead(t, "../../migrations/0139_support_email_resend.up.sql")
	fn := mig[strings.Index(mig, "FUNCTION get_resend_support_identity("):]
	fn = fn[:strings.Index(fn, "$$;")]
	if strings.Contains(fn, "p.status = 'verified'") {
		t.Error("get_resend_support_identity must return non-verified profiles so the sender refuses instead of falling back")
	}
}
```

Create `internal/security_regression/mf017_raw_download_pin_test.go`:

```go
// Finding MF-017-RAW-DOWNLOAD (doc_support017.go / spec 017 R2, R6).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017RawDownloadBoundary(t *testing.T) {
	src := mustRead(t, "../mailing/provider/resend_receiving.go")
	start := strings.Index(src, "func (r *Resend) DownloadReceivedRaw(")
	end := strings.Index(src, "func (r *Resend) EnsureReceivingEvents(")
	if start < 0 || end < start {
		t.Fatal("DownloadReceivedRaw not found")
	}
	body := src[start:end]
	for _, pin := range []string{`u.Scheme == "https"`, "io.LimitReader(resp.Body, maxBytes+1)", "urlErr.Err"} {
		if !strings.Contains(body, pin) {
			t.Errorf("DownloadReceivedRaw missing %q", pin)
		}
	}
	if strings.Contains(body, "Authorization") {
		t.Error("the pre-signed raw URL must never receive the API key")
	}
	if !strings.Contains(mustRead(t, "../../cmd/manyforge/main.go"), "RawDownloadClient = netsafe.NewClient(") {
		t.Error("production must fetch raw URLs through the SSRF-guarded netsafe client")
	}
}
```

Create `internal/security_regression/mf017_definers_pin_test.go`:

```go
// Finding MF-017-DEFINERS (doc_support017.go / spec 017 R7 + data model).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017DefinersAndGlobalUniqueness(t *testing.T) {
	mig := mustRead(t, "../../migrations/0139_support_email_resend.up.sql")
	for _, fn := range []string{
		"resolve_inbound_address_for_business", "get_resend_support_identity",
		"set_message_dropped_attachments", "support_audit_inbound_dropped",
		"mailing_received_webhook_state", "mailing_record_received_webhook",
		"mailing_list_verified_resend_profiles",
	} {
		start := strings.Index(mig, "FUNCTION "+fn+"(")
		if start < 0 {
			t.Errorf("missing function %s", fn)
			continue
		}
		body := mig[start:]
		body = body[:strings.Index(body, "$$;")]
		if !strings.Contains(body, "SECURITY DEFINER") || !strings.Contains(body, "SET search_path = ") {
			t.Errorf("%s is not a search-path-pinned SECURITY DEFINER", fn)
		}
		if !strings.Contains(mig, "REVOKE ALL ON FUNCTION "+fn+"(") || !strings.Contains(mig, "GRANT EXECUTE ON FUNCTION "+fn+"(") {
			t.Errorf("%s must REVOKE PUBLIC and GRANT manyforge_app", fn)
		}
	}
	for _, pin := range []string{
		"CREATE UNIQUE INDEX email_domain_verified_global_uq",
		"WHERE verified_at IS NOT NULL",
		"CREATE UNIQUE INDEX inbound_address_custom_global_uq",
		"RAISE EXCEPTION '0139: a verified email domain is shared",
	} {
		if !strings.Contains(mig, pin) {
			t.Errorf("0139 missing %q", pin)
		}
	}
}
```

Run: `go test -count=1 -run 'TestPin_MF017' ./internal/security_regression/`
Expected: PASS (6 pins). If a pin fails, an earlier task drifted from the literal this plan specified. Fix the code, not the pin.

- [ ] **Step 4: Write the runbook**

Create `docs/runbooks/support-inbound-resend.md`:

````markdown
# Support inbound mail via Resend

How a business receives support mail on its own subdomain through its Resend
account (spec 017). Replies go out through the same Resend profile.

## Before deploying this release (hub)

The server now refuses to start in production without
`MANYFORGE_INBOUND_REPLY_TOKEN_SECRET`. Flux deploys the chart straight from
`master`, so add the key **before** merging:

```bash
export KUBECONFIG=~/.kube/talos
kubectl -n manyforge patch secret manyforge-masterkeys --type=merge \
  -p "{\"stringData\":{\"inbound-reply-token\":\"$(openssl rand -base64 48)\"}}"
kubectl -n manyforge get secret manyforge-masterkeys -o jsonpath='{.data.inbound-reply-token}' | base64 -d | wc -c   # > 0
```

Also add the line to `~/dev/infra-k8s/docs/manyforge/deploy-secrets.sh`.

Consequence: reply tokens minted before this change were signed with an empty
key and no longer verify. Those replies still thread by `In-Reply-To`/`References`.

Optional, closes `manyforge-g97y`: set `blob.url` (for example
`s3://manyforge-attachments?region=us-east-1`) in the HelmRelease values. If the
bucket needs static credentials, set `secrets.blob.secretName` to a Secret holding
`access-key-id` and `secret-access-key`.

## After deploying

- On boot, each replica runs a sweep that adds `email.received` to every verified
  Resend profile's webhook (`PATCH /webhooks/{id}`). Check the logs for
  `mailing Resend webhook subscribed to email.received` (one per profile, first
  boot only). A warning `signing secret changed` means that profile must be
  re-verified from **Mailing → Sending profile → Verify**.

## Onboarding a business (garden.gg example)

1. The business has a verified Resend sending profile (Mailing settings).
2. In Resend: **Domains → Add** `support.garden.gg`, enable **Receiving**.
3. In ManyForge: **Support → Email domains**, add `support.garden.gg` with mode
   `provider_route`. The page shows "Received via Resend" and the checklist.
4. Publish every record from the checklist. All of them sit under
   `support.garden.gg`:
   - `TXT _manyforge.support.garden.gg` = the shown `mf-verify=…` token
   - `MX support.garden.gg` = Resend's receiving MX (lowest priority on that name)
   - `TXT resend._domainkey.support.garden.gg` = Resend's DKIM value
   - `MX send.support.garden.gg` and `TXT send.support.garden.gg` = Resend's SPF records
   The apex `garden.gg` MX (Microsoft 365) and SPF are not touched.
5. Verify the domain in Resend, then click **Verify** in ManyForge.
6. Add the inbound address `support@support.garden.gg`.

Note for spec 018: the future portal at `support.garden.gg` must use proxied
`A`/`AAAA` records, never a `CNAME`, because a CNAME cannot coexist with the MX above.

## Live verification (hub)

1. Send a real email to `support@support.garden.gg`. A ticket appears in garden.gg's queue.
2. Reply from the ticket. The customer receives it from `support@support.garden.gg`
   with `Reply-To: support+<token>@support.garden.gg`, DKIM-aligned to the subdomain.
3. Reply from the customer's mail client. The message lands on the same ticket.
4. `dig MX garden.gg` and `dig TXT garden.gg` show the same values as before rollout.

## Observed facts (fill in during live verification)

Search the logs for `mailing Resend inbound recipients observed` on step 3 and
write down what you see:

- Webhook `to` keeps `+token` tags: `webhook_to_has_plus_tag=` ___
- Receiving API `to` keeps `+token` tags: `api_to_has_plus_tag=` ___
- `PATCH /webhooks/{id}` kept the signing secret: yes/no ___

Routing never depends on these answers. Recipients are read from the raw `To`/`Cc` headers.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Pod crash-loops with `MANYFORGE_INBOUND_REPLY_TOKEN_SECRET: required in production` | key missing from `manyforge-masterkeys` | patch the Secret (above) |
| Resend shows webhook deliveries failing with 503 | Resend fetch failed, ingest error, or `Inbound` not wired | check `mailing Resend received email fetch failed` / `inbound ingest failed` logs; Resend retries automatically |
| Mail accepted (200) but no ticket | no recipient owned by the profile's business, or the domain is unverified | look for `audit_entry.action = 'ticket.inbound_dropped'`; verify the domain and address |
| Reply shows **Delivery failed** | the business's Resend profile can't send (unverified, revoked key) | re-verify the sending profile; replies never fall back to the shared transport |
| Verify on Email domains returns 409 | another business already verified that domain | domains are globally unique; use a different subdomain |
````

- [ ] **Step 5: Run every gate**

Run, in order:

```bash
go build ./...
go vet ./...
make lint
go test ./...
go test -tags contract -count=1 ./cmd/...
DOCKER_HOST=unix://$HOME/.colima/mftest/docker.sock go test -tags integration -timeout 1200s -p 1 ./...
make sdk-check
cd web && npx ng test --watch=false && npm run check:tokens && E2E_PORT=4311 npx playwright test && cd ..
go test -count=1 ./charts/manyforge/
```

Expected: every command PASSES. Fix any failure, whenever it was introduced. The global instructions treat no failure as "pre-existing".

- [ ] **Step 6: Commit**

```bash
git add internal/inbox/resend_loop_integration_test.go internal/security_regression/doc_support017.go \
  internal/security_regression/mf017_*_pin_test.go docs/runbooks/support-inbound-resend.md
git commit -m "test(support): end-to-end Resend support loop, MF-017 pins, and runbook"
```

- [ ] **Step 7: Clean up and push**

Run: `git worktree list` (expect only the main checkout), then `ps aux | grep 'output-format stream-json' | grep -v grep | wc -l` (expect `0` once subagents finish), then `git status` (expect clean).
Push the branch: `git push -u origin feat/support-email-resend`. Open one PR into `master`. In the body, include a `feat(sdk): TicketMessage gains dropped_attachments; verify email domain documents 409` line and link the runbook's "Before deploying" section. **Do not merge** until the `manyforge-masterkeys` secret has the `inbound-reply-token` key, and merge manually (no `gh pr merge --auto`). Check `mergeable` before merging, because `.beads/issues.jsonl` conflicts silently skip CI.

---

## Self-Review Checklist (completed while writing)

- R1 → Task 4 (create with three events, PATCH upgrade in reconcile) + Task 6 (boot sweep). R2 → Tasks 4, 5, 6. R3 → Task 5 (headers first, authenticated envelope fallback). R4 → Tasks 1, 5 (+ MF-017-SAME-BUSINESS). R5 → Tasks 1, 6, 9. R6 → Tasks 4, 6. R7 → Task 1. R8 → Task 1 (DKIM informational). R9 → Task 8. R10-R12 → Task 7. R13 → Task 7 (both transports). R14 → Task 2. R15 → Tasks 3, 8. R16 → Task 2.
- Success criteria: loop + token-only threading → Task 9. Cross-business, duplicate delivery, fetch retry, disallowed attachment, final failure, domain conflict → Tasks 5, 6, 9, 3, 7, 1 respectively. Hub checks and the plus-tag observation → runbook.
- Names used across tasks: `resolveRecipientForBusiness`, `RawMessage.RestrictToBusiness`, `ReceiveResend(ctx, businessID, eventID, envelopeRecipients, raw)`, `ResendInboundReceiver`, `ResendWebhookEvents`, `GetReceivedEmail`/`DownloadReceivedRaw`/`EnsureReceivingEvents`, `ErrReceivedEmailTooLarge`/`ErrReceivedEmailGone`/`ErrReceivedRawUnavailable`, `SendSupportReply`, `SupportProfileSender`, `RecipientSuppression`, `isFinalAttempt`, `get_resend_support_identity(business, tenant) → (profile_id, from_address, profile_status)`.
