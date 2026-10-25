//go:build integration

package ticketing

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

type portalNoPermissions struct{}

func (portalNoPermissions) Has(string) bool { return false }

func TestPortalLifetimeTicketCapAndCredentialBoundProof(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	m := &portalTestMailer{}
	portalTestSender(t, ctx, tdb, rt)
	s := &PortalService{DB: tdb.App, Resolve: portalTestResolver, Mailer: m, ReplyTokenKey: replyKey, SystemDomain: "portal.test"}
	p, _ := portalTestScope(t, ctx, s, rt, "tph:"+uuid.New().String())
	otherKey, _ := portalTestScope(t, ctx, s, rt, p.ExternalID)
	if err := s.SendContactVerification(ctx, p, "cap@example.com"); err != nil {
		t.Fatal(err)
	}
	code := portalTestCode(t, m)
	if err := s.VerifyContact(ctx, otherKey, code); !errors.Is(err, ErrPortalInvalid) {
		t.Fatal("another integration credential accepted proof")
	}
	if err := s.VerifyContact(ctx, p, code); err != nil {
		t.Fatal(err)
	}
	var requester uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `SELECT requester_id FROM support_portal_contact WHERE business_id=$1 AND external_id=$2`, p.BusinessID, p.ExternalID).Scan(&requester); err != nil {
		t.Fatal(err)
	}
	portalTestSQL(t, ctx, tdb, `WITH made AS(INSERT INTO ticket(business_id,tenant_root_id,requester_id,subject,reply_token) SELECT $1,$2,$3,'Lifetime cap',gen_random_uuid()::text FROM generate_series(1,200) RETURNING id) INSERT INTO support_portal_ticket(ticket_id,business_id,tenant_root_id,external_id) SELECT id,$1,$2,$4 FROM made`, p.BusinessID, p.TenantRootID, requester, p.ExternalID)
	if _, err := s.CreateTicket(ctx, p, "Unreachable ticket", "No", uuid.New()); !errors.Is(err, ErrPortalConflict) {
		t.Fatal("customer creation cap not enforced")
	}
	tickets, err := s.ListTickets(ctx, p)
	if err != nil || len(tickets) != 200 {
		t.Fatal("created tickets not fully reachable")
	}
	// Losing the creating principal's permission invalidates even an unexpired key.
	s.Resolve = func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) (httpx.Permissions, error) {
		return portalNoPermissions{}, nil
	}
	if _, err := s.Authenticate(ctx, p.token, p.ExternalID, p.DisplayName); !errors.Is(err, ErrPortalUnauthorized) {
		t.Fatal("creator permission loss did not invalidate credential")
	}
}
