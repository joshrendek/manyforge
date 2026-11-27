package portaldomain

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

const (
	testInstance = "app.example.test"
	testPortal   = "portal.example.test"
)

var testBusiness = uuid.MustParse("11111111-1111-1111-1111-111111111111")

type routingHarness struct {
	lookups   int
	lookupErr error
	nextCalls int
	gotBiz    uuid.UUID
	gotOK     bool
	h         http.Handler
}

func newHarness(instance string) *routingHarness {
	rh := &routingHarness{}
	lookup := func(_ context.Context, host string) (uuid.UUID, bool, error) {
		rh.lookups++
		if rh.lookupErr != nil {
			return uuid.Nil, false, rh.lookupErr
		}
		if host == testPortal {
			return testBusiness, true, nil
		}
		return uuid.Nil, false, nil
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rh.nextCalls++
		rh.gotBiz, rh.gotOK = httpx.PortalBusiness(r.Context())
		w.WriteHeader(http.StatusTeapot)
	})
	rh.h = HostRouting(instance, lookup, next)
	return rh
}

func (rh *routingHarness) do(method, host, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	rh.h.ServeHTTP(rec, req)
	return rec
}

func TestInstanceHostUnchanged(t *testing.T) {
	rh := newHarness(testInstance)
	for _, p := range []string{"/api/v1/auth/login", "/login", "/metrics"} {
		before := rh.nextCalls
		rh.do("GET", testInstance, p)
		if rh.nextCalls != before+1 {
			t.Errorf("%s did not reach next", p)
		}
		if rh.gotOK {
			t.Errorf("%s: portal business must not be set on the instance host", p)
		}
	}
	if rh.lookups != 0 {
		t.Errorf("lookups = %d, want 0", rh.lookups)
	}
}

func TestNoInstanceHostConfiguredPassesEverything(t *testing.T) {
	rh := newHarness("")
	for _, host := range []string{"anything.example.test", "bad_host!", ""} {
		rec := rh.do("GET", host, "/metrics")
		if rec.Code != http.StatusTeapot {
			t.Errorf("host %q: status %d", host, rec.Code)
		}
	}
	if rh.lookups != 0 {
		t.Errorf("lookups = %d", rh.lookups)
	}
}

func TestHealthOnAnyHost(t *testing.T) {
	rh := newHarness(testInstance)
	for _, p := range []string{"/healthz", "/readyz"} {
		if rec := rh.do("GET", "nobody.example.test", p); rec.Code != http.StatusTeapot {
			t.Errorf("%s: status %d", p, rec.Code)
		}
	}
	if rh.lookups != 0 {
		t.Errorf("lookups = %d", rh.lookups)
	}
}

func TestInternalHostsAreInstance(t *testing.T) {
	rh := newHarness(testInstance)
	for _, host := range []string{"10.0.0.7:8080", "localhost:8081", "api.ns.svc.cluster.local", "manyforge:8080"} {
		if rec := rh.do("GET", host, "/metrics"); rec.Code != http.StatusTeapot {
			t.Errorf("host %q: status %d", host, rec.Code)
		}
	}
	if rh.lookups != 0 {
		t.Errorf("lookups = %d", rh.lookups)
	}
}

var allowCases = []struct{ method, path string }{
	{"GET", "/p/abc"}, {"HEAD", "/p/abc"},
	{"GET", "/m/s/tok"}, {"HEAD", "/m/s/tok"},
	{"GET", "/m/confirm/tok"}, {"HEAD", "/m/confirm/tok"}, {"POST", "/m/confirm/tok"},
	{"GET", "/api/v1/feedback/public/k/posts"}, {"POST", "/api/v1/feedback/public/k/posts"}, {"OPTIONS", "/api/v1/feedback/public/k/posts"},
	{"GET", "/api/v1/mailing/public/x"}, {"POST", "/api/v1/mailing/public/x"}, {"OPTIONS", "/api/v1/mailing/public/x"},
	{"GET", "/a.js"}, {"HEAD", "/a.js"},
	{"POST", "/a/e"}, {"OPTIONS", "/a/e"},
	{"GET", "/media/x/y.png"}, {"HEAD", "/media/x"},
	{"GET", "/main.js"}, {"GET", "/styles.css"}, {"GET", "/favicon.ico"}, {"GET", "/logo.svg"},
	{"HEAD", "/logo.png"}, {"GET", "/font.woff2"},
}

