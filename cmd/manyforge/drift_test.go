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
	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/agents/coding"
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/crm"
	"github.com/manyforge/manyforge/internal/feedback"
	"github.com/manyforge/manyforge/internal/githubapp"
	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/invitations"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/tenancy"
	"github.com/manyforge/manyforge/internal/ticketing"
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

// noop is an identity middleware. The drift test mounts route groups with no-op
// middleware in place of the production rate-limiters / permission gate: route
// registration is structural and never invokes the chain, so the gates' real
// behavior is irrelevant here (it is covered by the per-handler tests).
func noop(next http.Handler) http.Handler { return next }

// apiRoutes walks the FULL production /api/v1 router — every module, including the
// 002 inbound webhook and ticketing read slice — and returns the set of
// "METHOD /normalized/path" it serves. It mounts routes through the SAME
// mountAPIRoutes seam main uses, so the test's view of the route table cannot
// drift from production. Handlers are built with zero-value services and middleware
// is replaced with no-ops; route registration never invokes either.
func apiRoutes(t *testing.T) map[string]bool {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	mux := httpx.NewRouter(ring)
	mountAPIRoutes(mux, apiHandlers{
		account:          account.NewHandler(&account.Service{}),
		tenancy:          tenancy.NewHandler(&tenancy.Service{}),
		authz:            authz.NewHandler(&authz.Service{}),
		invitations:      invitations.NewHandler(&invitations.Service{}),
		ticketing:        ticketing.NewHandler(&ticketing.Service{}, nil, nil),
		identity:         ticketing.NewIdentityHandler(&ticketing.IdentityService{}),
		inboxWebhook:     inbox.NewWebhookHandler(nil, "", 0, inbox.Config{}, nil),
		bounce:           inbox.NewBounceHandler(nil, "", 0, nil),
		authLimit:        noop,
		ingestLimit:      noop,
		ticketsRead:      noop,
		ticketsReply:     noop,
		ticketsWrite:     noop,
		ticketsAssign:    noop,
		ticketsDelete:    noop,
		inboxManage:      noop,
		agents:           agents.NewHandler(nil),
		agentsConfigure:  noop,
		agentRuns:        agents.NewRunHandler(nil),
		agentsRun:        noop,
		accounting:       agents.NewAccountingHandler(nil),
		approvals:        agents.NewApprovalHandler(nil),
		agentsApprove:    noop,
		mcp:              agents.NewMCPServerHandler(nil, agents.NewMCPToolPolicyHandler(nil, nil)),
		mcpConfigure:     noop,
		crm:              crm.NewHandler(&crm.ContactService{}, &crm.CompanyService{}, &crm.ActivityService{}, nil, nil),
		crmRead:          noop,
		crmWrite:         noop,
		feedback:         feedback.NewHandler(&feedback.Service{}),
		feedbackPublic:   feedback.NewPublicHandler(nil, nil),
		feedbackRead:     noop,
		feedbackWrite:    noop,
		codingReviews:    &coding.Handler{},
		githubApp:        &githubapp.Handler{},
		connectorsManage: noop,
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

// specPath resolves an OpenAPI contract file relative to the repo root.
func specPath(parts ...string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(append([]string{root}, parts...)...)
}

// specRoutesFrom returns the set of "METHOD /normalized/path" declared in the
// OpenAPI contract at path.
func specRoutesFrom(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read openapi %s: %v", path, err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi %s: %v", path, err)
	}
	verbs := map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}
	out := map[string]bool{}
	for p, ops := range doc.Paths {
		for verb := range ops {
			if verbs[verb] {
				out[strings.ToUpper(verb)+" "+normalizePath(p)] = true
			}
		}
	}
	return out
}

// spec001Routes returns the operations declared in the spec-001 contract.
func spec001Routes(t *testing.T) map[string]bool {
	t.Helper()
	return specRoutesFrom(t, specPath("specs", "001-tenant-foundation", "contracts", "openapi.yaml"))
}

// spec002Routes returns the operations declared in the spec-002 contract.
func spec002Routes(t *testing.T) map[string]bool {
	t.Helper()
	return specRoutesFrom(t, specPath("specs", "002-support-desk", "contracts", "openapi.yaml"))
}

