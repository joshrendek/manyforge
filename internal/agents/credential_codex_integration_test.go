//go:build integration

package agents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/codexoauth"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// codexIDToken builds an unsigned JWT (header.payload.sig) carrying the
// "https://api.openai.com/auth" namespaced claim that codexoauth.parseIDTokenClaims
// expects. Mirrors internal/codexoauth's makeIDToken test helper — that helper lives in
// a different package/test file, so it is replicated here rather than exported for tests.
// json.Marshal of these literal maps cannot fail, so the error is ignored (this must be
// safe to call from the mock server's handler goroutine, not just the test goroutine).
func codexIDToken(accountID, plan string) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	payload := map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  plan,
		},
	}
	return enc(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + enc(payload) + ".sig"
}

// codexRefreshMode is the mock's switchable behavior for a refresh_token grant.
type codexRefreshMode int32

const (
	codexRefreshOK codexRefreshMode = iota
	codexRefreshDeny
)

// codexMockServer fakes auth.openai.com's /oauth/token endpoint. The authorization_code grant
// (the PKCE paste-redirect exchange) always mints the initial token pair; the refresh_token
// grant's behavior is switched between sub-steps via an atomic mode, and each OK refresh mints a
// new incrementing token pair so the test can pin exactly which rotation landed in the DB.
type codexMockServer struct {
	srv        *httptest.Server
	mode       atomic.Int32
	refreshSeq atomic.Int32
	accountID  string
	plan       string
}

func newCodexMockServer(t *testing.T) *codexMockServer {
	t.Helper()
	m := &codexMockServer{
		accountID: "acct-int-test",
		plan:      "pro",
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *codexMockServer) setMode(mode codexRefreshMode) { m.mode.Store(int32(mode)) }

func (m *codexMockServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/oauth/token" {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		m.writeTokenSet(w, "access-initial", "refresh-initial")
	case "refresh_token":
		if codexRefreshMode(m.mode.Load()) == codexRefreshDeny {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
			return
		}
		n := m.refreshSeq.Add(1)
		m.writeTokenSet(w, "access-rotated-"+strconv.Itoa(int(n)), "refresh-rotated-"+strconv.Itoa(int(n)))
	default:
		http.Error(w, "unknown grant_type: "+r.Form.Get("grant_type"), http.StatusBadRequest)
	}
}

func (m *codexMockServer) writeTokenSet(w http.ResponseWriter, access, refresh string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"id_token":      codexIDToken(m.accountID, m.plan),
		"expires_in":    3600,
	})
}

// codexExpireNow forces the tenant's codex credential's access token into the past via the
// RLS-exempt Super pool (only a superuser/direct-SQL seam can do this — the service never
// exposes a "make this stale" operation).
func codexExpireNow(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID) {
	t.Helper()
	if _, err := tdb.Super.Exec(ctx,
		`UPDATE ai_provider_credential SET oauth_access_expiry = now() - interval '1 hour'
		 WHERE business_id=$1 AND provider='openai_codex'`, businessID); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
}

// codexReadTokens re-reads the codex credential's sealed columns directly (bypassing the
// service) so assertions about DB write-back don't depend on the service's own read path.
func codexReadTokens(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID) (sealedKey, sealedRefresh *string, expiry *time.Time) {
	t.Helper()
	var exp *time.Time
	var key, ref *string
	err := tdb.Super.QueryRow(ctx,
		`SELECT sealed_key_ref, oauth_refresh_token, oauth_access_expiry
		 FROM ai_provider_credential WHERE business_id=$1 AND provider='openai_codex'`,
		businessID).Scan(&key, &ref, &exp)
	if err != nil {
		t.Fatalf("read codex tokens: %v", err)
	}
	return key, ref, exp
}

