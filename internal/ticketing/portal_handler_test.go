package ticketing

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type portalDenyLimiter struct{}

func (portalDenyLimiter) Allow(string) bool { return false }

func TestPortalHTTPRejectsUntrustedIdentityBeforeDatabase(t *testing.T) {
	h := NewPortalHandler(&PortalService{}, nil)
	r := chi.NewRouter()
	h.CustomerRoutes(r)
	token := "mfsp_" + strings.Repeat("A", 43)
	cases := []struct{ auth, external, name string }{
		{"", "tph:" + uuid.New().String(), "Customer"},
		{"Bearer short", "tph:" + uuid.New().String(), "Customer"},
		{"Bearer " + token, "customer@example.com", "Customer"},
		{"Bearer " + token, "tph:not-a-uuid", "Customer"},
		{"Bearer " + token, "tph:" + uuid.New().String(), strings.Repeat("x", 101)},
		{"Bearer " + token, "tph:" + uuid.New().String(), "control\x01"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/support/portal/contact", nil)
		req.Header.Set("Authorization", c.auth)
		req.Header.Set("X-MF-External-User", c.external)
		req.Header.Set("X-MF-External-Name", c.name)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Fatalf("invalid identity returned %d", w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/support/portal/contact", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Set("X-MF-External-User", "tph:"+uuid.New().String())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("duplicate authorization returned %d", w.Code)
	}
}

func TestPortalHTTPAdmissionPrecedesDatabaseAndBody(t *testing.T) {
	h := NewPortalHandler(&PortalService{}, nil)
	h.global = portalDenyLimiter{}
	r := chi.NewRouter()
	h.CustomerRoutes(r)
	req := httptest.NewRequest(http.MethodPost, "/support/portal/tickets", strings.NewReader(strings.Repeat("x", 128<<10)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Fatalf("global admission returned %d", w.Code)
	}
}

func TestPortalJSONBoundsAndStrictFields(t *testing.T) {
	for _, body := range []string{`{"body":"ok","identity":"forged"}`, `{"body":"one"} {"body":"two"}`, `{"body":"` + strings.Repeat("x", 64<<10) + `"}`} {
		var out struct {
			Body string `json:"body"`
		}
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if portalDecode(w, req, &out) || w.Code != 400 {
			t.Fatal("unbounded or ambiguous JSON accepted")
		}
	}
	var out struct {
		Body string `json:"body"`
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/?token=forbidden", strings.NewReader(`{"body":"ok"}`))
	if portalDecode(w, req, &out) || w.Code != 400 {
		t.Fatal("query-string credentials allowed")
	}
}

func TestPortalCanonicalIdentifiersAndContactAddress(t *testing.T) {
	for _, v := range []string{"", uuid.Nil.String(), "550E8400-E29B-41D4-A716-446655440000", "{550e8400-e29b-41d4-a716-446655440000}"} {
		if _, err := portalUUID(v); !errors.Is(err, ErrPortalInvalid) {
			t.Fatal("noncanonical UUID accepted")
		}
	}
	for _, v := range []string{"Name <user@example.com>", "user@example.com\r\nBcc: victim@example.com", "user@localhost"} {
		if _, err := portalEmail(v); !errors.Is(err, ErrPortalInvalid) {
			t.Fatal("unsafe contact address accepted")
		}
	}
	id := uuid.New().String()
	if !portalIdentity("tph:"+id, "Customer") {
		t.Fatal("canonical external identity rejected")
	}
	if portalText(strings.Repeat("x", 20001), 20000) || portalText("\x00", 20000) {
		t.Fatal("unsafe body accepted")
	}
}

// Real TCP reads prove admission releases even if the client never finishes JSON.
func TestPortalSlowBodyReleasesAdmission(t *testing.T) {
	h := NewPortalHandler(&PortalService{}, nil)
	h.timeout = 100 * time.Millisecond
	h.inflight = make(chan struct{}, 1)
	completed := make(chan struct{}, 2)
	server := httptest.NewServer(h.bounded(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out struct {
			Body string `json:"body"`
		}
		if portalDecode(w, r, &out) {
			portalJSON(w, 200, out)
		}
		completed <- struct{}{}
	})))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 100\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("slow body retained admission")
	}
	if _, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil {
		t.Fatalf("slow request received no bounded error: %v", err)
	}
	response, err := http.Post(server.URL, "application/json", strings.NewReader(`{"body":"authorized success"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		t.Fatalf("normal request failed: %d", response.StatusCode)
	}
}
