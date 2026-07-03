package coding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// fakeRepos is a minimal repoResolver returning a valid GitHub connector so
// Enqueue reaches the egress pre-flight check without needing a DB.
type fakeRepos struct {
	rc  connectors.ResolvedRepoConnector
	err error
}

func (f *fakeRepos) Resolve(_ context.Context, _, _, _ uuid.UUID) (connectors.ResolvedRepoConnector, error) {
	return f.rc, f.err
}

// errFakeDB is returned by fakeServiceDB.WithPrincipal so an Enqueue that gets
// past the egress pre-flight stops deterministically at the first DB step.
var errFakeDB = errors.New("fake db reached")

type fakeServiceDB struct{}

// WithPrincipal returns errFakeDB WITHOUT invoking fn — it exists only to prove
// control flow reached the insert step (i.e. the egress check did not reject).
func (fakeServiceDB) WithPrincipal(_ context.Context, _ uuid.UUID, _ func(pgx.Tx) error) error {
	return errFakeDB
}

func githubRepos() *fakeRepos {
	return &fakeRepos{rc: connectors.ResolvedRepoConnector{
		Type:       "github",
		Repo:       "o/r",
		Credential: connectors.Credential{APIToken: "ghp_x"},
	}}
}

// resolvePanel must NEVER brick a review on a panel-resolution failure — a DB error degrades to
// the default single "general" lane (legacy-shaped review) rather than a failed job (manyforge-vay).
func TestResolvePanelDegradesToDefaultOnDBError(t *testing.T) {
	svc := &CodeReviewService{DB: fakeServiceDB{}} // WithPrincipal returns errFakeDB, never runs fn
	panel := svc.resolvePanel(context.Background(), uuid.New(), uuid.New())
	if len(panel) != 1 || panel[0].Key != generalDimensionKey {
		t.Fatalf("want default single %q lane on DB error, got %d: %+v", generalDimensionKey, len(panel), panel)
	}
	if !panel[0].Enabled {
		t.Fatal("default lane must be enabled")
	}
}

// manyforge-0qj: a provider host outside the configured egress allowlist must be
// rejected up front with ErrValidation — never silently launched into a sandbox
// whose egress the boot-static proxy will block.
func TestTriggerRejectsHostOutsideEgressAllowlist(t *testing.T) {
	runner := &sandbox.FakeRunner{}
	svc := &CodeReviewService{
		Repos:       githubRepos(),
		Sandbox:     runner,
		Creds:       &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://evil.example.com", Model: "m", Provider: "x"}},
		EgressAllow: netsafe.ParseHostAllowlist("api.anthropic.com,openrouter.ai"),
		// DB intentionally nil: the check MUST fire before any DB write.
	}

	_, err := svc.Enqueue(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), 1)
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("disallowed host: want ErrValidation, got %v", err)
	}
	if runner.Last.Image != "" {
		t.Error("sandbox must NOT be launched when the egress host is disallowed")
	}
}

// The mirror case: a host that IS in the allowlist must pass the pre-flight and
// proceed (here it then stops at the fake DB), proving the gate doesn't over-reject.
func TestTriggerAllowsHostInEgressAllowlist(t *testing.T) {
	svc := &CodeReviewService{
		DB:          fakeServiceDB{},
		Repos:       githubRepos(),
		Sandbox:     &sandbox.FakeRunner{},
		Creds:       &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}},
		EgressAllow: netsafe.ParseHostAllowlist("api.anthropic.com,openrouter.ai"),
	}

	_, err := svc.Enqueue(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), 1)
	if errors.Is(err, errs.ErrValidation) {
		t.Fatalf("allowed host must not be rejected by the egress gate, got ErrValidation: %v", err)
	}
	if !errors.Is(err, errFakeDB) {
		t.Fatalf("expected control flow to reach the DB insert (errFakeDB), got %v", err)
	}
}

// TestEnqueueDoesNotRunSandbox verifies that Enqueue never touches the sandbox:
// even when the egress gate passes and the DB insert is hit, the SandboxRunner
// must not be called. The fakeServiceDB short-circuits at the insert boundary,
// which is the correct termination point for a no-sandbox Enqueue path.
func TestEnqueueDoesNotRunSandbox(t *testing.T) {
	runner := &sandbox.FakeRunner{}
	svc := &CodeReviewService{
		DB:          fakeServiceDB{},
		Repos:       githubRepos(),
		Sandbox:     runner,
		Creds:       &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}},
		EgressAllow: netsafe.ParseHostAllowlist("api.anthropic.com"),
	}

	_, err := svc.Enqueue(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), 1)
	// errFakeDB is expected — we hit the DB step.
	if !errors.Is(err, errFakeDB) {
		t.Fatalf("expected errFakeDB from the DB insert step, got %v", err)
	}
	// Sandbox must never have been invoked.
	if runner.Last.Image != "" {
		t.Error("Enqueue must NOT invoke the sandbox runner; it only queues a pending row")
	}
}

// TestReviewURLConstructedFromConnectorAndRef covers the pure helper directly.
// Format: https://github.com/{repo}/pull/{pr}#pullrequestreview-{ref}
func TestReviewURLConstructedFromConnectorAndRef(t *testing.T) {
	cases := []struct {
		name        string
		repo        string
		pr          int
		externalRef string
		want        string
	}{
		{
			name:        "populated",
			repo:        "owner/repo",
			pr:          5,
			externalRef: "42",
			want:        "https://github.com/owner/repo/pull/5#pullrequestreview-42",
		},
		{
			name:        "empty_repo_returns_empty",
			repo:        "",
			pr:          5,
			externalRef: "42",
			want:        "",
		},
		{
			name:        "empty_ref_returns_empty",
			repo:        "owner/repo",
			pr:          5,
			externalRef: "",
			want:        "",
		},
		{
			name:        "both_empty_returns_empty",
			repo:        "",
			pr:          0,
			externalRef: "",
			want:        "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reviewURL(tc.repo, tc.pr, tc.externalRef)
			if got != tc.want {
				t.Errorf("reviewURL(%q, %d, %q) = %q; want %q", tc.repo, tc.pr, tc.externalRef, got, tc.want)
			}
		})
	}
}

func TestSandboxStderrTail_Redacts(t *testing.T) {
	dir := t.TempDir()
	secret := "sk-LIVE0123456789abcdefghij"
	if err := os.WriteFile(filepath.Join(dir, "stderr.log"),
		[]byte("Error: Unauthorized: bad key "+secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := sandboxStderrTail(dir, secret)
	if strings.Contains(tail, secret) {
		t.Fatalf("secret leaked in stderr tail: %s", tail)
	}
	if !strings.Contains(tail, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %s", tail)
	}
}

func TestSandboxEnv(t *testing.T) {
	env := sandboxEnv(AICredential{APIKey: "k", BaseURL: "https://api.openai.com", Model: "gpt-4o", Provider: "openai"})
	if env["LLM_PROVIDER"] != "openai" || env["LLM_MODEL"] != "gpt-4o" ||
		env["LLM_API_KEY"] != "k" || env["LLM_BASE_URL"] != "https://api.openai.com" {
		t.Fatalf("env = %+v", env)
	}
}
