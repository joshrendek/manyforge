package main

import (
	"crypto/ed25519"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/invitations"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// normalizePath collapses every `{param}` segment to `{}` and trims a trailing
// slash, so the router's param names (e.g. {principalID}) and chi's index-route
// trailing slash compare equal to the spec's ({principalId}, no slash).
func normalizePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "{}"
		}
	}
	out := strings.Join(segs, "/")
	if len(out) > 1 {
		out = strings.TrimSuffix(out, "/")
	}
	return out
}

// apiRoutes walks the production /api/v1 router and returns the set of
// "METHOD /normalized/path" it serves. Handlers are mounted with zero-value
// services — route registration never invokes them.
func apiRoutes(t *testing.T) map[string]bool {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	acctH := account.NewHandler(&account.Service{})
	mux := httpx.NewRouter(ring)
	mux.Route("/api/v1", func(r chi.Router) {
		r.Group(func(p chi.Router) { acctH.PublicRoutes(p) })
		r.Group(func(pr chi.Router) {
			pr.Use(httpx.RequireAuth)
			acctH.ProtectedRoutes(pr)
			tenancy.NewHandler(&tenancy.Service{}).ProtectedRoutes(pr)
			authz.NewHandler(&authz.Service{}).ProtectedRoutes(pr)
			invitations.NewHandler(&invitations.Service{}).ProtectedRoutes(pr)
		})
	})

	routes := map[string]bool{}
	walk := func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimPrefix(route, "/api/v1")
		if route == "" {
			route = "/"
		}
		routes[method+" "+normalizePath(route)] = true
		return nil
	}
	if err := chi.Walk(mux, walk); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	return routes
}

// specRoutes returns the set of "METHOD /normalized/path" declared in the OpenAPI
// contract.
func specRoutes(t *testing.T) map[string]bool {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	specPath := filepath.Join(root, "specs", "001-tenant-foundation", "contracts", "openapi.yaml")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi: %v", err)
	}
	verbs := map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}
	out := map[string]bool{}
	for path, ops := range doc.Paths {
		for verb := range ops {
			if verbs[verb] {
				out[strings.ToUpper(verb)+" "+normalizePath(path)] = true
			}
		}
	}
	return out
}

// TestOpenAPIDrift fails if the router and the OpenAPI contract disagree on which
// operations exist (T082): an operation specced but not served, or served but not
// documented. Param-name and trailing-slash differences are normalized away.
func TestOpenAPIDrift(t *testing.T) {
	routes := apiRoutes(t)
	spec := specRoutes(t)

	var missing, undocumented []string
	for op := range spec {
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	for op := range routes {
		if !spec[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(missing)
	sort.Strings(undocumented)

	for _, op := range missing {
		t.Errorf("spec drift: %q is in openapi.yaml but not served by the router", op)
	}
	for _, op := range undocumented {
		t.Errorf("spec drift: %q is served by the router but not in openapi.yaml", op)
	}
}