// spec003Routes returns the operations declared in the spec-003 contract, or an
// empty set if the contract file does not yet exist (so the untagged drift test
// does not fail before Task 9 creates the file; the contract-tagged drift_003_test
// enforces the full two-way check once the file is present).
func spec003Routes(t *testing.T) map[string]bool {
	t.Helper()
	p := specPath("specs", "003-agent-runtime", "contracts", "openapi.yaml")
	if _, err := os.Stat(p); err != nil {
		return map[string]bool{}
	}
	return specRoutesFrom(t, p)
}

// spec005Routes returns the operations declared in the spec-005 contract, or an
// empty set if the contract file does not yet exist (so the untagged drift test does
// not fail before Task 8 creates the file; a future contract-tagged drift_005_test
// enforces the full two-way check once the file is present).
func spec005Routes(t *testing.T) map[string]bool {
	t.Helper()
	p := specPath("specs", "005-crm-contacts-timeline", "contracts", "openapi.yaml")
	if _, err := os.Stat(p); err != nil {
		return map[string]bool{}
	}
	return specRoutesFrom(t, p)
}

// spec007Routes returns the operations declared in the spec-007 contract, or an
// empty set if the contract file does not yet exist (so the untagged drift test does
// not fail before the file is committed; the contract-tagged drift_007_test enforces
// the full two-way check once the file is present).
func spec007Routes(t *testing.T) map[string]bool {
	t.Helper()
	p := specPath("specs", "007-coding-review-agents", "contracts", "openapi.yaml")
	if _, err := os.Stat(p); err != nil {
		return map[string]bool{}
	}
	return specRoutesFrom(t, p)
}

// spec008Routes returns the operations declared in the spec-008 contract, or an
// empty set if the contract file does not yet exist (so the untagged drift test does
// not fail before the file is committed; the contract-tagged drift_008_test enforces
// the full two-way check once the file is present).
func spec008Routes(t *testing.T) map[string]bool {
	t.Helper()
	p := specPath("specs", "008-review-dimensions", "contracts", "openapi.yaml")
	if _, err := os.Stat(p); err != nil {
		return map[string]bool{}
	}
	return specRoutesFrom(t, p)
}

// spec009Routes returns the operations declared in the spec-009 contract, or an
// empty set if the contract file does not yet exist (so the untagged drift test does
// not fail before the file is committed; the contract-tagged drift_009_test enforces
// the full two-way check once the file is present).
func spec009Routes(t *testing.T) map[string]bool {
	t.Helper()
	p := specPath("specs", "009-github-app-review", "contracts", "openapi.yaml")
	if _, err := os.Stat(p); err != nil {
		return map[string]bool{}
	}
	return specRoutesFrom(t, p)
}

// spec006Routes returns the operations declared in the spec-006 contract, or an
// empty set if the contract file does not yet exist (so the untagged drift test does
// not fail before the file is committed; the contract-tagged drift_006_test enforces
// the full two-way check once the file is present).
func spec006Routes(t *testing.T) map[string]bool {
	t.Helper()
	p := specPath("specs", "006-feedback-boards", "contracts", "openapi.yaml")
	if _, err := os.Stat(p); err != nil {
		return map[string]bool{}
	}
	return specRoutesFrom(t, p)
}

