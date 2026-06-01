//go:build integration

package ticketing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// readTenant is a tenant seeded for the read slice: a master business, three
// human principals (a member WITH tickets.read via the `member` preset, a viewer
// WITH tickets.read via the `viewer` preset, and an outsider with a custom role
// that LACKS tickets.read), a requester, and a set of tickets/messages.
type readTenant struct {
	master       uuid.UUID
	tenantRootID uuid.UUID
	owner        uuid.UUID // owner preset → satisfies the "retain ≥1 Owner" trigger
	reader       uuid.UUID // member preset → has tickets.read
	noReader     uuid.UUID // custom role → NO tickets.read
	requester    uuid.UUID
	requester2   uuid.UUID
}

func presetRole(ctx context.Context, t *testing.T, tdb *testdb.TestDB, key string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key=$1", key).Scan(&id); err != nil {
		t.Fatalf("preset role %q: %v", key, err)
	}
	return id
}

// seedReadTenant seeds a complete read-slice fixture via the RLS-exempt Super pool.
func seedReadTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) readTenant {
	t.Helper()
	rt := readTenant{
		master: uuid.New(), tenantRootID: uuid.New(),
		owner: uuid.New(), reader: uuid.New(), noReader: uuid.New(),
		requester: uuid.New(), requester2: uuid.New(),
	}
	rt.tenantRootID = rt.master // master: business id == tenant_root_id

	ownerRole := presetRole(ctx, t, tdb, "owner")   // full catalog
	memberRole := presetRole(ctx, t, tdb, "member") // has tickets.read
	noRole := uuid.New()                            // custom role w/o tickets.read

	aOwner, aReader, aNoReader := uuid.New(), uuid.New(), uuid.New()
	em := func(p uuid.UUID) string { return "rd-" + p.String() + "@x.test" }

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'O','active',now(),now(),now()),($3,$4,'R','active',now(),now(),now()),($5,$6,'N','active',now(),now(),now())`,
			[]any{aOwner, em(rt.owner), aReader, em(rt.reader), aNoReader, em(rt.noReader)}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now()),($3,'human',$4,now()),($5,'human',$6,now())`,
			[]any{rt.owner, aOwner, rt.reader, aReader, rt.noReader, aNoReader}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'ReadCo','active',now(),now())`, []any{rt.master}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{rt.master}},
		// A custom role that exists in the tenant but grants nothing support-related.
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'rd-norole','NoRole',false,now())`, []any{noRole, rt.master}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now()),($4,$2,$2,$5,now()),($6,$2,$2,$7,now())`,
			[]any{rt.owner, rt.master, ownerRole, rt.reader, memberRole, rt.noReader, noRole}},
		// Two requesters.
		{`INSERT INTO requester (id,business_id,tenant_root_id,email,display_name,first_seen_at,last_seen_at,created_at,updated_at) VALUES ($1,$2,$2,'ada@example.com','Ada',now(),now(),now(),now()),($3,$2,$2,'grace@example.com','Grace',now(),now(),now(),now())`,
			[]any{rt.requester, rt.master, rt.requester2}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return rt
}

// seedTicket inserts a ticket (+ its inbound message) for a tenant with explicit
// facets so filter/keyset tests are deterministic. lastMsgOffset shifts
// last_message_at so ordering is predictable.
func seedTicket(ctx context.Context, t *testing.T, tdb *testdb.TestDB, rt readTenant, ticketID uuid.UUID, status, priority, subject string, assignee *uuid.UUID, tags []string, lastMsgOffset time.Duration) uuid.UUID {
	t.Helper()
	msgID := uuid.New()
	base := time.Now().Add(lastMsgOffset)

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed ticket: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO ticket (id,business_id,tenant_root_id,requester_id,subject,status,priority,assignee_principal_id,reply_token,last_message_at,created_at,updated_at)
		 VALUES ($1,$2,$2,$3,$4,$5::ticket_status,$6::ticket_priority,$7,$8,$9,now(),now())`,
		ticketID, rt.master, rt.requester, subject, status, priority, assignee,
		"tok-"+ticketID.String(), base); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,message_id,"references",body_text,auth_results,is_auto_reply,created_at)
		 VALUES ($1,$2,$3,$3,'inbound',$4,'{}',$5,$6::jsonb,false,$7)`,
		msgID, ticketID, rt.master, "m-"+msgID.String()+"@example.com", "hello "+subject,
		`{"spf":"pass","dkim":"pass","dmarc":"pass"}`, base); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	for _, tag := range tags {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ticket_tag (ticket_id,tag,business_id,tenant_root_id,created_at) VALUES ($1,$2,$3,$3,now())`,
			ticketID, tag, rt.master); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed ticket: %v", err)
	}
	return msgID
}

