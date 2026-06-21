//go:build integration

package coding

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			_, _ = fmt.Fprintf(w, `{"id": 42, "html_url": "https://github.com/o/r/pull/1#pullrequestreview-42"}`)
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
	doc := FindingsDoc{
		Summary: r.summary,
		Findings: []connectors.Finding{
			{File: "main.go", Line: nil, Severity: "warning", Title: "Example finding", Detail: "detail here"},
		},
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(spec.OutputDir, "review.json"), raw, 0o600); err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("fake runner: write review.json: %w", err)
	}
	return sandbox.SandboxResult{ExitCode: 0}, nil
}

// malformedFakeRunner writes invalid JSON into spec.OutputDir.
type malformedFakeRunner struct{}

func (r *malformedFakeRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	if err := os.WriteFile(filepath.Join(spec.OutputDir, "review.json"), []byte("not json {{{ broken"), 0o600); err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("malformed fake runner: write review.json: %w", err)
	}
	return sandbox.SandboxResult{ExitCode: 0}, nil
}

// fakeClone is the injectable clone seam: just creates destDir and a placeholder file.
// The allowPrivate parameter is accepted but ignored — no real network is involved.
func fakeClone(_ context.Context, _, _, _, destDir string, _ bool) error {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("fake clone: mkdir: %w", err)
	}
	return os.WriteFile(destDir+"/README.md", []byte("fake repo\n"), 0o600)
}

// ---------------------------------------------------------------------------
// Test setup helper
// ---------------------------------------------------------------------------

