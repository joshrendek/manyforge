# US2 — Reply to a Ticket & Keep the Conversation Threaded — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. The project is **TDD-mandatory** and **bd-tracked** (no TodoWrite). Commit per task; the repo is **local-only (no remote)** — never `git push`.

**Goal:** Let an authorized member reply to a ticket (threaded outbound email) or add an internal note (never delivered); thread the requester's response back via headers + the reply token; suppress hard-bouncing recipients and surface failures on the ticket.

**Architecture:** Thin HTTP handlers → `ticketing.Service` write methods (run under `db.WithPrincipal`, dual-enforced ownership predicate, 404-no-oracle) that persist an outbound/note `ticket_message`, bump `last_message_at`, write an in-tx audit entry, and (reply only) enqueue an `outbox` event — all in one transaction. The SL-C outbox worker drains the event to a `notify` subscriber that builds a threaded `notify.Mail` (In-Reply-To/References + `Reply-To: <verp>+<reply_token>@domain`) and dispatches it through a configurable `notify.Sender` (real SMTP with **optional** DKIM; `LogSender` default in dev/test). Hard bounces arrive on a dedicated HMAC-authed `POST /inbound/bounce` webhook → insert spec-001 `email_suppression`, mark the outbound message failed; the reply path refuses suppressed recipients (409).

**Tech Stack:** Go 1.x (`internal/` layout, thin handlers, services hold logic), PostgreSQL + RLS (`ENABLE` not `FORCE`), `sqlc`/`dbgen` for plain-table queries, raw pgx only for `RETURNS TABLE` DEFINERs, golang-migrate (`migrations/NNNN_*.{up,down}.sql`), chi router, testcontainers (`-tags integration`, `make int-test` is `-p 1`), Angular + Playwright (`web/`), `github.com/emersion/go-msgauth/dkim` for optional DKIM.

---

## Decisions (locked with the user — do not relitigate)

1. **Transport depth:** Implement a real `SMTPSender` behind `notify.Sender` with **optional, config-gated DKIM**. It must work with DKIM *unconfigured* (for testing / un-provisioned environments) — sign only when a signing key is present. `LogSender` stays the default when SMTP isn't configured. Custom-domain DKIM identities remain **US4**; US2 sends from the business's **system** inbound address only (T039).
2. **Hard-bounce intake:** Dedicated **`POST /inbound/bounce`** webhook, HMAC-authed and body-capped, **symmetric with the US1 inbound webhook** (own purpose-separated secret `InboundBounceSecret`; uniform no-oracle ack).
3. **Note permission:** Gate `POST …/note` on **`tickets.reply`** (the permission catalog in `migrations/0015` describes `tickets.reply` as *"Send replies **and internal notes** on a ticket"* — authoritative). The OpenAPI `note` summary currently says `tickets.write`; this plan **corrects the OpenAPI prose to `tickets.reply`** (Task 5). Functionally identical for preset roles (owner/admin/member have both); only a custom role split would differ.
4. **Delivery surfacing:** Add a nullable `delivery_state` enum (`pending|sent|failed`) + `delivery_error text` to `ticket_message` (nullable ⇒ the US1 inbound DEFINER insert is untouched; inbound/note rows stay NULL). Expose `delivery_state` on the `TicketMessage` schema + DTO so a failed reply is visible on the ticket.
5. **Outbound rate limit (T041):** per-`business` AND per-`requester(recipient)` token-bucket at the reply **service boundary** (clamp at the service, not the handler — non-handler callers must be limited too). Folded into Task 3.

---

## File Structure

**Migrations**
- Create `migrations/0018_ticket_message_delivery.up.sql` / `.down.sql` — `message_delivery_state` enum + `delivery_state`/`delivery_error` columns on `ticket_message`.

**SQL queries (sqlc → dbgen)**
- Modify `db/query/ticketing.sql` — add `InsertOutboundMessage`, `InsertNoteMessage`, `GetThreadingParent`, `MarkMessageDelivered`, `MarkMessageFailed`, `GetBusinessSystemInboundAddress`, `GetOutboundMessageForBounce`; add `delivery_state`/`delivery_error` to the message read query. Run `make generate`.
- Modify `db/query/suppression.sql` (create if absent) — `InsertSuppression`, `IsSuppressed`.

**notify (send + suppression)**
- Modify `internal/platform/notify/notify.go` — add `Mail.AutoSubmitted`, `Mail.Headers` (optional), `Mail.EnvelopeFrom`.
- Create `internal/platform/notify/smtp.go` — `SMTPSender` (implements `Sender`), `SMTPConfig`, `DKIMConfig`, pure `buildMIME`.
- Create `internal/platform/notify/suppression.go` — DB-backed `SuppressionChecker`.
- Create `internal/platform/notify/sender_subscriber.go` — outbox subscriber that dispatches a queued reply through `Sender` and records delivery.
- Create `internal/platform/notify/smtp_test.go`, `internal/platform/notify/send_integration_test.go`.

**ticketing (service + handlers)**
- Modify `internal/ticketing/service.go` — `Reply(...)`, `AddNote(...)`, helpers (`buildOutboundHeaders`, `mintMessageID`), `Service` gains `Clock`/limiter/secret deps. `Message` gains `DeliveryState *string`.
- Modify `internal/ticketing/handler.go` — `WriteRoutes`, `reply`, `addNote` handlers; `ReplyBody`/`NoteBody`; `messageResp.DeliveryState`.
- Create `internal/ticketing/reply_integration_test.go` (T036) and `internal/ticketing/note_integration_test.go`.

**inbox (bounce webhook)**
- Create `internal/inbox/bounce.go` — `BounceHandler` (HMAC, body cap, parse, suppress, mark-failed).
- Create `internal/inbox/bounce_test.go` + `internal/inbox/bounce_integration_test.go`.

**wiring + contract**
- Modify `cmd/manyforge/main.go` — `ticketsReply` middleware, mount `WriteRoutes`, init outbound limiter, register notify subscriber + topic, mount bounce webhook, build `SMTPSender`-or-`LogSender` from config, init `BounceHandler`.
- Modify `internal/platform/config/*` — SMTP + DKIM + bounce-secret + outbound-rate config.
- Modify `internal/platform/events/bus.go` — `TopicTicketReplied`.
- Modify `cmd/manyforge/drift_002_test.go` — add reply/note/bounce operationIds to `inScope002Ops`.
- Modify `specs/002-support-desk/contracts/openapi.yaml` — note summary → `tickets.reply`; add `delivery_state` to `TicketMessage`; add `/inbound/bounce` path.

**frontend**
- Modify `web/src/app/pages/support/` thread view + `ticket.service.ts` — reply composer + note toggle (T042).
- Modify `web/e2e/support.spec.ts` — US2 portion (T043).

---

## Task 1 — Migration 0018 (delivery state) + sqlc queries

Maps to: groundwork for T036–T040. **No DEFINER change** (new column is nullable; the US1 inbound insert omits it → NULL).

**Files:**
- Create: `migrations/0018_ticket_message_delivery.up.sql`, `migrations/0018_ticket_message_delivery.down.sql`
- Modify: `db/query/ticketing.sql`, `db/query/suppression.sql` (create if absent)
- Regenerate: `internal/platform/db/dbgen/*` (via `make generate`)

- [ ] **Step 1: Write the up migration**

`migrations/0018_ticket_message_delivery.up.sql`:
```sql
-- US2: surface outbound delivery on the ticket (T040). Nullable so the US1 inbound
-- DEFINER insert (which omits the column) leaves it NULL; only outbound rows carry a
-- delivery lifecycle (pending -> sent | failed). Notes are never delivered (NULL).
CREATE TYPE message_delivery_state AS ENUM ('pending', 'sent', 'failed');

ALTER TABLE ticket_message
    ADD COLUMN delivery_state message_delivery_state,
    ADD COLUMN delivery_error text;
```