func newReadService(tdb *testdb.TestDB) *Service { return &Service{DB: tdb.App} }

func startReadDB(t *testing.T) (context.Context, *testdb.TestDB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb
}

func countSuper(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// TestListTicketsScopedToBusiness — list returns ONLY the caller's business's
// tickets; a second tenant's tickets never leak (RLS + SQL predicate).
func TestListTicketsScopedToBusiness(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)

	t1 := seedReadTenant(ctx, t, tdb)
	t2 := seedReadTenant(ctx, t, tdb)
	seedTicket(ctx, t, tdb, t1, uuid.New(), "open", "normal", "t1-a", nil, nil, -1*time.Hour)
	seedTicket(ctx, t, tdb, t1, uuid.New(), "open", "normal", "t1-b", nil, nil, -2*time.Hour)
	seedTicket(ctx, t, tdb, t2, uuid.New(), "open", "normal", "t2-a", nil, nil, -1*time.Hour)

	page, err := svc.ListTickets(ctx, t1.reader, t1.master, TicketFilter{}, "", 50)
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("want 2 t1 tickets, got %d", len(page.Items))
	}
	for _, tk := range page.Items {
		if tk.BusinessID != t1.master {
			t.Errorf("ticket %s leaked from another business %s", tk.ID, tk.BusinessID)
		}
		if tk.Requester.ID == uuid.Nil {
			t.Errorf("ticket %s missing embedded requester", tk.ID)
		}
	}

	// A second tenant's reader sees only its single ticket.
	page2, err := svc.ListTickets(ctx, t2.reader, t2.master, TicketFilter{}, "", 50)
	if err != nil {
		t.Fatalf("ListTickets t2: %v", err)
	}
	if len(page2.Items) != 1 {
		t.Errorf("want 1 t2 ticket, got %d", len(page2.Items))
	}
}

// TestListTicketsFilters — status/priority/assignee=unassigned/tag facets work.
func TestListTicketsFilters(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)
	rt := seedReadTenant(ctx, t, tdb)

	assignee := rt.reader
	seedTicket(ctx, t, tdb, rt, uuid.New(), "open", "high", "open-high-assigned", &assignee, []string{"billing"}, -1*time.Hour)
	seedTicket(ctx, t, tdb, rt, uuid.New(), "pending", "low", "pending-low-unassigned", nil, []string{"shipping"}, -2*time.Hour)
	seedTicket(ctx, t, tdb, rt, uuid.New(), "open", "normal", "open-normal-unassigned", nil, nil, -3*time.Hour)

	st := "open"
	if p, _ := svc.ListTickets(ctx, rt.reader, rt.master, TicketFilter{Status: &st}, "", 50); len(p.Items) != 2 {
		t.Errorf("status=open: want 2, got %d", len(p.Items))
	}
	pr := "high"
	if p, _ := svc.ListTickets(ctx, rt.reader, rt.master, TicketFilter{Priority: &pr}, "", 50); len(p.Items) != 1 {
		t.Errorf("priority=high: want 1, got %d", len(p.Items))
	}
	if p, _ := svc.ListTickets(ctx, rt.reader, rt.master, TicketFilter{Unassigned: true}, "", 50); len(p.Items) != 2 {
		t.Errorf("assignee=unassigned: want 2, got %d", len(p.Items))
	}
	if p, _ := svc.ListTickets(ctx, rt.reader, rt.master, TicketFilter{Assignee: &assignee}, "", 50); len(p.Items) != 1 {
		t.Errorf("assignee=reader: want 1, got %d", len(p.Items))
	}
	tag := "billing"
	if p, _ := svc.ListTickets(ctx, rt.reader, rt.master, TicketFilter{Tag: &tag}, "", 50); len(p.Items) != 1 {
		t.Errorf("tag=billing: want 1, got %d", len(p.Items))
	}
}

