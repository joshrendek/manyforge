//go:build integration

package ticketing

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/notify"
	"github.com/manyforge/manyforge/internal/tenancy"
)

type portalTestPermissions struct{}

func (portalTestPermissions) Has(key string) bool { return key == "tickets.write" }
func portalTestResolver(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) (httpx.Permissions, error) {
	return portalTestPermissions{}, nil
}

type portalTestMailer struct {
	mails    []notify.Mail
	profiles []uuid.UUID
	err      error
}

func (m *portalTestMailer) SendSupportReply(_ context.Context, p uuid.UUID, v notify.Mail) error {
	if m.err != nil {
		return m.err
	}
	m.mails = append(m.mails, v)
	m.profiles = append(m.profiles, p)
	return nil
}
func portalTestCode(t *testing.T, m *portalTestMailer) string {
	t.Helper()
	if len(m.mails) == 0 {
		t.Fatal("no provider-accepted mail")
	}
	code := regexp.MustCompile(`\b[0-9]{8}\b`).FindString(m.mails[len(m.mails)-1].BodyText)
	if len(code) != 8 {
		t.Fatal("verification mail missing code")
	}
	return code
}
func portalTestSQL(t *testing.T, ctx context.Context, tdb *testdb.TestDB, q string, args ...any) {
	t.Helper()
	if _, err := tdb.Super.Exec(ctx, q, args...); err != nil {
		t.Fatalf("fixture mutation failed: %v", err)
	}
}
func portalTestSender(t *testing.T, ctx context.Context, tdb *testdb.TestDB, rt readTenant) uuid.UUID {
	t.Helper()
	domain := "support-" + rt.master.String()[:8] + ".example.com"
	did, sid, pid := uuid.New(), uuid.New(), uuid.New()
	portalTestSQL(t, ctx, tdb, `INSERT INTO email_domain(id,business_id,tenant_root_id,domain,mode,verify_token,verified_at) VALUES($1,$2,$3,$4,'provider_route','fixture',now())`, did, rt.master, rt.tenantRootID, domain)
	portalTestSQL(t, ctx, tdb, `INSERT INTO inbound_address(business_id,tenant_root_id,address,kind,email_domain_id) VALUES($1,$2,$3,'custom',$4)`, rt.master, rt.tenantRootID, "support@"+domain, did)
	portalTestSQL(t, ctx, tdb, `INSERT INTO secret(id,business_id,tenant_root_id,scope,sealed_value) VALUES($1,$2,$3,'mailing','fixture')`, sid, rt.master, rt.tenantRootID)
	portalTestSQL(t, ctx, tdb, `INSERT INTO mailing_sending_profile(id,business_id,tenant_root_id,mode,from_email,from_name,secret_ref,status,feedback_status,feedback_confirmed_at) VALUES($1,$2,$3,'resend',$4,'Support',$5,'verified','ready',now())`, pid, rt.master, rt.tenantRootID, "support@"+domain, sid)
	return pid
}
func portalTestScope(t *testing.T, ctx context.Context, s *PortalService, rt readTenant, external string) (PortalScope, PortalKey) {
	t.Helper()
	key, err := s.CreateKey(ctx, rt.owner, rt.master, "integration", 1)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	p, err := s.Authenticate(ctx, key.Token, external, "Customer")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return p, key
}
func portalTestVerify(t *testing.T, ctx context.Context, s *PortalService, p PortalScope, m *portalTestMailer, email string) {
	t.Helper()
	if err := s.SendContactVerification(ctx, p, email); err != nil {
		t.Fatalf("send challenge: %v", err)
	}
	if err := s.VerifyContact(ctx, p, portalTestCode(t, m)); err != nil {
		t.Fatalf("verify challenge: %v", err)
	}
}

