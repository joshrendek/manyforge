package coding

import (
	"context"
	"encoding/json"
	"errors"
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
	secret := "sk-LIVE0123456789abcdefghij"
	stderr := []byte("Error: Unauthorized: bad key " + secret + "\n")
	tail := sandboxStderrTail(stderr, secret)
	if strings.Contains(tail, secret) {
		t.Fatalf("secret leaked in stderr tail: %s", tail)
	}
	if !strings.Contains(tail, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %s", tail)
	}
}

// progressStreamWriter must forward the sandbox's live stderr into the Progress heartbeat so a
// cloud review streams like the local path (manyforge cloud streaming).
func TestProgressStreamWriter_FeedsProgress(t *testing.T) {
	prog := &Progress{}
	prog.SetPhase("reviewing") // Snapshot is nil until a phase is set (real flow sets it first)
	w := &progressStreamWriter{prog: prog, dim: "security"}
	n, err := w.Write([]byte("Read main.go\n"))
	if err != nil || n != len("Read main.go\n") {
		t.Fatalf("Write = %d, %v", n, err)
	}
	_, _ = w.Write([]byte("Grep func\n"))
	snap := prog.Snapshot()
	if !strings.Contains(string(snap), "security") || !strings.Contains(string(snap), "Grep func") {
		t.Fatalf("progress preview missing streamed narration: %s", snap)
	}
	// A nil prog must not panic (defensive — Progress.UpdateStream is nil-safe).
	wn := &progressStreamWriter{prog: nil, dim: "x"}
	if _, err := wn.Write([]byte("hi")); err != nil {
		t.Fatalf("nil-prog write errored: %v", err)
	}
}

// fakeTokens is a test double for installationTokenSource. It records the args of
// the last Token call so a test can assert the mint ran with the right (installation
// id, repo) derived from the connector's Config + Repo.
type fakeTokens struct {
	tok       string
	err       error
	calls     int
	gotInstID int64
	gotRepo   string
}

func (f *fakeTokens) Token(_ context.Context, instID int64, repo string) (string, error) {
	f.calls++
	f.gotInstID, f.gotRepo = instID, repo
	return f.tok, f.err
}

// githubAppRepos returns a resolver yielding an app-backed connector (no stored
// credential — runJob mints the token). installation_id arrives as float64 because
// the connector Config round-trips through json.Unmarshal(map[string]any).
func githubAppRepos() *fakeRepos {
	return &fakeRepos{rc: connectors.ResolvedRepoConnector{
		Type:   "github_app",
		Repo:   "o/r",
		Config: map[string]any{"installation_id": float64(4242)},
	}}
}

func appJob() ClaimedReview {
	return ClaimedReview{
		ID: uuid.New(), BusinessID: uuid.New(), PrincipalID: uuid.New(),
		AgentID: uuid.New(), RepoConnectorID: uuid.New(), PRNumber: 7, Attempts: 1,
	}
}

// A github_app connector reaching runJob without a token source can't authenticate —
// fail the job with ErrValidation (bounded worker retry), never launch a sandbox.
func TestRunJobAppConnectorNilTokenSource(t *testing.T) {
	runner := &sandbox.FakeRunner{}
	svc := &CodeReviewService{
		DB:      fakeServiceDB{},
		Repos:   githubAppRepos(),
		Sandbox: runner,
		Creds:   &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}},
		// Tokens intentionally nil.
	}
	err := svc.runJob(context.Background(), appJob(), nil)
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("nil token source: want ErrValidation, got %v", err)
	}
	if !strings.Contains(err.Error(), "no installation-token source") {
		t.Errorf("want an explicit no-token-source message, got %q", err.Error())
	}
	if runner.Last.Image != "" {
		t.Error("sandbox must NOT launch when the app connector has no token source")
	}
}

// A github_app connector whose Config lacks installation_id fails the job — the mint
// call can't be scoped without it.
func TestRunJobAppConnectorMissingInstallationID(t *testing.T) {
	repos := &fakeRepos{rc: connectors.ResolvedRepoConnector{Type: "github_app", Repo: "o/r", Config: map[string]any{}}}
	svc := &CodeReviewService{
		DB:     fakeServiceDB{},
		Repos:  repos,
		Creds:  &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}},
		Tokens: &fakeTokens{tok: "ghs_x"},
	}
	err := svc.runJob(context.Background(), appJob(), nil)
	if !errors.Is(err, errs.ErrValidation) || !strings.Contains(err.Error(), "missing installation_id") {
		t.Fatalf("missing installation_id: want ErrValidation with a clear message, got %v", err)
	}
}

// A mint failure (e.g. a suspended/deleted install → 401/403/404) is a plain error →
// failJob → bounded worker retry. The original cause must be wrapped through.
func TestRunJobAppConnectorMintError(t *testing.T) {
	mintErr := errors.New("github 401 suspended")
	runner := &sandbox.FakeRunner{}
	svc := &CodeReviewService{
		DB:      fakeServiceDB{},
		Repos:   githubAppRepos(),
		Sandbox: runner,
		Creds:   &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}},
		Tokens:  &fakeTokens{err: mintErr},
	}
	err := svc.runJob(context.Background(), appJob(), nil)
	if !errors.Is(err, mintErr) {
		t.Fatalf("mint error must propagate as the cause, got %v", err)
	}
	if runner.Last.Image != "" {
		t.Error("sandbox must NOT launch on a mint failure")
	}
}

