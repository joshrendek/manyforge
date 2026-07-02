package coding

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// The Slice-2 review-config handlers reuse the shared serveCoding/nilHandler/ring helpers from
// handler_test.go. These cover the pre-service paths (bad ids → 404, bad JSON → 400) plus the
// validation→400 mapping through the HTTP layer; the happy paths are integration-tested
// (TestReviewDimensionServiceCRUD).

func TestHandlerListDimensions_BadBusinessID(t *testing.T) {
	ring := newCodingTestRing(t)
	bearer := mintCodingBearer(t, ring, uuid.New())
	rec := serveCoding(nilHandler(), ring, http.MethodGet, "/businesses/not-a-uuid/review-dimensions", bearer, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerUpsertDimension_BadJSON(t *testing.T) {
	ring := newCodingTestRing(t)
	bearer := mintCodingBearer(t, ring, uuid.New())
	bid := uuid.New()
	rec := serveCoding(nilHandler(), ring, http.MethodPost,
		"/businesses/"+bid.String()+"/review-dimensions", bearer, []byte(`{not json`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandlerUpsertDimension_InvalidDimension exercises the validation→400 mapping through the
// HTTP layer: a well-formed body with an unknown dimension reaches the service, which rejects it
// with ErrValidation BEFORE any DB call (so a nil-DB service is fine here).
func TestHandlerUpsertDimension_InvalidDimension(t *testing.T) {
	ring := newCodingTestRing(t)
	bearer := mintCodingBearer(t, ring, uuid.New())
	bid := uuid.New()
	h := &Handler{ReviewDimSvc: &ReviewDimensionService{}}
	rec := serveCoding(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/review-dimensions", bearer,
		[]byte(`{"dimension":"kitchen-sink","min_severity":"info"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown dimension); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerDeleteDimension_BadDimID(t *testing.T) {
	ring := newCodingTestRing(t)
	bearer := mintCodingBearer(t, ring, uuid.New())
	bid := uuid.New()
	rec := serveCoding(nilHandler(), ring, http.MethodDelete,
		"/businesses/"+bid.String()+"/review-dimensions/not-a-uuid", bearer, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerPutReviewConfig_BadJSON(t *testing.T) {
	ring := newCodingTestRing(t)
	bearer := mintCodingBearer(t, ring, uuid.New())
	bid := uuid.New()
	rec := serveCoding(nilHandler(), ring, http.MethodPut,
		"/businesses/"+bid.String()+"/review-config", bearer, []byte(`{not json`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandlerReviewConfigRoutes_Registered confirms the new routes are mounted (a wrong method
// on a matched path returns 405, not 404).
func TestHandlerReviewConfigRoutes_Registered(t *testing.T) {
	ring := newCodingTestRing(t)
	mux := httpx.NewRouter(ring)
	mux.Group(func(pr chi.Router) {
		pr.Use(httpx.RequireAuth)
		nilHandler().ProtectedRoutes(pr)
	})
	bid := uuid.New()
	dimID := uuid.New()
	bearer := mintCodingBearer(t, ring, uuid.New())
	for _, path := range []string{
		"/businesses/" + bid.String() + "/review-dimensions",
		"/businesses/" + bid.String() + "/review-dimensions/" + dimID.String(),
		"/businesses/" + bid.String() + "/review-config",
	} {
		req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("PATCH %s → %d, want 405 (route registered)", path, rec.Code)
		}
	}
}
