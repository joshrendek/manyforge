package main

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/agents/coding"
	"github.com/manyforge/manyforge/internal/analytics"
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/automations"
	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/crm"
	"github.com/manyforge/manyforge/internal/feedback"
	"github.com/manyforge/manyforge/internal/githubapp"
	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/invitations"
	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/telemetry"
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

// testHandlers builds the FULL production handler set with zero-value services and no-op
// middleware. Shared by the drift walker and the authorization-wiring test so the two cannot
// disagree about what the router actually mounts.
func testHandlers() apiHandlers {
	credentials := agents.NewCredentialHandler(&agents.CredentialService{})
	credentials.SetCodex(&agents.CodexTokenService{})
	return apiHandlers{
		account:          account.NewHandler(&account.Service{}),
		tenancy:          tenancy.NewHandler(&tenancy.Service{}),
		authz:            authz.NewHandler(&authz.Service{}),
		invitations:      invitations.NewHandler(&invitations.Service{}),
		ticketing:        ticketing.NewHandler(&ticketing.Service{}, nil, nil),
		identity:         ticketing.NewIdentityHandler(&ticketing.IdentityService{}),
		inboxWebhook:     inbox.NewWebhookHandler(nil, "", 0, inbox.Config{}, nil),
		bounce:           inbox.NewBounceHandler(nil, "", 0, nil),
		authLimit:        noop,
		tenantMergeLimit: noop,
		ingestLimit:      noop,
		ticketsRead:      noop,
		ticketsReply:     noop,
		ticketsWrite:     noop,
		ticketsAssign:    noop,
		ticketsDelete:    noop,
		inboxManage:      noop,
		agents:           agents.NewHandler(nil),
		credentials:      credentials,
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
		feedbackPublic:   feedback.NewPublicHandler(nil, nil, nil),
		feedbackRead:     noop,
		feedbackWrite:    noop,
		mailing:          mailing.NewHandler(&mailing.Service{}),
		mailingSetup:     mailing.NewSetupHandler(mailing.SetupConfig{}),
		mailingPublic:    mailing.NewPublicHandler(&mailing.Service{}, nil, nil, nil),
		mailingWebhook:   mailing.NewWebhookHandler(nil, nil, nil),
		mailingRead:      noop,
		mailingWrite:     noop,
		mailingSend:      noop,
		automations:      automations.NewHandler(&automations.Service{}),
		telemetry:        telemetry.NewHandler(&telemetry.Service{}),
		telemetryPublic:  &telemetry.PublicHandler{},
		telemetryRead:    noop,
		telemetryWrite:   noop,
		analytics:        analytics.NewHandler(&analytics.Service{}),
		analyticsPublic:  &analytics.PublicHandler{},
		codingReviews:    &coding.Handler{},
		githubApp:        &githubapp.Handler{},
		connectors:       connectors.NewHandler(&connectors.Service{}),
		connWebhookH:     connectors.NewWebhookHandler(nil, nil, nil, nil),
		connectorsManage: noop,
	}
}

// apiRoutes enumerates every application route through the production registration
// seam, with optional modules enabled. Infrastructure routes and the SPA catch-all
// live outside this seam. No zero-service handler is executed by the walker.
func apiRoutes(t *testing.T) map[string]bool {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	mux := httpx.NewRouter(ring)
	mountAPIRoutes(mux, testHandlers())

	routes := map[string]bool{}
	walk := func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		op := strings.ToUpper(method) + " " + normalizePath(route)
		if routes[op] {
			t.Fatalf("duplicate normalized router operation %q (from %s %s)", op, method, route)
		}
		routes[op] = true
		return nil
	}
	if err := chi.Walk(mux, walk); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	return routes
}

func TestTenantMergeRoutesUseDedicatedRateLimit(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	ring, err := auth.NewKeyRing(
		"manyforge", "manyforge-api", "k1", priv,
		map[string]ed25519.PublicKey{"k1": pub},
	)
	if err != nil {
		t.Fatalf("key ring: %v", err)
	}
	called := false
	handlers := testHandlers()
	handlers.tenantMergeLimit = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			httpx.WriteJSON(w, http.StatusTooManyRequests, httpx.ErrorBody{
				Code: "RATE_LIMITED", Message: "too many requests",
			})
		})
	}
	router := httpx.NewRouter(ring)
	mountAPIRoutes(router, handlers)
	token, err := ring.Sign(uuid.New(), time.Hour, time.Now())
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/tenant-merges/"+uuid.NewString(),
		nil,
	)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if !called || response.Code != http.StatusTooManyRequests {
		t.Fatalf("tenant merge limiter called/status = %t/%d, want true/429",
			called, response.Code)
	}
}

type canonicalSpec struct {
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas    map[string]yaml.Node `yaml:"schemas"`
		Parameters map[string]yaml.Node `yaml:"parameters"`
	} `yaml:"components"`
}

// loadCanonicalSpec is the only contract loader used by the application tests.
func loadCanonicalSpec(t *testing.T) canonicalSpec {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "api", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read canonical OpenAPI: %v", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parse canonical OpenAPI: %v", err)
	}
	var checkKeys func(*yaml.Node)
	checkKeys = func(node *yaml.Node) {
		if node.Kind == yaml.MappingNode {
			seen := make(map[string]bool, len(node.Content)/2)
			for i := 0; i < len(node.Content); i += 2 {
				key := node.Content[i]
				if seen[key.Value] {
					t.Fatalf("duplicate OpenAPI key %q at line %d", key.Value, key.Line)
				}
				seen[key.Value] = true
			}
		}
		for _, child := range node.Content {
			checkKeys(child)
		}
	}
	checkKeys(&root)
	var doc canonicalSpec
	if err := root.Decode(&doc); err != nil {
		t.Fatalf("decode canonical OpenAPI: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("canonical OpenAPI declares no paths")
	}
	return doc
}

func canonicalRoutes(t *testing.T, doc canonicalSpec) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	operationIDs := map[string]string{}
	for path, operations := range doc.Paths {
		for verb, node := range operations {
			switch strings.ToUpper(verb) {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
				http.MethodDelete, http.MethodOptions, http.MethodHead,
				http.MethodTrace, http.MethodConnect:
			default:
				continue
			}
			op := strings.ToUpper(verb) + " " + normalizePath(path)
			if out[op] {
				t.Fatalf("duplicate normalized OpenAPI operation %q (from %s %s)", op, verb, path)
			}
			var operation struct {
				ID string `yaml:"operationId"`
			}
			if err := node.Decode(&operation); err != nil {
				t.Fatalf("decode %s: %v", op, err)
			}
			if operation.ID == "" {
				t.Fatalf("%s has no operationId", op)
			}
			if previous, exists := operationIDs[operation.ID]; exists {
				t.Fatalf("duplicate operationId %q: %s and %s", operation.ID, previous, op)
			}
			operationIDs[operation.ID] = op
			out[op] = true
		}
	}
	return out
}

// TestOpenAPIDrift requires a two-way match for all enabled application modules.
func TestOpenAPIDrift(t *testing.T) {
	routes := apiRoutes(t)
	documented := canonicalRoutes(t, loadCanonicalSpec(t))
	var missing, undocumented []string
	for op := range documented {
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	for op := range routes {
		if !documented[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(missing)
	sort.Strings(undocumented)
	for _, op := range missing {
		t.Errorf("contract drift: %q is documented but not served", op)
	}
	for _, op := range undocumented {
		t.Errorf("contract drift: %q is served but undocumented", op)
	}
}
