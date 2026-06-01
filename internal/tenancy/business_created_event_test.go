//go:build integration

package tenancy_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// seedVerifiedFounder seeds a verified account + human principal (no business yet)
// so CreateMasterBusiness can run as that principal. Returns the principal id.
func seedVerifiedFounder(ctx context.Context, t *testing.T, tdb *testdb.TestDB, email string) uuid.UUID {
	t.Helper()
	account := uuid.New()
	principal := uuid.New()
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'F','active',now(),now(),now())`, []any{account, email}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{principal, account}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return principal
}

// TestCreateMasterBusinessEnqueuesBusinessCreated (T030) — creating a master
// business enqueues exactly one business.created outbox event, in the same tx, with
// a payload carrying the business_id + tenant_root_id the inbox provisioner needs.
// This is the decoupling seam: tenancy emits the event (it does NOT import inbox).
func TestCreateMasterBusinessEnqueuesBusinessCreated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	p := seedVerifiedFounder(ctx, t, tdb, "emit@x.test")
	svc := &tenancy.Service{DB: tdb.App}

	biz, err := svc.CreateMasterBusiness(ctx, p, "EmitCo")
	if err != nil {
		t.Fatalf("create master: %v", err)
	}

	var (
		topic   string
		payload []byte
		count   int
	)
	if err := tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic=$2",
		biz.TenantRootID, events.TopicBusinessCreated).Scan(&count); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if count != 1 {
		t.Fatalf("business.created outbox count = %d, want 1", count)
	}
	if err := tdb.Super.QueryRow(ctx,
		"SELECT topic, payload FROM outbox WHERE tenant_root_id=$1 AND topic=$2",
		biz.TenantRootID, events.TopicBusinessCreated).Scan(&topic, &payload); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}

	var got struct {
		BusinessID   uuid.UUID `json:"business_id"`
		TenantRootID uuid.UUID `json:"tenant_root_id"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal payload %q: %v", payload, err)
	}
	if got.BusinessID != biz.ID {
		t.Errorf("payload business_id = %s, want %s", got.BusinessID, biz.ID)
	}
	if got.TenantRootID != biz.TenantRootID {
		t.Errorf("payload tenant_root_id = %s, want %s", got.TenantRootID, biz.TenantRootID)
	}
}

// TestCreateSubBusinessEnqueuesBusinessCreated (T030) — nesting a sub-business also
// emits business.created so every business (not just masters) gets a zero-config
// system inbound address.
func TestCreateSubBusinessEnqueuesBusinessCreated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	p, master := seedFounder(ctx, t, tdb, "subemit@x.test")
	svc := &tenancy.Service{DB: tdb.App}

	sub, err := svc.CreateSubBusiness(ctx, p, master, "SubEmitCo")
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}

	var count int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic=$2 AND payload->>'business_id'=$3",
		sub.TenantRootID, events.TopicBusinessCreated, sub.ID.String()).Scan(&count); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if count != 1 {
		t.Errorf("business.created outbox count for sub-business = %d, want 1", count)
	}
}
