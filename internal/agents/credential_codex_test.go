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

	// readCodex / refreshLockedTx (Task 6 — lazy get-or-refresh)
	cred         codexCredRow
	readErr      error
	wroteAccess  string
	wroteRefresh string
	wrotePlan    string
	disconnected bool
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

func (f *fakeCodexStore) readCodex(_ context.Context, _, _ uuid.UUID) (codexCredRow, error) {
	return f.cred, f.readErr
}

// refreshLockedTx mimics the prod tx boundary closely enough for unit tests: it runs fn against
// the scripted row and records the effect fn asked for (write-back vs. disconnect vs. no-op).
func (f *fakeCodexStore) refreshLockedTx(_ context.Context, _, _ uuid.UUID, fn func(codexCredRow) (*upsertTokens, bool, error)) error {
	toks, disconnect, err := fn(f.cred)
	if err != nil {
		return err
	}
	if disconnect {
		f.disconnected = true
		return nil
	}
	if toks != nil {
		f.wroteAccess = toks.SealedAccess
		f.wroteRefresh = toks.SealedRefresh
		f.wrotePlan = toks.Plan
	}
	return nil
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

func TestPollDevice_expiredReturnsExpired(t *testing.T) {
	// An expired poll must NOT create a credential and must NOT touch the DB write path.
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
		}{{st: codexoauth.PollExpired}}},
		Store: store,
	}
	got, err := svc.PollDevice(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "expired" {
		t.Fatalf("status = %q", got.Status)
	}
	if store.finishCalled {
		t.Fatal("finishConnect must not be called for an expired poll")
	}
}