// TestCodexOAuthIntegration drives the whole Codex OAuth path against a real Postgres (the
// testdb harness runs every migration, including 0096's SECURITY DEFINER refresh-sweep
// functions) and a mocked OpenAI auth server: PKCE connect, a per-run Resolve mint, a lazy
// refresh-on-expiry, the cross-tenant RefreshDue sweep (the first real-Postgres exercise of
// codex_claim_for_refresh's []string->text[] exclude param), and disconnect-on-invalid_grant.
// Steps share one tenant + mock server + service pair and run in sequence — each step's DB
// state is the precondition for the next.
func TestCodexOAuthIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedAgentTenant(ctx, t, tdb)
	sealer := newTestSealer(t)
	mock := newCodexMockServer(t)

	codexSvc := NewCodexTokenService(tdb.App, sealer, &codexoauth.Client{HTTP: mock.srv.Client(), AuthBase: mock.srv.URL}, 5*time.Minute)
	credSvc := &CredentialService{DB: tdb.App, Sealer: sealer, Codex: codexSvc}

	var pendingID uuid.UUID

	t.Run("pkce_connect", func(t *testing.T) {
		ps, err := codexSvc.StartPKCE(ctx, ten.principalID, ten.businessID, CodexConnectInput{DefaultModel: "gpt-5.6-sol"})
		if err != nil {
			t.Fatalf("StartPKCE: %v", err)
		}
		if ps.PendingID == uuid.Nil {
			t.Fatal("StartPKCE: PendingID is nil")
		}
		if ps.AuthorizeURL == "" {
			t.Fatal("StartPKCE: AuthorizeURL empty")
		}
		pendingID = ps.PendingID

		// Reproduce the pasted redirect URL: state must equal the pending jti, plus an auth code.
		redirectURL := "http://localhost:1455/auth/callback?state=" + pendingID.String() + "&code=auth-code-1"
		cs, err := codexSvc.ExchangePKCE(ctx, ten.principalID, ten.businessID, pendingID, redirectURL)
		if err != nil {
			t.Fatalf("ExchangePKCE: %v", err)
		}
		if cs.Status != "approved" {
			t.Fatalf("ExchangePKCE status = %q, want approved", cs.Status)
		}
		if cs.CredentialID == uuid.Nil {
			t.Fatal("ExchangePKCE: CredentialID is nil")
		}

		// The pending row is single-use: a second exchange for the same jti must not find it.
		if _, err := codexSvc.ExchangePKCE(ctx, ten.principalID, ten.businessID, pendingID, redirectURL); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("second ExchangePKCE: want ErrNotFound, got %v", err)
		}
	})

	t.Run("resolve_mints_access_token", func(t *testing.T) {
		rc, err := credSvc.Resolve(ctx, ten.principalID, ten.businessID, "openai_codex")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if rc.APIKey != "access-initial" {
			t.Fatalf("Resolve.APIKey = %q, want access-initial (fresh token, no refresh)", rc.APIKey)
		}
	})

	t.Run("lazy_refresh_on_expiry", func(t *testing.T) {
		codexExpireNow(ctx, t, tdb, ten.businessID)
		// mock is already in its zero-value codexRefreshOK mode here — no earlier sub-test
		// has switched it to Deny yet — so an explicit setMode(codexRefreshOK) would be a no-op.

		rc, err := credSvc.Resolve(ctx, ten.principalID, ten.businessID, "openai_codex")
		if err != nil {
			t.Fatalf("Resolve after expiry: %v", err)
		}
		if rc.APIKey != "access-rotated-1" {
			t.Fatalf("Resolve.APIKey = %q, want access-rotated-1", rc.APIKey)
		}

		// Re-read the DB directly: the rotated refresh token must be sealed and stored, and
		// the expiry must have moved to the future.
		_, sealedRefresh, expiry := codexReadTokens(ctx, t, tdb, ten.businessID)
		if sealedRefresh == nil {
			t.Fatal("oauth_refresh_token is nil after rotation")
		}
		rt, err := sealer.Open(*sealedRefresh)
		if err != nil {
			t.Fatalf("unseal stored refresh token: %v", err)
		}
		if string(rt) != "refresh-rotated-1" {
			t.Fatalf("stored refresh token = %q, want refresh-rotated-1", rt)
		}
		if expiry == nil || !expiry.After(time.Now()) {
			t.Fatalf("oauth_access_expiry = %v, want in the future", expiry)
		}

		// A subsequent Mint must hit the fast path and return the SAME rotated token — if it
		// silently refreshed again, refreshSeq would tick and this would read access-rotated-2.
		tok, err := codexSvc.Mint(ctx, ten.principalID, ten.businessID)
		if err != nil {
			t.Fatalf("Mint after rotation: %v", err)
		}
		if tok != "access-rotated-1" {
			t.Fatalf("Mint after rotation = %q, want cached access-rotated-1 (unexpected extra refresh)", tok)
		}
	})

	t.Run("cross_tenant_sweep_refresh_due", func(t *testing.T) {
		// Seed a second, wholly independent tenant and connect its own openai_codex credential
		// through the same PKCE-connect flow used for tenant 1. The whole reason the 0096
		// functions are SECURITY DEFINER is to see across tenants — a sweep proven against only
		// one tenant's row never actually exercises that, so this sub-test needs two.
		ten2 := seedAgentTenant(ctx, t, tdb)
		ps2, err := codexSvc.StartPKCE(ctx, ten2.principalID, ten2.businessID, CodexConnectInput{DefaultModel: "gpt-5.6-sol"})
		if err != nil {
			t.Fatalf("StartPKCE (tenant 2): %v", err)
		}
		redirect2 := "http://localhost:1455/auth/callback?state=" + ps2.PendingID.String() + "&code=auth-code-2"
		cs2, err := codexSvc.ExchangePKCE(ctx, ten2.principalID, ten2.businessID, ps2.PendingID, redirect2)
		if err != nil {
			t.Fatalf("ExchangePKCE (tenant 2): %v", err)
		}
		if cs2.Status != "approved" {
			t.Fatalf("ExchangePKCE (tenant 2) status = %q, want approved", cs2.Status)
		}

		// Force BOTH tenants' codex credentials due for refresh in a single statement.
		codexExpireNow(ctx, t, tdb, ten.businessID)
		if _, err := tdb.Super.Exec(ctx,
			`UPDATE ai_provider_credential SET oauth_access_expiry = now() - interval '1 hour'
			 WHERE business_id=$1 AND provider='openai_codex'`, ten2.businessID); err != nil {
			t.Fatalf("force expiry (tenant 2): %v", err)
		}
		mock.setMode(codexRefreshOK)

		// This is the first real-Postgres exercise of codex_claim_for_refresh's []string ->
		// text[] exclude-param encoding and the 0096 SECURITY DEFINER wiring end to end. A
		// function-resolution or type error here is a real bug, not a test bug. n == 2 in one
		// sweep call also proves the exclude accumulator advances past the first claimed row
		// (tenant 1's) to reach tenant 2's distinct row, rather than reclaiming the same one.
		n, err := codexSvc.RefreshDue(ctx)
		if err != nil {
			t.Fatalf("RefreshDue: %v", err)
		}
		if n != 2 {
			t.Fatalf("RefreshDue refreshed = %d, want 2 (one per tenant)", n)
		}

		// Both tenants' credentials must show a genuine rotation: sealed and non-nil, a
		// "refresh-rotated-*" token distinct from what each held going in (and from each
		// other, so the same row wasn't just rotated twice), and an expiry moved back into
		// the future.
		priorRefreshToken := map[uuid.UUID]string{
			ten.businessID:  "refresh-rotated-1", // set by the lazy_refresh_on_expiry sub-test
			ten2.businessID: "refresh-initial",   // set by tenant 2's pkce-connect grant
		}
		seenRefreshToken := make(map[string]uuid.UUID, 2)
		for _, biz := range []uuid.UUID{ten.businessID, ten2.businessID} {
			sealedKey, sealedRefresh, expiry := codexReadTokens(ctx, t, tdb, biz)
			if sealedKey == nil || sealedRefresh == nil {
				t.Fatalf("sweep left tokens nil for business %s (expected a rotated set, not a disconnect)", biz)
			}
			rt, err := sealer.Open(*sealedRefresh)
			if err != nil {
				t.Fatalf("unseal stored refresh token for business %s: %v", biz, err)
			}
			if !strings.HasPrefix(string(rt), "refresh-rotated-") {
				t.Fatalf("stored refresh token for business %s = %q, want a refresh-rotated-* value", biz, rt)
			}
			if string(rt) == priorRefreshToken[biz] {
				t.Fatalf("stored refresh token for business %s = %q, want it advanced past %q (sweep didn't rotate this row)", biz, rt, priorRefreshToken[biz])
			}
			if other, dup := seenRefreshToken[string(rt)]; dup {
				t.Fatalf("stored refresh token %q reused across businesses %s and %s — sweep rotated one row twice instead of surfacing both", rt, other, biz)
			}
			seenRefreshToken[string(rt)] = biz
			if expiry == nil || !expiry.After(time.Now()) {
				t.Fatalf("oauth_access_expiry for business %s = %v, want moved to the future by the sweep", biz, expiry)
			}
		}
	})

	t.Run("disconnect_on_invalid_grant", func(t *testing.T) {
		codexExpireNow(ctx, t, tdb, ten.businessID)
		mock.setMode(codexRefreshDeny)

		_, err := credSvc.Resolve(ctx, ten.principalID, ten.businessID, "openai_codex")
		if !errors.Is(err, errs.ErrCodexDisconnected) {
			t.Fatalf("Resolve after invalid_grant: want ErrCodexDisconnected, got %v", err)
		}

		views, err := credSvc.List(ctx, ten.principalID, ten.businessID)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var found bool
		for _, v := range views {
			if v.Provider != "openai_codex" {
				continue
			}
			found = true
			if v.ConnectionStatus != "disconnected" {
				t.Fatalf("ConnectionStatus = %q, want disconnected", v.ConnectionStatus)
			}
		}
		if !found {
			t.Fatal("openai_codex credential missing from List after disconnect")
		}

		sealedKey, sealedRefresh, _ := codexReadTokens(ctx, t, tdb, ten.businessID)
		if sealedKey != nil || sealedRefresh != nil {
			t.Fatalf("tokens not cleared after disconnect: sealed_key_ref=%v oauth_refresh_token=%v", sealedKey, sealedRefresh)
		}
	})
}