func TestPortalHostAllowlist(t *testing.T) {
	for _, host := range []string{testPortal, "PORTAL.Example.Test:443", "portal.example.test."} {
		rh := newHarness(testInstance)
		for _, c := range allowCases {
			rh.gotOK = false
			rh.gotBiz = uuid.Nil
			rec := rh.do(c.method, host, c.path)
			if rec.Code != http.StatusTeapot {
				t.Errorf("host %q %s %s: status %d, want pass-through", host, c.method, c.path, rec.Code)
				continue
			}
			if !rh.gotOK || rh.gotBiz != testBusiness {
				t.Errorf("host %q %s %s: context business = %v,%v", host, c.method, c.path, rh.gotBiz, rh.gotOK)
			}
		}
	}
}

func TestPortalHostDenies(t *testing.T) {
	rh := newHarness(testInstance)
	cases := []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/login"}, {"POST", "/api/v1/auth/login"}, {"GET", "/api/v1/me"},
		{"GET", "/metrics"}, {"GET", "/cli/install.sh"},
		{"POST", "/api/v1/support/portal/tickets"}, {"POST", "/api/v1/mailing/s2s/k/subscribers"},
		{"POST", "/api/v1/inbound/email/x"}, {"DELETE", "/p/abc"}, {"GET", "/assets/x.js"},
		{"GET", "/m/u/tok"}, {"POST", "/m/u/tok"}, {"GET", "/m/o/tok"}, {"GET", "/m/c/tok"}, {"GET", "/m/b/abc/logo"},
		{"GET", "/p/"}, {"GET", "/m/s/"}, {"GET", "/m/confirm/"}, {"POST", "/p/abc"},
		{"POST", "/a.js"}, {"GET", "/a/e"}, {"POST", "/media/x"}, {"POST", "/main.js"},
		{"GET", "/api/passkeys/login/begin"}, {"GET", "/sub/main.js"}, {"GET", "/main.js.map"},
		{"DELETE", "/api/v1/feedback/public/k"}, {"GET", "/api/v1/feedback/public"},
	}
	for _, c := range cases {
		rec := rh.do(c.method, testPortal, c.path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", c.method, c.path, rec.Code)
		}
	}
	if rh.nextCalls != 0 {
		t.Errorf("next called %d times", rh.nextCalls)
	}
}

func TestPortalHostPathTricks(t *testing.T) {
	rh := newHarness(testInstance)
	for _, p := range []string{"/p/../api/v1/auth/login", "//api/v1/me", "/p/abc/../../login", "/p/abc/", "/p/./abc"} {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Host = testPortal
		req.URL.Path = p
		rec := httptest.NewRecorder()
		rh.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %q: status %d", p, rec.Code)
		}
	}
	req := httptest.NewRequest("GET", "/p/abc%2f..%2flogin", nil)
	req.Host = testPortal
	if req.URL.RawPath == "" {
		t.Fatal("test setup: RawPath not populated")
	}
	rec := httptest.NewRecorder()
	rh.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("encoded slash: status %d", rec.Code)
	}
	if rh.nextCalls != 0 {
		t.Errorf("next called %d times", rh.nextCalls)
	}
}

func TestForwardedHostIsIgnored(t *testing.T) {
	rh := newHarness(testInstance)
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Host = testPortal
	req.Header.Set("X-Forwarded-Host", testInstance)
	req.Header.Set("Forwarded", "host="+testInstance)
	rec := httptest.NewRecorder()
	rh.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || rh.nextCalls != 0 {
		t.Errorf("forwarded host influenced routing: status %d", rec.Code)
	}
}

func TestUnknownAndInvalidHosts(t *testing.T) {
	rh := newHarness(testInstance)
	if rec := rh.do("GET", "nobody.example.test", "/p/abc"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown host status %d", rec.Code)
	}
	if rh.lookups != 1 {
		t.Errorf("lookups = %d, want 1", rh.lookups)
	}
	rh.lookups = 0
	for _, host := range []string{"bad_host!.example.test", "*.example.test", ""} {
		if rec := rh.do("GET", host, "/p/abc"); rec.Code != http.StatusNotFound {
			t.Errorf("host %q status %d", host, rec.Code)
		}
	}
	if rh.lookups != 0 || rh.nextCalls != 0 {
		t.Errorf("lookups=%d next=%d, want 0/0", rh.lookups, rh.nextCalls)
	}
}

func TestLookupErrorFailsClosed(t *testing.T) {
	rh := newHarness(testInstance)
	rh.lookupErr = errors.New("db down: secret detail")
	rec := rh.do("GET", testPortal, "/p/abc")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if want := `"code":"UNAVAILABLE"`; !contains(body, want) {
		t.Errorf("body %q missing %s", body, want)
	}
	if contains(body, "secret detail") {
		t.Errorf("body leaks error: %q", body)
	}
	if rh.nextCalls != 0 {
		t.Error("next called on lookup error")
	}
	if rec := rh.do("GET", testInstance, "/login"); rec.Code != http.StatusTeapot {
		t.Errorf("instance host status %d", rec.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
