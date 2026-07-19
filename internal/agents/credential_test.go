package agents

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// aiProviderEnumRE extracts the ai_provider value list from the CREATE TYPE statement in
// db/schema.sql (the schema sqlc generates from — see sqlc.yaml).
var aiProviderEnumRE = regexp.MustCompile(`(?i)CREATE\s+TYPE\s+ai_provider\s+AS\s+ENUM\s*\(([^)]*)\)`)

// aiProviderEnumValues reads the ai_provider enum straight out of db/schema.sql. Reading the
// schema (rather than restating it in a Go slice) is what makes TestKnownProvidersTrackEnum a
// real pin: a new enum value shows up here without anyone remembering to update the test.
func aiProviderEnumValues(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "db", "schema.sql"))
	if err != nil {
		t.Fatalf("read db/schema.sql: %v", err)
	}
	m := aiProviderEnumRE.FindSubmatch(src)
	if m == nil {
		t.Fatal("db/schema.sql: no CREATE TYPE ai_provider AS ENUM (...) found")
	}
	var out []string
	for raw := range strings.SplitSeq(string(m[1]), ",") {
		if v := strings.Trim(strings.TrimSpace(raw), "'"); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// knownProviders must stay in lockstep with the ai_provider PG enum (manyforge-uc2). The enum
// is read from db/schema.sql, so adding a value there without adding it to knownProviders (or
// vice-versa) fails loudly — no hand-maintained duplicate of the value list to drift.
func TestKnownProvidersTrackEnum(t *testing.T) {
	want := aiProviderEnumValues(t)
	for _, p := range want {
		if !knownProviders[p] {
			t.Errorf("ai_provider enum value %q (db/schema.sql) not accepted by knownProviders", p)
		}
	}
	for p := range knownProviders {
		if !slices.Contains(want, p) {
			t.Errorf("knownProviders accepts %q, which is not an ai_provider enum value in db/schema.sql", p)
		}
	}
	// Every enum value must have a generated dbgen constant keyed off it; this catches a
	// schema.sql edit that was never followed by `make generate`.
	for _, p := range want {
		if !slices.Contains(dbgenAiProviders, dbgen.AiProvider(p)) {
			t.Errorf("ai_provider enum value %q has no dbgen.AiProvider constant — run `make generate`", p)
		}
	}
}

// dbgenAiProviders lists the generated constants. Adding an enum value regenerates models.go
// with a new constant, which must be appended here — the compiler cannot enumerate them.
var dbgenAiProviders = []dbgen.AiProvider{
	dbgen.AiProviderAnthropic, dbgen.AiProviderOpenai, dbgen.AiProviderOllama, dbgen.AiProviderVllm,
	dbgen.AiProviderOpenrouter, dbgen.AiProviderHuggingface, dbgen.AiProviderOpenaiCodex,
}

func newTestSealer(t *testing.T) *crypto.Sealer {
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

func TestSealAPIKeyAndResolveRoundTrip(t *testing.T) {
	sealer := newTestSealer(t)
	svc := &CredentialService{Sealer: sealer}

	ref, err := svc.sealAPIKey("sk-secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if ref == "" || ref == "sk-secret" {
		t.Fatalf("ref must be a sealed, non-plaintext string, got %q", ref)
	}

	// Resolve unseals a stored row into a usable credential.
	got, err := svc.resolveRow(storedCredential{
		Provider: "anthropic", SealedKeyRef: &ref, DefaultModel: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.APIKey != "sk-secret" || got.Provider != "anthropic" || got.Model != "claude-sonnet-4-6" {
		t.Fatalf("resolved = %+v", got)
	}
}

func TestResolveKeylessProvider(t *testing.T) {
	svc := &CredentialService{Sealer: newTestSealer(t)}
	got, err := svc.resolveRow(storedCredential{Provider: "ollama", SealedKeyRef: nil, DefaultModel: "llama3"})
	if err != nil {
		t.Fatalf("resolve keyless: %v", err)
	}
	if got.APIKey != "" {
		t.Errorf("keyless provider APIKey = %q, want empty", got.APIKey)
	}
}

func TestResolveRowCarriesAllowPrivate(t *testing.T) {
	svc := &CredentialService{} // no sealer needed when SealedKeyRef is nil
	got, err := svc.resolveRow(storedCredential{
		Provider: "ollama", SealedKeyRef: nil, DefaultModel: "llama3", AllowPrivateBaseURL: true,
	})
	if err != nil {
		t.Fatalf("resolveRow: %v", err)
	}
	if !got.AllowPrivateBaseURL {
		t.Fatal("AllowPrivateBaseURL did not round-trip through resolveRow")
	}
}

// TestCreateNilSealerReturnsErrorNotPanic pins the run-engine-reachable nil-sealer
// path (MANYFORGE_AI_MASTER_KEY unset): Create with a non-empty API key must return a
// clean ErrValidation, never a nil-pointer panic. The sealAPIKey guard fires before any
// DB access on the create path, so a nil DB is fine here (no DB call is reached).
func TestCreateNilSealerReturnsErrorNotPanic(t *testing.T) {
	svc := &CredentialService{Sealer: nil} // DB nil: guard fires before any DB use
	_, err := svc.Create(context.Background(), uuid.New(), uuid.New(), CreateCredentialInput{
		Provider: "anthropic", APIKey: "sk-x", DefaultModel: "m",
	})
	if err == nil {
		t.Fatal("Create with nil sealer + non-empty API key must return an error, got nil")
	}
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("want errs.ErrValidation, got %v", err)
	}
}

func TestValidateInput(t *testing.T) {
	svc := &CredentialService{Sealer: newTestSealer(t)}
	if err := svc.validate(CreateCredentialInput{Provider: "anthropic", DefaultModel: ""}); err == nil {
		t.Error("empty default_model must be a validation error")
	}
	if err := svc.validate(CreateCredentialInput{Provider: "bogus", DefaultModel: "m"}); err == nil {
		t.Error("unknown provider must be a validation error")
	}
}

func TestValidateBaseURL(t *testing.T) {
	svc := &CredentialService{}
	cases := []struct {
		name    string
		in      CreateCredentialInput
		wantErr bool
	}{
		{"anthropic needs no base_url", CreateCredentialInput{Provider: "anthropic", DefaultModel: "m"}, false},
		{"openai missing base_url", CreateCredentialInput{Provider: "openai", DefaultModel: "m"}, true},
		{"openai public base_url", CreateCredentialInput{Provider: "openai", DefaultModel: "m", BaseURL: "https://api.example.com/v1"}, false},
		{"openai junk base_url", CreateCredentialInput{Provider: "openai", DefaultModel: "m", BaseURL: "not a url"}, true},
		{"openai non-http scheme", CreateCredentialInput{Provider: "openai", DefaultModel: "m", BaseURL: "ftp://x/v1"}, true},
		{"ollama private IP, trust off -> reject", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://192.168.1.10:11434/v1"}, true},
		{"ollama private IP, trust on -> ok", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://192.168.1.10:11434/v1", AllowPrivateBaseURL: true}, false},
		{"ollama loopback, trust on -> ok", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://127.0.0.1:11434/v1", AllowPrivateBaseURL: true}, false},
		{"ollama metadata IP, trust on -> STILL reject", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://169.254.169.254/v1", AllowPrivateBaseURL: true}, true},
		{"ollama IPv6 loopback, trust off -> reject", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://[::1]:11434/v1"}, true},
		{"ollama IPv6 loopback, trust on -> ok", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://[::1]:11434/v1", AllowPrivateBaseURL: true}, false},
		{"ollama hostname not resolved at create", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://my-ollama.local/v1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.validate(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
		})
	}
}

func TestValidateOpenRouterBaseURLOptional(t *testing.T) {
	s := &CredentialService{} // validate() touches no DB/sealer fields
	if err := s.validate(CreateCredentialInput{Provider: "openrouter", DefaultModel: "anthropic/claude-3.5-sonnet"}); err != nil {
		t.Fatalf("openrouter empty base_url should be valid, got %v", err)
	}
	if err := s.validate(CreateCredentialInput{Provider: "openai", DefaultModel: "gpt-5"}); err == nil {
		t.Fatal("openai with empty base_url must still be rejected")
	}
}

// TestOpenAICodexRequiresAccountID pins the extra invariant on the ChatGPT-subscription
// provider: the sealed_key_ref alone (the OAuth access token) isn't enough to call the
// codex backend — the account id is a required, non-secret companion value. validate()
// touches no DB/sealer fields, so a zero-value CredentialService is enough (mirrors
// TestValidateInput/TestValidateBaseURL above).
func TestOpenAICodexRequiresAccountID(t *testing.T) {
	s := &CredentialService{}
	err := s.validate(CreateCredentialInput{
		Provider: "openai_codex", APIKey: "codex-test-token", DefaultModel: "gpt-5",
	})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("openai_codex without chatgpt_account_id: err = %v; want ErrValidation", err)
	}
	if err := s.validate(CreateCredentialInput{
		Provider: "openai_codex", APIKey: "codex-test-token", DefaultModel: "gpt-5",
		ChatGPTAccountID: "acct-abc-123",
	}); err != nil {
		t.Fatalf("openai_codex with chatgpt_account_id should validate, got %v", err)
	}
}

// TestOpenAICodexAccountIDFormat pins the trust-boundary format guard on
// ChatGPTAccountID (manyforge-6fx PR #32 review round 2): the value is interpolated
// into the sandbox auth.json AND sent as the ChatGPT-Account-Id HTTP header, so
// beyond the entrypoint's generic `"`/`\` metacharacter guard, validate() must reject
// whitespace, newlines, and other injection metacharacters — real account ids are
// UUID-shaped. validate() touches no DB/sealer fields, so a zero-value
// CredentialService is enough (mirrors TestOpenAICodexRequiresAccountID above).
func TestOpenAICodexAccountIDFormat(t *testing.T) {
	s := &CredentialService{}

	t.Run("valid UUID-shaped id passes", func(t *testing.T) {
		err := s.validate(CreateCredentialInput{
			Provider: "openai_codex", APIKey: "codex-test-token", DefaultModel: "gpt-5",
			ChatGPTAccountID: "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		})
		if err != nil {
			t.Fatalf("valid UUID-shaped chatgpt_account_id should validate, got %v", err)
		}
	})

	rejectCases := []struct {
		name string
		id   string
	}{
		{"space", "acct 123"},
		{"newline", "a\nb"},
		{"double quote", "a\"b"},
		{"over-long", strings.Repeat("a", 129)},
	}
	for _, tc := range rejectCases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.validate(CreateCredentialInput{
				Provider: "openai_codex", APIKey: "codex-test-token", DefaultModel: "gpt-5",
				ChatGPTAccountID: tc.id,
			})
			if !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("chatgpt_account_id %q: err = %v; want ErrValidation", tc.id, err)
			}
		})
	}
}

// fakeMinter is a codexMinter test double (Task 7). failIfCalled, when set, fails the
// test if Mint is invoked — used to pin the non-codex no-op path.
type fakeMinter struct {
	tok          string
	err          error
	failIfCalled *testing.T
}

func (f fakeMinter) Mint(_ context.Context, _, _ uuid.UUID) (string, error) {
	if f.failIfCalled != nil {
		f.failIfCalled.Fatal("codexMinter.Mint called for non-codex provider")
	}
	return f.tok, f.err
}

// TestResolve_codexMintsFreshToken pins the per-run mint hook (Task 7, manyforge-gi9u):
// an openai_codex ResolvedCredential's APIKey is overwritten with a freshly-minted access
// token, not left as whatever resolveRow unsealed from sealed_key_ref.
func TestResolve_codexMintsFreshToken(t *testing.T) {
	svc := &CredentialService{Codex: fakeMinter{tok: "fresh-abc"}}
	rc := ResolvedCredential{Provider: "openai_codex", APIKey: "stale-sealed-open"}
	out, err := svc.applyCodexMint(context.Background(), uuid.New(), uuid.New(), rc)
	if err != nil || out.APIKey != "fresh-abc" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

// TestResolve_nonCodexUnchanged pins the no-op path: for every other provider the mint
// hook must not touch APIKey and must not even call Mint.
func TestResolve_nonCodexUnchanged(t *testing.T) {
	svc := &CredentialService{Codex: fakeMinter{failIfCalled: t}}
	rc := ResolvedCredential{Provider: "openai", APIKey: "sk-x"}
	out, _ := svc.applyCodexMint(context.Background(), uuid.New(), uuid.New(), rc)
	if out.APIKey != "sk-x" {
		t.Fatalf("non-codex APIKey changed: %q", out.APIKey)
	}
}

// TestResolve_nilMinterUnchanged pins the other no-op path (manyforge-gi9u): when Codex is
// nil (not wired — Increment 2 stops before Task 10), an openai_codex credential's APIKey
// resolves via sealed_key_ref like any other provider instead of panicking.
func TestResolve_nilMinterUnchanged(t *testing.T) {
	svc := &CredentialService{}
	rc := ResolvedCredential{Provider: "openai_codex", APIKey: "sealed-open-value"}
	out, err := svc.applyCodexMint(context.Background(), uuid.New(), uuid.New(), rc)
	if err != nil || out.APIKey != "sealed-open-value" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestOpenAICodexCreateRejectsMissingAccountID(t *testing.T) {
	// Create() runs validate() first and returns before any sealer/DB access, so a zero-value
	// service proves the boundary (not just validate() directly) rejects a codex credential with
	// no account id. Companion to TestOpenAICodexRequiresAccountID.
	s := &CredentialService{}
	_, err := s.Create(context.Background(), uuid.New(), uuid.New(), CreateCredentialInput{
		Provider: "openai_codex", APIKey: "codex-test-token", DefaultModel: "gpt-5",
	})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("Create(openai_codex) without chatgpt_account_id: err = %v; want ErrValidation", err)
	}
}