- [ ] **Step 2: Write the down migration**

`migrations/0018_ticket_message_delivery.down.sql`:
```sql
ALTER TABLE ticket_message DROP COLUMN IF EXISTS delivery_error;
ALTER TABLE ticket_message DROP COLUMN IF EXISTS delivery_state;
DROP TYPE IF EXISTS message_delivery_state;
```

- [ ] **Step 3: Add ticketing write/threading queries**

Append to `db/query/ticketing.sql` (mirror existing `-- name:` style; confirm exact column names against `migrations/0013_support_desk.up.sql`):
```sql
-- name: GetThreadingParent :one
-- Latest message on a ticket (any direction) — its message_id becomes the new
-- outbound In-Reply-To; its references chain (+ its own id) becomes References.
SELECT message_id, "references"
FROM ticket_message
WHERE ticket_id = $1 AND business_id = $2 AND tenant_root_id = $3
ORDER BY created_at DESC
LIMIT 1;

-- name: InsertOutboundMessage :one
INSERT INTO ticket_message (
    id, ticket_id, business_id, tenant_root_id, direction, author_principal_id,
    message_id, in_reply_to, "references", body_text, body_html, delivery_state)
VALUES ($1, $2, $3, $4, 'outbound', $5, $6, $7, $8, $9, $10, 'pending')
RETURNING *;

-- name: InsertNoteMessage :one
INSERT INTO ticket_message (
    id, ticket_id, business_id, tenant_root_id, direction, author_principal_id,
    message_id, body_text, body_html)
VALUES ($1, $2, $3, $4, 'note', $5, $6, $7, $8)
RETURNING *;

-- name: BumpTicketActivity :exec
UPDATE ticket SET last_message_at = now(), updated_at = now()
WHERE id = $1 AND business_id = $2 AND tenant_root_id = $3;

-- name: MarkMessageDelivered :exec
UPDATE ticket_message SET delivery_state = 'sent', delivery_error = NULL
WHERE id = $1 AND tenant_root_id = $2;

-- name: MarkMessageFailed :exec
UPDATE ticket_message SET delivery_state = 'failed', delivery_error = $3
WHERE id = $1 AND tenant_root_id = $2;

-- name: GetBusinessSystemInboundAddress :one
-- The system (kind='system') inbound address for a business — the From/Reply-To
-- routing base for outbound. US2 sends from the system identity only.
SELECT address FROM inbound_address
WHERE business_id = $1 AND tenant_root_id = $2 AND kind = 'system'
ORDER BY created_at ASC
LIMIT 1;

-- name: GetOutboundMessageForBounce :one
-- Correlate a bounce to the most recent outbound message to a recipient on a
-- business, for surfacing the failure. Bounce intake is principal-less.
SELECT tm.id, tm.tenant_root_id
FROM ticket_message tm
JOIN ticket t ON t.id = tm.ticket_id AND t.tenant_root_id = tm.tenant_root_id
JOIN requester rq ON rq.id = t.requester_id AND rq.tenant_root_id = t.tenant_root_id
WHERE tm.direction = 'outbound' AND rq.email = $1 AND tm.tenant_root_id = $2
ORDER BY tm.created_at DESC
LIMIT 1;
```

- [ ] **Step 4: Add suppression queries**

Create/append `db/query/suppression.sql`:
```sql
-- name: IsSuppressed :one
SELECT EXISTS (SELECT 1 FROM email_suppression WHERE email = $1);

-- name: InsertSuppression :exec
INSERT INTO email_suppression (email, reason) VALUES ($1, $2)
ON CONFLICT (email) DO NOTHING;
```

- [ ] **Step 5: Add `delivery_state` to the message read query**

In `db/query/ticketing.sql`, find the existing message-list query (the one backing `ListMessages`) and add `delivery_state` to its SELECT column list (keep `delivery_error` server-side only — do not expose). If it uses `SELECT tm.*`, no change is needed; if explicit columns, add `tm.delivery_state`.

- [ ] **Step 6: Regenerate dbgen**

Run: `make generate`
Expected: `internal/platform/db/dbgen/` updates; `TicketMessage` model gains `DeliveryState NullMessageDeliveryState` and `DeliveryError *string`; new `Queries` methods compile. **Never hand-edit generated files.**

- [ ] **Step 7: Verify migration reversibility + build**

Run:
```bash
PORT=55438; CID=$(docker run -d --rm -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=mf -p $PORT:5432 postgres:16)
until docker exec "$CID" pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
URL="pgx5://postgres:pw@localhost:$PORT/mf?sslmode=disable"
migrate -path migrations -database "$URL" up && migrate -path migrations -database "$URL" down 1 && migrate -path migrations -database "$URL" up 1
docker stop "$CID"
go build ./...
```
Expected: up→18, down→17, up→18 all clean; `go build ./...` succeeds.

- [ ] **Step 8: Commit**
```bash
git add migrations/0018_*.sql db/query/*.sql internal/platform/db/dbgen/
git commit -m "feat(002): T035 prep — ticket_message delivery_state + US2 sqlc queries"
```

---

## Task 2 — notify: DB suppression checker + configurable SMTP sender (optional DKIM)

Maps to: T039 transport (Decision 1). Build the *transport* before the service that enqueues to it, so the subscriber (Task 6) has a real `Sender`.

**Files:**
- Modify: `internal/platform/notify/notify.go` (extend `Mail`)
- Create: `internal/platform/notify/suppression.go`, `internal/platform/notify/smtp.go`, `internal/platform/notify/smtp_test.go`

- [ ] **Step 1: Extend `Mail`** — add fields the SMTP sender needs. In `internal/platform/notify/notify.go`, add to the `Mail` struct:
```go
	EnvelopeFrom string   // SMTP MAIL FROM (VERP/return-path); falls back to From if empty
	AutoSubmitted string  // RFC 3834 value, e.g. "auto-replied"; empty ⇒ header omitted (human reply)
	ExtraHeaders map[string]string // optional additional headers (e.g. List-Unsubscribe); may be nil
```

- [ ] **Step 2: Write the failing MIME-build test**

`internal/platform/notify/smtp_test.go`:
```go
package notify

import (
	"strings"
	"testing"
)

func TestBuildMIMEHasThreadingHeaders(t *testing.T) {
	m := Mail{
		From:       "support@inbound.localhost",
		To:         "ada@example.com",
		Subject:    "Re: login broken",
		BodyText:   "We are looking into it.",
		MessageID:  "out-1@inbound.localhost",
		InReplyTo:  "in-1@example.com",
		References: []string{"in-1@example.com"},
		ReplyTo:    "support+TOKEN.SIG@inbound.localhost",
	}
	raw, err := buildMIME(m)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	s := string(raw)
	for _, want := range []string{
		"Message-ID: <out-1@inbound.localhost>",
		"In-Reply-To: <in-1@example.com>",
		"References: <in-1@example.com>",
		"Reply-To: support+TOKEN.SIG@inbound.localhost",
		"From: support@inbound.localhost",
		"To: ada@example.com",
		"Subject: Re: login broken",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("MIME missing %q\n---\n%s", want, s)
		}
	}
	// Human reply: no Auto-Submitted header unless explicitly set.
	if strings.Contains(s, "Auto-Submitted:") {
		t.Errorf("unexpected Auto-Submitted header on a human reply")
	}
}

func TestBuildMIMEStampsAutoSubmittedWhenSet(t *testing.T) {
	raw, err := buildMIME(Mail{From: "a@b", To: "c@d", Subject: "s", MessageID: "m@b", AutoSubmitted: "auto-replied"})
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	if !strings.Contains(string(raw), "Auto-Submitted: auto-replied") {
		t.Errorf("missing Auto-Submitted header")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/platform/notify/ -run TestBuildMIME -v`
