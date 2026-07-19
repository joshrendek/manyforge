package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/codexoauth"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// fakeCodexOAuth is a scripted codexOAuth seam.
type fakeCodexOAuth struct {
	device codexoauth.DeviceAuth
	poll   []struct {
		ts  codexoauth.TokenSet
		st  codexoauth.PollStatus
		err error
	}
	pollIdx int
	refresh func(string) (codexoauth.TokenSet, error)
}

func (f *fakeCodexOAuth) StartDeviceAuth(context.Context) (codexoauth.DeviceAuth, error) {
	return f.device, nil
}
func (f *fakeCodexOAuth) PollDeviceToken(context.Context, string) (codexoauth.TokenSet, codexoauth.PollStatus, error) {
	r := f.poll[f.pollIdx]
	if f.pollIdx < len(f.poll)-1 {
		f.pollIdx++
	}
	return r.ts, r.st, r.err
}
func (f *fakeCodexOAuth) ExchangePKCE(context.Context, string, string) (codexoauth.TokenSet, error) {
	return f.poll[0].ts, f.poll[0].err
}
func (f *fakeCodexOAuth) Refresh(_ context.Context, rt string) (codexoauth.TokenSet, error) {
	return f.refresh(rt)
}
func (f *fakeCodexOAuth) AuthorizeURL(ch, st string) string {
	return "https://auth.openai.com/oauth/authorize?state=" + st
}

// fakeCodexStore is the codexStore test double: no pgx.Tx, scripted returns, records calls so
// tests can assert the DB-effect path was (or was NOT) exercised without a real database.
type fakeCodexStore struct {
	// insertPending
	insertErr   error
	insertedRow pendingRow
	insertedJTI uuid.UUID
	insertCalls int

	// getPendingLocked
	pending    pendingRow
	pendingErr error

	// finishConnect
	finishErr    error
	finishID     uuid.UUID
	finishInput  upsertCredInput
	finishCalled bool
}

func (f *fakeCodexStore) insertPending(_ context.Context, _, _ uuid.UUID, p pendingRow, jti uuid.UUID) error {
	f.insertCalls++
	f.insertedRow = p
	f.insertedJTI = jti
	return f.insertErr
}

func (f *fakeCodexStore) getPendingLocked(_ context.Context, _, _, _ uuid.UUID) (pendingRow, error) {
	return f.pending, f.pendingErr
}

func (f *fakeCodexStore) finishConnect(_ context.Context, _, _, _ uuid.UUID, in upsertCredInput) (uuid.UUID, error) {
	f.finishCalled = true
	f.finishInput = in
	if f.finishErr != nil {
		return uuid.Nil, f.finishErr
	}
	if f.finishID == uuid.Nil {
		f.finishID = uuid.New()
	}
	return f.finishID, nil
}