// The minted token IS the credential the GitHub client is built from: an empty mint
// makes NewFactory reject the build ("api_token required"), proving the token flows in.
func TestRunJobAppConnectorEmptyMintedTokenRejected(t *testing.T) {
	runner := &sandbox.FakeRunner{}
	svc := &CodeReviewService{
		DB:      fakeServiceDB{},
		Repos:   githubAppRepos(),
		Sandbox: runner,
		Creds:   &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://api.anthropic.com", Model: "m", Provider: "anthropic"}},
		Tokens:  &fakeTokens{tok: ""}, // mint returns an empty token
	}
	err := svc.runJob(context.Background(), appJob(), nil)
	if err == nil || !strings.Contains(err.Error(), "api_token required") {
		t.Fatalf("empty minted token: want NewFactory 'api_token required', got %v", err)
	}
	if runner.Last.Image != "" {
		t.Error("sandbox must NOT launch when the client build failed")
	}
}

// runJob's egress pre-flight (fable M5) must fail fast for a provider host outside the
// allowlist — AFTER a successful mint (proving the minted token was accepted by the
// client build) but BEFORE any sandbox launch. This also pins that the mint ran with
// the (installation id, repo) derived from the connector.
func TestRunJobEgressPreflightFailsFastAfterMint(t *testing.T) {
	runner := &sandbox.FakeRunner{}
	tokens := &fakeTokens{tok: "ghs_unit"}
	svc := &CodeReviewService{
		DB:          fakeServiceDB{},
		Repos:       githubAppRepos(),
		Sandbox:     runner,
		Creds:       &FakeCredResolver{Cred: AICredential{APIKey: "k", BaseURL: "https://evil.example.com", Model: "m", Provider: "x"}},
		EgressAllow: netsafe.ParseHostAllowlist("api.anthropic.com,openrouter.ai"),
		Tokens:      tokens,
	}
	err := svc.runJob(context.Background(), appJob(), nil)
	if !errors.Is(err, errs.ErrValidation) || !strings.Contains(err.Error(), "egress allowlist") {
		t.Fatalf("disallowed host: want ErrValidation mentioning the egress allowlist, got %v", err)
	}
	if runner.Last.Image != "" {
		t.Error("sandbox must NOT launch when the provider host is egress-blocked")
	}
	if tokens.calls != 1 || tokens.gotInstID != 4242 || tokens.gotRepo != "o/r" {
		t.Errorf("mint must run once with (4242, o/r); got calls=%d instID=%d repo=%q",
			tokens.calls, tokens.gotInstID, tokens.gotRepo)
	}
}

func TestInstallationIDFromConfig(t *testing.T) {
	cases := []struct {
		name   string
		cfg    map[string]any
		wantID int64
		wantOK bool
	}{
		{"float64 (json.Unmarshal default)", map[string]any{"installation_id": float64(4242)}, 4242, true},
		{"json.Number", map[string]any{"installation_id": json.Number("99")}, 99, true},
		{"int64", map[string]any{"installation_id": int64(7)}, 7, true},
		{"missing", map[string]any{}, 0, false},
		{"zero rejected", map[string]any{"installation_id": float64(0)}, 0, false},
		{"non-numeric rejected", map[string]any{"installation_id": "nope"}, 0, false},
		{"nil config", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := installationIDFromConfig(tc.cfg)
			if id != tc.wantID || ok != tc.wantOK {
				t.Fatalf("installationIDFromConfig = (%d,%v), want (%d,%v)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

func TestSandboxEnv(t *testing.T) {
	env := sandboxEnv(AICredential{APIKey: "k", BaseURL: "https://api.openai.com", Model: "gpt-4o", Provider: "openai"})
	if env["LLM_PROVIDER"] != "openai" || env["LLM_MODEL"] != "gpt-4o" ||
		env["LLM_API_KEY"] != "k" || env["LLM_BASE_URL"] != "https://api.openai.com" {
		t.Fatalf("env = %+v", env)
	}
}

// TestReviewModelLabel pins the model stamped on code_review.model per lane count
// (manyforge-vv6, PR #23 review): single-lane records the effective model, a
// multi-dimension panel records the sentinel.
func TestReviewModelLabel(t *testing.T) {
	dim := func(model string) Dimension { return Dimension{Key: "security", Model: model} }
	cases := []struct {
		name     string
		active   []Dimension
		resolved string
		want     string
	}{
		{"no dimensions (default lane)", nil, "glm-5.2", "glm-5.2"},
		{"single dimension, no own model", []Dimension{dim("")}, "glm-5.2", "glm-5.2"},
		{"single dimension, whitespace-only model", []Dimension{dim("   ")}, "glm-5.2", "glm-5.2"},
		{"single dimension, own model", []Dimension{dim("openai/gpt-5.5")}, "glm-5.2", "openai/gpt-5.5"},
		{"multi-dimension panel", []Dimension{dim("openai/gpt-5.5"), dim("deepseek/deepseek-v4-pro")}, "glm-5.2", "panel"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reviewModelLabel(c.active, c.resolved); got != c.want {
				t.Errorf("reviewModelLabel(%d dims) = %q, want %q", len(c.active), got, c.want)
			}
		})
	}
}