// TestListTicketsKeysetPagination — paging through with limit yields each page
// once, in order, with a working next_cursor and a final nil cursor.
func TestListTicketsKeysetPagination(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)
	rt := seedReadTenant(ctx, t, tdb)

	const total = 5
	for i := 0; i < total; i++ {
		seedTicket(ctx, t, tdb, rt, uuid.New(), "open", "normal", fmt.Sprintf("k-%d", i), nil, nil, -time.Duration(i+1)*time.Hour)
	}

	seen := map[uuid.UUID]bool{}
	cursor := ""
	pages := 0
	var lastActivity time.Time
	first := true
	for {
		p, err := svc.ListTickets(ctx, rt.reader, rt.master, TicketFilter{}, cursor, 2)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if len(p.Items) > 2 {
			t.Fatalf("page returned %d items, exceeds limit 2", len(p.Items))
		}
		for _, tk := range p.Items {
			if seen[tk.ID] {
				t.Errorf("ticket %s returned on more than one page", tk.ID)
			}
			seen[tk.ID] = true
			if tk.LastMessageAt == nil {
				t.Fatalf("ticket %s has nil last_message_at", tk.ID)
			}
			if !first && tk.LastMessageAt.After(lastActivity) {
				t.Errorf("ordering violated: %v after %v", *tk.LastMessageAt, lastActivity)
			}
			lastActivity = *tk.LastMessageAt
			first = false
		}
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("paged through %d unique tickets, want %d", len(seen), total)
	}
}

// TestListTicketsLimitCappedAt100 — a caller passing an absurd limit is silently
// capped at 100 (never the whole table). Seed 101 and assert page <= 100.
func TestListTicketsLimitCappedAt100(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)
	rt := seedReadTenant(ctx, t, tdb)

	for i := 0; i < 101; i++ {
		seedTicket(ctx, t, tdb, rt, uuid.New(), "open", "normal", fmt.Sprintf("c-%d", i), nil, nil, -time.Duration(i+1)*time.Minute)
	}
	p, err := svc.ListTickets(ctx, rt.reader, rt.master, TicketFilter{}, "", 10000000)
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if len(p.Items) > 100 {
		t.Errorf("limit not capped: got %d items, want <= 100", len(p.Items))
	}
	if p.NextCursor == nil {
		t.Errorf("with 101 rows and a cap of 100, next_cursor must be non-nil")
	}
}

// TestGetTicketCrossTenant404NoOracle — a valid ticket id belonging to ANOTHER
// business returns a 404 byte-for-byte identical to an unknown random id.
func TestGetTicketCrossTenant404NoOracle(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)
	t1 := seedReadTenant(ctx, t, tdb)
	t2 := seedReadTenant(ctx, t, tdb)
	t2Ticket := uuid.New()
	seedTicket(ctx, t, tdb, t2, t2Ticket, "open", "normal", "t2-secret", nil, nil, -1*time.Hour)

	// In-tenant control: t2's reader can load it.
	if _, err := svc.GetTicket(ctx, t2.reader, t2.master, t2Ticket); err != nil {
		t.Fatalf("control: t2 reader should see own ticket: %v", err)
	}

	// Cross-tenant: t1's reader asking for t2's ticket id under t1's business.
	_, errCross := svc.GetTicket(ctx, t1.reader, t1.master, t2Ticket)
	// Unknown random id under t1's business.
	_, errUnknown := svc.GetTicket(ctx, t1.reader, t1.master, uuid.New())

	if !errorsIsNotFound(errCross) {
		t.Errorf("cross-tenant ticket: want ErrNotFound, got %v", errCross)
	}
	if !errorsIsNotFound(errUnknown) {
		t.Errorf("unknown ticket: want ErrNotFound, got %v", errUnknown)
	}
	if (errCross == nil) != (errUnknown == nil) || errCross.Error() != errUnknown.Error() {
		t.Errorf("oracle: cross-tenant (%v) and unknown (%v) must be identical", errCross, errUnknown)
	}
}

// TestNonMemberSeesNotFound — a principal who is NOT a member of the business (and
// thus lacks tickets.read) gets ErrNotFound from the service, identical to an
// unknown-business id. (The HTTP layer's RequirePermission also returns 404; this
// asserts the service ownership predicate independently fails closed under RLS.)
func TestNonMemberSeesNotFound(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)
	t1 := seedReadTenant(ctx, t, tdb)
	t2 := seedReadTenant(ctx, t, tdb)
	t1Ticket := uuid.New()
	seedTicket(ctx, t, tdb, t1, t1Ticket, "open", "normal", "t1-only", nil, nil, -1*time.Hour)

	// t2.reader is not a member of t1 → RLS hides t1's rows → ErrNotFound.
	if _, err := svc.GetTicket(ctx, t2.reader, t1.master, t1Ticket); !errorsIsNotFound(err) {
		t.Errorf("non-member GetTicket: want ErrNotFound, got %v", err)
	}
	// List from a non-member returns an empty page (no leak, no error).
	if p, err := svc.ListTickets(ctx, t2.reader, t1.master, TicketFilter{}, "", 50); err != nil || len(p.Items) != 0 {
		t.Errorf("non-member ListTickets: want empty page, got items=%d err=%v", len(p.Items), err)
	}
}