func TestPortalNativeThreadOwnershipIdempotency(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	m := &portalTestMailer{}
	profile := portalTestSender(t, ctx, tdb, rt)
	s := &PortalService{DB: tdb.App, Resolve: portalTestResolver, Mailer: m, ReplyTokenKey: replyKey, SystemDomain: "portal.test"}
	p, _ := portalTestScope(t, ctx, s, rt, "tph:"+uuid.New().String())
	other, err := s.Authenticate(ctx, p.token, "tph:"+uuid.New().String(), "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTicket(ctx, p, "Help", "Please help", uuid.New()); !errors.Is(err, ErrPortalConflict) {
		t.Fatalf("unverified create: %v", err)
	}
	// Guessing a native requester email must never import its pre-existing tickets.
	foreign := uuid.New()
	seedTicket(ctx, t, tdb, rt, foreign, "open", "normal", "Email ticket", nil, nil, -time.Hour)
	portalTestVerify(t, ctx, s, p, m, "ada@example.com")
	if len(m.profiles) != 1 || m.profiles[0] != profile {
		t.Fatal("challenge did not use exact business profile")
	}
	if tickets, err := s.ListTickets(ctx, p); err != nil || len(tickets) != 0 {
		t.Fatal("email proof imported pre-existing email tickets")
	}
	request := uuid.New()
	ticket, err := s.CreateTicket(ctx, p, "Help", "Please help", request)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := s.CreateTicket(ctx, p, "Help", "Please help", request)
	if err != nil || repeat.ID != ticket.ID {
		t.Fatal("exact repeat was not deduplicated")
	}
	if _, err := s.CreateTicket(ctx, p, "Changed", "Please help", request); !errors.Is(err, ErrPortalConflict) {
		t.Fatalf("changed request: %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM ticket_message WHERE ticket_id=$1`, ticket.ID); n != 1 {
		t.Fatalf("duplicate messages: %d", n)
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM outbox WHERE topic='ticket.created' AND payload->>'ticket_id'=$1`, ticket.ID.String()); n != 1 {
		t.Fatalf("created events: %d", n)
	}
	mid := uuid.New()
	message, err := s.AddMessage(ctx, p, ticket.ID, "More information", mid)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.AddMessage(ctx, p, ticket.ID, "More information", mid)
	if err != nil || again.ID != message.ID {
		t.Fatal("message repeat not deduplicated")
	}
	if _, err := s.AddMessage(ctx, p, ticket.ID, "Changed", mid); !errors.Is(err, ErrPortalConflict) {
		t.Fatalf("changed message: %v", err)
	}
	native := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "portal.test"}
	if _, err := native.Reply(ctx, rt.owner, rt.master, ticket.ID, ReplyInput{BodyText: "Operator reply"}); err != nil {
		t.Fatal(err)
	}
	if _, err := native.AddNote(ctx, rt.owner, rt.master, ticket.ID, NoteInput{BodyText: "Private internal note"}); err != nil {
		t.Fatal(err)
	}
	thread, err := s.Thread(ctx, p, ticket.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 3 {
		t.Fatalf("public message count: %d", len(thread.Messages))
	}
	foundReply := false
	for _, msg := range thread.Messages {
		if msg.Direction == "note" || msg.Body == "Private internal note" {
			t.Fatal("internal note leaked")
		}
		if msg.Body == "Operator reply" {
			foundReply = true
		}
	}
	if !foundReply {
		t.Fatal("native operator reply absent")
	}
	for _, id := range []uuid.UUID{ticket.ID, foreign, uuid.New()} {
		if _, err := s.Thread(ctx, other, id, ""); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("foreign thread: %v", err)
		}
		if _, err := s.AddMessage(ctx, other, id, "forged", uuid.New()); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("foreign append: %v", err)
		}
	}
	if _, err := s.Authenticate(ctx, p.token, "tph:forged", "Customer"); !errors.Is(err, ErrPortalUnauthorized) {
		t.Fatalf("forged header accepted: %v", err)
	}
	if _, err := s.Thread(ctx, p, ticket.ID, uuid.New().String()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("foreign cursor accepted: %v", err)
	}
	// Same opaque customer UUID under another business has no access.
	rt2 := seedReadTenant(ctx, t, tdb)
	p2, _ := portalTestScope(t, ctx, s, rt2, p.ExternalID)
	if _, err := s.Thread(ctx, p2, ticket.ID, ""); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross business thread: %v", err)
	}
	for i := range 22 {
		if _, err := s.AddMessage(ctx, p, ticket.ID, fmt.Sprintf("page %d", i), uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.Thread(ctx, p, ticket.ID, "")
	if err != nil || len(page.Messages) != 20 || page.NextCursor == "" {
		t.Fatal("first page not bounded")
	}
	second, err := s.Thread(ctx, p, ticket.ID, page.NextCursor)
	if err != nil || len(second.Messages) != 5 || second.NextCursor != "" {
		t.Fatal("second public page wrong")
	}
}

func TestPortalChallengeDenialsAndReplacement(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	m := &portalTestMailer{}
	profile := portalTestSender(t, ctx, tdb, rt)
	s := &PortalService{DB: tdb.App, Resolve: portalTestResolver, Mailer: m, ReplyTokenKey: replyKey, SystemDomain: "portal.test"}
	p, _ := portalTestScope(t, ctx, s, rt, "tph:"+uuid.New().String())
	if err := s.VerifyContact(ctx, p, "12345678"); !errors.Is(err, ErrPortalInvalid) {
		t.Fatal("forged contact proof accepted")
	}
	if err := s.SendContactVerification(ctx, p, "user@example.com"); err != nil {
		t.Fatal(err)
	}
	code := portalTestCode(t, m)
	wrong := "00000000"
	if code == wrong {
		wrong = "11111111"
	}
	for range 6 {
		if err := s.VerifyContact(ctx, p, wrong); !errors.Is(err, ErrPortalInvalid) {
			t.Fatal("wrong code accepted")
		}
	}
	if err := s.VerifyContact(ctx, p, code); !errors.Is(err, ErrPortalInvalid) {
		t.Fatal("exhausted proof accepted")
	}
	if err := s.SendContactVerification(ctx, p, "user@example.com"); !errors.Is(err, ErrPortalRate) {
		t.Fatal("resend interval not enforced")
	}
	reset := func() {
		portalTestSQL(t, ctx, tdb, `UPDATE support_portal_contact SET last_sent_at=now()-interval '61 seconds' WHERE business_id=$1 AND external_id=$2`, p.BusinessID, p.ExternalID)
	}
	reset()
	if err := s.SendContactVerification(ctx, p, "user@example.com"); err != nil {
		t.Fatal(err)
	}
	code = portalTestCode(t, m)
	portalTestSQL(t, ctx, tdb, `UPDATE support_portal_contact SET challenge_expires_at=now()-interval '1 second' WHERE business_id=$1 AND external_id=$2`, p.BusinessID, p.ExternalID)
	if err := s.VerifyContact(ctx, p, code); !errors.Is(err, ErrPortalInvalid) {
		t.Fatal("expired code accepted")
	}
	reset()
	portalTestVerify(t, ctx, s, p, m, "user@example.com")
	code = portalTestCode(t, m)
	if err := s.VerifyContact(ctx, p, code); !errors.Is(err, ErrPortalInvalid) {
		t.Fatal("replayed code accepted")
	}
	ticket, err := s.CreateTicket(ctx, p, "Contact update", "Original", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	reset()
	if err := s.SendContactVerification(ctx, p, "replacement@example.com"); err != nil {
		t.Fatal(err)
	}
	pendingCode := portalTestCode(t, m)
	reset()
	m.err = errors.New("provider refused")
	if err := s.SendContactVerification(ctx, p, "failed@example.com"); !errors.Is(err, ErrPortalUnavailable) {
		t.Fatal("provider rejection accepted")
	}
	contact, err := s.Contact(ctx, p)
	if err != nil || !contact.Verified || contact.Email != "user@example.com" {
		t.Fatal("failed replacement lost verified contact")
	}
	if err := s.VerifyContact(ctx, p, pendingCode); err != nil {
		t.Fatal("failed resend destroyed previous pending proof")
	}
	var recipient string
	if err := tdb.Super.QueryRow(ctx, `SELECT r.email::text FROM ticket t JOIN requester r ON r.id=t.requester_id WHERE t.id=$1`, ticket.ID).Scan(&recipient); err != nil || recipient != "replacement@example.com" {
		t.Fatal("owned ticket notification contact not updated")
	}
	reset()
	m.err = nil
	if err := s.SendContactVerification(ctx, p, "sixth@example.com"); !errors.Is(err, ErrPortalRate) {
		t.Fatal("hourly five-send cap not enforced")
	}
	portalTestSQL(t, ctx, tdb, `UPDATE support_portal_contact SET send_times='{}' WHERE business_id=$1 AND external_id=$2`, p.BusinessID, p.ExternalID)
	portalTestSQL(t, ctx, tdb, `UPDATE mailing_sending_profile SET status='error' WHERE id=$1`, profile)
	before := len(m.mails)
	if err := s.SendContactVerification(ctx, p, "badprofile@example.com"); !errors.Is(err, ErrPortalUnavailable) {
		t.Fatal("unverified profile accepted")
	}
	if len(m.mails) != before {
		t.Fatal("unverified sender reached provider")
	}
}

func TestPortalKeyLifecycleAndAuthorization(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	s := &PortalService{DB: tdb.App, Resolve: portalTestResolver}
	if _, err := s.CreateKey(ctx, rt.reader, rt.master, "not owner", 1); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("nonowner key creation: %v", err)
	}
	p, key := portalTestScope(t, ctx, s, rt, "tph:"+uuid.New().String())
	if _, err := s.Authenticate(ctx, key.Token, p.ExternalID, "bad\nname"); !errors.Is(err, ErrPortalUnauthorized) {
		t.Fatal("control header accepted")
	}
	if err := s.RevokeKey(ctx, rt.owner, rt.master, key.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, rt.owner, rt.master, key.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("repeated revocation should have no mutation: %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM audit_entry WHERE action='support.portal_key.revoked' AND target_id=$1`, key.ID); n != 1 {
		t.Fatal("unchanged revocation wrote audit")
	}
	if _, err := s.Authenticate(ctx, key.Token, p.ExternalID, ""); !errors.Is(err, ErrPortalUnauthorized) {
		t.Fatal("revoked key authenticated")
	}
	if _, err := s.Contact(ctx, p); !errors.Is(err, ErrPortalUnauthorized) {
		t.Fatal("cached revoked scope accepted")
	}
	_, expired := portalTestScope(t, ctx, s, rt, p.ExternalID)
	portalTestSQL(t, ctx, tdb, `UPDATE support_portal_key SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE id=$1`, expired.ID)
	if _, err := s.Authenticate(ctx, expired.Token, p.ExternalID, ""); !errors.Is(err, ErrPortalUnauthorized) {
		t.Fatal("expired key authenticated")
	}
	for range 10 {
		if _, err := s.CreateKey(ctx, rt.owner, rt.master, "active", 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateKey(ctx, rt.owner, rt.master, "eleventh", 1); !errors.Is(err, ErrPortalConflict) {
		t.Fatal("active key cap not enforced")
	}
}

func TestPortalSiblingRequesterReuseDoesNotGrantOwnership(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	m := &portalTestMailer{}
	s := &PortalService{DB: tdb.App, Resolve: portalTestResolver, Mailer: m, ReplyTokenKey: replyKey, SystemDomain: "portal.test"}
	rootTicket := uuid.New()
	seedTicket(ctx, t, tdb, rt, rootTicket, "open", "normal", "Root email ticket", nil, nil, -time.Hour)
	child := rt
	child.master = uuid.New()
	portalTestSQL(t, ctx, tdb, `INSERT INTO business(id,parent_id,tenant_root_id,name,status) VALUES($1,$2,$2,'Child','active')`, child.master, rt.master)
	portalTestSQL(t, ctx, tdb, `INSERT INTO business_closure(ancestor_id,descendant_id,depth,tenant_root_id) VALUES($1,$1,0,$2),($2,$1,1,$2)`, child.master, rt.master)
	portalTestSender(t, ctx, tdb, child)
	external := "tph:" + uuid.New().String()
	parentScope, _ := portalTestScope(t, ctx, s, rt, external)
	p, _ := portalTestScope(t, ctx, s, child, external)
	portalTestVerify(t, ctx, s, p, m, "ada@example.com")
	ticket, err := s.CreateTicket(ctx, p, "Child support", "Contact proven", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	var requester uuid.UUID
	var name string
	if err := tdb.Super.QueryRow(ctx, `SELECT t.requester_id,r.display_name FROM ticket t JOIN requester r ON r.id=t.requester_id WHERE t.id=$1`, ticket.ID).Scan(&requester, &name); err != nil {
		t.Fatal(err)
	}
	if requester != rt.requester || name != "Ada" {
		t.Fatal("tenant requester identity was duplicated or metadata overwritten")
	}
	if _, err := s.Thread(ctx, p, rootTicket, ""); !errors.Is(err, errs.ErrNotFound) {
		t.Fatal("shared requester imported sibling email ticket")
	}
	if _, err := s.Thread(ctx, parentScope, ticket.ID, ""); !errors.Is(err, errs.ErrNotFound) {
		t.Fatal("same UUID crossed business ownership")
	}
	native := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "portal.test"}
	if _, err := native.Reply(ctx, rt.owner, child.master, ticket.ID, ReplyInput{BodyText: "Native child reply"}); err != nil {
		t.Fatal(err)
	}
	thread, err := s.Thread(ctx, p, ticket.ID, "")
	if err != nil || len(thread.Messages) != 2 {
		t.Fatal("native sibling requester reply missing")
	}
	// An explicit business scope on the child record may not mask a foreign parent.
	err = tdb.App.WithPrincipal(ctx, rt.owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO support_portal_ticket(ticket_id,business_id,tenant_root_id,external_id) VALUES($1,$2,$3,$4)`, rootTicket, child.master, rt.master, external)
		return err
	})
	if err == nil {
		t.Fatal("cross-business native parent accepted")
	}
	rt2 := seedReadTenant(ctx, t, tdb)
	if err := tdb.App.WithPrincipal(ctx, rt2.owner, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM support_portal_contact WHERE business_id=$1`, child.master).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatal("portal RLS leaked contact")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Account state, not retained membership, determines whether a portal key lives.
func TestPortalCreatorAccountLifecycle(t *testing.T) {
	ctx, tdb := startReadDB(t)
	for _, state := range []string{"deactivated", "deleted"} {
		t.Run(state, func(t *testing.T) {
			rt := seedReadTenant(ctx, t, tdb)
			s := &PortalService{DB: tdb.App, Resolve: portalTestResolver}
			p, key := portalTestScope(t, ctx, s, rt, "tph:"+uuid.NewString())
			if _, err := s.Contact(ctx, p); err != nil {
				t.Fatal(err)
			}
			query := `UPDATE account SET status='deactivated' WHERE id=(SELECT account_id FROM principal WHERE id=$1)`
			if state == "deleted" {
				query = `UPDATE account SET deleted_at=now() WHERE id=(SELECT account_id FROM principal WHERE id=$1)`
			}
			portalTestSQL(t, ctx, tdb, query, rt.owner)
			if _, err := s.CreateKey(ctx, rt.owner, rt.master, "ineligible", 1); !errors.Is(err, errs.ErrNotFound) {
				t.Fatalf("ineligible creator: %v", err)
			}
			if _, err := s.Authenticate(ctx, key.Token, p.ExternalID, ""); !errors.Is(err, ErrPortalUnauthorized) {
				t.Fatalf("ineligible authentication: %v", err)
			}
			if _, err := s.Contact(ctx, p); !errors.Is(err, ErrPortalUnauthorized) {
				t.Fatalf("cached scope survived account lifecycle: %v", err)
			}
			// Reactivation restores an otherwise valid key; revocation remains separate.
			portalTestSQL(t, ctx, tdb, `UPDATE account SET status='active',deleted_at=NULL WHERE id=(SELECT account_id FROM principal WHERE id=$1)`, rt.owner)
			if _, err := s.Contact(ctx, p); err != nil {
				t.Fatalf("active authorized creator: %v", err)
			}
		})
	}
}

type portalCallbackMailer func(context.Context, uuid.UUID, notify.Mail) error

func (f portalCallbackMailer) SendSupportReply(ctx context.Context, id uuid.UUID, m notify.Mail) error {
	return f(ctx, id, m)
}

func TestPortalSendAllowanceSurvivesCancellationAndRevocation(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	m := &portalTestMailer{}
	portalTestSender(t, ctx, tdb, rt)
	s := &PortalService{DB: tdb.App, Resolve: portalTestResolver, Mailer: m}
	p, _ := portalTestScope(t, ctx, s, rt, "tph:"+uuid.NewString())
	if err := s.SendContactVerification(ctx, p, "old@example.com"); err != nil {
		t.Fatal(err)
	}
	oldCode := portalTestCode(t, m)
	for _, mode := range []string{"cancel", "revoke", "superseded"} {
		t.Run(mode, func(t *testing.T) {
			portalTestSQL(t, ctx, tdb, `UPDATE support_portal_contact SET last_sent_at=now()-interval '61 seconds' WHERE business_id=$1 AND external_id=$2`, p.BusinessID, p.ExternalID)
			sendCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			s.Mailer = portalCallbackMailer(func(_ context.Context, _ uuid.UUID, _ notify.Mail) error {
				// A separate connection observes the durable allowance before delivery and
				// can acquire the row: provider I/O holds no portal transaction/row locks.
				var count int
				if err := tdb.Super.QueryRow(ctx, `SELECT cardinality(send_times) FROM support_portal_contact WHERE business_id=$1 AND external_id=$2`, p.BusinessID, p.ExternalID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count < 2 {
					t.Fatal("provider send precedes durable quota reservation")
				}
				switch mode {
				case "cancel":
					cancel()
				case "revoke":
					if err := s.RevokeKey(ctx, p.PrincipalID, p.BusinessID, p.KeyID); err != nil {
						t.Fatal(err)
					}
				case "superseded":
					portalTestSQL(t, ctx, tdb, `UPDATE support_portal_contact SET send_nonce=gen_random_uuid() WHERE business_id=$1 AND external_id=$2`, p.BusinessID, p.ExternalID)
				}
				return nil // provider accepted; finalization must still fail
			})
			if err := s.SendContactVerification(sendCtx, p, "new@example.com"); err == nil {
				t.Fatal("invalidated send activated a challenge")
			}
			var pending string
			var count int
			if err := tdb.Super.QueryRow(ctx, `SELECT pending_email,cardinality(send_times) FROM support_portal_contact WHERE business_id=$1 AND external_id=$2`, p.BusinessID, p.ExternalID).Scan(&pending, &count); err != nil {
				t.Fatal(err)
			}
			if pending != "old@example.com" {
				t.Fatal("failed finalization destroyed pending proof")
			}
			if count < 2 {
				t.Fatal("accepted send lost quota accounting")
			}
			portalTestSQL(t, ctx, tdb, `UPDATE support_portal_key SET revoked_at=NULL WHERE id=$1`, p.KeyID)
			if err := s.SendContactVerification(ctx, p, "retry@example.com"); !errors.Is(err, ErrPortalRate) {
				t.Fatalf("immediate retry bypassed durable allowance: %v", err)
			}
		})
	}
	if err := s.VerifyContact(ctx, p, oldCode); err != nil {
		t.Fatalf("prior challenge lost after failed deliveries: %v", err)
	}
}

func TestPortalThreadPreservesNativeUnicodeAndBoundsPages(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	m := &portalTestMailer{}
	portalTestSender(t, ctx, tdb, rt)
	s := &PortalService{DB: tdb.App, Resolve: portalTestResolver, Mailer: m, ReplyTokenKey: replyKey, SystemDomain: "portal.test"}
	p, _ := portalTestScope(t, ctx, s, rt, "tph:"+uuid.NewString())
	portalTestVerify(t, ctx, s, p, m, "unicode@example.com")
	ticket, err := s.CreateTicket(ctx, p, "Native Unicode", "Original", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	native := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "portal.test"}
	body := strings.Repeat("界", 25000)
	reply, err := native.Reply(ctx, rt.owner, rt.master, ticket.ID, ReplyInput{BodyText: body})
	if err != nil {
		t.Fatal(err)
	}
	thread, err := s.Thread(ctx, p, ticket.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 2 || thread.Messages[0].Body != body {
		t.Fatal("native reply was truncated or rejected by byte/rune mismatch")
	}
	// Repeated JSON-escaped bodies exercise the encoded page budget. SQL limits
	// each individual body before allocation, independent of this page bound.
	portalTestSQL(t, ctx, tdb, `INSERT INTO ticket_message(id,ticket_id,business_id,tenant_root_id,direction,author_principal_id,message_id,body_text,created_at) SELECT gen_random_uuid(),$1,$2,$3,'outbound',$4,gen_random_uuid()::text,repeat('<',1048576),now()+i*interval '1 second' FROM generate_series(1,3) i`, ticket.ID, p.BusinessID, p.TenantRootID, rt.owner)
	page, err := s.Thread(ctx, p, ticket.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.NextCursor == "" {
		t.Fatal("encoded response page bound not enforced")
	}
	older, err := s.Thread(ctx, p, ticket.ID, page.NextCursor)
	if err != nil || len(older.Messages) != 1 {
		t.Fatal("byte-budget cursor lost a message")
	}
	portalTestSQL(t, ctx, tdb, `UPDATE ticket_message SET body_text=repeat('x',1048577) WHERE id=$1`, reply.ID)
	if _, err := s.Thread(ctx, p, ticket.ID, reply.ID.String()); err != nil {
		t.Fatal("other bounded messages became inaccessible")
	}
	if _, err := s.Thread(ctx, p, ticket.ID, older.Messages[0].ID.String()); err == nil {
		t.Fatal("oversized native message silently truncated")
	}
}

func TestPortalSurvivesFencedTenantMerge(t *testing.T) {
	ctx, tdb := startReadDB(t)
	source := seedReadTenant(ctx, t, tdb)
	destination := seedReadTenant(ctx, t, tdb)
	// Distinct native email identities avoid the existing, intentional collision
	// blocker. This test exercises a lossless cutover, not requester reconciliation.
	portalTestSQL(t, ctx, tdb, `DELETE FROM requester WHERE tenant_root_id=$1`, destination.master)
	portalTestSQL(t, ctx, tdb, `UPDATE role SET key='destination-only-role' WHERE tenant_root_id=$1 AND key='rd-norole'`, destination.master)
	ownerRole := presetRole(ctx, t, tdb, "owner")
	portalTestSQL(t, ctx, tdb, `INSERT INTO membership(principal_id,business_id,tenant_root_id,role_id) VALUES($1,$2,$2,$3)`, source.owner, destination.master, ownerRole)
	m := &portalTestMailer{}
	portalTestSender(t, ctx, tdb, source)
	s := &PortalService{DB: tdb.App, Resolve: portalTestResolver, Mailer: m, ReplyTokenKey: replyKey, SystemDomain: "portal.test"}
	p, key := portalTestScope(t, ctx, s, source, "tph:"+uuid.NewString())
	portalTestVerify(t, ctx, s, p, m, "merge-customer@example.com")
	requestID := uuid.New()
	ticket, err := s.CreateTicket(ctx, p, "Preserve this ticket", "Private original message", requestID)
	if err != nil {
		t.Fatal(err)
	}
	merges := &tenancy.Service{DB: tdb.App}
	operation, err := merges.CreateTenantMergeOperation(ctx, source.owner, source.master, destination.master, "portal-merge")
	if err != nil {
		t.Fatal(err)
	}
	operation, err = merges.PreflightTenantMerge(ctx, source.owner, operation.ID)
	if err != nil || operation.Status != "ready" {
		t.Fatalf("merge preflight: status=%s conflicts=%+v err=%v", operation.Status, operation.Conflicts, err)
	}
	// Fixture authorization for the low-level cutover; interactive confirmation is
	// covered by tenancy's own service/HTTP tests, never bypassed in production.
	portalTestSQL(t, ctx, tdb, `UPDATE tenant_merge_operation SET confirmed_at=now(),confirmation_method='password_and_typed_names',confirmation_hash=repeat('a',64),confirmation_preflight_generation=preflight_generation WHERE id=$1`, operation.ID)
	if _, err := merges.BeginTenantMergeFence(ctx, source.owner, operation.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SendContactVerification(ctx, p, "fenced@example.com"); err == nil {
		t.Fatal("portal write bypassed durable merge fence")
	}
	operation, err = merges.CutoverTenantMerge(ctx, source.owner, operation.ID)
	if err != nil || operation.Status != "succeeded" {
		t.Fatalf("merge cutover: status=%s failure=%+v err=%v", operation.Status, operation.Failure, err)
	}
	if _, err := s.Contact(ctx, p); !errors.Is(err, ErrPortalUnauthorized) {
		t.Fatalf("cached pre-cutover scope survived: %v", err)
	}
	current, err := s.Authenticate(ctx, key.Token, p.ExternalID, p.DisplayName)
	if err != nil {
		t.Fatalf("fresh post-cutover auth: %v", err)
	}
	if current.BusinessID != p.BusinessID || current.TenantRootID != destination.master || current.KeyID != p.KeyID {
		t.Fatal("cutover changed credential/business identity")
	}
	contact, err := s.Contact(ctx, current)
	if err != nil || !contact.Verified || contact.Email != "merge-customer@example.com" {
		t.Fatal("verified contact lost")
	}
	thread, err := s.Thread(ctx, current, ticket.ID, "")
	if err != nil || len(thread.Messages) != 1 || thread.Messages[0].Body != "Private original message" {
		t.Fatalf("native thread lost: %v", err)
	}
	replay, err := s.CreateTicket(ctx, current, "Preserve this ticket", "Private original message", requestID)
	if err != nil || replay.ID != ticket.ID {
		t.Fatal("cutover lost idempotency")
	}
	other, err := s.Authenticate(ctx, key.Token, "tph:"+uuid.NewString(), "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Thread(ctx, other, ticket.ID, ""); !errors.Is(err, errs.ErrNotFound) {
		t.Fatal("cutover widened customer access")
	}
}
