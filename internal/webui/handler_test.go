package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
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
