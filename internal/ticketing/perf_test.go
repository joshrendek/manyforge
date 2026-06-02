//go:build integration

package ticketing

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// SC-010 performance gate (T069).
//
// "At 10,000 tickets per business and realistic thread depth, ticket-list and
// ticket-load operations complete within a p95 latency target of 200 ms,
// enforced by automated performance tests" — with RLS ENABLED.
//
// We seed the measured business with 10,000 tickets at realistic thread depth
// (3–8 messages each, alternating inbound/outbound, attachments on ~1/4 of
// messages) AND a co-resident neighbour business with its own tickets, so the
// RLS predicate (`business_id IN authorized_businesses(...)`) and the supporting
// indexes must actually EXCLUDE another tenant's rows rather than scanning a
// single-tenant table. All measurement goes through the *Service read methods on
// the RLS-bound App pool — the same path the HTTP handlers use.
//
// "ticket-load" is measured as the realistic composite a UI performs when opening
// a ticket: GetTicket (header + counts) followed by ListMessages (the thread with
// its batched attachments). Both share the 200 ms budget; component p95s are
// logged so a regression points at the culprit.
const (
	sc010PrimaryTickets  = 10_000              // FR/SC-010: 10k tickets in the measured business
	sc010NeighborTickets = 5_000               // a second tenant so RLS must discriminate
	sc010TargetP95       = 200 * time.Millisecond
	sc010Samples         = 200                 // p95 over 200 samples tolerates 10 outliers
	sc010Warmup          = 20                  // discarded — warms pool + plan cache
)