// TestOpenAPIDrift fails if the router and the OpenAPI contracts disagree on which
// operations exist (T082): an operation specced (in spec 001) but not served, or an
// operation served but documented in NEITHER spec. Param-name and trailing-slash
// differences are normalized away.
//
// Direction 1 (spec→router) is checked against spec 001 only here, because some
// spec-002 operations (US2 reply/note/inbox-management) are documented ahead of
// their handlers; the US1 in-scope 002 operations are pinned by TestOpenAPIDrift002
// (cmd/manyforge/drift_002_test.go, contract build tag).
//
// Direction 2 (router→spec) is checked against the UNION of both contracts so a
// registered 002 route is not falsely flagged as undocumented while still catching
// any route served by the router but documented in no contract at all.
func TestOpenAPIDrift(t *testing.T) {
	routes := apiRoutes(t)
	spec001 := spec001Routes(t)

	documented := map[string]bool{}
	for op := range spec001 {
		documented[op] = true
	}
	for op := range spec002Routes(t) {
		documented[op] = true
	}
	spec003 := spec003Routes(t)
	spec003Available := len(spec003) > 0
	for op := range spec003 {
		documented[op] = true
	}
	spec005 := spec005Routes(t)
	spec005Available := len(spec005) > 0
	for op := range spec005 {
		documented[op] = true
	}
	spec007 := spec007Routes(t)
	spec007Available := len(spec007) > 0
	for op := range spec007 {
		documented[op] = true
	}
	spec008 := spec008Routes(t)
	spec008Available := len(spec008) > 0
	for op := range spec008 {
		documented[op] = true
	}
	spec009 := spec009Routes(t)
	spec009Available := len(spec009) > 0
	for op := range spec009 {
		documented[op] = true
	}
	spec006 := spec006Routes(t)
	spec006Available := len(spec006) > 0
	for op := range spec006 {
		documented[op] = true
	}

	var missing, undocumented []string
	for op := range spec001 {
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	for op := range routes {
		if !documented[op] {
			// When the spec-003 contract file does not yet exist, skip routes that
			// belong to the 003 surface (identified by /agents in the path) — they
			// will be pinned by TestOpenAPIDrift003 once the file is committed.
			// INERT post-commit: specs/003-agent-runtime/contracts/openapi.yaml now
			// exists, so spec003Available is always true and this branch never fires.
			// It remains only as a guard if that contract file is ever removed;
			// TestOpenAPIDrift003 enforces the strict spec-003 two-way check.
			if !spec003Available && strings.Contains(op, "/agents") {
				continue
			}
			// Likewise skip the spec-005 CRM surface (/contacts, /companies) until
			// Task 8 commits specs/005-crm-contacts-timeline/contracts/openapi.yaml.
			// Once that file exists spec005Available is true and these routes must be
			// documented (the strict two-way check is owned by Task 8's contract test).
			if !spec005Available && (strings.Contains(op, "/contacts") || strings.Contains(op, "/companies")) {
				continue
			}
			// Likewise skip the spec-007 code-review surface (/repo-connectors,
			// /code-reviews) until specs/007-coding-review-agents/contracts/openapi.yaml
			// is committed. Once that file exists spec007Available is true and these
			// routes must be documented (the strict two-way check is TestOpenAPIDrift007).
			if !spec007Available && (strings.Contains(op, "/repo-connectors") || strings.Contains(op, "/code-reviews")) {
				continue
			}
			// Likewise skip the spec-008 review-panel config surface (/review-dimensions,
			// /review-config) until specs/008-review-dimensions/contracts/openapi.yaml is
			// committed. Once that file exists spec008Available is true and these routes must
			// be documented (the strict two-way check is TestOpenAPIDrift008).
			if !spec008Available && (strings.Contains(op, "/review-dimensions") || strings.Contains(op, "/review-config")) {
				continue
			}
			// Likewise skip the spec-009 GitHub App surface (every 009 route contains
			// /github/) until specs/009-github-app-review/contracts/openapi.yaml is
			// committed. Once that file exists spec009Available is true and these routes
			// must be documented (the strict two-way check is TestOpenAPIDrift009).
			if !spec009Available && strings.Contains(op, "/github/") {
				continue
			}
			// Likewise skip the spec-006 feedback surface (every 006 route contains
			// /feedback) until specs/006-feedback-boards/contracts/openapi.yaml is
			// committed. Once that file exists spec006Available is true and these routes
			// must be documented (the strict two-way check is TestOpenAPIDrift006).
			if !spec006Available && strings.Contains(op, "/feedback") {
				continue
			}
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(missing)
	sort.Strings(undocumented)

	for _, op := range missing {
		t.Errorf("spec drift: %q is in 001 openapi.yaml but not served by the router", op)
	}
	for _, op := range undocumented {
		t.Errorf("spec drift: %q is served by the router but not in any openapi.yaml", op)
	}
}
