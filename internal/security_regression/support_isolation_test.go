//go:build integration

package security_regression

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// seedSupportRowsForTenant inserts one row into every RLS-protected support table
// (the seven new in spec 002: requester, ticket, ticket_message, ticket_tag,
// attachment, email_domain, inbound_address) for a master tenant whose business id
// == tenant_root_id. All inserts go through the RLS-exempt Super pool so the rows
// exist regardless of RLS — the matrix below then proves no foreign principal can
// see or mutate them.
func seedSupportRowsForTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB, master uuid.UUID) {
	t.Helper()
	requesterID := uuid.New()
	ticketID := uuid.New()
	msgID := uuid.New()
	emailDomainID := uuid.New()

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin support seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO requester (id,business_id,tenant_root_id,email,display_name,first_seen_at,last_seen_at,created_at,updated_at)
		  VALUES ($1,$2,$2,$3,'Iso Tester',now(),now(),now(),now())`,
			[]any{requesterID, master, "iso-" + master.String()[:8] + "@x.test"}},
		{`INSERT INTO ticket (id,business_id,tenant_root_id,requester_id,subject,reply_token,last_message_at,created_at,updated_at)
		  VALUES ($1,$2,$2,$3,'iso subject',$4,now(),now(),now())`,
			[]any{ticketID, master, requesterID, "isotok-" + ticketID.String()}},
		{`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,message_id,"references",body_text,is_auto_reply,created_at)
		  VALUES ($1,$2,$3,$3,'inbound',$4,'{}','iso body',false,now())`,
			[]any{msgID, ticketID, master, "isomsg-" + msgID.String() + "@x.test"}},
		{`INSERT INTO ticket_tag (ticket_id,tag,business_id,tenant_root_id,created_at) VALUES ($1,'iso',$2,$2,now())`,
			[]any{ticketID, master}},
		{`INSERT INTO attachment (id,ticket_message_id,business_id,tenant_root_id,blob_key,filename,content_type,size,created_at)
		  VALUES ($1,$2,$3,$3,$4,'iso.png','image/png',7,now())`,
			[]any{uuid.New(), msgID, master, "isoblob-" + msgID.String()}},
		{`INSERT INTO email_domain (id,business_id,tenant_root_id,domain,mode,verify_token,created_at,updated_at)
		  VALUES ($1,$2,$2,$3,'forward_in','isovt',now(),now())`,
			[]any{emailDomainID, master, "iso-" + master.String()[:8] + ".test"}},
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		  VALUES ($1,$2,$2,$3,'system',NULL,now(),now())`,
			[]any{uuid.New(), master, "iso-" + master.String()[:8] + "@iso.test"}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("support seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit support seed: %v", err)
	}
}

// TestSupportTablesRLSMatrix (T062) is the spec-002 analogue of TestRLSMatrix: it
// sweeps all SEVEN new support tables against the principal contexts that must see
// nothing — an absent (nil) principal (fail-closed), a foreign tenant's Owner
// (sideways / cross-root), and an unknown principal — and asserts both halves of
// isolation at the DB boundary: reads return zero cross-tenant rows, and a foreign
// Owner's UPDATE/DELETE on cross-tenant rows affects zero rows (SC-004/SC-006).
// Runs as the real, non-bypass manyforge_app role; T067 covers the same boundary at
// the service/HTTP layer (uniform 404).
func TestSupportTablesRLSMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	t1 := seedEscalationTenant(ctx, t, tdb)
	t2 := seedEscalationTenant(ctx, t, tdb)
	seedSupportRowsForTenant(ctx, t, tdb, t2.master)

	// Tenant-2-owned row count per support table (keyed by tenant_root_id == master).
	tables := []struct{ name, query string }{
		{"requester", "SELECT count(*) FROM requester WHERE tenant_root_id=$1"},
		{"ticket", "SELECT count(*) FROM ticket WHERE tenant_root_id=$1"},
		{"ticket_message", "SELECT count(*) FROM ticket_message WHERE tenant_root_id=$1"},
		{"ticket_tag", "SELECT count(*) FROM ticket_tag WHERE tenant_root_id=$1"},
		{"attachment", "SELECT count(*) FROM attachment WHERE tenant_root_id=$1"},
		{"email_domain", "SELECT count(*) FROM email_domain WHERE tenant_root_id=$1"},
		{"inbound_address", "SELECT count(*) FROM inbound_address WHERE tenant_root_id=$1"},
	}

	// Sanity: the superuser DOES see t2's rows — otherwise a 0 below would be vacuous
	// rather than proof RLS hid the rows.
	for _, tb := range tables {
		var n int
		if err := tdb.Super.QueryRow(ctx, tb.query, t2.master).Scan(&n); err != nil {
			t.Fatalf("super count %s: %v", tb.name, err)
		}
		if n == 0 {
			t.Fatalf("seed gap: superuser sees 0 %s rows for t2 (assertion would be vacuous)", tb.name)
		}
	}

	// READ isolation: no non-authorized context sees any of t2's rows. Covers
	// absent/unknown/sideways principals; a malformed principal is the nil case
	// (WithPrincipal sets an empty GUC, so RLS fails closed identically).
	viewers := []struct {
		name string
		pid  uuid.UUID
	}{
		{"absent (nil principal)", uuid.Nil},
		{"foreign tenant owner (cross-root)", t1.owner},
		{"unknown principal", uuid.New()},
	}
	for _, v := range viewers {
		for _, tb := range tables {
			var n int
			if err := tdb.App.WithPrincipal(ctx, v.pid, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, tb.query, t2.master).Scan(&n)
			}); err != nil {
				t.Fatalf("%s reading %s: %v", v.name, tb.name, err)
			}
			if n != 0 {
				t.Errorf("RLS read breach: %q sees %d %s row(s) of tenant 2", v.name, n, tb.name)
			}
		}
	}

	// WRITE isolation: a foreign Owner's UPDATE/DELETE on t2 rows affects nothing
	// (the USING predicate filters the rows out before the write applies).
	writes := []struct{ name, sql string }{
		{"update t2 ticket", "UPDATE ticket SET subject='pwned' WHERE tenant_root_id=$1"},
		{"delete t2 ticket_tag", "DELETE FROM ticket_tag WHERE tenant_root_id=$1"},
		{"update t2 requester", "UPDATE requester SET display_name='pwned' WHERE tenant_root_id=$1"},
		{"update t2 inbound_address", "UPDATE inbound_address SET address='pwned@x.test' WHERE tenant_root_id=$1"},
	}
	for _, wr := range writes {
		if err := tdb.App.WithPrincipal(ctx, t1.owner, func(tx pgx.Tx) error {
			tag, eerr := tx.Exec(ctx, wr.sql, t2.master)
			if eerr != nil {
				return eerr
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("RLS write breach: foreign owner %q affected %d row(s)", wr.name, tag.RowsAffected())
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", wr.name, err)
		}
	}

	// Ground truth: t2's ticket survived every foreign write attempt (subject intact).
	var subject string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT subject FROM ticket WHERE tenant_root_id=$1", t2.master).Scan(&subject); err != nil {
		t.Fatalf("super read t2 ticket: %v", err)
	}
	if subject != "iso subject" {
		t.Errorf("t2 ticket subject = %q, want intact 'iso subject' (a foreign write got through)", subject)
	}
}
