package coding

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/agents/coding/sandbox"
	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// fakeRepos is a minimal repoResolver returning a valid GitHub connector so
// Trigger reaches the egress pre-flight check without needing a DB.
type fakeRepos struct {
	rc  connectors.ResolvedRepoConnector
	err error
}

func (f *fakeRepos) Resolve(_ context.Context, _, _, _ uuid.UUID) (connectors.ResolvedRepoConnector, error) {
	return f.rc, f.err
}

// errFakeDB is returned by fakeServiceDB.WithPrincipal so a Trigger that gets
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

	_, err := svc.Trigger(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), 1)
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

	_, err := svc.Trigger(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), 1)
	if errors.Is(err, errs.ErrValidation) {
		t.Fatalf("allowed host must not be rejected by the egress gate, got ErrValidation: %v", err)
	}
	if !errors.Is(err, errFakeDB) {
		t.Fatalf("expected control flow to reach the DB insert (errFakeDB), got %v", err)
	}
}
