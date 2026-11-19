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

func TestReplyTextLimitWithAttachments(t *testing.T) {
	router := chi.NewRouter()
	NewHandler(nil, nil, nil).WriteRoutes(router)
	path := "/businesses/" + uuid.NewString() + "/tickets/" + uuid.NewString() + "/reply"
	payload := `{"body_text":"` + strings.Repeat("x", int(httpx.MaxJSONBodyBytes)) +
		`","body_html":"<p>Example</p>","attachments":[{"filename":"example.txt","content":"RXhhbXBsZQ=="}]}`
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req = req.WithContext(httpx.WithPrincipal(req.Context(), uuid.New()))
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized reply returned %d", res.Code)
	}
}
