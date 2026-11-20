//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/ticketing"
)

func TestHistoricalBatchResumeAndReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	account, actor, business := uuid.New(), uuid.New(), uuid.New()
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO account(id,email,display_name,status,email_verified_at) VALUES($1,'fixture-owner@example.com','Example owner','active',now())`, []any{account}},
		{`INSERT INTO principal(id,kind,account_id) VALUES($1,'human',$2)`, []any{actor, account}},
		{`INSERT INTO business(id,tenant_root_id,name) VALUES($1,$1,'Example business')`, []any{business}},
		{`INSERT INTO business_closure(ancestor_id,descendant_id,depth,tenant_root_id) VALUES($1,$1,0,$1)`, []any{business}},
		{`INSERT INTO membership(principal_id,business_id,tenant_root_id,role_id)
		   SELECT $1,$2,$2,id FROM role WHERE key='owner' AND tenant_root_id IS NULL`, []any{actor, business}},
	} {
		if _, err := tdb.Super.Exec(ctx, stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	const replyKey = "synthetic-reply-token-32-byte-key"
	storeURL := "file://" + t.TempDir()
	t.Setenv("MANYFORGE_ENVIRONMENT", "development")
	t.Setenv("MANYFORGE_DATABASE_URL", tdb.AppDSN)
	t.Setenv("MANYFORGE_BLOB_URL", storeURL)
	t.Setenv("MANYFORGE_INBOUND_REPLY_TOKEN_SECRET", replyKey)
	t.Setenv("MANYFORGE_INBOUND_SYSTEM_DOMAIN", "example.com")

	first, second := historicalBatchFixture("example-1"), historicalBatchFixture("example-2")
	dir, digest := writeHistoricalFixtureBundle(t, first, second)
	svc, closeService, err := openHistoricalImportService(ctx, config.Config{
		DatabaseURL: tdb.AppDSN, BlobURL: storeURL, InboundReplyTokenSecret: []byte(replyKey), InboundSystemDomain: "example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeService()
	if _, err := svc.ImportHistoricalTicket(ctx, actor, business, first); err != nil {
		t.Fatal(err)
	}

	args := []string{"--apply", "--expected-sha256", digest, actor.String(), business.String(), dir}
	run := func() (historicalBatchReport, error) {
		t.Helper()
		var out bytes.Buffer
		err := importSupportTickets(ctx, args, &out)
		var report historicalBatchReport
		if decodeErr := json.Unmarshal(out.Bytes(), &report); decodeErr != nil {
			t.Fatalf("apply report unavailable: %v (apply error %v)", decodeErr, err)
		}
		return report, err
	}
	report, err := run()
	if err != nil || !report.Complete || report.Created != 1 || report.Replayed != 1 ||
		report.Reconciled == nil || *report.Reconciled != report.Expected {
		t.Fatalf("resume: %+v, %v", report, err)
	}
	report, err = run()
	if err != nil || !report.Complete || report.Created != 0 || report.Replayed != 2 {
		t.Fatalf("replay: %+v, %v", report, err)
	}
	var key string
	if err := tdb.Super.QueryRow(ctx, `
		SELECT a.blob_key FROM attachment a JOIN ticket_message m ON m.id=a.ticket_message_id
		JOIN ticket t ON t.id=m.ticket_id WHERE t.business_id=$1 AND t.historical_id='example-2'`, business).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if err := svc.Blob.Put(ctx, key, []byte("fake"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	report, err = run()
	if !errors.Is(err, ticketing.ErrHistoricalReconciliation) || report.Complete || report.FailedSnapshot != 2 ||
		report.Reconciled == nil || report.Reconciled.Tickets != 1 {
		t.Fatalf("corrupt object: %+v, %v", report, err)
	}
	var tickets, events int
	if err := tdb.Super.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM ticket WHERE business_id=$1),
		       (SELECT count(*) FROM outbox WHERE tenant_root_id=$1)`, business).Scan(&tickets, &events); err != nil {
		t.Fatal(err)
	}
	if tickets != 2 || events != 0 {
		t.Fatalf("want two tickets and zero live events, got %d tickets and %d events", tickets, events)
	}
}
