//go:build integration

package account_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestPasswordResetFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	acct, _, r := newServices(t, tdb)

	_, tp := makeAccount(ctx, t, acct, r, "pr@x.test")

	t.Run("request is uniform: a token for a known email, nothing for an unknown one", func(t *testing.T) {
		tok, err := acct.RequestPasswordReset(ctx, "pr@x.test")
		if err != nil || tok == "" {
			t.Fatalf("reset request (known): want a token, got tok=%q err=%v", tok, err)
		}
		// No existence oracle: an unknown email yields the same success, no token.
		tok2, err := acct.RequestPasswordReset(ctx, "nobody@x.test")
		if err != nil || tok2 != "" {
			t.Fatalf("reset request (unknown): want silent success, got tok=%q err=%v", tok2, err)
		}
	})

	t.Run("confirm sets a new password, revokes sessions, and is single-use", func(t *testing.T) {
		tok, err := acct.RequestPasswordReset(ctx, "pr@x.test")
		if err != nil || tok == "" {
			t.Fatalf("reset request: %v", err)
		}
		if err := acct.ConfirmPasswordReset(ctx, tok, "brandnewpassword12"); err != nil {
			t.Fatalf("confirm reset: %v", err)
		}
		if _, err := acct.Login(ctx, "pr@x.test", "supersecretpassword"); !errors.Is(err, account.ErrInvalidCredentials) {
			t.Errorf("old password must no longer work, got %v", err)
		}
		if _, err := acct.Login(ctx, "pr@x.test", "brandnewpassword12"); err != nil {
			t.Errorf("new password must work, got %v", err)
		}
		if _, err := acct.Refresh(ctx, tp.Refresh); err == nil {
			t.Error("a password reset must revoke existing sessions")
		}
		// The reset token cannot be replayed.
		if err := acct.ConfirmPasswordReset(ctx, tok, "yetanotherpw123"); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("replayed reset token: want ErrValidation, got %v", err)
		}
	})

	t.Run("confirm rejects a too-short password", func(t *testing.T) {
		tok, err := acct.RequestPasswordReset(ctx, "pr@x.test")
		if err != nil || tok == "" {
			t.Fatalf("reset request: %v", err)
		}
		if err := acct.ConfirmPasswordReset(ctx, tok, "short"); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("short password: want ErrValidation, got %v", err)
		}
	})
}

func TestEmailChangeFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	acct, _, r := newServices(t, tdb)

	pid, _ := makeAccount(ctx, t, acct, r, "ec@x.test")

	t.Run("confirming the change moves the login identity to the new address", func(t *testing.T) {
		tok, err := acct.RequestEmailChange(ctx, pid, "ec-new@x.test")
		if err != nil || tok == "" {
			t.Fatalf("email-change request: want a token, got tok=%q err=%v", tok, err)
		}
		if err := acct.ConfirmEmailChange(ctx, tok); err != nil {
			t.Fatalf("confirm email change: %v", err)
		}
		if _, err := acct.Login(ctx, "ec-new@x.test", "supersecretpassword"); err != nil {
			t.Errorf("login with the new email must work, got %v", err)
		}
		if _, err := acct.Login(ctx, "ec@x.test", "supersecretpassword"); !errors.Is(err, account.ErrInvalidCredentials) {
			t.Errorf("login with the old email must fail, got %v", err)
		}
	})

	t.Run("changing to an already-registered email is refused at confirm", func(t *testing.T) {
		makeAccount(ctx, t, acct, r, "taken@x.test")
		tok, err := acct.RequestEmailChange(ctx, pid, "taken@x.test")
		if err != nil || tok == "" {
			t.Fatalf("email-change request: %v", err)
		}
		if err := acct.ConfirmEmailChange(ctx, tok); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("collision at confirm: want ErrValidation, got %v", err)
		}
	})
}

func TestMagicLinkFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	acct, _, r := newServices(t, tdb)

	pid, _ := makeAccount(ctx, t, acct, r, "ml@x.test")

	t.Run("a magic link issues a working session for the right principal, single-use", func(t *testing.T) {
		tok, err := acct.RequestMagicLink(ctx, "ml@x.test")
		if err != nil || tok == "" {
			t.Fatalf("magic-link request: want a token, got tok=%q err=%v", tok, err)
		}
		tp, err := acct.ConsumeMagicLink(ctx, tok)
		if err != nil || tp.Access == "" {
			t.Fatalf("consume magic link: %v", err)
		}
		got, err := r.Parse(tp.Access)
		if err != nil || got != pid {
			t.Errorf("magic-link session principal: want %s, got %s (err %v)", pid, got, err)
		}
		if _, err := acct.ConsumeMagicLink(ctx, tok); err == nil {
			t.Error("a magic-link token must be single-use")
		}
	})

	t.Run("request is uniform for an unknown email", func(t *testing.T) {
		tok, err := acct.RequestMagicLink(ctx, "nobody@x.test")
		if err != nil || tok != "" {
			t.Fatalf("magic-link request (unknown): want silent success, got tok=%q err=%v", tok, err)
		}
	})
}
