package portaldomain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

type fakeSvc struct {
	create func(hostname string) (Domain, error)
	list   func() ([]Domain, string, error)
	verify func() (Domain, error)
	del    func() error
}

func (f *fakeSvc) Create(_ context.Context, _, _ uuid.UUID, hostname string) (Domain, error) {
	return f.create(hostname)
}
func (f *fakeSvc) List(_ context.Context, _, _ uuid.UUID, _ string, _ int) ([]Domain, string, error) {
	return f.list()
}
func (f *fakeSvc) Verify(_ context.Context, _, _, _ uuid.UUID) (Domain, error) { return f.verify() }
func (f *fakeSvc) Delete(_ context.Context, _, _, _ uuid.UUID) error           { return f.del() }

func sampleDomain() Domain {
	return Domain{
		ID: uuid.New(), BusinessID: uuid.New(), Hostname: "portal.example.test",
		Verification: TXTRecord{Name: "_manyforge-portal.portal.example.test", Value: "mf-portal-verify=abc"},
		CreatedAt:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func serve(t *testing.T, svc *fakeSvc, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{svc: svc, instanceHost: "app.example.test"}
	r := chi.NewRouter()
	h.Routes(r)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(httpx.WithPrincipal(req.Context(), uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestHandlerCreateShape(t *testing.T) {
	d := sampleDomain()
	svc := &fakeSvc{create: func(host string) (Domain, error) {
		if host != "portal.example.test" {
			t.Errorf("hostname passed = %q", host)
		}
		return d, nil
	}}
	rec := serve(t, svc, http.MethodPost, "/businesses/"+uuid.NewString()+"/portal-domains", `{"hostname":"portal.example.test"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != d.ID.String() || got["hostname"] != "portal.example.test" || got["verified"] != false || got["verified_at"] != nil {
		t.Errorf("unexpected body: %v", got)
	}
	if got["created_at"] != "2026-01-02T03:04:05Z" {
		t.Errorf("created_at = %v", got["created_at"])
	}
	dns := got["dns"].(map[string]any)
	txt := dns["verification_txt"].(map[string]any)
	if txt["name"] != "_manyforge-portal.portal.example.test" || txt["value"] != "mf-portal-verify=abc" {
		t.Errorf("verification_txt = %v", txt)
	}
	target := dns["target"].(map[string]any)
	if target["type"] != "CNAME" || target["name"] != "portal.example.test" || target["value"] != "app.example.test" {
		t.Errorf("target = %v", target)
	}
}

func TestHandlerVerifiedFields(t *testing.T) {
	d := sampleDomain()
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	d.VerifiedAt = &at
	svc := &fakeSvc{verify: func() (Domain, error) { return d, nil }}
	rec := serve(t, svc, http.MethodPost, fmt.Sprintf("/businesses/%s/portal-domains/%s/verify", uuid.New(), d.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["verified"] != true || got["verified_at"] != "2026-02-03T04:05:06Z" {
		t.Errorf("unexpected body: %v", got)
	}
}

func TestHandlerCreateMissingHostname(t *testing.T) {
	for _, body := range []string{`{}`, `{"hostname":"   "}`} {
		rec := serve(t, &fakeSvc{}, http.MethodPost, "/businesses/"+uuid.NewString()+"/portal-domains", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d", body, rec.Code)
		}
	}
}

func TestHandlerNonUUIDPathIs404(t *testing.T) {
	good := uuid.NewString()
	cases := []struct{ method, path string }{
		{http.MethodGet, "/businesses/nope/portal-domains"},
		{http.MethodPost, "/businesses/nope/portal-domains"},
		{http.MethodPost, "/businesses/" + good + "/portal-domains/nope/verify"},
		{http.MethodDelete, "/businesses/" + good + "/portal-domains/nope"},
		{http.MethodDelete, "/businesses/nope/portal-domains/" + good},
	}
	for _, c := range cases {
		rec := serve(t, &fakeSvc{}, c.method, c.path, `{"hostname":"x.example.test"}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d", c.method, c.path, rec.Code)
		}
	}
}

func TestHandlerServiceErrors(t *testing.T) {
	path := "/businesses/" + uuid.NewString() + "/portal-domains"
	conflict := &fakeSvc{create: func(string) (Domain, error) { return Domain{}, fmt.Errorf("x: %w", errs.ErrConflict) }}
	if rec := serve(t, conflict, http.MethodPost, path, `{"hostname":"portal.example.test"}`); rec.Code != http.StatusConflict {
		t.Errorf("conflict: status = %d", rec.Code)
	}
	missing := &fakeSvc{verify: func() (Domain, error) { return Domain{}, fmt.Errorf("x: %w", errs.ErrNotFound) }}
	if rec := serve(t, missing, http.MethodPost, path+"/"+uuid.NewString()+"/verify", ""); rec.Code != http.StatusNotFound {
		t.Errorf("not found: status = %d", rec.Code)
	}
}

func TestHandlerDeleteNoContent(t *testing.T) {
	svc := &fakeSvc{del: func() error { return nil }}
	rec := serve(t, svc, http.MethodDelete, "/businesses/"+uuid.NewString()+"/portal-domains/"+uuid.NewString(), "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("status = %d body = %q", rec.Code, rec.Body)
	}
}

func TestHandlerListCursor(t *testing.T) {
	path := "/businesses/" + uuid.NewString() + "/portal-domains?limit=5"
	last := &fakeSvc{list: func() ([]Domain, string, error) { return []Domain{sampleDomain()}, "", nil }}
	rec := serve(t, last, http.MethodGet, path, "")
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got["next_cursor"] != nil || len(got["items"].([]any)) != 1 {
		t.Errorf("exhausted: status=%d body=%s", rec.Code, rec.Body)
	}
	more := &fakeSvc{list: func() ([]Domain, string, error) { return nil, "tok", nil }}
	rec = serve(t, more, http.MethodGet, path, "")
	got = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["next_cursor"] != "tok" || len(got["items"].([]any)) != 0 {
		t.Errorf("more: body=%s", rec.Body)
	}
}
