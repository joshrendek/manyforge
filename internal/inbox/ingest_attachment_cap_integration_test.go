//go:build integration

package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// attachmentMetaJSON builds the p_attachments jsonb the Go service passes to
// ingest_inbound_message: n entries whose sizes are given by size(i).
func attachmentMetaJSON(t *testing.T, n int, size func(i int) int64) string {
	t.Helper()
	meta := make([]map[string]any, n)
	for i := range meta {
		meta[i] = map[string]any{
			"blob_key":     fmt.Sprintf("cap-test/%s/%d", uuid.NewString(), i),
			"filename":     fmt.Sprintf("part-%d.png", i),
			"content_type": "image/png",
			"size":         size(i),
		}
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ingestWithAttachments calls ingest_inbound_message directly (as the RLS-subject
// app role would, through its SECURITY DEFINER grant) with the given metadata.
func ingestWithAttachments(ctx context.Context, tdb *testdb.TestDB, ten ingestTenant, attachments string) (created bool, err error) {
	newTicket := uuid.New()
	err = tdb.Super.QueryRow(ctx, `
		SELECT out_created
		FROM ingest_inbound_message($1, $2, $3, 'ada@example.com', 'Ada', 'cap', $4, NULL, '{}',
			'body', NULL, '{}'::jsonb, false, NULL, $5, $6, $7::jsonb, 'inbox:test', 0)`,
		ten.master, ten.tenantRootID, ten.address, "cap-"+uuid.NewString()+"@example.com",
		newTicket, "tok-"+newTicket.String(), attachments,
	).Scan(&created)
	return
}

// TestIngestSQLAttachmentCaps (manyforge-bolc) — defence in depth for SECREVIEW-M5.
// The Go service stages at most maxAcceptedAttachments parts totalling at most
// maxAcceptedAttachmentBytes; ingest_inbound_message enforces the same caps so a
// future caller (or a Go regression) cannot fan one message out into unbounded
// attachment rows. At the caps the call succeeds; one over fails with SQLSTATE
// IN413 and stores nothing.
func TestIngestSQLAttachmentCaps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedIngestTenant(ctx, t, tdb)

	const perPart = int64(maxAcceptedAttachmentBytes / maxAcceptedAttachments)
	lastPart := func(total int64) func(int) int64 {
		// maxAcceptedAttachments parts summing to exactly total.
		return func(i int) int64 {
			if i == maxAcceptedAttachments-1 {
				return total - perPart*int64(maxAcceptedAttachments-1)
			}
			return perPart
		}
	}

	for _, tc := range []struct {
		name    string
		meta    string
		wantErr string
	}{
		{"count at cap", attachmentMetaJSON(t, maxAcceptedAttachments, func(int) int64 { return 1 }), ""},
		{"bytes at cap", attachmentMetaJSON(t, maxAcceptedAttachments, lastPart(maxAcceptedAttachmentBytes)), ""},
		{"count over cap", attachmentMetaJSON(t, maxAcceptedAttachments+1, func(int) int64 { return 1 }), "attachment count"},
		{"bytes over cap", attachmentMetaJSON(t, maxAcceptedAttachments, lastPart(maxAcceptedAttachmentBytes+1)), "attachment bytes"},
		{"bytes over cap with few parts", attachmentMetaJSON(t, 1, func(int) int64 { return maxAcceptedAttachmentBytes + 1 }), "attachment bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM attachment WHERE tenant_root_id=$1", ten.tenantRootID)
			beforeTickets := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE tenant_root_id=$1", ten.tenantRootID)
			created, err := ingestWithAttachments(ctx, tdb, ten, tc.meta)
			after := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM attachment WHERE tenant_root_id=$1", ten.tenantRootID)
			afterTickets := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE tenant_root_id=$1", ten.tenantRootID)
			if tc.wantErr == "" {
				if err != nil || !created {
					t.Fatalf("ingest at cap: created=%v err=%v, want success", created, err)
				}
				var parts []any
				if err := json.Unmarshal([]byte(tc.meta), &parts); err != nil {
					t.Fatal(err)
				}
				if after-before != len(parts) {
					t.Errorf("stored %d attachments, want %d", after-before, len(parts))
				}
				return
			}
			var pgErr *pgconn.PgError
			if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "IN413" || !strings.Contains(pgErr.Message, tc.wantErr) {
				t.Fatalf("ingest over cap: err=%v, want SQLSTATE IN413 mentioning %q", err, tc.wantErr)
			}
			if after != before || afterTickets != beforeTickets {
				t.Errorf("rejected ingest stored rows: attachments %d->%d tickets %d->%d", before, after, beforeTickets, afterTickets)
			}
		})
	}
}
