//go:build integration

package security_regression

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/invitations"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/mailer"

	"github.com/google/uuid"
)

type noopMailer struct{}

func (noopMailer) Send(context.Context, mailer.Message) error { return nil }

const oraclePassword = "supersecretpassword"

// TestOracleUniformity proves SC-010 / FR-026: failures that an attacker could
// otherwise use to enumerate accounts or token state are indistinguishable in
// SHAPE, and the login failure path is fixed-cost (no fast email-miss branch).
func TestOracleUniformity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ring, err := auth.NewDevKeyRing("manyforge", "manyforge-api")
	if err != nil {
		t.Fatalf("key ring: %v", err)
	}
	acct := &account.Service{
		DB: tdb.App, Ring: ring, Mailer: noopMailer{},
		AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour, TokenTTL: time.Hour,
	}

	// An active account, and a deactivated one (correct password, but disabled).
	if _, _, err := acct.Signup(ctx, "real@x.test", "Real", oraclePassword); err != nil {
		t.Fatalf("signup real: %v", err)
	}
	if _, _, err := acct.Signup(ctx, "off@x.test", "Off", oraclePassword); err != nil {
		t.Fatalf("signup off: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE account SET status='deactivated' WHERE email='off@x.test'"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	t.Run("login failures are uniform (miss == wrong-pw == deactivated)", func(t *testing.T) {
		cases := map[string]error{
			"unknown email":      login(acct, ctx, "nobody@x.test", oraclePassword),
			"wrong password":     login(acct, ctx, "real@x.test", "wrong-"+oraclePassword),
			"deactivated account": login(acct, ctx, "off@x.test", oraclePassword),
		}
		for name, err := range cases {
			if !errors.Is(err, account.ErrInvalidCredentials) {
				t.Errorf("%s: want ErrInvalidCredentials (no oracle), got %v", name, err)
			}
		}
		// Control: the active account with the right password succeeds.
		if err := login(acct, ctx, "real@x.test", oraclePassword); err != nil {
			t.Errorf("control: valid login should succeed, got %v", err)
		}
	})

	t.Run("login failure is fixed-cost (email-miss not suspiciously faster)", func(t *testing.T) {
		miss := medianLoginLatency(acct, ctx, "nobody@x.test", oraclePassword)
		wrong := medianLoginLatency(acct, ctx, "real@x.test", "wrong-"+oraclePassword)
		// Both run a full password hash (DummyVerify on the miss branch), so they
		// must be within a generous factor — a miss returning ~instantly would be
		// the enumeration oracle. Lenient bound to stay CI-stable.
		if miss*4 < wrong || wrong*4 < miss {
			t.Errorf("login latency oracle: miss=%v wrong-pw=%v (should be within 4x)", miss, wrong)
		}
	})

	t.Run("email-verification token failures are uniform", func(t *testing.T) {
		_, vtok, err := acct.Signup(ctx, "verify@x.test", "V", oraclePassword)
		if err != nil {
			t.Fatalf("signup verify: %v", err)
		}
		if err := acct.VerifyEmail(ctx, vtok); err != nil {
			t.Fatalf("first verify should succeed: %v", err)
		}
		// Reused (already-consumed) and unknown tokens map to the same validation error.
		if err := acct.VerifyEmail(ctx, vtok); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("reused token: want ErrValidation, got %v", err)
		}
		if err := acct.VerifyEmail(ctx, "not-a-real-token"); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("unknown token: want ErrValidation, got %v", err)
		}
	})

	t.Run("invitation-accept of an unknown token is gone (no oracle)", func(t *testing.T) {
		e := seedEscalationTenant(ctx, t, tdb)
		// A verified invitee principal to accept as.
		_, vtok, err := acct.Signup(ctx, "invitee@x.test", "I", oraclePassword)
		if err != nil {
			t.Fatalf("signup invitee: %v", err)
		}
		if err := acct.VerifyEmail(ctx, vtok); err != nil {
			t.Fatalf("verify invitee: %v", err)
		}
		var invitee uuid.UUID
		if err := tdb.Super.QueryRow(ctx, "SELECT id FROM principal WHERE account_id=(SELECT id FROM account WHERE email='invitee@x.test')").Scan(&invitee); err != nil {
			t.Fatalf("invitee principal: %v", err)
		}
		_ = e
		inv := &invitations.Service{DB: tdb.App}
		res, err := inv.Accept(ctx, invitee, "garbage-token")
		if err != nil {
			t.Fatalf("accept unknown token errored: %v", err)
		}
		if res.Status != "gone" {
			t.Errorf("unknown invitation token: want status 'gone', got %q", res.Status)
		}
	})
}

func login(acct *account.Service, ctx context.Context, email, pw string) error {
	_, err := acct.Login(ctx, email, pw)
	return err
}

func medianLoginLatency(acct *account.Service, ctx context.Context, email, pw string) time.Duration {
	const n = 7
	ds := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		_, _ = acct.Login(ctx, email, pw)
		ds[i] = time.Since(t0)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[n/2]
}
