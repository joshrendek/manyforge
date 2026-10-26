//go:build integration

package ticketing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// Spec 019 — cross-business support overview. Every expected number below is derived by hand
// from the seeded fixture; see specs/019-support-overview/spec.md for the definitions.

type ovTicket struct {
	status, priority string
	assignee         *uuid.UUID
	created, updated time.Time
	redacted         bool
}

// seedOvTicket inserts a ticket in business (tenant root) with explicit timestamps, plus its
// messages: each msgs entry is a direction at a time. last_message_at follows the newest message.
func seedOvTicket(ctx context.Context, t *testing.T, tdb *testdb.TestDB, business, root, requester, author uuid.UUID,
	subject string, tk ovTicket, msgs ...struct {
		dir string
		at  time.Time
	}) uuid.UUID {
	t.Helper()
	id := uuid.New()
	last := tk.created
	for _, m := range msgs {
		if m.at.After(last) {
			last = m.at
		}
	}
	var redacted *time.Time
	if tk.redacted {
		redacted = &tk.updated
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO ticket (id,business_id,tenant_root_id,requester_id,subject,status,priority,assignee_principal_id,
		                     reply_token,last_message_at,redacted_at,created_at,updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6::ticket_status,$7::ticket_priority,$8,$9,$10,$11,$12,$13)`,
		id, business, root, requester, subject, tk.status, tk.priority, tk.assignee,
		"tok-"+id.String(), last, redacted, tk.created, tk.updated); err != nil {
		t.Fatalf("seed ov ticket %s: %v", subject, err)
	}
	for _, m := range msgs {
		var by *uuid.UUID
		if m.dir != "inbound" {
			by = &author
		}
		mid := uuid.New()
		if _, err := tdb.Super.Exec(ctx,
			`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,author_principal_id,message_id,"references",body_text,is_auto_reply,created_at)
			 VALUES ($1,$2,$3,$4,$5::ticket_message_direction,$6,$7,'{}','x',false,$8)`,
			mid, id, business, root, m.dir, by, "m-"+mid.String()+"@example.com", m.at); err != nil {
			t.Fatalf("seed ov message %s/%s: %v", subject, m.dir, err)
		}
	}
	return id
}

func msg(dir string, at time.Time) struct {
	dir string
	at  time.Time
} {
	return struct {
		dir string
		at  time.Time
	}{dir, at}
}

func grantMembership(ctx context.Context, t *testing.T, tdb *testdb.TestDB, principal, business, root, role uuid.UUID) {
	t.Helper()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$3,$4,now())`,
		principal, business, root, role); err != nil {
		t.Fatalf("grant membership: %v", err)
	}
}

