package ticketing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// The PATCH send-as body is rejected before the service is reached when send_as
// is absent (an empty body must never silently clear it) or not a string/null.
func TestUpdateInboundAddressRejectsBadBodies(t *testing.T) {
	r := chi.NewRouter()
	NewIdentityHandler(&IdentityService{}).Routes(r)
	path := "/businesses/" + uuid.NewString() + "/inbound-addresses/" + uuid.NewString()

	for name, c := range map[string]struct {
		path, body string
		want       int
	}{
		"missing send_as":   {path, `{}`, http.StatusBadRequest},
		"numeric send_as":   {path, `{"send_as": 7}`, http.StatusBadRequest},
		"malformed address": {"/businesses/" + uuid.NewString() + "/inbound-addresses/not-a-uuid", `{"send_as": null}`, http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodPatch, c.path, strings.NewReader(c.body))
		req = req.WithContext(httpx.WithPrincipal(req.Context(), uuid.New()))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d (body %s)", name, w.Code, c.want, w.Body.String())
		}
	}
}
