//go:build integration

package account

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
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

func TestPasskeyCredentialLifecycle(t *testing.T) {
	ctx := t.Context()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)

	const origin = "https://hub.example.test"
	const rpID = "hub.example.test"
	p, err := NewPasskeys(origin)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := auth.NewDevKeyRing("test", "test")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: tdb.App, Ring: ring, Passkeys: p, TokenTTL: time.Hour, AccessTTL: time.Hour, RefreshTTL: time.Hour}
	const email = "passkey-lifecycle@example.test"
	const password = "test-password-that-is-long"
	accountID, _, err := svc.Signup(ctx, email, "Passkey user", password)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := dbgen.New(tdb.App.Pool()).GetPrincipalByAccount(ctx, db.PGUUID(accountID))
	if err != nil {
		t.Fatal(err)
	}

	magicBeforeEnrollment, err := svc.RequestMagicLink(ctx, email)
	if err != nil || magicBeforeEnrollment == "" {
		t.Fatalf("request magic link before enrollment: %v", err)
	}
	passwordPair, err := svc.Login(ctx, email, password)
	if err != nil {
		t.Fatal(err)
	}
	_, passwordLogin, err := ring.ParseWithRecentLogin(passwordPair.Access)
	if err != nil {
		t.Fatal(err)
	}

	// Model a discoverable ES256 credential with user verification and a hardware counter.
	credentialID := uuid.New()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := webauthncbor.Marshal(map[int]any{
		1: 2, 3: -7, -1: 1, -2: key.X.FillBytes(make([]byte, 32)), -3: key.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	encodeJSON := func(value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	authData := func(flags protocol.AuthenticatorFlags, counter uint32) []byte {
		hash := sha256.Sum256([]byte(rpID))
		data := append(hash[:], byte(flags|protocol.FlagUserPresent|protocol.FlagUserVerified))
		return binary.BigEndian.AppendUint32(data, counter)
	}
	credentialResponse := func(response map[string]any) []byte {
		id := protocol.URLEncodedBase64(credentialID[:])
		return encodeJSON(map[string]any{"type": "public-key", "id": id, "rawId": id, "response": response})
	}

	options, token, err := svc.beginPasskeyRegistration(ctx, principal.ID, passwordLogin)
	if err != nil {
		t.Fatal(err)
	}
	pendingOptions, pendingToken, err := svc.beginPasskeyRegistration(ctx, principal.ID, passwordLogin)
	if err != nil {
		t.Fatal(err)
	}
	data := append(authData(protocol.FlagAttestedCredentialData, 0), make([]byte, 16)...)
	data = binary.BigEndian.AppendUint16(data, uint16(len(credentialID)))
	data = append(data, credentialID[:]...)
	data = append(data, publicKey...)
	attestation, err := webauthncbor.Marshal(map[string]any{
		"fmt": "none", "attStmt": map[string]any{}, "authData": data,
	})
	if err != nil {
		t.Fatal(err)
	}
	creation := func(challenge string) []byte {
		return credentialResponse(map[string]any{
			"clientDataJSON": protocol.URLEncodedBase64(encodeJSON(protocol.CollectedClientData{
				Type: protocol.CreateCeremony, Challenge: challenge, Origin: origin,
			})),
			"attestationObject": protocol.URLEncodedBase64(attestation),
			"transports":        []string{"internal"},
		})
	}
	passkey, err := svc.finishPasskeyRegistration(ctx, principal.ID, passwordLogin, token, "Test key", creation(options.Response.Challenge.String()))
	if err != nil || passkey.LastUsedAt != nil {
		t.Fatalf("register: %+v, %v", passkey, err)
	}

	if pair, err := svc.Login(ctx, email, password); !errors.Is(err, ErrInvalidCredentials) || pair != (TokenPair{}) {
		t.Fatalf("password sign-in after enrollment issued a session or surfaced the wrong error: %v", err)
	}
	if pair, err := svc.ConsumeMagicLink(ctx, magicBeforeEnrollment); !errors.Is(err, errs.ErrValidation) || pair != (TokenPair{}) {
		t.Fatalf("magic link issued before enrollment allowed sign-in or surfaced the wrong error: %v", err)
	}
	if raw, err := svc.RequestMagicLink(ctx, email); err != nil || raw != "" {
		t.Fatalf("magic link request after enrollment issued a link or failed: %v", err)
	}
	if _, _, err := svc.beginPasskeyRegistration(ctx, principal.ID, passwordLogin); !errors.Is(err, errs.ErrReauthenticationRequired) {
		t.Fatalf("registration begin with existing password session: %v", err)
	}
	if _, err := svc.finishPasskeyRegistration(ctx, principal.ID, passwordLogin, pendingToken, "Test key", creation(pendingOptions.Response.Challenge.String())); !errors.Is(err, errs.ErrReauthenticationRequired) {
		t.Fatalf("registration began before enrollment and finished with a password session: %v", err)
	}
	if err := svc.removePasskey(ctx, principal.ID, passwordLogin, passkey.ID, true); !errors.Is(err, errs.ErrReauthenticationRequired) {
		t.Fatalf("last passkey removal with existing password session: %v", err)
	}

	signIn := func(counter uint32) (TokenPair, error) {
		t.Helper()
		options, token, err := svc.beginPasskeyLogin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		data := authData(0, counter)
		clientData := encodeJSON(protocol.CollectedClientData{
			Type: protocol.AssertCeremony, Challenge: options.Response.Challenge.String(), Origin: origin,
		})
		clientHash := sha256.Sum256(clientData)
		signatureHash := sha256.Sum256(append(data, clientHash[:]...))
		signature, err := ecdsa.SignASN1(rand.Reader, key, signatureHash[:])
		if err != nil {
			t.Fatal(err)
		}
		response := credentialResponse(map[string]any{
			"clientDataJSON":    protocol.URLEncodedBase64(clientData),
			"authenticatorData": protocol.URLEncodedBase64(data),
			"signature":         protocol.URLEncodedBase64(signature),
			"userHandle":        protocol.URLEncodedBase64(accountID[:]),
		})
		return svc.finishPasskeyLogin(ctx, token, response)
	}

	pair, err := signIn(1)
	if err != nil || pair.Refresh == "" {
		t.Fatalf("passkey sign-in failed or omitted refresh token: %v", err)
	}
	pid, passkeyLogin, err := ring.ParseWithRecentLogin(pair.Access)
	if err != nil || pid != principal.ID {
		t.Fatalf("passkey sign-in token principal=%v error=%v", pid, err)
	}
	list, err := svc.listPasskeys(ctx, principal.ID, passkeyLogin)
	if err != nil || !list.CanManage || len(list.Items) != 1 || list.Items[0].ID != passkey.ID || list.Items[0].LastUsedAt == nil {
		t.Fatalf("last-used metadata and recent sign-in: %+v, %v", list, err)
	}
	t.Run("serializes with session revocation", func(t *testing.T) {
		tx, err := tdb.App.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := svc.lockPasskeyLogin(ctx, dbgen.New(tx), principal.ID, passkeyLogin, true); err != nil {
			t.Fatal(err)
		}
		var id uuid.UUID
		err = tdb.App.Pool().QueryRow(ctx, "SELECT id FROM refresh_token WHERE id = $1 FOR UPDATE NOWAIT", passkeyLogin.RefreshID).Scan(&id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("expected the session row to stay locked through the mutation, got %v", err)
		}
	})
	if _, err := signIn(1); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("non-increasing hardware signature counter: %v", err)
	}
	if err := svc.Logout(ctx, pair.Refresh); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.beginPasskeyRegistration(ctx, principal.ID, passkeyLogin); !errors.Is(err, errs.ErrReauthenticationRequired) {
		t.Fatalf("registration with signed-out passkey session: %v", err)
	}

	pair, err = signIn(2)
	if err != nil {
		t.Fatal(err)
	}
	_, passkeyLogin, err = ring.ParseWithRecentLogin(pair.Access)
	if err != nil {
		t.Fatal(err)
	}
	resetToken, err := svc.RequestPasswordReset(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	const newPassword = "new-test-password-that-is-long"
	if err := svc.ConfirmPasswordReset(ctx, resetToken, newPassword); err != nil {
		t.Fatal(err)
	}
	if pair, err := svc.Login(ctx, email, newPassword); !errors.Is(err, ErrInvalidCredentials) || pair != (TokenPair{}) {
		t.Fatalf("password reset cleared the passkey sign-in requirement: %v", err)
	}
	if err := svc.removePasskey(ctx, principal.ID, passkeyLogin, passkey.ID, true); !errors.Is(err, errs.ErrReauthenticationRequired) {
		t.Fatalf("removal with session from before password reset: %v", err)
	}

	pair, err = signIn(3)
	if err != nil {
		t.Fatal(err)
	}
	_, passkeyLogin, err = ring.ParseWithRecentLogin(pair.Access)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.removePasskey(ctx, principal.ID, passkeyLogin, passkey.ID, false); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("unconfirmed removal of the last passkey: %v", err)
	}
	if err := svc.removePasskey(ctx, principal.ID, passkeyLogin, passkey.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := signIn(4); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("removed passkey sign-in: %v", err)
	}
	if pair, err := svc.Login(ctx, email, newPassword); err != nil || pair.Access == "" {
		t.Fatalf("password sign-in after deliberate removal of last passkey: %v", err)
	}
	if _, err := svc.ConsumeMagicLink(ctx, magicBeforeEnrollment); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("blocked magic link should remain consumed: %v", err)
	}
}
