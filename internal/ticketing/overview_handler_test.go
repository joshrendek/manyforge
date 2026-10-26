package ticketing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Spec 019: the overview has no path id and so no RequirePermission gate in front of it; the
// handler itself must refuse a request that carries no principal, before touching the service
// (a nil *db.DB here would panic if it were reached).
func TestSupportOverviewRequiresPrincipal(t *testing.T) {
	r := chi.NewRouter()
	NewHandler(&Service{}, nil, nil).OverviewRoutes(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/support/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"UNAUTHORIZED"`) {
		t.Fatalf("body = %s, want UNAUTHORIZED code", rec.Body.String())
	}
}
