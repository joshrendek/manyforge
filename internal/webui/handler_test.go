package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    {Data: []byte("<html>INDEX_MARKER</html>")},
		"assets/app.js": {Data: []byte("console.log('app')")},
	}
}

func TestSPA_ServesIndexAtRoot(t *testing.T) {
	h := newSPAHandler(testFS())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INDEX_MARKER") {
		t.Fatalf("body = %q, want to contain INDEX_MARKER", rec.Body.String())
	}
}

func TestSPA_FallbackForClientRoute(t *testing.T) {
	h := newSPAHandler(testFS())

	req := httptest.NewRequest(http.MethodGet, "/tickets/123", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INDEX_MARKER") {
		t.Fatalf("body = %q, want fallback to index.html", rec.Body.String())
	}
}

func TestSPA_ServesAsset(t *testing.T) {
	h := newSPAHandler(testFS())

	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/javascript") && !strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("Content-Type = %q, want text/javascript or application/javascript prefix", ct)
	}
}

func TestSPA_MissingAsset404(t *testing.T) {
	h := newSPAHandler(testFS())

	req := httptest.NewRequest(http.MethodGet, "/assets/nope.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "INDEX_MARKER") {
		t.Fatalf("body should not fall back to index.html for a missing asset with an extension")
	}
}

func TestSPA_TraversalRejected(t *testing.T) {
	h := newSPAHandler(testFS())

	req := httptest.NewRequest(http.MethodGet, "/../../etc/passwd", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 404 or 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "INDEX_MARKER") {
		t.Fatalf("traversal request must never resolve into the SPA filesystem")
	}
}

// On a custom portal hostname only the public page routes may fall back to
// index.html; any other miss must be a plain 404, or the web app would route an
// unknown path to the operator sign-in screen. Without the portal context the
// fallback is unchanged.
func TestSPA_PortalHostFallbackOnlyForPublicPages(t *testing.T) {
	h := newSPAHandler(testFS())
	business := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	serve := func(portal bool, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		if portal {
			req = req.WithContext(httpx.WithPortalBusiness(req.Context(), business))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	isIndex := func(rec *httptest.ResponseRecorder) bool {
		return rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "INDEX_MARKER")
	}

	for _, target := range []string{"/media/nope", "/nope.js", "/p/abc/extra", "/tickets/123", "/"} {
		rec := serve(true, target)
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "INDEX_MARKER") {
			t.Errorf("portal %s: status %d body %q, want plain 404", target, rec.Code, rec.Body.String())
		}
	}
	for _, target := range []string{"/p/abc", "/m/s/abc"} {
		if rec := serve(true, target); !isIndex(rec) {
			t.Errorf("portal %s: status %d, want index.html", target, rec.Code)
		}
	}
	if rec := serve(true, "/assets/app.js"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.log") {
		t.Errorf("portal real asset: status %d, want the file", rec.Code)
	}

	// Without the portal context every one of these still falls back as before.
	for _, target := range []string{"/media/nope", "/nope.js", "/p/abc/extra", "/tickets/123", "/", "/p/abc", "/m/s/abc"} {
		if rec := serve(false, target); !isIndex(rec) {
			t.Errorf("instance %s: status %d, want index.html fallback", target, rec.Code)
		}
	}
}
