//go:build integration

package account_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/mailer"
	"github.com/manyforge/manyforge/internal/tenancy"
)

func newServices(t *testing.T, tdb *testdb.TestDB) (*account.Service, *tenancy.Service, *auth.KeyRing) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	acct := &account.Service{
		DB: tdb.App, Ring: ring, Mailer: mailer.LogMailer{},
		AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour, TokenTTL: time.Hour,
	}
	return acct, &tenancy.Service{DB: tdb.App}, ring
}

func TestUS1_SignupVerifyLoginCreateBusiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	acct, ten, ring := newServices(t, tdb)

	_, verifyToken, err := acct.Signup(ctx, "founder@x.test", "Founder", "supersecretpassword")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}

	// Login works pre-verification and yields the principal id (via the access token).
	tp, err := acct.Login(ctx, "founder@x.test", "supersecretpassword")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	pid, err := ring.Parse(tp.Access)
	if err != nil {
		t.Fatalf("parse access token: %v", err)
	}

	// Creating a business before verification is refused (FR-002).
	if _, err := ten.CreateMasterBusiness(ctx, pid, "Acme"); err == nil {
		t.Error("create business should be refused before email verification")
	}

	if err := acct.VerifyEmail(ctx, verifyToken); err != nil {
		t.Fatalf("verify email: %v", err)
	}
	biz, err := ten.CreateMasterBusiness(ctx, pid, "Acme")
	if err != nil {
		t.Fatalf("create master business: %v", err)
	}
	if biz.TenantRootID != biz.ID {
		t.Errorf("master business tenant_root_id (%s) should equal id (%s)", biz.TenantRootID, biz.ID)
	}

	// The creator is the Owner: full permissions on the new business.
	if err := tdb.App.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		perms, err := authz.Resolve(ctx, tx, pid, biz.ID)
		if err != nil {
			return err
		}
		for _, k := range []string{"business.delete", "ownership.transfer", "members.manage"} {
			if !perms.Has(k) {
				t.Errorf("creator should hold %q", k)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("resolve creator perms: %v", err)
	}

	// The creation was audited.
	var n int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM audit_entry WHERE action='business.created' AND business_id=$1", biz.ID).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 business.created audit entry, got %d", n)
	}

	// Wrong password is rejected with the generic credential error (no oracle).
	if _, err := acct.Login(ctx, "founder@x.test", "wrong-password!!"); !errors.Is(err, account.ErrInvalidCredentials) {
		t.Errorf("wrong password: want ErrInvalidCredentials, got %v", err)
	}
	// Unknown email is rejected the same way.
	if _, err := acct.Login(ctx, "nobody@x.test", "whatever-password"); !errors.Is(err, account.ErrInvalidCredentials) {
		t.Errorf("unknown email: want ErrInvalidCredentials, got %v", err)
	}
}

func TestUS1_RefreshRotationAndReuse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	acct, _, _ := newServices(t, tdb)

	if _, _, err := acct.Signup(ctx, "rot@x.test", "Rot", "supersecretpassword"); err != nil {
		t.Fatalf("signup: %v", err)
	}
	tp, err := acct.Login(ctx, "rot@x.test", "supersecretpassword")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	tp2, err := acct.Refresh(ctx, tp.Refresh)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	// Re-presenting the already-used original token is reuse → family revoked.
	if _, err := acct.Refresh(ctx, tp.Refresh); !errors.Is(err, auth.ErrRefreshReuse) {
		t.Errorf("reuse: want ErrRefreshReuse, got %v", err)
	}
	// And the rotated token is now invalid because its family was revoked.
	if _, err := acct.Refresh(ctx, tp2.Refresh); err == nil {
		t.Error("rotated token should be invalid after family revoke")
	}
}
