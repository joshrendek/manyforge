//go:build integration

package account_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// makeAccount signs up + verifies an account and returns its principal id and a
// live token pair. Tokens are minted at wall-clock time, so callers that pin a
// fixed clock must do so AFTER this returns.
func makeAccount(ctx context.Context, t *testing.T, acct *account.Service, ring *auth.KeyRing, email string) (uuid.UUID, account.TokenPair) {
	t.Helper()
	if _, vtok, err := acct.Signup(ctx, email, "Name", "supersecretpassword"); err != nil {
		t.Fatalf("signup %s: %v", email, err)
	} else if err := acct.VerifyEmail(ctx, vtok); err != nil {
		t.Fatalf("verify %s: %v", email, err)
	}
	tp, err := acct.Login(ctx, email, "supersecretpassword")
	if err != nil {
		t.Fatalf("login %s: %v", email, err)
	}
	pid, err := ring.Parse(tp.Access)
	if err != nil {
		t.Fatalf("parse access %s: %v", email, err)
	}
	return pid, tp
}

// seedOwnerAt creates a verified account + human principal and grants it the
// locked Owner role at the tenant root, so the founder is no longer its sole Owner.
func seedOwnerAt(ctx context.Context, t *testing.T, tdb *testdb.TestDB, root uuid.UUID, email string) {
	t.Helper()
	acctID, prin := uuid.New(), uuid.New()
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("owner role: %v", err)
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Co','active',now(),now(),now())`, []any{acctID, email}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{prin, acctID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`, []any{prin, root, ownerRole}},
	}
	for _, s := range stmts {
		if _, err := tdb.Super.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed owner: %v", err)
		}
	}
}

func acctAuditCount(ctx context.Context, t *testing.T, tdb *testdb.TestDB, action string, target uuid.UUID) int {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM audit_entry WHERE action=$1 AND target_id=$2", action, target).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

func accountStatus(ctx context.Context, t *testing.T, tdb *testdb.TestDB, email string) (status string, deleted bool) {
	t.Helper()
	var deletedAt *time.Time
	if err := tdb.Super.QueryRow(ctx, "SELECT status, deleted_at FROM account WHERE email=$1", email).Scan(&status, &deletedAt); err != nil {
		t.Fatalf("account status: %v", err)
	}
	return status, deletedAt != nil
}

func TestAccountDeactivate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	acct, ten, ring := newServices(t, tdb)

	founder, _ := makeAccount(ctx, t, acct, ring, "deact-founder@x.test")
	biz, err := ten.CreateMasterBusiness(ctx, founder, "Acme")
	if err != nil {
		t.Fatalf("create master: %v", err)
	}

	t.Run("the last Owner of a tenant cannot deactivate", func(t *testing.T) {
		if err := acct.Deactivate(ctx, founder); !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("last-owner deactivate: want ErrConflict, got %v", err)
		}
		if status, _ := accountStatus(ctx, t, tdb, "deact-founder@x.test"); status != "active" {
			t.Errorf("account must stay active after a refused deactivation, got %q", status)
		}
	})

	t.Run("deactivation succeeds once a second Owner exists; login is then denied and it is audited", func(t *testing.T) {
		seedOwnerAt(ctx, t, tdb, biz.ID, "deact-co@x.test")
		if err := acct.Deactivate(ctx, founder); err != nil {
			t.Fatalf("deactivate with a second owner: %v", err)
		}
		if status, _ := accountStatus(ctx, t, tdb, "deact-founder@x.test"); status != "deactivated" {
			t.Errorf("status: want deactivated, got %q", status)
		}
		if _, err := acct.Login(ctx, "deact-founder@x.test", "supersecretpassword"); !errors.Is(err, account.ErrInvalidCredentials) {
			t.Errorf("deactivated login: want ErrInvalidCredentials, got %v", err)
		}
		if n := acctAuditCount(ctx, t, tdb, "account.deactivated", founder); n != 1 {
			t.Errorf("want 1 account.deactivated audit entry, got %d", n)
		}
	})
}

func TestAccountDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	acct, ten, ring := newServices(t, tdb)

	t.Run("a non-owner deletes: soft-deleted, sessions revoked, erasure scheduled, audited", func(t *testing.T) {
		pid, tp := makeAccount(ctx, t, acct, ring, "del-user@x.test")

		// Pin the clock AFTER minting the live session so the 30-day window is exact.
		fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		acct.Now = func() time.Time { return fixed }
		defer func() { acct.Now = nil }()

		purgeAfter, err := acct.Delete(ctx, pid)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if want := fixed.Add(30 * 24 * time.Hour); !purgeAfter.Equal(want) {
			t.Errorf("purge_after: want %s (now+30d), got %s", want, purgeAfter)
		}
		if _, deleted := accountStatus(ctx, t, tdb, "del-user@x.test"); !deleted {
			t.Error("account must be soft-deleted (deleted_at set)")
		}
		if _, err := acct.Login(ctx, "del-user@x.test", "supersecretpassword"); !errors.Is(err, account.ErrInvalidCredentials) {
			t.Errorf("deleted-account login: want ErrInvalidCredentials, got %v", err)
		}
		if _, err := acct.Refresh(ctx, tp.Refresh); err == nil {
			t.Error("the deleted account's session must be revoked")
		}
		if n := acctAuditCount(ctx, t, tdb, "account.deletion_scheduled", pid); n != 1 {
			t.Errorf("want 1 account.deletion_scheduled audit entry, got %d", n)
		}
		var scheduled time.Time
		if err := tdb.Super.QueryRow(ctx,
			"SELECT ae.purge_after FROM account_erasure ae JOIN account a ON a.id=ae.account_id WHERE a.email=$1", "del-user@x.test").Scan(&scheduled); err != nil {
			t.Fatalf("read erasure schedule: %v", err)
		}
		if want := fixed.Add(30 * 24 * time.Hour); !scheduled.Equal(want) {
			t.Errorf("scheduled purge_after: want %s, got %s", want, scheduled)
		}
	})

	t.Run("the last Owner of a tenant cannot delete", func(t *testing.T) {
		founder, _ := makeAccount(ctx, t, acct, ring, "del-founder@x.test")
		if _, err := ten.CreateMasterBusiness(ctx, founder, "Acme2"); err != nil {
			t.Fatalf("create master: %v", err)
		}
		if _, err := acct.Delete(ctx, founder); !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("last-owner delete: want ErrConflict, got %v", err)
		}
		if _, deleted := accountStatus(ctx, t, tdb, "del-founder@x.test"); deleted {
			t.Error("account must not be deleted after a refused delete")
		}
	})
}

func TestAccountExport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	acct, ten, ring := newServices(t, tdb)

	founder, _ := makeAccount(ctx, t, acct, ring, "exp@x.test")
	biz, err := ten.CreateMasterBusiness(ctx, founder, "ExportCo")
	if err != nil {
		t.Fatalf("create master: %v", err)
	}

	exp, err := acct.Export(ctx, founder)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if exp.Account.Email != "exp@x.test" || exp.Account.DisplayName != "Name" {
		t.Errorf("exported account identity: %+v", exp.Account)
	}
	found := false
	for _, m := range exp.Memberships {
		if m.BusinessID == biz.ID.String() {
			found = true
			if m.RoleKey != "owner" || m.BusinessName != "ExportCo" {
				t.Errorf("exported membership: want owner @ ExportCo, got role=%q name=%q", m.RoleKey, m.BusinessName)
			}
		}
	}
	if !found {
		t.Errorf("export must include the founder's own membership at %s, got %+v", biz.ID, exp.Memberships)
	}
}
