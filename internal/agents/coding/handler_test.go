package coding

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// newCodingTestRing builds an in-memory Ed25519 key ring for handler tests.
func newCodingTestRing(t *testing.T) *auth.KeyRing {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return ring
}

func mintCodingBearer(t *testing.T, ring *auth.KeyRing, pid uuid.UUID) string {
	t.Helper()
	tok, err := ring.Sign(pid, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

// serveCoding mounts the coding Handler behind RequireAuth and serves one request.
func serveCoding(h *Handler, ring *auth.KeyRing, method, target, bearer string, body []byte) *httptest.ResponseRecorder {
	mux := httpx.NewRouter(ring)
	mux.Group(func(pr chi.Router) {
		pr.Use(httpx.RequireAuth)
		h.ProtectedRoutes(pr)
	})
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, bodyReader)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// nilHandler builds a Handler with nil services (sufficient for auth/validation tests
// that never reach the service layer).
func nilHandler() *Handler {
	return &Handler{RepoSvc: nil, ReviewSvc: nil}
}

// --- createRepoConnector ---

func TestHandlerCreateRepoConnector_MissingPrincipal(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bid := uuid.New()
	rec := serveCoding(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/repo-connectors",
		"", // no bearer → no principal
		[]byte(`{}`),
	)
	if rec.Code != http.StatusUnauthorized {
		// RequireAuth middleware returns 401 before we ever reach the handler,
		// so missing principal → 401 (not 404) at this layer.
		t.Logf("body: %s", rec.Body.String())
	}
	// RequireAuth returns 401; handler itself would return 404 if principal absent
	// after auth passes — but RequireAuth fires first. Accept 401.
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 401 or 404", rec.Code)
	}
}

func TestHandlerCreateRepoConnector_BadBusinessID(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	rec := serveCoding(h, ring, http.MethodPost,
		"/businesses/not-a-uuid/repo-connectors",
		bearer,
		[]byte(`{}`),
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerCreateRepoConnector_BadJSON(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	bid := uuid.New()
	rec := serveCoding(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/repo-connectors",
		bearer,
		[]byte(`not-json`),
	)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- triggerReview ---

func TestHandlerTriggerReview_MissingPrincipal(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bid := uuid.New()
	rec := serveCoding(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/code-reviews",
		"", // no bearer
		[]byte(`{}`),
	)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 401 or 404", rec.Code)
	}
}

func TestHandlerTriggerReview_BadBusinessID(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	rec := serveCoding(h, ring, http.MethodPost,
		"/businesses/not-a-uuid/code-reviews",
		bearer,
		[]byte(`{}`),
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerTriggerReview_BadJSON(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	bid := uuid.New()
	rec := serveCoding(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/code-reviews",
		bearer,
		[]byte(`not-json`),
	)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerTriggerReview_BadUUIDs(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	bid := uuid.New()
	body := `{"agent_id":"not-uuid","repo_connector_id":"also-not","pr_number":1}`
	rec := serveCoding(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/code-reviews",
		bearer,
		[]byte(body),
	)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerTriggerReview_ZeroPRNumber(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	bid := uuid.New()
	body := `{"agent_id":"` + uuid.New().String() + `","repo_connector_id":"` + uuid.New().String() + `","pr_number":0}`
	rec := serveCoding(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/code-reviews",
		bearer,
		[]byte(body),
	)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- getReview ---

func TestHandlerGetReview_MissingPrincipal(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bid := uuid.New()
	rid := uuid.New()
	rec := serveCoding(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/code-reviews/"+rid.String(),
		"", // no bearer
		nil,
	)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 401 or 404", rec.Code)
	}
}

func TestHandlerGetReview_BadBusinessID(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	rid := uuid.New()
	rec := serveCoding(h, ring, http.MethodGet,
		"/businesses/not-a-uuid/code-reviews/"+rid.String(),
		bearer,
		nil,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerGetReview_BadReviewID(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	bid := uuid.New()
	rec := serveCoding(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/code-reviews/not-a-uuid",
		bearer,
		nil,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- listRepoConnectors ---

func TestHandlerListRepoConnectors_MissingPrincipal(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bid := uuid.New()
	rec := serveCoding(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/repo-connectors",
		"", // no bearer → RequireAuth returns 401
		nil,
	)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 401 or 404", rec.Code)
	}
}

func TestHandlerListRepoConnectors_BadBusinessID(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	rec := serveCoding(h, ring, http.MethodGet,
		"/businesses/not-a-uuid/repo-connectors",
		bearer,
		nil,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- deleteRepoConnector ---

func TestHandlerDeleteRepoConnector_MissingPrincipal(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bid := uuid.New()
	rcID := uuid.New()
	rec := serveCoding(h, ring, http.MethodDelete,
		"/businesses/"+bid.String()+"/repo-connectors/"+rcID.String(),
		"", // no bearer
		nil,
	)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 401 or 404", rec.Code)
	}
}

func TestHandlerDeleteRepoConnector_BadBusinessID(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	rcID := uuid.New()
	rec := serveCoding(h, ring, http.MethodDelete,
		"/businesses/not-a-uuid/repo-connectors/"+rcID.String(),
		bearer,
		nil,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerDeleteRepoConnector_BadRCID(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	bid := uuid.New()
	rec := serveCoding(h, ring, http.MethodDelete,
		"/businesses/"+bid.String()+"/repo-connectors/not-a-uuid",
		bearer,
		nil,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- listReviews ---

func TestHandlerListReviews_MissingPrincipal(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bid := uuid.New()
	rec := serveCoding(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/code-reviews",
		"", // no bearer
		nil,
	)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 401 or 404", rec.Code)
	}
}

func TestHandlerListReviews_BadBusinessID(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	bearer := mintCodingBearer(t, ring, uuid.New())
	rec := serveCoding(h, ring, http.MethodGet,
		"/businesses/not-a-uuid/code-reviews",
		bearer,
		nil,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandlerRoutes_Registered verifies all six routes are actually mounted
// (wrong method → 405, not 404, proves the path matched).
func TestHandlerRoutes_Registered(t *testing.T) {
	ring := newCodingTestRing(t)
	h := nilHandler()
	mux := httpx.NewRouter(ring)
	mux.Group(func(pr chi.Router) {
		pr.Use(httpx.RequireAuth)
		h.ProtectedRoutes(pr)
	})

	bid := uuid.New()
	rid := uuid.New()
	rcID := uuid.New()
	bearer := mintCodingBearer(t, ring, uuid.New())

	cases := []struct {
		method string
		path   string
	}{
		// Wrong-method probes: PATCH on each route → 405 (path matched, method not).
		{http.MethodPatch, "/businesses/" + bid.String() + "/repo-connectors"},
		{http.MethodPatch, "/businesses/" + bid.String() + "/repo-connectors/" + rcID.String()},
		{http.MethodPatch, "/businesses/" + bid.String() + "/code-reviews"},
		{http.MethodPatch, "/businesses/" + bid.String() + "/code-reviews/" + rid.String()},
		// Original POST-only probes kept with PUT.
		{http.MethodPut, "/businesses/" + bid.String() + "/repo-connectors"},
		{http.MethodPut, "/businesses/" + bid.String() + "/code-reviews"},
		{http.MethodPut, "/businesses/" + bid.String() + "/code-reviews/" + rid.String()},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		// chi returns 405 when the path matches but method doesn't.
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s → %d, want 405", tc.method, tc.path, rec.Code)
		}
	}
}