// TestListMessagesWithAttachmentsAndAuth — the messages endpoint returns the thread
// with attachments and the projected spf/dkim/dmarc auth-result fields.
func TestListMessagesWithAttachmentsAndAuth(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)
	rt := seedReadTenant(ctx, t, tdb)
	ticketID := uuid.New()
	msgID := seedTicket(ctx, t, tdb, rt, ticketID, "open", "normal", "with-attach", nil, nil, -1*time.Hour)

	// Attach one file to the seeded inbound message.
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO attachment (id,ticket_message_id,business_id,tenant_root_id,blob_key,filename,content_type,size,created_at)
		 VALUES ($1,$2,$3,$3,$4,'invoice.pdf','application/pdf',1234,now())`,
		uuid.New(), msgID, rt.master, "blob-"+msgID.String()); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}

	page, err := svc.ListMessages(ctx, rt.reader, rt.master, ticketID, "", 50)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("want 1 message, got %d", len(page.Items))
	}
	m := page.Items[0]
	if m.Direction != "inbound" {
		t.Errorf("direction = %q, want inbound", m.Direction)
	}
	if m.SPFResult != "pass" || m.DKIMResult != "pass" || m.DMARCResult != "pass" {
		t.Errorf("auth results not projected: spf=%q dkim=%q dmarc=%q", m.SPFResult, m.DKIMResult, m.DMARCResult)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "invoice.pdf" || m.Attachments[0].ContentType != "application/pdf" {
		t.Errorf("attachment not projected: %+v", m.Attachments)
	}

	// Cross-tenant ticket id under a foreign business → empty page (no leak).
	other := seedReadTenant(ctx, t, tdb)
	if p, err := svc.ListMessages(ctx, other.reader, other.master, ticketID, "", 50); err != nil || len(p.Items) != 0 {
		t.Errorf("cross-tenant ListMessages: want empty, got items=%d err=%v", len(p.Items), err)
	}
}

// TestGetRequesterScopingAndOracle — in-tenant get works; a requester id from
// another business → 404 identical to an unknown id; email filter narrows the list.
func TestGetRequesterScopingAndOracle(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)
	t1 := seedReadTenant(ctx, t, tdb)
	t2 := seedReadTenant(ctx, t, tdb)

	// In-tenant control.
	r, err := svc.GetRequester(ctx, t1.reader, t1.master, t1.requester)
	if err != nil {
		t.Fatalf("control GetRequester: %v", err)
	}
	if r.Email != "ada@example.com" || r.ContactID != nil {
		t.Errorf("requester projection wrong: email=%q contact_id=%v", r.Email, r.ContactID)
	}

	// Cross-tenant requester id and unknown id both 404, identical.
	_, errCross := svc.GetRequester(ctx, t1.reader, t1.master, t2.requester)
	_, errUnknown := svc.GetRequester(ctx, t1.reader, t1.master, uuid.New())
	if !errorsIsNotFound(errCross) || !errorsIsNotFound(errUnknown) {
		t.Errorf("requester oracle: cross=%v unknown=%v, both want ErrNotFound", errCross, errUnknown)
	}
	if errCross.Error() != errUnknown.Error() {
		t.Errorf("requester oracle: messages differ cross=%q unknown=%q", errCross, errUnknown)
	}

	// Email filter.
	email := "grace@example.com"
	p, err := svc.ListRequesters(ctx, t1.reader, t1.master, &email, "", 50)
	if err != nil {
		t.Fatalf("ListRequesters email filter: %v", err)
	}
	if len(p.Items) != 1 || p.Items[0].ID != t1.requester2 {
		t.Errorf("email filter: want exactly grace, got %+v", p.Items)
	}
	if got := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM requester WHERE business_id=$1", t1.master); got != 2 {
		t.Errorf("sanity: t1 should have 2 requesters, got %d", got)
	}
}

func errorsIsNotFound(err error) bool {
	return errors.Is(err, errs.ErrNotFound)
}
