//go:build integration

package account

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestPasskeyChallengeIsBoundToAccountAndPurposeAndConsumedOnce(t *testing.T) {
	ctx := t.Context()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)

	p, err := NewPasskeys("https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: tdb.App, Passkeys: p}
	owner, _, err := svc.Signup(ctx, "passkey-owner@example.test", "Owner", "test-password-that-is-long")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := svc.Signup(ctx, "passkey-other@example.test", "Other", "test-password-that-is-long")
	if err != nil {
		t.Fatal(err)
	}

	_, session, err := p.server.BeginRegistration(passkeyUser{account: dbgen.Account{ID: owner, Email: "passkey-owner@example.test", DisplayName: "Owner"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := svc.storePasskeyChallenge(ctx, session, db.PGUUID(owner), passkeyRegistration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.consumePasskeyChallenge(ctx, token, db.PGUUID(other), passkeyRegistration); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong account: %v", err)
	}
	if _, err := svc.consumePasskeyChallenge(ctx, token, pgtype.UUID{}, passkeyLogin); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong purpose: %v", err)
	}

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := svc.consumePasskeyChallenge(ctx, token, db.PGUUID(owner), passkeyRegistration)
			results <- err
		}()
	}
	first, second := <-results, <-results
	if first == nil {
		first, second = second, first
	}
	if second != nil || !errors.Is(first, ErrInvalidCredentials) {
		t.Fatalf("expected one successful consumption, got %v and %v", first, second)
	}
}
