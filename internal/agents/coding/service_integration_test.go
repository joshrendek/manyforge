//go:build integration

package coding

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

type codingSeed struct {
	businessID  uuid.UUID
	principalID uuid.UUID
}

func newTestSealerCoding(t *testing.T) *crypto.Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	s, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

// seedCodingTenant inserts the minimal tenant/principal/business rows needed by
// the coding integration test. Mirrors connectors.seedConnectorTenant.
func seedCodingTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) codingSeed {
	t.Helper()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}

	masterID := uuid.New()
	agentID := uuid.New()
	benignRoleID := uuid.New()
	ownerAcctID := uuid.New()
	ownerHumanID := uuid.New()
	ownerEmail := "coding-owner-" + masterID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{ownerAcctID, ownerEmail}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{ownerHumanID, ownerAcctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'CodingCo','active',now(),now())`,
			[]any{masterID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{masterID}},
		{`INSERT INTO principal (id,kind,home_business_id,tenant_root_id,created_at) VALUES ($1,'agent',$2,$2,now())`,
			[]any{agentID, masterID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{ownerHumanID, masterID, ownerRole}},
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'coding-read','CodingRead',false,now())`,
			[]any{benignRoleID, masterID}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.read')`,
			[]any{benignRoleID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{agentID, masterID, benignRoleID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return codingSeed{businessID: masterID, principalID: agentID}
}

// startCoding starts a testdb and seeds a tenant. Returns the context, the DB, and the seed.
func startCoding(t *testing.T) (context.Context, *testdb.TestDB, codingSeed) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb, seedCodingTenant(ctx, t, tdb)
}

// ---------------------------------------------------------------------------
// GitHub stub server
// ---------------------------------------------------------------------------

type githubStub struct {
	prJSON      []byte // canned PR response
	reviewPosts [][]byte
	postCount   atomic.Int64
}