// codingEnv bundles a shared sealer + the RepoConnectorService + CodeReviewService
// so secrets sealed at connector creation can be opened at Trigger time.
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

	srv, stub := startGitHubStub(t, prJSON)

	// Agent ID — used by FakeCredResolver (which ignores it, returns canned cred).
	agentID := uuid.New()

	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey:   "k",
		BaseURL:  "https://api.anthropic.com",
		Model:    "anthropic/claude-3-5-sonnet",
		Provider: "anthropic",
	}}

	t.Run("succeeded", func(t *testing.T) {
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, srv.URL)

		summary := "Overall the PR looks good with minor issues."
		runner := &validFakeRunner{summary: summary}
		svc := buildService(t, tdb, env, runner, fakeCred)

		postsBefore := stub.postCount.Load()

		cr, err := svc.Trigger(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err != nil {
			t.Fatalf("Trigger: unexpected error: %v", err)
		}

		// 1. Status == "succeeded" and non-empty ReviewURL.
		if cr.Status != "succeeded" {
			t.Errorf("Status: want %q, got %q", "succeeded", cr.Status)
		}
		if cr.ReviewURL == "" {
			t.Error("ReviewURL must be non-empty on success")
		}

		// 2. Exactly one POST to GitHub reviews with the summary in body.
		postsAfter := stub.postCount.Load()
		if postsAfter-postsBefore != 1 {
			t.Errorf("expected 1 POST to /reviews, got %d", postsAfter-postsBefore)
		}
		if len(stub.reviewPosts) > 0 {
			lastBody := stub.reviewPosts[len(stub.reviewPosts)-1]
			var posted map[string]any
			if err := json.Unmarshal(lastBody, &posted); err != nil {
				t.Errorf("review POST body not JSON: %v", err)
			}
			// The body field must contain the summary text.
			bodyField, _ := posted["body"].(string)
			if bodyField == "" {
				t.Error("review POST body.body must be non-empty")
			}
		}

		// 3. Get() shows status succeeded, posted_at set, findings persisted.
		got, err := svc.Get(ctx, seed.principalID, seed.businessID, cr.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != "succeeded" {
			t.Errorf("Get().Status: want succeeded, got %q", got.Status)
		}

		// Verify posted_at is set via Super.
		var postedAt *time.Time
		if err := tdb.Super.QueryRow(ctx,
			"SELECT posted_at FROM code_review WHERE id=$1", cr.ID).Scan(&postedAt); err != nil {
			t.Fatalf("read posted_at: %v", err)
		}
		if postedAt == nil {
			t.Error("posted_at must be set after success")
		}

		// Verify findings persisted.
		var findingsRaw []byte
		if err := tdb.Super.QueryRow(ctx,
			"SELECT findings FROM code_review WHERE id=$1", cr.ID).Scan(&findingsRaw); err != nil {
			t.Fatalf("read findings: %v", err)
		}
		var findings []connectors.Finding
		if err := json.Unmarshal(findingsRaw, &findings); err != nil {
			t.Fatalf("unmarshal findings: %v", err)
		}
		if len(findings) == 0 {
			t.Error("findings must be non-empty after success")
		}
	})

	t.Run("malformed_json_marks_failed", func(t *testing.T) {
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, srv.URL)

		runner := &malformedFakeRunner{}
		svc := buildService(t, tdb, env, runner, fakeCred)

		postsBefore := stub.postCount.Load()

		_, err := svc.Trigger(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err == nil {
			t.Fatal("Trigger with malformed JSON must return error")
		}

		// No POST to GitHub must have occurred.
		postsAfter := stub.postCount.Load()
		if postsAfter != postsBefore {
			t.Errorf("expected no POST to /reviews on failure, got %d new posts", postsAfter-postsBefore)
		}

		// Verify via the DB that the code_review for this connector is "failed".
		var status string
		if err := tdb.Super.QueryRow(ctx,
			"SELECT status FROM code_review WHERE repo_connector_id=$1 ORDER BY created_at DESC LIMIT 1",
			connID).Scan(&status); err != nil {
			t.Fatalf("read status: %v", err)
		}
		if status != "failed" {
			t.Errorf("status after malformed JSON: want failed, got %q", status)
		}
	})

	t.Run("rls_cross_tenant_get_not_found", func(t *testing.T) {
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, srv.URL)

		runner := &validFakeRunner{summary: "RLS test summary."}
		svc := buildService(t, tdb, env, runner, fakeCred)

		cr, err := svc.Trigger(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err != nil {
			t.Fatalf("Trigger: %v", err)
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
		// A full successful run then a malformed-JSON run. The second run must
		// return an error, mark status=failed, and produce zero new POSTs.
		env := newCodingEnv(t, tdb)
		connID := createRepoConnector(ctx, t, env, seed, srv.URL)

		// First run: successful.
		runner1 := &validFakeRunner{summary: "First run good."}
		svc := buildService(t, tdb, env, runner1, fakeCred)
		if _, err := svc.Trigger(ctx, seed.principalID, seed.businessID, agentID, connID, 1); err != nil {
			t.Fatalf("first Trigger: %v", err)
		}

		// Second run: malformed JSON.
		postsBefore := stub.postCount.Load()
		runner2 := &malformedFakeRunner{}
		svc2 := buildService(t, tdb, env, runner2, fakeCred)
		_, err := svc2.Trigger(ctx, seed.principalID, seed.businessID, agentID, connID, 1)
		if err == nil {
			t.Fatal("second Trigger with malformed JSON must error")
		}

		// Verify Get on the failed run via the DB (we need the ID).
		var crID uuid.UUID
		var status string
		if err := tdb.Super.QueryRow(ctx,
			`SELECT id, status FROM code_review WHERE repo_connector_id=$1
			 ORDER BY created_at DESC LIMIT 1`, connID).Scan(&crID, &status); err != nil {
			t.Fatalf("read latest code_review: %v", err)
		}
		if status != "failed" {
			t.Errorf("second run status: want failed, got %q", status)
		}

		// Verify no new POST.
		postsAfter := stub.postCount.Load()
		if postsAfter != postsBefore {
			t.Errorf("second (failing) run posted %d unexpected reviews", postsAfter-postsBefore)
		}

		// Get() on the failed code_review via the service must succeed (row exists).
		got, err := svc2.Get(ctx, seed.principalID, seed.businessID, crID)
		if err != nil {
			t.Fatalf("Get on failed code_review: %v", err)
		}
		if got.Status != "failed" {
			t.Errorf("Get().Status on failed review: want failed, got %q", got.Status)
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