Expected: FAIL — `undefined: buildMIME`.

- [ ] **Step 4: Implement `smtp.go`**

`internal/platform/notify/smtp.go`:
```go
package notify

import (
	"bytes"
	"context"
	"crypto"
	"fmt"
	"net/smtp"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// DKIMConfig is optional; when nil the sender emits unsigned mail (valid for the
// system domain in dev / un-provisioned environments). When set, outbound is
// DKIM-signed (FR-013 deliverability for the system identity).
type DKIMConfig struct {
	Domain     string
	Selector   string
	PrivateKey crypto.Signer // ed25519.PrivateKey or *rsa.PrivateKey
}

// SMTPConfig drives the real sender. Host == "" means "not configured" — callers
// should fall back to LogSender (see cmd/manyforge wiring).
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	DKIM     *DKIMConfig // optional
}

// SMTPSender implements Sender over a real MTA, with optional DKIM signing and a
// suppression gate. Build via NewSMTPSender.
type SMTPSender struct {
	cfg         SMTPConfig
	suppression mailerSuppression
}

// mailerSuppression is the minimal suppression contract (mailer.SuppressionChecker).
type mailerSuppression interface {
	IsSuppressed(ctx context.Context, email string) (bool, error)
}

func NewSMTPSender(cfg SMTPConfig, suppression mailerSuppression) *SMTPSender {
	return &SMTPSender{cfg: cfg, suppression: suppression}
}

func (s *SMTPSender) Send(ctx context.Context, m Mail) error {
	if s.suppression != nil {
		suppressed, err := s.suppression.IsSuppressed(ctx, m.To)
		if err != nil {
			return fmt.Errorf("suppression check: %w", err)
		}
		if suppressed {
			return ErrSuppressed
		}
	}
	raw, err := buildMIME(m)
	if err != nil {
		return err
	}
	if s.cfg.DKIM != nil {
		signed, serr := signDKIM(raw, *s.cfg.DKIM)
		if serr != nil {
			return fmt.Errorf("dkim sign: %w", serr)
		}
		raw = signed
	}
	from := m.EnvelopeFrom
	if from == "" {
		from = m.From
	}
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	var auth smtp.Auth
	if s.cfg.Username != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	}
	return smtp.SendMail(addr, auth, from, []string{m.To}, raw)
}

// buildMIME renders an RFC 822 message. Pure (no network) so it is unit-tested.
func buildMIME(m Mail) ([]byte, error) {
	var b bytes.Buffer
	h := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	h("From", m.From)
	h("To", m.To)
	h("Subject", m.Subject)
	if m.MessageID != "" {
		h("Message-ID", "<"+m.MessageID+">")
	}
	if m.InReplyTo != "" {
		h("In-Reply-To", "<"+m.InReplyTo+">")
	}
	if len(m.References) > 0 {
		refs := make([]string, len(m.References))
		for i, r := range m.References {
			refs[i] = "<" + r + ">"
		}
		h("References", strings.Join(refs, " "))
	}
	h("Reply-To", m.ReplyTo)
	h("Auto-Submitted", m.AutoSubmitted)
	for k, v := range m.ExtraHeaders {
		h(k, v)
	}
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	b.WriteString("\r\n")
	b.WriteString(m.BodyText)
	if !strings.HasSuffix(m.BodyText, "\r\n") {
		b.WriteString("\r\n")
	}
	return b.Bytes(), nil
}

func signDKIM(raw []byte, cfg DKIMConfig) ([]byte, error) {
	opts := &dkim.SignOptions{
		Domain:   cfg.Domain,
		Selector: cfg.Selector,
		Signer:   cfg.PrivateKey,
		Hash:     crypto.SHA256,
	}
	var out bytes.Buffer
	if err := dkim.Sign(&out, bytes.NewReader(raw), opts); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
```

- [ ] **Step 5: Ensure the dkim dependency is present**

Run: `go get github.com/emersion/go-msgauth/dkim && go mod tidy`
Expected: no error; `go.mod` includes `github.com/emersion/go-msgauth`.

- [ ] **Step 6: Run the MIME tests to verify they pass**

Run: `go test ./internal/platform/notify/ -run TestBuildMIME -v`
Expected: PASS (both). Output pristine.

- [ ] **Step 7: Write + implement the DB suppression checker (TDD)**

`internal/platform/notify/suppression.go`:
```go
package notify

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// DBSuppression checks spec-001's email_suppression table. Bounce intake is
// principal-less, and email_suppression has no tenant scope, so the lookup runs in
// a plain WithTx (no RLS principal). Implements mailer.SuppressionChecker.
type DBSuppression struct{ DB *db.DB }

func (s DBSuppression) IsSuppressed(ctx context.Context, email string) (bool, error) {
	var out bool
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		v, qerr := dbgen.New(tx).IsSuppressed(ctx, email)
		out = v
		return qerr
	})
	return out, err
}
```
Add a quick integration test `internal/platform/notify/suppression_integration_test.go` (`//go:build integration`): seed a row via `tdb.Super`, assert `IsSuppressed` true for it / false for another. Use the `testdb` harness pattern from `internal/inbox/ingest_integration_test.go`.

- [ ] **Step 8: Verify + commit**

Run: `go build ./... && go test ./internal/platform/notify/ && go test -tags integration -run Suppress ./internal/platform/notify/`
Expected: all PASS.
```bash
git add internal/platform/notify/ go.mod go.sum
git commit -m "feat(002): T039 transport — configurable SMTPSender (optional DKIM) + DB suppression checker"
```

---

## Task 3 — ticketing.Service.Reply (+ outbound rate limit T041)

Maps to: **T037**, **T041**, part of **T036**. Mirrors the existing `GetTicket` pattern (`WithPrincipal` + explicit `business_id` predicate + `mapErr`). Reply is a write, so the whole thing is one tx: insert outbound message (pending) → bump activity → audit → enqueue outbox — then commit.

**Files:**
- Modify: `internal/ticketing/service.go`
- Create: `internal/ticketing/reply_integration_test.go`

- [ ] **Step 1: Extend `Service` + `Message` + add input type**

In `internal/ticketing/service.go`:
```go
// Service deps for US2 writes. ReplyTokenKey signs the VERP Reply-To; SystemDomain
// is the outbound mail domain for minted Message-IDs; OutboundLimiter caps sends
// per business/recipient (FR-020, T041); Suppression refuses suppressed recipients.
type Service struct {
	DB             *db.DB
	ReplyTokenKey  []byte
	SystemDomain   string
	OutboundLimiter ratelimit.Limiter         // nil ⇒ no limit (tests/dev)
	Suppression    mailer.SuppressionChecker  // nil ⇒ no pre-check (worker still gates)
}

// ReplyInput is the validated reply payload.
type ReplyInput struct {
	BodyText string
	BodyHTML *string
}
```
Add `DeliveryState *string` to the `Message` struct and map it in the read assembler + `toMessageResp` (Task 5 covers the DTO side). Add imports: `ratelimit`, `mailer`, `events`, `ticketing` already in package; `uuid`, `fmt`, `errors`.

- [ ] **Step 2: Write the failing reply integration test**

