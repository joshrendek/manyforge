package work

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A missing, non-bearer, or malformed credential is rejected with the uniform 401 before any
// database access (the Service here has no DB, so reaching it would panic).
func TestSyncRejectsBadCredentialsBeforeDB(t *testing.T) {
	r := chi.NewRouter()
	NewSyncHandler(&Service{}, nil).Routes(r)
	for _, auth := range []string{
		"",
		"Basic dXNlcjpwYXNz",
		"Bearer ",
		"Bearer mfw_tooshort",
		"Bearer mfk_" + strings.Repeat("a", 43),
		"Bearer " + strings.Repeat("a", 47),
	} {
		req := httptest.NewRequest(http.MethodPost, "/work/sync", strings.NewReader(`{"since":0}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", auth, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, `"code":"UNAUTHORIZED"`) {
			t.Errorf("Authorization %q: body %s, want the uniform UNAUTHORIZED envelope", auth, body)
		}
	}
}