func TestSupportOverview(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	day := 24 * time.Hour

	t1 := seedReadTenant(ctx, t, tdb) // ReadCo — the caller (t1.reader) is a member here
	child := seedChildBusiness(ctx, t, tdb, t1)
	t2 := seedReadTenant(ctx, t, tdb) // a second tenant the caller is ALSO a member of
	t3 := seedReadTenant(ctx, t, tdb) // a tenant where the caller has a role WITHOUT tickets.read
	t4 := seedReadTenant(ctx, t, tdb) // a tenant the caller has no membership in at all
	grantMembership(ctx, t, tdb, t1.reader, t2.master, t2.master, presetRole(ctx, t, tdb, "member"))
	var t3NoRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `SELECT id FROM role WHERE tenant_root_id=$1 AND key='rd-norole'`, t3.master).Scan(&t3NoRole); err != nil {
		t.Fatalf("t3 norole: %v", err)
	}
	grantMembership(ctx, t, tdb, t1.reader, t3.master, t3.master, t3NoRole)

	m := t1.master
	owner := t1.owner
	// A: new urgent, customer waiting 3h, never answered.
	a := seedOvTicket(ctx, t, tdb, m, m, t1.requester, owner, "A", ovTicket{status: "new", priority: "urgent", created: ago(3 * time.Hour), updated: ago(3 * time.Hour)},
		msg("inbound", ago(3*time.Hour)))
	// B: open high, assigned, answered after 1h → first response 3600s; not waiting.
	seedOvTicket(ctx, t, tdb, m, m, t1.requester, owner, "B", ovTicket{status: "open", priority: "high", assignee: &owner, created: ago(5 * time.Hour), updated: ago(4 * time.Hour)},
		msg("inbound", ago(5*time.Hour)), msg("outbound", ago(4*time.Hour)))
	// C: open normal, answered after 30m (1800s), customer replied 2h ago, then an internal note —
	// the note must not count as an answer, so C is waiting since the 2h-old inbound.
	c := seedOvTicket(ctx, t, tdb, m, m, t1.requester, owner, "C", ovTicket{status: "open", priority: "normal", created: ago(10 * time.Hour), updated: ago(time.Hour)},
		msg("inbound", ago(10*time.Hour)), msg("outbound", ago(10*time.Hour-30*time.Minute)), msg("inbound", ago(2*time.Hour)), msg("note", ago(time.Hour)))
	// D: pending low, old — open set, not waiting (pending = waiting on the customer).
	seedOvTicket(ctx, t, tdb, m, m, t1.requester, owner, "D", ovTicket{status: "pending", priority: "low", assignee: &owner, created: ago(30 * day), updated: ago(30 * day)},
		msg("inbound", ago(30*day)), msg("outbound", ago(30*day-time.Hour)))
	// E: solved yesterday → resolved_7d.
	seedOvTicket(ctx, t, tdb, m, m, t1.requester, owner, "E", ovTicket{status: "solved", priority: "normal", created: ago(20 * day), updated: ago(day)},
		msg("inbound", ago(20*day)))
	// F: closed 10 days ago → counts nowhere.
	seedOvTicket(ctx, t, tdb, m, m, t1.requester, owner, "F", ovTicket{status: "closed", priority: "normal", created: ago(40 * day), updated: ago(10 * day)},
		msg("inbound", ago(40*day)))
	// G: redacted urgent waiting ticket → excluded everywhere.
	seedOvTicket(ctx, t, tdb, m, m, t1.requester, owner, "G", ovTicket{status: "open", priority: "urgent", created: ago(time.Hour), updated: ago(time.Hour), redacted: true},
		msg("inbound", ago(time.Hour)))

	// H: child business, urgent, waiting 30h.
	childReq := seedRequesterInBusiness(ctx, t, tdb, t1, child)
	h := seedOvTicket(ctx, t, tdb, child, m, childReq, owner, "H", ovTicket{status: "open", priority: "urgent", created: ago(30 * time.Hour), updated: ago(30 * time.Hour)},
		msg("inbound", ago(30*time.Hour)))
	// I: second tenant, low, waiting 50h.
	i := seedOvTicket(ctx, t, tdb, t2.master, t2.master, t2.requester, t2.owner, "I", ovTicket{status: "new", priority: "low", created: ago(50 * time.Hour), updated: ago(50 * time.Hour)},
		msg("inbound", ago(50*time.Hour)))
	// J/K: tenants the caller can't read support for — must not appear in totals or attention.
	seedOvTicket(ctx, t, tdb, t3.master, t3.master, t3.requester, t3.owner, "J", ovTicket{status: "open", priority: "urgent", created: ago(80 * time.Hour), updated: ago(80 * time.Hour)},
		msg("inbound", ago(80*time.Hour)))
	seedOvTicket(ctx, t, tdb, t4.master, t4.master, t4.requester, t4.owner, "K", ovTicket{status: "open", priority: "urgent", created: ago(90 * time.Hour), updated: ago(90 * time.Hour)},
		msg("inbound", ago(90*time.Hour)))

	ov, err := svc.Overview(ctx, t1.reader)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}

	// Businesses: ReadCo (2 waiting) first, then the 1-waiting tie broken by name.
	if len(ov.Businesses) != 3 {
		t.Fatalf("businesses = %d (%+v), want 3: t1, its child, t2 — t3/t4 excluded", len(ov.Businesses), ov.Businesses)
	}
	wantOrder := []uuid.UUID{t1.master, child, t2.master}
	for k, b := range ov.Businesses {
		if b.BusinessID != wantOrder[k] {
			t.Fatalf("business[%d] = %s (%s), want %s", k, b.BusinessID, b.BusinessName, wantOrder[k])
		}
	}

	r := ov.Businesses[0].OverviewCounts
	checks := []struct {
		name      string
		got, want int64
	}{
		{"open", r.Open, 4}, {"new", r.New, 1}, {"pending", r.Pending, 1}, {"awaiting", r.AwaitingReply, 2},
		{"unassigned", r.Unassigned, 2}, {"urgent", r.Urgent, 1}, {"high", r.High, 1},
		{"created_7d", r.Created7d, 3}, {"resolved_7d", r.Resolved7d, 1},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("ReadCo %s = %d, want %d", ck.name, ck.got, ck.want)
		}
	}
	if r.MedianFirstResponseSeconds7d == nil || absf(*r.MedianFirstResponseSeconds7d-2700) > 1 {
		t.Errorf("ReadCo median first response = %v, want 2700 (median of 3600 and 1800)", r.MedianFirstResponseSeconds7d)
	}
	if r.OldestAwaitingAt == nil || absd(r.OldestAwaitingAt.Sub(ago(3*time.Hour))) > time.Second {
		t.Errorf("ReadCo oldest awaiting = %v, want ~%v (ticket A)", r.OldestAwaitingAt, ago(3*time.Hour))
	}

	tot := ov.Totals
	totChecks := []struct {
		name      string
		got, want int64
	}{
		{"open", tot.Open, 6}, {"awaiting", tot.AwaitingReply, 4}, {"urgent", tot.Urgent, 2},
		{"unassigned", tot.Unassigned, 4}, {"created_7d", tot.Created7d, 5}, {"resolved_7d", tot.Resolved7d, 1},
	}
	for _, ck := range totChecks {
		if ck.got != ck.want {
			t.Errorf("totals %s = %d, want %d", ck.name, ck.got, ck.want)
		}
	}
	if tot.MedianFirstResponseSeconds7d == nil || absf(*tot.MedianFirstResponseSeconds7d-2700) > 1 {
		t.Errorf("totals median = %v, want 2700", tot.MedianFirstResponseSeconds7d)
	}
	if tot.OldestAwaitingAt == nil || absd(tot.OldestAwaitingAt.Sub(ago(50*time.Hour))) > time.Second {
		t.Errorf("totals oldest awaiting = %v, want ~%v (ticket I)", tot.OldestAwaitingAt, ago(50*time.Hour))
	}
	if child := ov.Businesses[1]; child.MedianFirstResponseSeconds7d != nil {
		t.Errorf("ChildCo median = %v, want nil (no replies)", *child.MedianFirstResponseSeconds7d)
	}

	// Attention: urgent first (H waited longer than A), then normal C, then low I.
	wantAttn := []uuid.UUID{h, a, c, i}
	if len(ov.Attention) != len(wantAttn) {
		t.Fatalf("attention = %d items (%+v), want %d", len(ov.Attention), ov.Attention, len(wantAttn))
	}
	for k, it := range ov.Attention {
		if it.TicketID != wantAttn[k] {
			t.Errorf("attention[%d] = %s (%s), want %s", k, it.TicketID, it.Subject, wantAttn[k])
		}
	}
	if got := ov.Attention[2]; absd(got.WaitingSince.Sub(ago(2*time.Hour))) > time.Second || got.Assigned {
		t.Errorf("attention C waiting_since=%v assigned=%v, want ~%v (the inbound, not the later note), false", got.WaitingSince, got.Assigned, ago(2*time.Hour))
	}
	if ov.Attention[3].BusinessID != t2.master || ov.Attention[3].BusinessName != "ReadCo" {
		t.Errorf("attention I business = %s/%q, want t2 ReadCo", ov.Attention[3].BusinessID, ov.Attention[3].BusinessName)
	}
}