`internal/ticketing/reply_integration_test.go` (`//go:build integration`). Reuse the `testdb` + seeding pattern; you will need a seeded business, a member principal with `tickets.reply`, a ticket with one inbound message, and a requester. (Mirror `internal/inbox/ingest_integration_test.go` seeding + any existing ticketing integration seed helper.)
```go
//go:build integration

package ticketing_test
// ... imports: context, testing, time, uuid, testdb, ticketing, ratelimit ...

func TestReplyInsertsOutboundAndEnqueues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb := mustStartTestDB(ctx, t)            // local helper wrapping testdb.Start
	fx := seedReplyFixture(ctx, t, tdb)        // business, member pid (tickets.reply), ticket+inbound msg, requester email

	svc := &ticketing.Service{DB: tdb.App, ReplyTokenKey: []byte("test-reply-token-key-0123456789ab"), SystemDomain: "inbound.localhost"}

	msg, err := svc.Reply(ctx, fx.principalID, fx.businessID, fx.ticketID, ticketing.ReplyInput{BodyText: "we are on it"})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if msg.Direction != "outbound" {
		t.Errorf("direction = %q, want outbound", msg.Direction)
	}
	if msg.DeliveryState == nil || *msg.DeliveryState != "pending" {
		t.Errorf("delivery_state = %v, want pending", msg.DeliveryState)
	}
	// In-Reply-To threads to the seeded inbound message.
	if msg.InReplyTo == nil || *msg.InReplyTo != fx.inboundMessageID {
		t.Errorf("in_reply_to = %v, want %q", msg.InReplyTo, fx.inboundMessageID)
	}
	// One outbound message persisted, last_message_at bumped, audit + outbox written.
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='outbound'", fx.ticketID); n != 1 {
		t.Errorf("outbound count = %d, want 1", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", fx.tenantRootID); n != 1 {
		t.Errorf("outbox ticket.replied = %d, want 1", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ticket.replied'", msg.ID); n != 1 {
		t.Errorf("audit count = %d, want 1", n)
	}
}

func TestReplyUnknownTicketIsNotFound(t *testing.T) {
	// ... reply to a random ticketID ⇒ errors.Is(err, errs.ErrNotFound) (no oracle).
}

func TestReplyToSuppressedRecipientIsConflict(t *testing.T) {
	// ... seed email_suppression for the requester; svc.Suppression = notify.DBSuppression{DB: tdb.App};
	// Reply ⇒ errors.Is(err, errs.ErrConflict).
}

func TestReplyRateLimitedIsConflictOr429(t *testing.T) {
	// ... svc.OutboundLimiter = ratelimit.NewTokenBucket(0, 1) (1 burst then deny);
	// first Reply ok, second ⇒ errors.Is(err, errs.ErrRateLimited).
}
```
> Note: add `ErrRateLimited` to `internal/platform/errs/errs.go` and map it to **429** in `httpx.WriteError` (the contract lists `429` for reply). Do this as the first sub-step if the sentinel is absent.

- [ ] **Step 3: Run to verify failure**

Run: `go test -tags integration -run TestReply ./internal/ticketing/ -v`
Expected: FAIL — `svc.Reply` undefined (and `errs.ErrRateLimited` undefined until added).

- [ ] **Step 4: Add `ErrRateLimited` + 429 mapping**

In `internal/platform/errs/errs.go` add `ErrRateLimited = errors.New("rate limited")`. In `internal/platform/httpx/errors.go` `WriteError`, add a case **before** the default:
```go
	case errors.Is(err, errs.ErrRateLimited):
		WriteJSON(w, http.StatusTooManyRequests, ErrorBody{Code: "RATE_LIMITED", Message: "rate limited"})
```
And map it through `ticketing.mapErr` (pass-through like `ErrValidation`).

- [ ] **Step 5: Implement `Reply` + helpers**

