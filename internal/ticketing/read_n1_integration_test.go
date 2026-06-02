//go:build integration

package ticketing

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	appdb "github.com/manyforge/manyforge/internal/platform/db"
)

// queryCounter is a pgx.QueryTracer that records every Query/Exec issued on the
// pool — the instrument for proving the ticket-list read does not scale its query
// count with the number of rows on the page (the assembleTicket N+1, manyforge-iiq).
type queryCounter struct {
	mu   sync.Mutex
	sqls []string
}

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	c.sqls = append(c.sqls, d.SQL)
	c.mu.Unlock()
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *queryCounter) reset() {
	c.mu.Lock()
	c.sqls = nil
	c.mu.Unlock()
}

// ticketQueries counts only the round-trips that touch a ticket table, ignoring
// transaction control and the principal-GUC plumbing (begin/commit/set_config/
// current_setting). These are exactly the data queries the N+1 multiplied per row,
// so their count is page-size-independent only once the fan-out is folded away.
func (c *queryCounter) ticketQueries() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.sqls {
		if strings.Contains(strings.ToLower(s), "ticket") {
			n++
		}
	}
	return n
}

// TestListTicketsNoPerRowQueries pins the N+1 fix: a ticket-list page must issue
// exactly ONE ticket-table query regardless of page size. Pre-fix, assembleTicket
// added three per-row sub-queries (requester + tags + message_count), so a page of
// N rows ran 1+3N ticket-table queries; folding them into the list query makes it 1.
func TestListTicketsNoPerRowQueries(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	for i := 0; i < 6; i++ {
		seedTicket(ctx, t, tdb, rt, uuid.New(), "open", "normal",
			"subj", nil, []string{"a", "b"}, time.Duration(-i)*time.Hour)
	}

	counter := &queryCounter{}
	traced, err := appdb.Open(ctx, tdb.AppDSN, appdb.WithTracer(counter))
	if err != nil {
		t.Fatalf("open traced db: %v", err)
	}
	t.Cleanup(traced.Close)
	svc := &Service{DB: traced}

	dataQueries := func(limit int) int {
		counter.reset()
		page, perr := svc.ListTickets(ctx, rt.reader, rt.master, TicketFilter{}, "", limit)
		if perr != nil {
			t.Fatalf("list (limit=%d): %v", limit, perr)
		}
		if len(page.Items) != limit {
			t.Fatalf("limit=%d: got %d items, want %d", limit, len(page.Items), limit)
		}
		return counter.ticketQueries()
	}

	// Two very different page sizes must both issue exactly one ticket-table query.
	for _, limit := range []int{2, 6} {
		if n := dataQueries(limit); n != 1 {
			t.Errorf("limit=%d: %d ticket-table queries; want exactly 1 (requester/tags/message_count folded into the list query — no per-row N+1)", limit, n)
		}
	}
}

// TestListTicketsEnrichment verifies the folded list query returns the same
// requester / tags / message_count the per-row sub-queries did: tags are sorted and
// non-null-empty, message_count reflects all messages, and the requester is joined.
func TestListTicketsEnrichment(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)

	tagged := uuid.New()   // unsorted tags + extra messages
	untagged := uuid.New() // no tags → must come back as [] not nil
	seedTicket(ctx, t, tdb, rt, tagged, "open", "normal", "tagged", nil, []string{"zeta", "alpha", "mike"}, -1*time.Hour)
	seedTicket(ctx, t, tdb, rt, untagged, "open", "normal", "untagged", nil, nil, -2*time.Hour)

	// seedTicket inserts one inbound message; add two more so message_count = 3.
	for i := 0; i < 2; i++ {
		mid := uuid.New()
		if _, err := tdb.Super.Exec(ctx,
			`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,message_id,"references",body_text,is_auto_reply,created_at)
			 VALUES ($1,$2,$3,$3,'inbound',$4,'{}',$5,false,now())`,
			mid, tagged, rt.master, "extra-"+mid.String()+"@x.test", "more"); err != nil {
			t.Fatalf("add extra message: %v", err)
		}
	}

	svc := newReadService(tdb)
	page, err := svc.ListTickets(ctx, rt.reader, rt.master, TicketFilter{}, "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := make(map[uuid.UUID]Ticket, len(page.Items))
	for _, tk := range page.Items {
		byID[tk.ID] = tk
	}

	a, ok := byID[tagged]
	if !ok {
		t.Fatalf("tagged ticket missing from list")
	}
	if got := strings.Join(a.Tags, ","); got != "alpha,mike,zeta" {
		t.Errorf("tags = %q, want sorted \"alpha,mike,zeta\"", got)
	}
	if a.MessageCount != 3 {
		t.Errorf("message_count = %d, want 3 (1 seeded + 2 added)", a.MessageCount)
	}
	if a.Requester.Email != "ada@example.com" {
		t.Errorf("requester email = %q, want joined \"ada@example.com\"", a.Requester.Email)
	}

	b, ok := byID[untagged]
	if !ok {
		t.Fatalf("untagged ticket missing from list")
	}
	if b.Tags == nil || len(b.Tags) != 0 {
		t.Errorf("untagged tags = %#v, want non-nil empty slice", b.Tags)
	}
	if b.MessageCount != 1 {
		t.Errorf("untagged message_count = %d, want 1", b.MessageCount)
	}
}