// TestSC010 asserts ticket-list and ticket-load p95 stay under 200 ms at scale,
// through RLS. It is the regression gate that the read paths remain O(1) in query
// count (the manyforge-iiq list-fold + the batched loadAttachments) as data grows.
func TestSC010(t *testing.T) {
	ctx, tdb := startReadDB(t)
	svc := newReadService(tdb)

	primary := seedReadTenant(ctx, t, tdb)
	neighbor := seedReadTenant(ctx, t, tdb)
	seedPerfTickets(ctx, t, tdb, primary, sc010PrimaryTickets)
	seedPerfTickets(ctx, t, tdb, neighbor, sc010NeighborTickets)

	// Sanity: confirm the dataset is the expected size and that RLS is genuinely
	// live (the reader sees its own business's tickets, scoped). If this regressed
	// to "RLS off / wrong scope" the latency numbers below would be meaningless.
	if got := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", primary.master); got != sc010PrimaryTickets {
		t.Fatalf("seed sanity: want %d primary tickets, got %d", sc010PrimaryTickets, got)
	}
	first, err := svc.ListTickets(ctx, primary.reader, primary.master, TicketFilter{}, "", 50)
	if err != nil {
		t.Fatalf("sanity ListTickets: %v", err)
	}
	if len(first.Items) == 0 {
		t.Fatal("sanity: reader sees no tickets — RLS scope wrong, perf numbers would be meaningless")
	}
	for _, tk := range first.Items {
		if tk.BusinessID != primary.master {
			t.Fatalf("sanity: ticket %s leaked from business %s — RLS not scoping", tk.ID, tk.BusinessID)
		}
	}

	// A spread of random ticket ids across the whole 10k (md5 ordering), so loads
	// exercise index seeks throughout the table, not just the hot recent rows.
	loadIDs := pickTicketIDs(ctx, t, tdb, primary.master, 300)
	// A spread of list cursors (first page + deep keyset pages), so the list
	// measurement covers shallow AND deep pagination — keyset's whole point.
	listCursors := collectListCursors(ctx, t, svc, primary, 60)

	// --- warm up (discarded) ---
	for i := 0; i < sc010Warmup; i++ {
		if _, err := svc.ListTickets(ctx, primary.reader, primary.master, TicketFilter{}, listCursors[i%len(listCursors)], 50); err != nil {
			t.Fatalf("warmup list: %v", err)
		}
		id := loadIDs[i%len(loadIDs)]
		if _, err := svc.GetTicket(ctx, primary.reader, primary.master, id); err != nil {
			t.Fatalf("warmup get: %v", err)
		}
		if _, err := svc.ListMessages(ctx, primary.reader, primary.master, id, "", 100); err != nil {
			t.Fatalf("warmup messages: %v", err)
		}
	}

	// --- ticket-list ---
	listDur := make([]time.Duration, 0, sc010Samples)
	for i := 0; i < sc010Samples; i++ {
		cur := listCursors[i%len(listCursors)]
		start := time.Now()
		if _, err := svc.ListTickets(ctx, primary.reader, primary.master, TicketFilter{}, cur, 50); err != nil {
			t.Fatalf("list iteration %d: %v", i, err)
		}
		listDur = append(listDur, time.Since(start))
	}

	// --- ticket-load (GetTicket + thread) ---
	loadDur := make([]time.Duration, 0, sc010Samples)
	getDur := make([]time.Duration, 0, sc010Samples)
	threadDur := make([]time.Duration, 0, sc010Samples)
	for i := 0; i < sc010Samples; i++ {
		id := loadIDs[i%len(loadIDs)]
		start := time.Now()

		g0 := time.Now()
		if _, err := svc.GetTicket(ctx, primary.reader, primary.master, id); err != nil {
			t.Fatalf("get iteration %d (%s): %v", i, id, err)
		}
		getDur = append(getDur, time.Since(g0))

		t0 := time.Now()
		if _, err := svc.ListMessages(ctx, primary.reader, primary.master, id, "", 100); err != nil {
			t.Fatalf("messages iteration %d (%s): %v", i, id, err)
		}
		threadDur = append(threadDur, time.Since(t0))

		loadDur = append(loadDur, time.Since(start))
	}

	listP95, loadP95 := p95(listDur), p95(loadDur)
	t.Logf("SC-010 @ %d primary / %d neighbor tickets, RLS ON: "+
		"list[p50=%v p95=%v max=%v] load[p50=%v p95=%v max=%v] (get p95=%v, thread p95=%v)",
		sc010PrimaryTickets, sc010NeighborTickets,
		p50(listDur), listP95, maxDur(listDur),
		p50(loadDur), loadP95, maxDur(loadDur),
		p95(getDur), p95(threadDur))

	if listP95 > sc010TargetP95 {
		t.Errorf("ticket-list p95 %v exceeds SC-010 target %v", listP95, sc010TargetP95)
	}
	if loadP95 > sc010TargetP95 {
		t.Errorf("ticket-load p95 %v exceeds SC-010 target %v", loadP95, sc010TargetP95)
	}
}