In `internal/ticketing/service.go`:
```go
// Reply sends an outbound reply on a ticket (FR-008). One transaction: insert the
// outbound message (delivery_state='pending'), bump last_message_at, audit, and
// enqueue the 'ticket.replied' outbox event the notify worker drains to actually
// send mail. Dual-enforced (WithPrincipal + business_id predicate); unknown/foreign
// ticket ⇒ ErrNotFound (no oracle). Suppressed recipient ⇒ ErrConflict. Rate-limited
// ⇒ ErrRateLimited.
func (s *Service) Reply(ctx context.Context, principalID, businessID, ticketID uuid.UUID, in ReplyInput) (Message, error) {
	if len(in.BodyText) == 0 {
		return Message{}, fmt.Errorf("ticketing: empty reply: %w", errs.ErrValidation)
	}
	var out Message
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		// Load + own-check the ticket (404-no-oracle via mapErr on ErrNoRows).
		tk, terr := q.GetTicket(ctx, dbgen.GetTicketParams{ID: ticketID, BusinessID: businessID})
		if terr != nil {
			return terr
		}

		// Recipient suppression (pre-check; the worker re-checks at send).
		recipient := tk.RequesterEmail // adjust to the actual GetTicket projection
		if s.Suppression != nil {
			suppressed, serr := s.Suppression.IsSuppressed(ctx, recipient)
			if serr != nil {
				return serr
			}
			if suppressed {
				return fmt.Errorf("ticketing: recipient suppressed: %w", errs.ErrConflict)
			}
		}

		// Outbound rate limit (FR-020/T041): per-business AND per-recipient.
		if s.OutboundLimiter != nil {
			if !s.OutboundLimiter.Allow("ob:biz:"+businessID.String()) ||
				!s.OutboundLimiter.Allow("ob:rcpt:"+tk.TenantRootID.String()+":"+recipient) {
				return fmt.Errorf("ticketing: outbound rate limit: %w", errs.ErrRateLimited)
			}
		}

		// Threading headers from the latest message on the ticket.
		parent, perr := q.GetThreadingParent(ctx, dbgen.GetThreadingParentParams{
			TicketID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
		})
		if perr != nil && !errors.Is(perr, pgx.ErrNoRows) {
			return perr
		}
		var inReplyTo *string
		refs := []string{}
		if perr == nil {
			pid := parent.MessageID
			inReplyTo = &pid
			refs = append(append([]string{}, parent.References...), parent.MessageID)
		}

		msgID, gerr := uuid.NewV7()
		if gerr != nil {
			return gerr
		}
		rfcMessageID := msgID.String() + "@" + s.SystemDomain

		row, ierr := q.InsertOutboundMessage(ctx, dbgen.InsertOutboundMessageParams{
			ID: msgID, TicketID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
			AuthorPrincipalID: pgUUID(principalID), MessageID: rfcMessageID,
			InReplyTo: inReplyTo, References: refs,
			BodyText: &in.BodyText, BodyHtml: in.BodyHTML,
		})
		if ierr != nil {
			return ierr
		}
		if berr := q.BumpTicketActivity(ctx, dbgen.BumpTicketActivityParams{ID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID}); berr != nil {
			return berr
		}

		// Audit-in-tx (FR-014).
		if aerr := writeAudit(ctx, tx, auditRow{
			businessID: businessID, tenantRootID: tk.TenantRootID, actor: &principalID,
			action: "ticket.replied", targetType: "ticket_message", targetID: msgID,
			newValue: map[string]any{"ticket_id": ticketID, "direction": "outbound"},
		}); aerr != nil {
			return aerr
		}

		// Outbox event — the worker builds the threaded Mail and dispatches it.
		replyToken := SignReplyToken(ticketID, s.ReplyTokenKey)
		if eerr := events.Enqueue(ctx, tx, tk.TenantRootID, events.TopicTicketReplied, map[string]any{
			"message_row_id": msgID,
			"ticket_id":      ticketID,
			"business_id":    businessID,
			"recipient":      recipient,
			"subject":        replySubject(tk.Subject),
			"rfc_message_id": rfcMessageID,
			"in_reply_to":    inReplyTo,
			"references":     refs,
			"reply_token":    replyToken,
		}); eerr != nil {
			return eerr
		}

		out = assembleOutboundMessage(row) // map dbgen row -> Message (Direction, DeliveryState, etc.)
		return nil
	})
	if err != nil {
		return Message{}, mapErr(err)
	}
	return out, nil
}

// replySubject ensures an "Re: " prefix and stamps the human-readable thread tag.
func replySubject(s string) string {
	if !strings.HasPrefix(strings.ToLower(s), "re:") {
		s = "Re: " + s
	}
	return s
}
```
> Implementation notes for the engineer:
> - `tk.RequesterEmail` / `tk.TenantRootID`: confirm the exact fields the existing `GetTicket` dbgen row projects; if `GetTicket` does not return the requester email, add it to that query (and regenerate) or fetch via `GetRequester`. Pick the smallest change.
> - `writeAudit` / `auditRow`: reuse the existing audit-insert helper used elsewhere in ticketing/inbox if one exists; otherwise add a small package helper that inserts into `audit_entry` (same column shape as the DEFINER's audit insert in `migrations/0014`). Grep for `audit_entry` to find the established pattern before writing a new one.
> - `pgUUID(uuid.UUID) pgtype.UUID` and `assembleOutboundMessage`: small local mappers; mirror existing `assembleTicket`/projection helpers.

- [ ] **Step 6: Run to verify pass**

Run: `go test -tags integration -run TestReply ./internal/ticketing/ -v`
Expected: all four PASS.

- [ ] **Step 7: Commit**
```bash
git add internal/ticketing/ internal/platform/errs/ internal/platform/httpx/ db/query/ internal/platform/db/dbgen/
git commit -m "feat(002): T037/T041 reply service — outbound message + threading + audit + outbox + per-biz/recipient rate limit"
```

---

## Task 4 — ticketing.Service.AddNote

Maps to: **T038**. Internal note: insert `note`-direction message, audit, **never** enqueue outbound (FR-009).

**Files:**
- Modify: `internal/ticketing/service.go`
- Create: `internal/ticketing/note_integration_test.go`

- [ ] **Step 1: Failing test** (`//go:build integration`):
```go
func TestAddNoteRecordsButNeverMails(t *testing.T) {
	// seed as in reply fixture; svc.AddNote(...)
	msg, err := svc.AddNote(ctx, fx.principalID, fx.businessID, fx.ticketID, ticketing.NoteInput{BodyText: "internal: VIP customer"})
	// assert msg.Direction == "note", author set
	// assert ZERO outbox rows for this tenant with topic 'ticket.replied'
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", fx.tenantRootID); n != 0 {
		t.Errorf("note enqueued outbound mail (%d), want 0 — notes must never be delivered", n)
	}
	// assert audit_entry action 'ticket.noted' present
}
func TestAddNoteUnknownTicketIsNotFound(t *testing.T) { /* 404 no-oracle */ }
```

- [ ] **Step 2: Run → FAIL** (`AddNote` undefined). Run: `go test -tags integration -run TestAddNote ./internal/ticketing/ -v`

- [ ] **Step 3: Implement `AddNote`** — same shape as `Reply` minus threading/outbox/suppression/rate-limit:
```go
type NoteInput struct{ BodyText string; BodyHTML *string }

func (s *Service) AddNote(ctx context.Context, principalID, businessID, ticketID uuid.UUID, in NoteInput) (Message, error) {
	if len(in.BodyText) == 0 {
		return Message{}, fmt.Errorf("ticketing: empty note: %w", errs.ErrValidation)
	}
	var out Message
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tk, terr := q.GetTicket(ctx, dbgen.GetTicketParams{ID: ticketID, BusinessID: businessID})
		if terr != nil {
			return terr
		}
		msgID, gerr := uuid.NewV7()
		if gerr != nil {
			return gerr
		}
		row, ierr := q.InsertNoteMessage(ctx, dbgen.InsertNoteMessageParams{
			ID: msgID, TicketID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
			AuthorPrincipalID: pgUUID(principalID),
			MessageID: msgID.String() + "@note." + s.SystemDomain, // internal id; never sent
			BodyText: &in.BodyText, BodyHtml: in.BodyHTML,
		})
		if ierr != nil {
			return ierr
		}
		if aerr := writeAudit(ctx, tx, auditRow{
			businessID: businessID, tenantRootID: tk.TenantRootID, actor: &principalID,
			action: "ticket.noted", targetType: "ticket_message", targetID: msgID,
			newValue: map[string]any{"ticket_id": ticketID, "direction": "note"},
		}); aerr != nil {
			return aerr
		}
		out = assembleOutboundMessage(row)
		return nil
	})
	if err != nil {
		return Message{}, mapErr(err)
	}
	return out, nil
}
```
> Notes get a synthetic, never-sent `message_id` (the column is `NOT NULL`); the `note.`-tagged domain makes it obviously internal and impossible to collide with a real id.

- [ ] **Step 4: Run → PASS.** Run: `go test -tags integration -run TestAddNote ./internal/ticketing/ -v`

- [ ] **Step 5: Commit**
```bash
git add internal/ticketing/
git commit -m "feat(002): T038 internal note service — note-direction message, audited, never mailed (FR-009)"
```

---

## Task 5 — HTTP handlers, routes, permission gate, contract (T035)

Maps to: **T035**, handler side of T037/T038, OpenAPI alignment.

**Files:**
- Modify: `internal/ticketing/handler.go`, `cmd/manyforge/main.go`, `cmd/manyforge/drift_002_test.go`, `specs/002-support-desk/contracts/openapi.yaml`

- [ ] **Step 1: Add request DTOs + `delivery_state` to `messageResp`**

In `internal/ticketing/handler.go`:
```go
type replyBody struct {
	BodyText string  `json:"body_text"`
	BodyHTML *string `json:"body_html"`
}
type noteBody struct {
	BodyText string `json:"body_text"`
}
```
Add `DeliveryState *string \`json:"delivery_state"\`` to `messageResp` and set it in `toMessageResp` from `m.DeliveryState`.

- [ ] **Step 2: Add handlers + `WriteRoutes`**
```go
// WriteRoutes mounts the authenticated write endpoints; the caller wraps these with
// RequirePermission("tickets.reply", …) so reply AND note are gated identically
// (the permission catalog scopes notes under tickets.reply).
func (h *Handler) WriteRoutes(r chi.Router) {
	r.Post("/businesses/{id}/tickets/{tid}/reply", h.reply)
	r.Post("/businesses/{id}/tickets/{tid}/note", h.addNote)
}

func (h *Handler) reply(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok { httpx.WriteError(w, r, errs.ErrNotFound); return }
	bid, err := pathUUID(r, "id"); if err != nil { httpx.WriteError(w, r, errs.ErrNotFound); return }
	tid, err := pathUUID(r, "tid"); if err != nil { httpx.WriteError(w, r, errs.ErrNotFound); return }
	var body replyBody
	if !httpx.DecodeJSON(w, r, &body) { return }
	if strings.TrimSpace(body.BodyText) == "" { httpx.WriteError(w, r, errValidation("body_text required")); return }
	m, err := h.svc.Reply(r.Context(), pid, bid, tid, ticketing_ReplyInput(body))
	if err != nil { httpx.WriteError(w, r, err); return }
	httpx.WriteJSON(w, http.StatusCreated, toMessageResp(m))
}

func (h *Handler) addNote(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok { httpx.WriteError(w, r, errs.ErrNotFound); return }
	bid, err := pathUUID(r, "id"); if err != nil { httpx.WriteError(w, r, errs.ErrNotFound); return }
	tid, err := pathUUID(r, "tid"); if err != nil { httpx.WriteError(w, r, errs.ErrNotFound); return }
	var body noteBody
	if !httpx.DecodeJSON(w, r, &body) { return }
	if strings.TrimSpace(body.BodyText) == "" { httpx.WriteError(w, r, errValidation("body_text required")); return }
	m, err := h.svc.AddNote(r.Context(), pid, bid, tid, ticketing.NoteInput{BodyText: body.BodyText})
	if err != nil { httpx.WriteError(w, r, err); return }
	httpx.WriteJSON(w, http.StatusCreated, toMessageResp(m))
}
```
> `ticketing_ReplyInput(body)` is shorthand — construct `ticketing.ReplyInput{BodyText: body.BodyText, BodyHTML: body.BodyHTML}` (these types are in-package, so no conversion fn is needed; written inline). Add `"strings"` to the handler imports.

- [ ] **Step 3: Wire the `tickets.reply` gate + mount in `main.go`**

In `cmd/manyforge/main.go`: add a field `ticketsReply func(http.Handler) http.Handler` to `apiHandlers`; build it next to `ticketsRead`:
```go
ticketsReply: httpx.RequirePermission(database, permResolve, "tickets.reply", businessIDFromPath),
```
In `mountAPIRoutes`, inside the authenticated group, add a sibling permission group:
```go
pr.Group(func(tw chi.Router) {
	tw.Use(h.ticketsReply)
	h.ticketing.WriteRoutes(tw)
})
```
Initialize the outbound limiter and pass deps when constructing the ticketing service:
```go
outboundLimiter := ratelimit.NewTokenBucket(cfg.OutboundRatePerSec, cfg.OutboundBurst) // e.g. 1, 30
ticketingSvc := &ticketing.Service{
	DB: database, ReplyTokenKey: cfg.InboundReplyTokenSecret, SystemDomain: cfg.InboundSystemDomain,
	OutboundLimiter: outboundLimiter, Suppression: notify.DBSuppression{DB: database},
}
```

- [ ] **Step 4: Update OpenAPI (note perm + delivery_state)**

In `specs/002-support-desk/contracts/openapi.yaml`:
- In the `/note` `post.summary`, change `Requires tickets.write.` → `Requires tickets.reply.`
- In the `TicketMessage` schema, add: `delivery_state: { type: [string, "null"], enum: [pending, sent, failed], description: outbound delivery lifecycle; null for inbound/note }`

- [ ] **Step 5: Add operationIds to the drift test**

In `cmd/manyforge/drift_002_test.go`, add to `inScope002Ops`:
```go
	"POST /businesses/{}/tickets/{}/reply",
	"POST /businesses/{}/tickets/{}/note",
```

- [ ] **Step 6: Run the contract test (T035) + build**

Run: `go build ./... && make contract-test`
Expected: PASS — both new ops registered AND documented; `TicketMessage` schema drift clean.

- [ ] **Step 7: Handler-level tests** — add `internal/ticketing/handler_write_test.go` (httptest, no DB; inject a fake service via a small interface or a service with a stub DB) asserting: 201 returns the message JSON incl. `delivery_state`; empty `body_text` → 400; service `ErrNotFound` → 404; `ErrConflict` → 409; `ErrRateLimited` → 429. If the handler holds a concrete `*Service`, refactor `Handler` to depend on a minimal interface (`replier`/`noter`) to make this unit-testable — otherwise cover these via the integration test in Task 3/4 and skip the httptest.

- [ ] **Step 8: Commit**
```bash
git add internal/ticketing/ cmd/manyforge/ specs/002-support-desk/contracts/openapi.yaml
git commit -m "feat(002): T035 reply/note HTTP routes — tickets.reply gate, 201/400/404/409/429, OpenAPI + drift updated"
```

---

## Task 6 — Outbound send worker subscriber (T039)

Maps to: **T039**, rest of **T036**. Drains `ticket.replied` → builds the threaded `Mail` → `Sender.Send` → records `sent`/`failed`. Idempotent (skip if already `sent`).

**Files:**
- Modify: `internal/platform/events/bus.go` (add `TopicTicketReplied`)
- Create: `internal/platform/notify/sender_subscriber.go`, `internal/platform/notify/send_integration_test.go`
- Modify: `cmd/manyforge/main.go` (register subscriber + sender selection)

- [ ] **Step 1: Add the topic** — in `internal/platform/events/bus.go`:
```go
	// TopicTicketReplied is emitted in the reply tx; the notify worker drains it to
	// dispatch the threaded outbound email. Payload carries the message row id,
	// recipient, subject, and threading headers.
	TopicTicketReplied = "ticket.replied"
```

- [ ] **Step 2: Failing integration test** — `internal/platform/notify/send_integration_test.go` (`//go:build integration`): enqueue a `ticket.replied` outbox row (or call `ticketing.Reply`), run one drain with a **capturing** `Sender`, assert the captured `Mail` has the recipient/Reply-To(`support+<token>@…`)/In-Reply-To, and that the message row flips to `delivery_state='sent'`. Add a second test where the capturing Sender returns `ErrSuppressed` ⇒ message row `delivery_state='failed'`, no retry.
```go
type captureSender struct{ last *Mail; err error }
func (c *captureSender) Send(_ context.Context, m Mail) error { c.last = &m; return c.err }
```

- [ ] **Step 3: Run → FAIL** (subscriber undefined). Run: `go test -tags integration -run TestSend ./internal/platform/notify/ -v`

- [ ] **Step 4: Implement the subscriber** — `internal/platform/notify/sender_subscriber.go`:
```go
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/events"
)

type repliedPayload struct {
	MessageRowID uuid.UUID `json:"message_row_id"`
	Recipient    string    `json:"recipient"`
	Subject      string    `json:"subject"`
	RFCMessageID string    `json:"rfc_message_id"`
	InReplyTo    *string   `json:"in_reply_to"`
	References   []string  `json:"references"`
	ReplyToken   string    `json:"reply_token"`
	BusinessID   uuid.UUID `json:"business_id"`
}

// SendSubscriber dispatches queued replies. From/Reply-To are built on the system
// inbound address of the business (US2: system identity only). Runs in the worker's
// SAVEPOINT tx; idempotent (a message already 'sent' is skipped).
type SendSubscriber struct {
	Sender       Sender
	SystemDomain string
	Logger       *slog.Logger
}

func (s SendSubscriber) Handle(ctx context.Context, tx pgx.Tx, e events.Event) error {
	var p repliedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return fmt.Errorf("notify: send: unmarshal: %w", err)
	}
	q := dbgen.New(tx)

	// Idempotency: skip if already delivered.
	state, derr := q.GetMessageDeliveryState(ctx, dbgen.GetMessageDeliveryStateParams{ID: p.MessageRowID, TenantRootID: e.TenantRootID})
	if derr != nil {
		return derr
	}
	if state.Valid && state.MessageDeliveryState == dbgen.MessageDeliveryStateSent {
		return nil
	}

	// System inbound address ⇒ From + VERP Reply-To.
	sysAddr, aerr := q.GetBusinessSystemInboundAddress(ctx, dbgen.GetBusinessSystemInboundAddressParams{BusinessID: p.BusinessID, TenantRootID: e.TenantRootID})
	if aerr != nil {
		return aerr
	}
	replyTo := verpAddress(sysAddr, p.ReplyToken)

	mail := Mail{
		From: sysAddr, To: p.Recipient, Subject: p.Subject,
		MessageID: p.RFCMessageID, References: p.References, ReplyTo: replyTo,
		EnvelopeFrom: replyTo, // VERP return-path so DSNs/bounces are correlatable
	}
	if p.InReplyTo != nil {
		mail.InReplyTo = *p.InReplyTo
	}

	if serr := s.Sender.Send(ctx, mail); serr != nil {
		if errors.Is(serr, ErrSuppressed) {
			return q.MarkMessageFailed(ctx, dbgen.MarkMessageFailedParams{ID: p.MessageRowID, TenantRootID: e.TenantRootID, DeliveryError: ptr("recipient suppressed")})
		}
		return serr // transient ⇒ worker retries with backoff
	}
	return q.MarkMessageDelivered(ctx, dbgen.MarkMessageDeliveredParams{ID: p.MessageRowID, TenantRootID: e.TenantRootID})
}

// verpAddress inserts +token before '@' of the system address, preserving the
// token's case (the inbound normalizer now preserves plus-token case — manyforge-btv).
func verpAddress(sysAddr, token string) string {
	at := -1
	for i := len(sysAddr) - 1; i >= 0; i-- {
		if sysAddr[i] == '@' { at = i; break }
	}
	if at < 0 {
		return sysAddr
	}
	return sysAddr[:at] + "+" + token + sysAddr[at:]
}

func ptr(s string) *string { return &s }
```
> Add `GetMessageDeliveryState :one` to `db/query/ticketing.sql` (`SELECT delivery_state FROM ticket_message WHERE id=$1 AND tenant_root_id=$2`) and regenerate.

- [ ] **Step 5: Run → PASS.** Run: `go test -tags integration -run TestSend ./internal/platform/notify/ -v`

- [ ] **Step 6: Register in `main.go` + sender selection**
```go
var sender notify.Sender
if cfg.SMTPHost != "" {
	sender = notify.NewSMTPSender(notify.SMTPConfig{
		Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUser, Password: cfg.SMTPPass,
		DKIM: dkimConfigFromCfg(cfg), // returns nil when no DKIM key configured
	}, notify.DBSuppression{DB: database})
} else {
	sender = notify.LogSender{Logger: logger, Suppression: notify.DBSuppression{DB: database}}
}
sendSub := notify.SendSubscriber{Sender: sender, SystemDomain: cfg.InboundSystemDomain, Logger: logger}
eventBus.Subscribe(events.TopicTicketReplied, sendSub.Handle)
```
> `dkimConfigFromCfg` returns `*notify.DKIMConfig` only when a system-domain DKIM private key + selector are configured; otherwise `nil` (Decision 1: works without DKIM).

- [ ] **Step 7: Build + commit**
```bash
go build ./...
git add internal/platform/notify/ internal/platform/events/ cmd/manyforge/ db/query/ internal/platform/db/dbgen/
git commit -m "feat(002): T039 outbound send subscriber — drain ticket.replied -> threaded Mail via Sender, record delivery"
```

---

## Task 7 — Bounce webhook (T040)

Maps to: **T040**. HMAC-authed `POST /inbound/bounce`, symmetric with the US1 inbound webhook: parse hard bounce → `email_suppression` + mark the outbound message `failed`. Uniform no-oracle ack.

**Files:**
- Create: `internal/inbox/bounce.go`, `internal/inbox/bounce_test.go`, `internal/inbox/bounce_integration_test.go`
- Modify: `cmd/manyforge/main.go`, `internal/platform/config/*`, `cmd/manyforge/drift_002_test.go`, `openapi.yaml`

- [ ] **Step 1: Failing signature/parse unit test** — `internal/inbox/bounce_test.go` (no tag): a request with a valid HMAC over the body is accepted (200/202); a bad signature is rejected uniformly; an oversized body is capped. Mirror the existing webhook signature test in `internal/inbox/handler_test.go`/`webhook.go` (reuse the same HMAC verify helper — grep `hmac.New(sha256.New` in `webhook.go`).

- [ ] **Step 2: Failing integration test** — `internal/inbox/bounce_integration_test.go` (`//go:build integration`): seed a business + ticket + an **outbound** message to `ada@example.com`; POST a hard-bounce body for `ada@example.com`; assert `email_suppression` has the row AND the outbound message `delivery_state='failed'`. Then call `ticketing.Reply` (or the suppression checker) and assert a subsequent send to `ada@example.com` is refused (`ErrConflict` / `ErrSuppressed`).

- [ ] **Step 3: Run → FAIL.** Run: `go test ./internal/inbox/ -run Bounce -v` and `go test -tags integration -run Bounce ./internal/inbox/ -v`

- [ ] **Step 4: Implement `bounce.go`**
```go
package inbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

const bounceMaxBody = 1 << 20 // 1 MiB, capped in-helper (defense in depth)

// BounceHandler ingests hard-bounce notifications from the mail provider. Auth is
// the provider HMAC over the raw body (own purpose-separated secret). A hard bounce
// suppresses the recipient and marks the most recent matching outbound message
// failed. The response is a uniform 202 regardless of match (no existence oracle).
type BounceHandler struct {
	db     *db.DB
	verify func(sig string, body []byte) bool // HMAC verifier built from InboundBounceSecret
}

func NewBounceHandler(database *db.DB, verify func(string, []byte) bool) *BounceHandler {
	return &BounceHandler{db: database, verify: verify}
}

func (h *BounceHandler) PublicRoutes(r chi.Router) {
	r.Post("/inbound/bounce", h.handle)
}

type bouncePayload struct {
	Recipient string `json:"recipient"`
	Type      string `json:"type"` // "hard" | "soft" | ...
	TenantRootID string `json:"tenant_root_id"` // provider echoes our VERP-derived tenant, or omitted
}

func (h *BounceHandler) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bounceMaxBody))
	if err != nil {
		w.WriteHeader(http.StatusAccepted) // uniform ack
		return
	}
	if !h.verify(r.Header.Get("X-MF-Signature"), body) {
		w.WriteHeader(http.StatusAccepted) // do not reveal auth outcome
		return
	}
	var p bouncePayload
	if jerr := json.Unmarshal(body, &p); jerr != nil || p.Type != "hard" || p.Recipient == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// Principal-less suppression + mark-failed in one tx.
	_ = h.db.WithTx(r.Context(), func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if serr := q.InsertSuppression(r.Context(), dbgen.InsertSuppressionParams{Email: p.Recipient, Reason: "hard_bounce"}); serr != nil {
			return serr
		}
		// Best-effort failure surfacing: correlate to the latest outbound message.
		// tenant scope comes from the VERP token if present; otherwise scan is skipped.
		// (Engineer: derive tenant_root_id from the bounced VERP local-part token via
		// ticketing.VerifyReplyToken -> ticket -> tenant; see note below.)
		return nil
	})
	w.WriteHeader(http.StatusAccepted)
}
```
> **Correlation note:** the cleanest tenant/message correlation is to recover the ticket from the bounced message's VERP `Reply-To`/return-path token (the provider should echo the original recipient/return-path). If the provider gives the VERP address, run `ticketing.VerifyReplyToken(token, key)` → ticket id → tenant → `GetOutboundMessageForBounce` → `MarkMessageFailed`. If only the bare recipient is available, suppression still applies globally (email PK) and the per-message failure surfacing is best-effort. Implement the VERP-token path; document the fallback.

- [ ] **Step 5: Config + secret** — add `InboundBounceSecret` to `internal/platform/config` (mirror `InboundWebhookSecret`); build the verifier in `main.go` exactly like the inbound webhook's. Add SMTP + DKIM + outbound-rate config fields here too if not already added in Tasks 2/3/5.

- [ ] **Step 6: Mount route** — in `mountAPIRoutes`, add to the **ingest-limited public group** (next to `h.inboxWebhook.PublicRoutes`):
```go
	ingress.Group(func(b chi.Router) { h.bounce.PublicRoutes(b) })
```
Add `bounce *inbox.BounceHandler` to `apiHandlers` and construct it in `main`.

- [ ] **Step 7: OpenAPI + drift** — add the `/inbound/bounce` path to `openapi.yaml` (POST, request body `BounceBody`, `202` ack, security: provider HMAC). Add `"POST /inbound/bounce"` to `inScope002Ops`.

- [ ] **Step 8: Run → PASS + contract.** Run: `go test ./internal/inbox/ -run Bounce -v && go test -tags integration -run Bounce ./internal/inbox/ -v && make contract-test`

- [ ] **Step 9: Commit**
```bash
git add internal/inbox/bounce*.go cmd/manyforge/ internal/platform/config/ specs/002-support-desk/contracts/openapi.yaml db/query/ internal/platform/db/dbgen/
git commit -m "feat(002): T040 bounce webhook — HMAC-authed hard-bounce -> email_suppression + mark outbound failed; reply refuses suppressed (409)"
```

---

## Task 8 — Frontend: reply composer + note toggle (T042)

Maps to: **T042**. Mirror the US1 thread view + `ticket.service.ts` patterns (commit `376af3a`). **Must verify in a real browser** (CLAUDE.md): unit tests are necessary but not sufficient.

**Files:**
- Modify: `web/src/app/pages/support/` thread view component(s) + `ticket.service.ts`
- Add/modify: component + service unit specs (`*.spec.ts`)

- [ ] **Step 1: Service methods (failing unit spec first)** — in the ticket service spec, assert `reply(businessId, ticketId, {body_text})` POSTs to `/api/v1/businesses/{bid}/tickets/{tid}/reply` and `addNote(...)` POSTs to `…/note`, returning the created `TicketMessage`. Then implement in `ticket.service.ts`:
```ts
reply(businessId: string, ticketId: string, body: { body_text: string; body_html?: string }) {
  return this.http.post<TicketMessage>(`/api/v1/businesses/${businessId}/tickets/${ticketId}/reply`, body);
}
addNote(businessId: string, ticketId: string, body: { body_text: string }) {
  return this.http.post<TicketMessage>(`/api/v1/businesses/${businessId}/tickets/${ticketId}/note`, body);
}
```
Add `delivery_state?: 'pending' | 'sent' | 'failed' | null` to the `TicketMessage` model.

- [ ] **Step 2: Composer component (failing unit spec first)** — in the thread view, add a composer with a **reply/note toggle**, a textarea, and a submit button. Spec asserts: toggling to "Note" calls `addNote` on submit; default "Reply" calls `reply`; on success the new message is appended; a `failed` `delivery_state` renders a visible failure badge; a `note`-direction message renders visually distinct from outbound. Implement the component to satisfy the spec.

- [ ] **Step 3: Run unit specs.** Run: `cd web && npm test`
Expected: PASS.

- [ ] **Step 4: Real-browser verification.** Build + drive the UI (CLAUDE.md mandate): `cd web && npm run build`, then verify the composer renders, the toggle switches, submit posts, and the new message appears — via `Skill: gstack` (`$B`) or the Playwright MCP. This is the regression target codified in Task 9.

- [ ] **Step 5: Commit**
```bash
git add web/src/app/pages/support/ web/src/app/**/ticket.service.ts web/src/app/**/*.spec.ts
git commit -m "feat(002): T042 support frontend — reply composer + note toggle wired to reply/note endpoints"
```

---

## Task 9 — Playwright US2 e2e (T043)

Maps to: **T043**. Mirror the US1 Playwright spec (commit `f3d3914`): `page.route` mocks + `addInitScript` auth seed (no live backend); baseURL `:4300`, **dev server must already be running** (no `webServer` block).

**Files:** Modify `web/e2e/support.spec.ts`

- [ ] **Step 1: Write the US2 spec** — open a ticket → type a reply → submit (mock `POST …/reply` → 201 outbound `TicketMessage`) → assert the outbound message renders in the thread, styled as outbound. Then toggle to Note → submit (mock `POST …/note` → 201 note `TicketMessage`) → assert it renders **distinct** from a reply (note styling/badge). Mock the thread GET to include the new messages on reload if the UI refetches.

- [ ] **Step 2: Run e2e in a real browser.** Run (with the dev server up): `cd web && npm run e2e`
Expected: PASS (green in chromium).

- [ ] **Step 3: Commit**
```bash
git add web/e2e/support.spec.ts
git commit -m "test(002): T043 Playwright US2 — reply renders outbound in thread; note renders distinct"
```

---

## Task 10 — Full gate, reversibility, tracking, wrap-up

- [ ] **Step 1: Full gate (must be green — no pre-existing-failure exceptions).**
```bash
make test && make lint && make contract-test && make int-test
cd web && npm test && npm run build
```
Expected: all PASS; `make lint` 0 issues; `make int-test` serialized (`-p 1`) green across all packages.

- [ ] **Step 2: Migration reversibility (0018) — up/down/up** on a throwaway PG (reuse Task 1 Step 7 recipe). Expected clean.

- [ ] **Step 3: Update `tasks.md`** — mark T035–T043 `[X]` in `specs/002-support-desk/tasks.md`.

- [ ] **Step 4: bd** — close the US2 work; record any follow-ups (e.g. custom-domain DKIM = US4; subject `[#ref]` *matching* deferred since the DEFINER doesn't implement it; X-MF-Timestamp skew window still open). `bd close <ids>` / `bd update <epic> --notes "US2 complete: …"`.

- [ ] **Step 5: Final commit** (gate green):
```bash
git add specs/002-support-desk/tasks.md .beads/issues.jsonl
git commit -m "chore(002): US2 complete — tasks T035–T043 checked, follow-ups filed"
```
> Repo is local-only: **do not** `git push` / `bd dolt push` (they will fail).

---

## Self-Review

**Spec coverage (T035–T043 + FRs):**
- FR-008 reply + threading → Tasks 3 (service), 5 (HTTP), 6 (send w/ In-Reply-To/References + Reply-To token). The btv fix (committed `cab5a8a`) guarantees the inbound side threads the requester's response back. ✔
- FR-009 internal note never delivered → Task 4 (no outbox) + Task 6 (only `ticket.replied` triggers send) + test asserts 0 outbox rows for a note. ✔
- FR-013 hard-bounce suppression + system-address fallback + (optional) DKIM → Tasks 2 (optional DKIM), 6 (system identity), 7 (suppression). Custom-domain DKIM explicitly US4. ✔
- FR-014 audit-in-tx for reply + note → Tasks 3, 4 (`writeAudit` in the same tx). ✔
- FR-020 outbound rate limit → Task 3 (per-biz/per-recipient at service boundary). ✔
- T035 contract / T036 integration / T037 reply / T038 note / T039 send / T040 bounce / T041 rate-limit / T042 frontend / T043 Playwright → Tasks 5, 3+4+6+7, 3, 4, 6, 7, 3, 8, 9. ✔
- SC-005 audit coverage → asserted in Task 3/4 tests. SC-011 loop guard: US2 sends only on explicit human reply (no auto-reply generation), inbound `is_auto_reply` already recorded in US1; the outbound rate limit bounds runaway. The full loop *test* is a Polish-phase item — **flagged**, not silently dropped.

**Placeholder scan:** No "TBD"/"add error handling"/"similar to Task N". Two explicit engineer-judgment notes remain (the `GetTicket` requester-email projection in Task 3 Step 5; the bounce VERP-token correlation in Task 7 Step 4) — both name the exact decision, the options, and the smallest-change guidance, with concrete code around them. The frontend tasks (8/9) reference the US1 components by commit and give exact service-method code + asserted behaviors rather than full component source (the Angular component scaffolding is best mirrored from the existing files, which the plan names).

**Type consistency:** `ReplyInput{BodyText, BodyHTML}`, `NoteInput{BodyText}`, `Mail` (+`EnvelopeFrom`/`AutoSubmitted`/`ExtraHeaders`), `Sender.Send`, `events.TopicTicketReplied`, `delivery_state` enum (`pending|sent|failed`), `errs.ErrRateLimited`→429, `messageResp.DeliveryState`, dbgen methods (`InsertOutboundMessage`/`InsertNoteMessage`/`GetThreadingParent`/`BumpTicketActivity`/`MarkMessageDelivered`/`MarkMessageFailed`/`GetBusinessSystemInboundAddress`/`GetOutboundMessageForBounce`/`GetMessageDeliveryState`/`IsSuppressed`/`InsertSuppression`) are used consistently across the tasks that define and consume them.

**Open follow-ups to file in bd (Task 10 Step 4):** custom-domain DKIM (US4); subject `[#ref]` *matching* in the DEFINER (only stamping is in US2); X-MF-Timestamp skew window (shared inbound hardening); `ratelimit.TokenBucket` unbounded key map (shared w/ auth + new outbound limiter).
```