func TestPollDevice_deniedReturnsDenied(t *testing.T) {
	// A denied poll must NOT create a credential and must NOT touch the DB write path.
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
		}{{st: codexoauth.PollDenied}}},
		Store: store,
	}
	got, err := svc.PollDevice(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "denied" {
		t.Fatalf("status = %q", got.Status)
	}
	if store.finishCalled {
		t.Fatal("finishConnect must not be called for a denied poll")
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

// --- Task 6: lazy get-or-refresh (Mint / refreshLocked / doRefresh) ---

func TestMint_freshTokenNoNetwork(t *testing.T) {
	sealer := testSealer(t)
	sealedAccess, err := sealer.Seal([]byte("live-token"))
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(30 * time.Minute)
	store := &fakeCodexStore{cred: codexCredRow{
		SealedKeyRef:      &sealedAccess,
		OAuthAccessExpiry: &expiry,
	}}
	oauth := &fakeCodexOAuth{refresh: func(string) (codexoauth.TokenSet, error) {
		t.Fatal("refresh must not be called for a still-fresh token")
		return codexoauth.TokenSet{}, nil
	}}
	svc := &CodexTokenService{Sealer: sealer, Now: time.Now, Store: store, OAuth: oauth}
	got, err := svc.Mint(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if got != "live-token" {
		t.Fatalf("token = %q", got)
	}
}

func TestMint_expiredRefreshesRotatesWritesBack(t *testing.T) {
	sealer := testSealer(t)
	sealedOldAccess, err := sealer.Seal([]byte("old-token"))
	if err != nil {
		t.Fatal(err)
	}
	sealedOldRefresh, err := sealer.Seal([]byte("r-old"))
	if err != nil {
		t.Fatal(err)
	}
	pastExpiry := time.Now().Add(-time.Hour)
	store := &fakeCodexStore{cred: codexCredRow{
		SealedKeyRef:      &sealedOldAccess,
		OAuthRefreshToken: &sealedOldRefresh,
		OAuthAccessExpiry: &pastExpiry,
	}}
	oauth := &fakeCodexOAuth{refresh: func(rt string) (codexoauth.TokenSet, error) {
		if rt != "r-old" {
			t.Fatalf("refresh called with %q, want r-old", rt)
		}
		return codexoauth.TokenSet{
			AccessToken: "new", RefreshToken: "r-new", Expiry: time.Now().Add(time.Hour),
			Claims: codexoauth.Claims{Plan: "pro"},
		}, nil
	}}
	svc := &CodexTokenService{Sealer: sealer, Now: time.Now, Store: store, OAuth: oauth}
	got, err := svc.Mint(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if got != "new" {
		t.Fatalf("token = %q", got)
	}
	openedAccess, err := sealer.Open(store.wroteAccess)
	if err != nil || string(openedAccess) != "new" {
		t.Fatalf("wrote access round-trip: got %q, err %v", openedAccess, err)
	}
	openedRefresh, err := sealer.Open(store.wroteRefresh)
	if err != nil || string(openedRefresh) != "r-new" {
		t.Fatalf("wrote refresh round-trip: got %q, err %v", openedRefresh, err)
	}
}

func TestMint_invalidGrantDisconnects(t *testing.T) {
	sealer := testSealer(t)
	sealedOldAccess, err := sealer.Seal([]byte("old-token"))
	if err != nil {
		t.Fatal(err)
	}
	sealedOldRefresh, err := sealer.Seal([]byte("r-old"))
	if err != nil {
		t.Fatal(err)
	}
	pastExpiry := time.Now().Add(-time.Hour)
	store := &fakeCodexStore{cred: codexCredRow{
		SealedKeyRef:      &sealedOldAccess,
		OAuthRefreshToken: &sealedOldRefresh,
		OAuthAccessExpiry: &pastExpiry,
	}}
	oauth := &fakeCodexOAuth{refresh: func(string) (codexoauth.TokenSet, error) {
		return codexoauth.TokenSet{}, codexoauth.ErrInvalidGrant
	}}
	svc := &CodexTokenService{Sealer: sealer, Now: time.Now, Store: store, OAuth: oauth}
	_, err = svc.Mint(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, errs.ErrCodexDisconnected) {
		t.Fatalf("expected ErrCodexDisconnected, got %v", err)
	}
	if !store.disconnected {
		t.Fatal("expected store.disconnected == true")
	}
}

// --- Task 6b: cross-tenant sweep (RefreshDue / systemCodexStore) ---

// fakeSystemStep scripts one claim (or "no credential due") for fakeSystemCodexStore.
// refreshErr/disconnect are documentation of the intended outcome for the reader; the actual
// outcome is driven end-to-end through the real fn (doRefresh -> Sealer -> fakeCodexOAuth), keyed
// off claim.SealedRefresh, exactly like the production path.
type fakeSystemStep struct {
	claim      claimedCodex
	none       bool  // no credential due
	refreshErr error // fn is expected to return this (simulate upstream failure)
	disconnect bool  // fn is expected to return disconnect=true
}

// fakeSystemCodexStore is the systemCodexStore test double: pops scripted steps in call order,
// skipping any step whose claim id is already in the caller's exclude list (mirrors
// `id <> ALL(p_exclude)` in codex_claim_for_refresh), and records what fn actually did with each
// claim so tests can assert apply vs. disconnect vs. failure without a real database.
type fakeSystemCodexStore struct {
	steps []fakeSystemStep
	idx   int

	// claimErr, when set, makes the FIRST call return (uuid.Nil, false, claimErr) — simulating a
	// claim/tx failure (WithTx/Begin error, or the claim QueryRow.Scan failing with something
	// other than pgx.ErrNoRows) rather than "nothing due". Consumed once, before any steps run.
	claimErr error

	applied      []uuid.UUID // ids where fn returned (toks, false, nil) — apply path
	disconnected []uuid.UUID // ids where fn returned (nil, true, nil) — disconnect path
	failed       []uuid.UUID // ids where fn returned a non-nil error
	excludeSeen  [][]string  // exclude slice as observed on each call, for assertions
}

func (f *fakeSystemCodexStore) refreshOneDue(_ context.Context, _ time.Time, exclude []string,
	fn func(claimedCodex) (*upsertTokens, bool, error)) (uuid.UUID, bool, error) {
	f.excludeSeen = append(f.excludeSeen, append([]string(nil), exclude...))
	if f.claimErr != nil {
		err := f.claimErr
		f.claimErr = nil
		return uuid.Nil, false, err
	}
	for f.idx < len(f.steps) {
		step := f.steps[f.idx]
		f.idx++
		if step.none {
			return uuid.Nil, false, nil
		}
		if containsStr(exclude, step.claim.ID.String()) {
			continue // already handled this sweep — the DB wouldn't re-serve it either
		}
		toks, disconnect, err := fn(step.claim)
		switch {
		case err != nil:
			f.failed = append(f.failed, step.claim.ID)
		case disconnect:
			f.disconnected = append(f.disconnected, step.claim.ID)
		case toks != nil:
			f.applied = append(f.applied, step.claim.ID)
		}
		return step.claim.ID, true, err
	}
	return uuid.Nil, false, nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sealFor(t *testing.T, sealer *crypto.Sealer, plaintext string) string {
	t.Helper()
	s, err := sealer.Seal([]byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRefreshDue_refreshesAllDue(t *testing.T) {
	sealer := testSealer(t)
	claim1 := claimedCodex{ID: uuid.New(), SealedRefresh: sealFor(t, sealer, "r1"), Plan: "pro"}
	claim2 := claimedCodex{ID: uuid.New(), SealedRefresh: sealFor(t, sealer, "r2"), Plan: "plus"}
	oauth := &fakeCodexOAuth{refresh: func(rt string) (codexoauth.TokenSet, error) {
		switch rt {
		case "r1":
			return codexoauth.TokenSet{AccessToken: "a1", RefreshToken: "nr1", Expiry: time.Now().Add(time.Hour), Claims: codexoauth.Claims{Plan: "pro"}}, nil
		case "r2":
			return codexoauth.TokenSet{AccessToken: "a2", RefreshToken: "nr2", Expiry: time.Now().Add(time.Hour), Claims: codexoauth.Claims{Plan: "plus"}}, nil
		default:
			t.Fatalf("unexpected refresh token %q", rt)
			return codexoauth.TokenSet{}, nil
		}
	}}
	store := &fakeSystemCodexStore{steps: []fakeSystemStep{{claim: claim1}, {claim: claim2}, {none: true}}}
	svc := &CodexTokenService{Sealer: sealer, Now: time.Now, OAuth: oauth, SystemStore: store}

	n, err := svc.RefreshDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("refreshed = %d, want 2", n)
	}
	if len(store.applied) != 2 || store.applied[0] != claim1.ID || store.applied[1] != claim2.ID {
		t.Fatalf("applied = %v, want [%v %v]", store.applied, claim1.ID, claim2.ID)
	}
	if len(store.excludeSeen) != 3 {
		t.Fatalf("expected 3 refreshOneDue calls, got %d", len(store.excludeSeen))
	}
	if len(store.excludeSeen[0]) != 0 {
		t.Fatalf("first call exclude should be empty, got %v", store.excludeSeen[0])
	}
	if len(store.excludeSeen[1]) != 1 || store.excludeSeen[1][0] != claim1.ID.String() {
		t.Fatalf("second call exclude = %v, want [%v]", store.excludeSeen[1], claim1.ID)
	}
	if len(store.excludeSeen[2]) != 2 {
		t.Fatalf("third call exclude = %v, want 2 entries", store.excludeSeen[2])
	}
}

func TestRefreshDue_disconnectsDeadToken(t *testing.T) {
	sealer := testSealer(t)
	claim := claimedCodex{ID: uuid.New(), SealedRefresh: sealFor(t, sealer, "dead"), Plan: "pro"}
	oauth := &fakeCodexOAuth{refresh: func(rt string) (codexoauth.TokenSet, error) {
		if rt != "dead" {
			t.Fatalf("unexpected refresh token %q", rt)
		}
		return codexoauth.TokenSet{}, codexoauth.ErrInvalidGrant
	}}
	store := &fakeSystemCodexStore{steps: []fakeSystemStep{{claim: claim, disconnect: true}, {none: true}}}
	svc := &CodexTokenService{Sealer: sealer, Now: time.Now, OAuth: oauth, SystemStore: store}

	n, err := svc.RefreshDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("refreshed = %d, want 0", n)
	}
	if len(store.disconnected) != 1 || store.disconnected[0] != claim.ID {
		t.Fatalf("disconnected = %v, want [%v]", store.disconnected, claim.ID)
	}
	if len(store.excludeSeen) != 2 {
		t.Fatalf("expected the sweep to terminate after 2 calls (claim + none), got %d", len(store.excludeSeen))
	}
	if len(store.excludeSeen[1]) != 1 || store.excludeSeen[1][0] != claim.ID.String() {
		t.Fatalf("disconnected id must be excluded from the rest of the sweep, got %v", store.excludeSeen[1])
	}
}

func TestRefreshDue_skipsUpstreamFailure(t *testing.T) {
	sealer := testSealer(t)
	claimA := claimedCodex{ID: uuid.New(), SealedRefresh: sealFor(t, sealer, "bad"), Plan: "pro"}
	claimB := claimedCodex{ID: uuid.New(), SealedRefresh: sealFor(t, sealer, "good"), Plan: "plus"}
	upstreamErr := errors.New("network fail")
	oauth := &fakeCodexOAuth{refresh: func(rt string) (codexoauth.TokenSet, error) {
		switch rt {
		case "bad":
			return codexoauth.TokenSet{}, upstreamErr
		case "good":
			return codexoauth.TokenSet{AccessToken: "ab", RefreshToken: "nb", Expiry: time.Now().Add(time.Hour), Claims: codexoauth.Claims{Plan: "plus"}}, nil
		default:
			t.Fatalf("unexpected refresh token %q", rt)
			return codexoauth.TokenSet{}, nil
		}
	}}
	store := &fakeSystemCodexStore{steps: []fakeSystemStep{
		{claim: claimA, refreshErr: errs.ErrUpstream},
		{claim: claimB},
		{none: true},
	}}
	svc := &CodexTokenService{Sealer: sealer, Now: time.Now, OAuth: oauth, SystemStore: store}

	n, err := svc.RefreshDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("refreshed = %d, want 1", n)
	}
	if len(store.failed) != 1 || store.failed[0] != claimA.ID {
		t.Fatalf("failed = %v, want [%v]", store.failed, claimA.ID)
	}
	if len(store.applied) != 1 || store.applied[0] != claimB.ID {
		t.Fatalf("applied = %v, want [%v]", store.applied, claimB.ID)
	}
	if len(store.excludeSeen) != 3 {
		t.Fatalf("expected 3 refreshOneDue calls, got %d", len(store.excludeSeen))
	}
	last := store.excludeSeen[2]
	if !containsStr(last, claimA.ID.String()) || !containsStr(last, claimB.ID.String()) {
		t.Fatalf("both A (failed) and B (applied) must be excluded from further claims, got %v", last)
	}
}

func TestRefreshDue_boundedLoop(t *testing.T) {
	sealer := testSealer(t)
	oauth := &fakeCodexOAuth{refresh: func(string) (codexoauth.TokenSet, error) {
		return codexoauth.TokenSet{AccessToken: "a", RefreshToken: "nr", Expiry: time.Now().Add(time.Hour), Claims: codexoauth.Claims{Plan: "pro"}}, nil
	}}
	sealedRefresh := sealFor(t, sealer, "r")
	steps := make([]fakeSystemStep, 0, maxCodexSweep+1)
	for i := 0; i < maxCodexSweep+1; i++ {
		steps = append(steps, fakeSystemStep{claim: claimedCodex{ID: uuid.New(), SealedRefresh: sealedRefresh, Plan: "pro"}})
	}
	store := &fakeSystemCodexStore{steps: steps}
	svc := &CodexTokenService{Sealer: sealer, Now: time.Now, OAuth: oauth, SystemStore: store}

	n, err := svc.RefreshDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != maxCodexSweep {
		t.Fatalf("refreshed = %d, want %d (bounded)", n, maxCodexSweep)
	}
	if store.idx != maxCodexSweep {
		t.Fatalf("store.idx = %d, want %d (a fresh due credential must have been left unconsumed)", store.idx, maxCodexSweep)
	}
}

// TestRefreshDue_surfacesClaimFailure asserts that a claim/tx failure (handled=false, err!=nil —
// e.g. WithTx/Begin failing, or the claim's QueryRow.Scan erroring with something other than
// pgx.ErrNoRows) is NOT swallowed as "nothing due". Before the fix, RefreshDue checked `!handled`
// before `err`, so this case silently returned (0, nil) forever.
func TestRefreshDue_surfacesClaimFailure(t *testing.T) {
	sealer := testSealer(t)
	claimErr := errors.New("claim: tx begin failed")
	store := &fakeSystemCodexStore{claimErr: claimErr}
	svc := &CodexTokenService{Sealer: sealer, Now: time.Now, SystemStore: store}

	n, err := svc.RefreshDue(context.Background())
	if err == nil {
		t.Fatal("expected a non-nil error from RefreshDue, got nil")
	}
	if !errors.Is(err, claimErr) {
		t.Fatalf("expected error to wrap claimErr, got %v", err)
	}
	if n != 0 {
		t.Fatalf("refreshed = %d, want 0", n)
	}
	if len(store.excludeSeen) != 1 {
		t.Fatalf("expected exactly 1 refreshOneDue call (sweep must stop on claim failure), got %d", len(store.excludeSeen))
	}
}

func TestRefresh_preservesPlanWhenIdTokenOmitted(t *testing.T) {
	sealer := testSealer(t)
	sealedOldAccess, err := sealer.Seal([]byte("old-token"))
	if err != nil {
		t.Fatal(err)
	}
	sealedOldRefresh, err := sealer.Seal([]byte("r-old"))
	if err != nil {
		t.Fatal(err)
	}
	pastExpiry := time.Now().Add(-time.Hour)
	existingPlan := "pro"
	store := &fakeCodexStore{cred: codexCredRow{
		SealedKeyRef:      &sealedOldAccess,
		OAuthRefreshToken: &sealedOldRefresh,
		OAuthAccessExpiry: &pastExpiry,
		ChatGPTPlan:       &existingPlan,
	}}
	oauth := &fakeCodexOAuth{refresh: func(string) (codexoauth.TokenSet, error) {
		return codexoauth.TokenSet{
			AccessToken: "new", RefreshToken: "r-new", Expiry: time.Now().Add(time.Hour),
			Claims: codexoauth.Claims{Plan: ""}, // id_token omitted this round -> Claims empty
		}, nil
	}}
	svc := &CodexTokenService{Sealer: sealer, Now: time.Now, Store: store, OAuth: oauth}
	if _, err := svc.Mint(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if store.wrotePlan != "pro" {
		t.Fatalf("wrotePlan = %q, want preserved existing plan %q", store.wrotePlan, "pro")
	}
}
