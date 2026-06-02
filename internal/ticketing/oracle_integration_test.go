//go:build integration

package ticketing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// T067 (SC-006/FR-015): the no-oracle 404 collapse, proven end-to-end through the
// REAL HTTP chain. Three GETs for a ticket id that the caller cannot have —
//   (a) an id that does not exist,
//   (b) an id that exists in ANOTHER tenant, and
//   (c) an id in the caller's own business that the caller lacks tickets.read for —
// must return BYTE-IDENTICAL 404 responses. A missing ticket, a foreign ticket, and
// an unauthorized ticket are indistinguishable: no 403, no "exists but not yours"
// oracle. The source-level backstops (no StatusForbidden in any handler; the renderer
// collapses ErrForbidden→404) live in internal/security_regression/oracle_collapse_pin_test.go.

// newTicketReadRouter builds the read-slice HTTP chain exactly as cmd/manyforge does:
// AuthToPrincipal → RequireAuth → tickets.read RequirePermission → the read handlers.
// It returns the test server plus the keyring used to mint bearer tokens.
func newTicketReadRouter(t *testing.T, tdb *testdb.TestDB) (*httptest.Server, *auth.KeyRing) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	permResolve := func(ctx context.Context, tx pgx.Tx, pid, bid uuid.UUID) (httpx.Permissions, error) {
		return authz.Resolve(ctx, tx, pid, bid)
	}
	h := NewHandler(&Service{DB: tdb.App}, tdb.App, permResolve)
	ticketsRead := httpx.RequirePermission(tdb.App, permResolve, "tickets.read", func(r *http.Request) (uuid.UUID, error) {
		return uuid.Parse(chi.URLParam(r, "id"))
	})
	r := chi.NewRouter()
	r.Use(httpx.AuthToPrincipal(ring))
	r.Use(httpx.RequireAuth)
	r.Group(func(tk chi.Router) {
		tk.Use(ticketsRead)
		h.ProtectedRoutes(tk)
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, ring
}

func mintBearer(t *testing.T, ring *auth.KeyRing, pid uuid.UUID) string {
	t.Helper()
	tok, err := ring.Sign(pid, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("sign token for %s: %v", pid, err)
	}
	return tok
}

// getRaw issues an authenticated GET and returns the status + raw response body so
// callers can assert byte-level equality (the no-oracle property).
func getRaw(t *testing.T, srv *httptest.Server, bearer, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

// TestNoOracleHTTP404Parity drives the three denial cases through the real router and
// asserts byte-identical 404 responses (SC-006/FR-015).
func TestNoOracleHTTP404Parity(t *testing.T) {
	ctx, tdb := startReadDB(t)
	t1 := seedReadTenant(ctx, t, tdb)
	t2 := seedReadTenant(ctx, t, tdb)

	t1Ticket := uuid.New()
	seedTicket(ctx, t, tdb, t1, t1Ticket, "open", "normal", "t1-real", nil, nil, -time.Hour)
	t2Ticket := uuid.New()
	seedTicket(ctx, t, tdb, t2, t2Ticket, "open", "normal", "t2-secret", nil, nil, -time.Hour)

	srv, ring := newTicketReadRouter(t, tdb)
	readerTok := mintBearer(t, ring, t1.reader)     // t1 member WITH tickets.read
	noReaderTok := mintBearer(t, ring, t1.noReader) // t1 member whose custom role LACKS tickets.read

	path := func(biz, tid uuid.UUID) string {
		return "/businesses/" + biz.String() + "/tickets/" + tid.String()
	}

	// Control first: the reader CAN load a real t1 ticket. This proves the 404s below
	// are genuine denials, not a router that 404s everything.
	if statusOK, _ := getRaw(t, srv, readerTok, path(t1.master, t1Ticket)); statusOK != http.StatusOK {
		t.Fatalf("control: reader GET own ticket = %d, want 200 (router/auth misconfigured)", statusOK)
	}

	// (a) unknown id under t1 (caller can read) → handler GetTicket → ErrNotFound → 404.
	statusUnknown, bodyUnknown := getRaw(t, srv, readerTok, path(t1.master, uuid.New()))
	// (b) cross-tenant: t2's real ticket id under t1 (caller can read t1) → RLS hides → 404.
	statusCross, bodyCross := getRaw(t, srv, readerTok, path(t1.master, t2Ticket))
	// (c) forbidden: t1's real ticket, caller lacks tickets.read → RequirePermission → 404.
	statusForbidden, bodyForbidden := getRaw(t, srv, noReaderTok, path(t1.master, t1Ticket))

	for _, c := range []struct {
		name   string
		status int
	}{
		{"unknown", statusUnknown},
		{"cross-tenant", statusCross},
		{"forbidden", statusForbidden},
	} {
		if c.status != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", c.name, c.status)
		}
	}

	// The core no-oracle property: the three 404 bodies are byte-identical.
	if bodyUnknown != bodyCross || bodyUnknown != bodyForbidden {
		t.Errorf("oracle leak: 404 bodies differ\n unknown=%q\n cross=%q\n forbidden=%q",
			bodyUnknown, bodyCross, bodyForbidden)
	}
	// And it is the generic NOT_FOUND envelope (never a 403 / forbidden shape).
	if got := strings.TrimSpace(bodyUnknown); got != `{"code":"NOT_FOUND","message":"not found"}` {
		t.Errorf("404 body = %q, want the generic NOT_FOUND envelope", got)
	}
}