// A caller with no tickets.read anywhere gets an empty, zeroed overview — not an error, and
// nothing from the tenant they do belong to.
func TestSupportOverviewNoReadableBusiness(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)
	t1 := seedReadTenant(ctx, t, tdb)
	seedTicket(ctx, t, tdb, t1, uuid.New(), "open", "urgent", "hidden", nil, nil, -time.Hour)

	ov, err := svc.Overview(ctx, t1.noReader)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(ov.Businesses) != 0 || len(ov.Attention) != 0 {
		t.Fatalf("noReader sees businesses=%+v attention=%+v, want none", ov.Businesses, ov.Attention)
	}
	if ov.Totals.Open != 0 || ov.Totals.AwaitingReply != 0 || ov.Totals.MedianFirstResponseSeconds7d != nil || ov.Totals.OldestAwaitingAt != nil {
		t.Fatalf("noReader totals = %+v, want zero", ov.Totals)
	}
	if ov.AsOf.IsZero() {
		t.Fatal("as_of not set")
	}
}

// The attention list is capped server-side.
func TestSupportOverviewAttentionCapped(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)
	t1 := seedReadTenant(ctx, t, tdb)
	for k := 0; k < OverviewAttentionLimit+5; k++ {
		seedTicket(ctx, t, tdb, t1, uuid.New(), "new", "normal", "w", nil, nil, -time.Duration(k+1)*time.Minute)
	}
	ov, err := svc.Overview(ctx, t1.reader)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(ov.Attention) != OverviewAttentionLimit {
		t.Fatalf("attention = %d, want cap %d", len(ov.Attention), OverviewAttentionLimit)
	}
	if ov.Totals.AwaitingReply != int64(OverviewAttentionLimit+5) {
		t.Fatalf("awaiting = %d, want %d (the cap limits the list, not the count)", ov.Totals.AwaitingReply, OverviewAttentionLimit+5)
	}
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func absd(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