// seedPerfTickets bulk-loads n tickets for rt.master entirely server-side
// (INSERT … SELECT … generate_series) via the RLS-exempt Super pool, so seeding
// 10k+ tickets and their threads costs a handful of statements, not 10k round
// trips. Each ticket gets a deterministic 3–8 message thread (depth derived from
// a hash of its id), alternating inbound/outbound to satisfy the direction CHECK,
// with an attachment on ~1/4 of messages so loadAttachments is exercised.
func seedPerfTickets(ctx context.Context, t *testing.T, tdb *testdb.TestDB, rt readTenant, n int) {
	t.Helper()

	// 1) Tickets — varied status/priority, last_message_at spread over time so the
	//    keyset ordering (last_message_at DESC, id DESC) is meaningful.
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO ticket
			(id, business_id, tenant_root_id, requester_id, subject, status, priority, reply_token, last_message_at, created_at, updated_at)
		SELECT gen_random_uuid(), $1, $1, $2,
		       'subject ' || g,
		       (ARRAY['new','open','pending','solved','closed']::ticket_status[])[1 + (g % 5)::int],
		       (ARRAY['low','normal','high','urgent']::ticket_priority[])[1 + (g % 4)::int],
		       'tok-' || gen_random_uuid(),
		       now() - (g * interval '1 minute'),
		       now(), now()
		FROM generate_series(1, $3) g`,
		rt.master, rt.requester, n); err != nil {
		t.Fatalf("seed perf tickets: %v", err)
	}

	// 2) Thread — 3..8 messages per ticket. First byte of md5(id) gives a stable
	//    per-ticket depth. Odd n = inbound (author NULL), even n = outbound (author
	//    set) so the (direction, author_principal_id) CHECK holds.
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO ticket_message
			(id, ticket_id, business_id, tenant_root_id, direction, author_principal_id, message_id, "references", body_text, auth_results, is_auto_reply, created_at)
		SELECT gen_random_uuid(), t.id, t.business_id, t.tenant_root_id,
		       (CASE WHEN s.n % 2 = 1 THEN 'inbound' ELSE 'outbound' END)::ticket_message_direction,
		       (CASE WHEN s.n % 2 = 1 THEN NULL ELSE $2::uuid END),
		       'm-' || t.id || '-' || s.n || '@x.test', '{}', 'body ' || s.n,
		       '{"spf":"pass","dkim":"pass","dmarc":"pass"}'::jsonb, false,
		       t.last_message_at - ((8 - s.n) * interval '1 minute')
		FROM ticket t
		CROSS JOIN LATERAL generate_series(1, 3 + (get_byte(decode(md5(t.id::text), 'hex'), 0) % 6)) AS s(n)
		WHERE t.business_id = $1`,
		rt.master, rt.reader); err != nil {
		t.Fatalf("seed perf messages: %v", err)
	}

	// 3) Attachments on ~1/4 of messages — gives loadAttachments real fan-in.
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO attachment
			(id, ticket_message_id, business_id, tenant_root_id, blob_key, filename, content_type, size, created_at)
		SELECT gen_random_uuid(), m.id, m.business_id, m.tenant_root_id,
		       'blob-' || m.id, 'invoice.pdf', 'application/pdf', 2048, now()
		FROM ticket_message m
		WHERE m.business_id = $1
		  AND (get_byte(decode(md5(m.id::text), 'hex'), 0) % 4) = 0`,
		rt.master); err != nil {
		t.Fatalf("seed perf attachments: %v", err)
	}
}

// pickTicketIDs returns up to limit ticket ids for a business spread across the
// whole table (md5 ordering ≈ random), via the RLS-exempt Super pool.
func pickTicketIDs(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID, limit int) []uuid.UUID {
	t.Helper()
	rows, err := tdb.Super.Query(ctx,
		`SELECT id FROM ticket WHERE business_id=$1 ORDER BY md5(id::text) LIMIT $2`, businessID, limit)
	if err != nil {
		t.Fatalf("pick ticket ids: %v", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan ticket id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pick ticket ids rows: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("pick ticket ids: none found")
	}
	return ids
}

// collectListCursors pages through the reader's ticket list, returning the cursor
// for each page (starting with "" for the first page) up to maxPages, so the list
// measurement exercises shallow and deep keyset pagination.
func collectListCursors(ctx context.Context, t *testing.T, svc *Service, rt readTenant, maxPages int) []string {
	t.Helper()
	cursors := []string{""}
	cur := ""
	for i := 0; i < maxPages; i++ {
		p, err := svc.ListTickets(ctx, rt.reader, rt.master, TicketFilter{}, cur, 50)
		if err != nil {
			t.Fatalf("collect cursors page %d: %v", i, err)
		}
		if p.NextCursor == nil {
			break
		}
		cur = *p.NextCursor
		cursors = append(cursors, cur)
	}
	return cursors
}

func p95(ds []time.Duration) time.Duration { return percentile(ds, 0.95) }
func p50(ds []time.Duration) time.Duration { return percentile(ds, 0.50) }

func percentile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)) * q)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func maxDur(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m
}