// startGitHubStub creates an httptest server that:
//   - GET /repos/o/r/pulls/{n} → 200 with prJSON
//   - POST /repos/o/r/pulls/{n}/reviews → 201 with a stub review object
//
// It records the request bodies of all POST review calls.
func startGitHubStub(t *testing.T, prJSON []byte) (*httptest.Server, *githubStub) {
	t.Helper()
	stub := &githubStub{prJSON: prJSON}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(stub.prJSON)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/o/r/pulls/1/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			stub.reviewPosts = append(stub.reviewPosts, body)
			stub.postCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 42, "html_url": "https://github.com/o/r/pull/1#pullrequestreview-42"}`))
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, stub
}

// ---------------------------------------------------------------------------
// Fake runners
// ---------------------------------------------------------------------------

// validFakeRunner writes a valid review.json into spec.OutputDir when Run is called.
type validFakeRunner struct {
	summary string
}

func (r *validFakeRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	// This runner is retained for future Task-7 async e2e use but is not invoked
	// by Enqueue (which only queues pending rows). See TestCodeReviewTrigger subtests.
	_ = spec
	return sandbox.SandboxResult{ExitCode: 0}, nil
}

// malformedFakeRunner writes invalid JSON into spec.OutputDir.
type malformedFakeRunner struct{}

func (r *malformedFakeRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	_ = spec
	return sandbox.SandboxResult{ExitCode: 0}, nil
}

// fakeClone is the injectable clone seam: just creates destDir and a placeholder file.
// The allowPrivate parameter is accepted but ignored — no real network is involved.
func fakeClone(_ context.Context, _, _, _, destDir string, _ bool) error {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(destDir+"/README.md", []byte("fake repo\n"), 0o600)
}

// ---------------------------------------------------------------------------
// Test setup helper
// ---------------------------------------------------------------------------

// codingEnv bundles a shared sealer + the RepoConnectorService + CodeReviewService
// so secrets sealed at connector creation can be opened at Enqueue time.
type codingEnv struct {
	Sealer *crypto.Sealer
	Repos  *connectors.RepoConnectorService
}

// newCodingEnv creates a shared sealer and the connector service that uses it.
func newCodingEnv(t *testing.T, tdb *testdb.TestDB) *codingEnv {
	t.Helper()
	sealer := newTestSealerCoding(t)
	repos := &connectors.RepoConnectorService{
		DB:    tdb.App,
		Vault: secrets.NewVault(sealer),
	}
	return &codingEnv{Sealer: sealer, Repos: repos}
}

// buildService constructs a CodeReviewService wired to a real DB, the shared
// RepoConnectorService (so sealer is shared), and the provided fake runner + cred resolver.
func buildService(
	t *testing.T,
	tdb *testdb.TestDB,
	env *codingEnv,
	runner sandbox.SandboxRunner,
	credResolver AICredentialResolver,
) *CodeReviewService {
	t.Helper()
	return &CodeReviewService{
		DB:       tdb.App,
		Repos:    env.Repos,
		Sandbox:  runner,
		Creds:    credResolver,
		Image:    "opencode:stub",
		WorkRoot: t.TempDir(),
		Timeout:  30 * time.Second,
		Clone:    fakeClone,
		// fakeCred resolves to api.anthropic.com; allow it so the egress pre-flight passes.
		EgressAllow: netsafe.ParseHostAllowlist("api.anthropic.com"),
	}
}

// createRepoConnector creates a repo_connector pointing at the stub GitHub server
// and returns its UUID. Uses the shared env so the sealer matches.
func createRepoConnector(
	ctx context.Context,
	t *testing.T,
	env *codingEnv,
	seed codingSeed,
	stubURL string,
) uuid.UUID {
	t.Helper()
	id, err := env.Repos.Create(ctx, seed.principalID, seed.businessID, connectors.CreateRepoConnectorInput{
		Type:                "github",
		DisplayName:         "Test GitHub",
		BaseURL:             stubURL,
		Repo:                "o/r",
		AllowPrivateBaseURL: true, // stub is 127.0.0.1
		APIToken:            "ghp_test_token",
	})
	if err != nil {
		t.Fatalf("create repo connector: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Test: TestCodeReviewTrigger
// ---------------------------------------------------------------------------

func TestCodeReviewTrigger(t *testing.T) {
	ctx, tdb, seed := startCoding(t)

	prJSON := []byte(`{
		"number": 1,
		"title": "Test PR",
		"state": "open",
		"merged": false,
		"head": {"sha": "abc123deadbeef", "ref": "feature/test"},
		"base": {"ref": "main"}
	}`)

	srv, _ := startGitHubStub(t, prJSON)

	// Agent ID — used by FakeCredResolver (which ignores it, returns canned cred).
	agentID := uuid.New()

	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey:   "k",
		BaseURL:  "https://api.anthropic.com",
		Model:    "anthropic/claude-3-5-sonnet",
		Provider: "anthropic",
	}}

	t.Run("succeeded", func(t *testing.T) {
		// After the Trigger→Enqueue split, Enqueue inserts a pending row and returns
		// immediately without running the pipeline. The synchronous end-to-end
		// assertions (status=succeeded, posted review, findings persisted) are
		// deferred to the async worker integration test (Task 7, manyforge-elo).
		t.Skip("async e2e rebuilt in Task 7 once the worker lands (manyforge-elo)")
	})

	t.Run("malformed_json_marks_failed", func(t *testing.T) {
		// After the Trigger→Enqueue split, Enqueue does not invoke the sandbox, so
		// the malformed-JSON path (which happens inside runJob) cannot be exercised
		// synchronously. Rebuilt in the async worker integration test (Task 7).
		t.Skip("async e2e rebuilt in Task 7 once the worker lands (manyforge-elo)")
	})

	t.Run("rls_cross_tenant_get_not_found", func(t *testing.T) {
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, srv.URL)

		svc := buildService(t, tdb, env, &validFakeRunner{}, fakeCred)

		// Enqueue inserts a pending row; that's sufficient to exercise RLS on Get.
		cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if cr.Status != "pending" {
			t.Errorf("Enqueue status: want %q, got %q", "pending", cr.Status)
		}

		// Seed a second independent tenant.
		otherSeed := seedCodingTenant(ctx, t, tdb)

		// Attempt to Get under other tenant's principal — must get ErrNotFound.
		_, err = svc.Get(ctx, otherSeed.principalID, otherSeed.businessID, cr.ID)
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("cross-tenant Get: want ErrNotFound, got %v", err)
		}
	})

	t.Run("two_runs_malformed_second_no_extra_post", func(t *testing.T) {
		// After Trigger→Enqueue split: Enqueue only inserts pending rows; pipeline
		// execution (and malformed-JSON failure path) moves to runJob in the worker.
		// The multi-run pipeline assertions are rebuilt in Task 7 (manyforge-elo).
		t.Skip("async e2e rebuilt in Task 7 once the worker lands (manyforge-elo)")
	})

	t.Run("enqueue_inserts_pending_row", func(t *testing.T) {
		// New integration subtest: verifies Enqueue inserts a pending row with the
		// correct principal_id and agent_id — assertions the old synchronous subtests
		// skipped over because they called Trigger synchronously.
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, srv.URL)
		svc := buildService(t, tdb, env, &validFakeRunner{}, fakeCred)

		cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if cr.Status != "pending" {
			t.Errorf("Enqueue status: want %q, got %q", "pending", cr.Status)
		}
		if cr.PRNumber != 1 {
			t.Errorf("Enqueue PRNumber: want 1, got %d", cr.PRNumber)
		}

		// Verify the row is in the DB with status=pending, correct principal+agent.
		var dbStatus string
		var dbPrincipal, dbAgent uuid.UUID
		if err := tdb.Super.QueryRow(ctx,
			`SELECT status, principal_id, agent_id FROM code_review WHERE id=$1`, cr.ID,
		).Scan(&dbStatus, &dbPrincipal, &dbAgent); err != nil {
			t.Fatalf("read code_review: %v", err)
		}
		if dbStatus != "pending" {
			t.Errorf("DB status: want pending, got %q", dbStatus)
		}
		if dbPrincipal != seed.principalID {
			t.Errorf("DB principal_id: want %v, got %v", seed.principalID, dbPrincipal)
		}
		if dbAgent != agentID {
			t.Errorf("DB agent_id: want %v, got %v", agentID, dbAgent)
		}
	})
}

// ---------------------------------------------------------------------------
// Compile-time check: *connectors.RepoConnectorService satisfies repoResolver.
// ---------------------------------------------------------------------------

var _ repoResolver = (*connectors.RepoConnectorService)(nil)

// ---------------------------------------------------------------------------
// Helper: ensure WithPrincipal from testdb.App works as a serviceDB.
// ---------------------------------------------------------------------------

func TestServiceDBInterfaceCompiles(t *testing.T) {
	// This test just verifies the interface is satisfied at compile time.
	// The actual assert is the var _ line above; this function exists so
	// 'go test' exercises the file.
	_ = func(ctx context.Context, db serviceDB, id uuid.UUID) {
		_ = db.WithPrincipal(ctx, id, func(pgx.Tx) error { return nil })
	}
}
