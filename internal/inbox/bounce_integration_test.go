//go:build integration

package inbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/notify"
	"github.com/manyforge/manyforge/internal/ticketing"
)

const bounceSystemDomain = "inbound.localhost"

// bounceTenant is a minimally-seeded tenant for the hard-bounce path: a master
// business with a system inbound address, a member principal (RLS read access), a
// requester, a ticket, and a single OUTBOUND ticket_message with a known message_id.
type bounceTenant struct {
	master       uuid.UUID
	tenantRootID uuid.UUID
	member       uuid.UUID // human principal w/ owner preset role (RLS access + satisfies the retain-≥1-Owner trigger)
	ticketID     uuid.UUID
	messageRowID uuid.UUID
	recipient    string
	rfcMessageID string
}

// seedBounceTenant seeds the full fixture via the RLS-exempt Super pool. The
// principal carries the `owner` preset role so ticketing.Service can load the
// ticket/requester under its RLS principal (the suppression check then fires) AND
// the tenant satisfies its retain-≥1-Owner invariant trigger.
func seedBounceTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) bounceTenant {
	t.Helper()
	bt := bounceTenant{
		master:       uuid.New(),
		member:       uuid.New(),
		ticketID:     uuid.New(),
		messageRowID: uuid.New(),
		recipient:    "bounced@example.com",
	}
	bt.tenantRootID = bt.master
	bt.rfcMessageID = bt.messageRowID.String() + "@" + bounceSystemDomain
	sysAddr := "b-" + bt.master.String()[:8] + "@" + bounceSystemDomain
	requesterID := uuid.New()
	memberAccount := uuid.New()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("owner preset role: %v", err)
	}

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Agent','active',now(),now(),now())`, []any{memberAccount, "agent-" + bt.member.String() + "@x.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{bt.member, memberAccount}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'BounceCo','active',now(),now())`, []any{bt.master}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{bt.master}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`, []any{bt.member, bt.master, ownerRole}},
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at) VALUES ($1,$2,$2,$3,'system',NULL,now(),now())`, []any{uuid.New(), bt.master, sysAddr}},
		{`INSERT INTO requester (id,business_id,tenant_root_id,email,display_name,first_seen_at,last_seen_at,created_at,updated_at) VALUES ($1,$2,$2,$3,'Bouncer',now(),now(),now(),now())`, []any{requesterID, bt.master, bt.recipient}},
		{`INSERT INTO ticket (id,business_id,tenant_root_id,requester_id,subject,status,priority,reply_token,last_message_at,created_at,updated_at) VALUES ($1,$2,$2,$3,'Need help','open','normal',$4,now(),now(),now())`, []any{bt.ticketID, bt.master, requesterID, "bt-" + bt.ticketID.String()[:8]}},
		// The outbound message we minted, correlated by its globally-unique message_id.
		{`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,author_principal_id,message_id,body_text,delivery_state,created_at) VALUES ($1,$2,$3,$3,'outbound',$4,$5,'we are on it','pending'::message_delivery_state,now())`,
			[]any{bt.messageRowID, bt.ticketID, bt.master, bt.member, bt.rfcMessageID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return bt
}

// TestBounceHardBounceSuppressesAndMarksFailed (T040 integration) — a valid-HMAC
// hard bounce carrying the outbound message's message_id (1) suppresses the
// recipient globally, (2) marks the outbound message delivery_state='failed' via
// the DEFINER, and (3) makes a subsequent ticketing.Service.Reply to that recipient
// fail with ErrConflict (suppressed).
func TestBounceHardBounceSuppressesAndMarksFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	bt := seedBounceTenant(ctx, t, tdb)

	// Mount the bounce handler against the real DB-backed suppressor.
	h := NewBounceHandler(NewDBBounceSuppressor(tdb.App), testBounceSecret, 1<<20,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	h.PublicRoutes(r)

	body := bounceBody(t, bt.recipient, "hard", bt.rfcMessageID)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/inbound/bounce", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MF-Signature", signBounce(testBounceSecret, body))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("hard bounce: want 202, got %d (body %q)", rec.Code, rec.Body.String())
	}

	// (1) The recipient is now globally suppressed (via the Super pool — no RLS).
	var suppressed bool
	if err := tdb.Super.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM email_suppression WHERE email=$1)", bt.recipient).Scan(&suppressed); err != nil {
		t.Fatalf("read suppression: %v", err)
	}
	if !suppressed {
		t.Errorf("recipient %q is not in email_suppression after a hard bounce", bt.recipient)
	}

	// (2) The outbound message is marked failed (DEFINER correlated by message_id).
	var state, derr string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT delivery_state, coalesce(delivery_error,'') FROM ticket_message WHERE id=$1", bt.messageRowID).Scan(&state, &derr); err != nil {
		t.Fatalf("read delivery_state: %v", err)
	}
	if state != "failed" {
		t.Errorf("delivery_state = %q, want failed", state)
	}
	if derr != "hard_bounce" {
		t.Errorf("delivery_error = %q, want hard_bounce", derr)
	}

	// (3) A subsequent Reply to the (now-suppressed) recipient is refused (ErrConflict).
	svc := &ticketing.Service{
		DB:           tdb.App,
		SystemDomain: bounceSystemDomain,
		Suppression:  notify.DBSuppression{DB: tdb.App},
	}
	_, replyErr := svc.Reply(ctx, bt.member, bt.master, bt.ticketID, ticketing.ReplyInput{BodyText: "ping"})
	if !errors.Is(replyErr, errs.ErrConflict) {
		t.Errorf("Reply to suppressed recipient: want ErrConflict, got %v", replyErr)
	}
}