// testSealer builds a real Sealer from a 32-byte key (seal/open must round-trip).
func testSealer(t *testing.T) *crypto.Sealer {
	t.Helper()
	s, err := crypto.NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func approvedTokenSet() codexoauth.TokenSet {
	return codexoauth.TokenSet{
		AccessToken: "acc", RefreshToken: "ref", IDToken: "id",
		Expiry: time.Now().Add(time.Hour),
		Claims: codexoauth.Claims{AccountID: "acc_1", Plan: "pro"},
	}
}

func TestPollDevice_pendingReturnsPending(t *testing.T) {
	// A pending poll must NOT create a credential and must NOT touch the DB write path.
	sealer := testSealer(t)
	sealedDC, err := sealer.Seal([]byte("device-code-123"))
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeCodexStore{pending: pendingRow{
		Flow: "device", SealedDeviceCode: &sealedDC, DefaultModel: "gpt-5-codex",
		MaxConcurrentLanes: 4, ExpiresAt: time.Now().Add(15 * time.Minute),
	}}
	svc := &CodexTokenService{
		Sealer: sealer, Now: time.Now, PendingTTL: 15 * time.Minute,
		OAuth: &fakeCodexOAuth{poll: []struct {
			ts  codexoauth.TokenSet
			st  codexoauth.PollStatus
			err error
		}{{st: codexoauth.PollPending}}},
		Store: store,
	}
	got, err := svc.PollDevice(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Fatalf("status = %q", got.Status)
	}
	if store.finishCalled {
		t.Fatal("finishConnect must not be called for a pending poll")
	}
}

func TestPollDevice_approvedSealsAndUpserts(t *testing.T) {
	sealer := testSealer(t)
	sealedDC, err := sealer.Seal([]byte("device-code-123"))
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeCodexStore{pending: pendingRow{
		Flow: "device", SealedDeviceCode: &sealedDC, DefaultModel: "gpt-5-codex",
		MaxConcurrentLanes: 4, ExpiresAt: time.Now().Add(15 * time.Minute),
	}}
	svc := &CodexTokenService{
		Sealer: sealer, Now: time.Now, PendingTTL: 15 * time.Minute,
		OAuth: &fakeCodexOAuth{poll: []struct {
			ts  codexoauth.TokenSet
			st  codexoauth.PollStatus
			err error
		}{{ts: approvedTokenSet(), st: codexoauth.PollApproved}}},
		Store: store,
	}
	got, err := svc.PollDevice(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "approved" {
		t.Fatalf("status = %q", got.Status)
	}
	if got.CredentialID == uuid.Nil {
		t.Fatal("expected a non-nil credential id")
	}
	if !store.finishCalled {
		t.Fatal("expected finishConnect to be called")
	}
	openedAccess, err := sealer.Open(store.finishInput.SealedAccess)
	if err != nil || string(openedAccess) != "acc" {
		t.Fatalf("sealed access round-trip: got %q, err %v", openedAccess, err)
	}
	openedRefresh, err := sealer.Open(store.finishInput.SealedRefresh)
	if err != nil || string(openedRefresh) != "ref" {
		t.Fatalf("sealed refresh round-trip: got %q, err %v", openedRefresh, err)
	}
	if store.finishInput.AccountID != "acc_1" {
		t.Fatalf("account id = %q", store.finishInput.AccountID)
	}
	if store.finishInput.Plan != "pro" {
		t.Fatalf("plan = %q", store.finishInput.Plan)
	}
	if store.finishInput.DefaultModel != "gpt-5-codex" {
		t.Fatalf("default model = %q", store.finishInput.DefaultModel)
	}
}

func TestExchangePKCE_stateMismatch(t *testing.T) {
	sealer := testSealer(t)
	sealedV, err := sealer.Seal([]byte("verifier-abc"))
	if err != nil {
		t.Fatal(err)
	}
	jti := uuid.New()
	store := &fakeCodexStore{pending: pendingRow{
		Flow: "pkce", SealedPKCEVerifier: &sealedV, DefaultModel: "gpt-5-codex",
		MaxConcurrentLanes: 4, ExpiresAt: time.Now().Add(15 * time.Minute),
	}}
	svc := &CodexTokenService{
		Sealer: sealer, Now: time.Now, PendingTTL: 15 * time.Minute,
		OAuth: &fakeCodexOAuth{poll: []struct {
			ts  codexoauth.TokenSet
			st  codexoauth.PollStatus
			err error
		}{{ts: approvedTokenSet()}}},
		Store: store,
	}
	_, err = svc.ExchangePKCE(context.Background(), uuid.New(), uuid.New(), jti,
		"https://localhost/callback?state=not-the-jti&code=abc123")
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if store.finishCalled {
		t.Fatal("finishConnect must not be called on a state mismatch")
	}
}

func TestPersistConnect_missingAccountID(t *testing.T) {
	store := &fakeCodexStore{}
	svc := &CodexTokenService{Sealer: testSealer(t), Now: time.Now, Store: store}
	ts := codexoauth.TokenSet{
		AccessToken: "acc", RefreshToken: "ref",
		Claims: codexoauth.Claims{AccountID: "", Plan: "pro"},
	}
	_, err := svc.persistConnect(context.Background(), uuid.New(), uuid.New(), uuid.New(), ts, pendingRow{})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if store.finishCalled {
		t.Fatal("finishConnect must not be called when account id is missing")
	}
}
