// Package agents is the agent runtime: agent definitions, the run loop, the
// autonomy gate, the approvals queue, and BYO provider credentials. This file is
// the credential store (Spec 003 US1a): CRUD over ai_provider_credential with the
// API key sealed at rest, plus Resolve to hand the gateway a usable credential.
package agents

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// knownProviders is the closed set accepted at the service boundary. Keyed off the
// generated dbgen.AiProvider* constants (which track the ai_provider PG enum, migration
// 0025) so adding a provider to the enum + sqlc regen surfaces a new constant to add here
// rather than a silently-untracked string. TestKnownProvidersTrackEnum pins coverage.
var knownProviders = map[string]bool{
	string(dbgen.AiProviderAnthropic):   true,
	string(dbgen.AiProviderOpenai):      true,
	string(dbgen.AiProviderOllama):      true,
	string(dbgen.AiProviderVllm):        true,
	string(dbgen.AiProviderOpenrouter):  true,
	string(dbgen.AiProviderHuggingface): true,
	string(dbgen.AiProviderOpenaiCodex): true,
}

// chatgptAccountIDRe is the trust-boundary format guard for openai_codex's
// ChatGPTAccountID: it is interpolated into the sandbox auth.json AND sent as the
// ChatGPT-Account-Id HTTP header, so beyond the entrypoint's generic `"`/`\`
// metacharacter guard (defense in depth, not a substitute for it) this pins the
// shape to what real account ids look like — no whitespace, newlines, control
// chars, or other injection metacharacters.
var chatgptAccountIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// credentialDB is the minimal DB surface this service needs — satisfied by the
// real *db.DB. Declared as an interface so unit tests can omit it.
type credentialDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// CredentialService manages per-business BYO provider credentials. DB is the
// RLS-scoped handle (nil in pure unit tests that only exercise seal/resolve).
type CredentialService struct {
	DB     credentialDB
	Sealer *crypto.Sealer
}

// CreateCredentialInput is the caller-supplied credential to store.
type CreateCredentialInput struct {
	Provider            string
	APIKey              string // plaintext; sealed before persistence, never stored/logged raw
	BaseURL             string // optional (openai-compat / self-host)
	DefaultModel        string
	AllowPrivateBaseURL bool // self-host opt-in: permit a loopback/RFC1918 base_url
	MaxConcurrentLanes  int  // 0 ⇒ default 4; review-lane cap for this endpoint (1–16)
	// ChatGPTAccountID is the ChatGPT-Account-Id header value for openai_codex credentials
	// (non-secret). Required when Provider == "openai_codex"; ignored otherwise.
	ChatGPTAccountID string
}

// credLanes clamps the endpoint concurrency cap into the DB's [1,16] (0/unset ⇒ 4), so a
// blank or out-of-range form value can't trip the CHECK constraint on insert.
func credLanes(n int) int32 {
	if n == 0 {
		return 4
	}
	if n < 1 {
		n = 1
	}
	if n > 16 {
		n = 16
	}
	return int32(n)
}

// ResolvedCredential is what the gateway needs to build a Provider.
type ResolvedCredential struct {
	Provider            string
	APIKey              string // plaintext, in-memory only
	BaseURL             string
	Model               string
	AllowPrivateBaseURL bool
	// MaxConcurrentLanes caps how many code-review lanes may hit THIS endpoint at once
	// (a credential is one provider+base_url); the review fan-out serializes per endpoint.
	MaxConcurrentLanes int
	ChatGPTAccountID   string // openai_codex only; "" for other providers
}

// CredentialView is the safe, key-free projection of a stored credential for
// list/management surfaces. It intentionally carries NO sealed_key_ref / API key —
// only the metadata a caller needs to display and manage a credential.
type CredentialView struct {
	ID                  uuid.UUID
	BusinessID          uuid.UUID
	Provider            string
	BaseURL             string
	DefaultModel        string
	AllowPrivateBaseURL bool
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ChatGPTAccountID    string // openai_codex only; "" for other providers; non-secret
}

// credViewFromRow projects a dbgen row into a key-free CredentialView,
// dereferencing the nullable base_url and chatgpt_account_id to "" when absent.
func credViewFromRow(row dbgen.AiProviderCredential) CredentialView {
	base := ""
	if row.BaseUrl != nil {
		base = *row.BaseUrl
	}
	acct := ""
	if row.ChatgptAccountID != nil {
		acct = *row.ChatgptAccountID
	}
	return CredentialView{
		ID:                  row.ID,
		BusinessID:          row.BusinessID,
		Provider:            string(row.Provider),
		BaseURL:             base,
		DefaultModel:        row.DefaultModel,
		AllowPrivateBaseURL: row.AllowPrivateBaseUrl,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
		ChatGPTAccountID:    acct,
	}
}

// storedCredential is the unsealed-at-rest shape (mirrors the dbgen row; defined
// here so seal/resolve are unit-testable without the DB). Task 8 maps the dbgen
// row into this.
type storedCredential struct {
	Provider            string
	SealedKeyRef        *string
	BaseURL             *string
	DefaultModel        string
	AllowPrivateBaseURL bool
	MaxConcurrentLanes  int32
	ChatGPTAccountID    *string
}

func (s *CredentialService) validate(in CreateCredentialInput) error {
	if !knownProviders[in.Provider] {
		return fmt.Errorf("agents: unknown provider %q: %w", in.Provider, errs.ErrValidation)
	}
	if in.DefaultModel == "" {
		return fmt.Errorf("agents: default_model required: %w", errs.ErrValidation)
	}
	// Self-host / OpenAI-compat providers (openai/ollama/vllm) route through a caller-supplied
	// base_url. Providers with a default endpoint (anthropic/openrouter/huggingface) may omit
	// it. ai.DefaultBaseURL is the single source of truth — do not restate the list here.
	if _, hasDefault := ai.DefaultBaseURL(in.Provider); !hasDefault && in.BaseURL == "" {
		return fmt.Errorf("agents: base_url required for provider %q: %w", in.Provider, errs.ErrValidation)
	}
	if in.BaseURL != "" {
		if err := validateBaseURL(in.BaseURL, in.AllowPrivateBaseURL); err != nil {
			return err
		}
	}
	// The ChatGPT-subscription provider authenticates with an OAuth access token (sealed
	// in sealed_key_ref like any other key) PLUS a non-secret account id the codex backend
	// requires on every call — without it the sandbox review would send an incomplete
	// request the backend rejects. (openai_codex is review-sandbox-only; it never reaches
	// the direct ai.New gateway, which rejects it.)
	if in.Provider == string(dbgen.AiProviderOpenaiCodex) {
		if in.ChatGPTAccountID == "" {
			return fmt.Errorf("openai_codex credential requires chatgpt_account_id: %w", errs.ErrValidation)
		}
		if !chatgptAccountIDRe.MatchString(in.ChatGPTAccountID) {
			return fmt.Errorf("openai_codex chatgpt_account_id has an invalid format: %w", errs.ErrValidation)
		}
	}
	return nil
}

// validateBaseURL is a best-effort create-time guard: it pins the URL shape and,
// for a LITERAL IP host, applies the exact netsafe dialer policy. Hostnames are
// NOT resolved here (DNS can rebind) — dial-time netsafe stays authoritative.
func validateBaseURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	// url.Parse is lenient and never errors on "not a url"; the scheme/host checks below catch it.
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("agents: base_url must be a valid http(s) URL: %w", errs.ErrValidation)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if netsafe.IsBlocked(ip, netsafe.Options{AllowLoopback: allowPrivate, AllowPrivate: allowPrivate}) {
			return fmt.Errorf("agents: base_url %q is a blocked address: %w", raw, errs.ErrValidation)
		}
	}
	return nil
}

// sealAPIKey returns an opaque sealed ref for a plaintext key ("" ⇒ no ref).
// A non-empty key with a nil Sealer (MANYFORGE_AI_MASTER_KEY unset) → clean
// validation error, never a nil-pointer panic — mirrors MCPServerService.sealAuth.
func (s *CredentialService) sealAPIKey(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if s.Sealer == nil {
		return "", fmt.Errorf("agents: AI master key not configured (cannot seal api key): %w", errs.ErrValidation)
	}
	ref, err := s.Sealer.Seal([]byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("agents: seal api key: %w", err)
	}
	return ref, nil
}

// resolveRow unseals a stored credential into a usable ResolvedCredential.
func (s *CredentialService) resolveRow(row storedCredential) (ResolvedCredential, error) {
	out := ResolvedCredential{Provider: row.Provider, Model: row.DefaultModel}
	if row.BaseURL != nil {
		out.BaseURL = *row.BaseURL
	}
	out.AllowPrivateBaseURL = row.AllowPrivateBaseURL
	out.MaxConcurrentLanes = int(row.MaxConcurrentLanes)
	if row.ChatGPTAccountID != nil {
		out.ChatGPTAccountID = *row.ChatGPTAccountID
	}
	if row.SealedKeyRef != nil && *row.SealedKeyRef != "" {
		// A sealed key with a nil Sealer (master key unset since the row was written)
		// → clean validation error, never a nil-pointer panic on Open.
		if s.Sealer == nil {
			return ResolvedCredential{}, fmt.Errorf("agents: AI master key not configured (cannot unseal api key): %w", errs.ErrValidation)
		}
		key, err := s.Sealer.Open(*row.SealedKeyRef)
		if err != nil {
			return ResolvedCredential{}, fmt.Errorf("agents: unseal api key: %w", err)
		}
		out.APIKey = string(key)
	}
	return out, nil
}

// Create seals the API key and inserts the credential, returning a key-free view.
func (s *CredentialService) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateCredentialInput) (CredentialView, error) {
	if err := s.validate(in); err != nil {
		return CredentialView{}, err
	}
	ref, err := s.sealAPIKey(in.APIKey)
	if err != nil {
		return CredentialView{}, err
	}
	id := uuid.New()
	var refArg *string
	if ref != "" {
		refArg = &ref
	}
	var baseArg *string
	if in.BaseURL != "" {
		baseArg = &in.BaseURL
	}
	var acctArg *string
	if in.Provider == string(dbgen.AiProviderOpenaiCodex) && in.ChatGPTAccountID != "" {
		acctArg = &in.ChatGPTAccountID
	}
	var row dbgen.AiProviderCredential
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).InsertAIProviderCredential(ctx, dbgen.InsertAIProviderCredentialParams{
			ID:                  id,
			BusinessID:          businessID,
			Provider:            dbgen.AiProvider(in.Provider),
			SealedKeyRef:        refArg,
			BaseUrl:             baseArg,
			DefaultModel:        in.DefaultModel,
			AllowPrivateBaseUrl: in.AllowPrivateBaseURL,
			MaxConcurrentLanes:  credLanes(in.MaxConcurrentLanes),
			ChatgptAccountID:    acctArg,
		})
		if qerr != nil {
			return qerr
		}
		row = r
		// Trusting a private/loopback endpoint is a security-sensitive grant — audit it
		// in the SAME tx as the insert so there is never a trusted credential without
		// its trail (atomicity invariant).
		if in.AllowPrivateBaseURL {
			tt := "ai_credential"
			dec := "trust_private_base_url"
			return audit.Write(ctx, tx, audit.Entry{
				BusinessID:       &businessID,
				ActorPrincipalID: &principalID,
				Action:           "ai_credential.created",
				TargetType:       &tt,
				TargetID:         &id,
				Decision:         &dec,
				Inputs:           map[string]any{"provider": in.Provider, "base_url": in.BaseURL},
			})
		}
		return nil
	})
	if err != nil {
		return CredentialView{}, mapCredErr(err)
	}
	return credViewFromRow(row), nil
}

// List returns all credentials for a business as key-free views, ordered by
// provider. RLS scopes rows to the caller's authorized businesses, so a principal
// without membership on businessID sees an empty list. Always non-nil.
func (s *CredentialService) List(ctx context.Context, principalID, businessID uuid.UUID) ([]CredentialView, error) {
	var rows []dbgen.AiProviderCredential
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).ListAIProviderCredentials(ctx, businessID)
		rows = r
		return qerr
	})
	if err != nil {
		return nil, mapCredErr(err)
	}
	out := make([]CredentialView, 0, len(rows))
	for _, row := range rows {
		out = append(out, credViewFromRow(row))
	}
	return out, nil
}

// Delete removes the (id, business_id) credential. Rows-affected 0 (unknown id,
// or an id the caller's RLS scope can't see) maps to ErrNotFound — no oracle.
func (s *CredentialService) Delete(ctx context.Context, principalID, businessID, credentialID uuid.UUID) error {
	var affected int64
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		n, qerr := dbgen.New(tx).DeleteAIProviderCredential(ctx, dbgen.DeleteAIProviderCredentialParams{
			ID:         credentialID,
			BusinessID: businessID,
		})
		affected = n
		return qerr
	})
	if err != nil {
		return mapCredErr(err)
	}
	if affected == 0 {
		return fmt.Errorf("agents: credential not found: %w", errs.ErrNotFound)
	}
	return nil
}

// Resolve fetches + unseals the credential for (business, provider).
func (s *CredentialService) Resolve(ctx context.Context, principalID, businessID uuid.UUID, provider string) (ResolvedCredential, error) {
	var row dbgen.AiProviderCredential
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).GetAIProviderCredential(ctx, dbgen.GetAIProviderCredentialParams{
			BusinessID: businessID, Provider: dbgen.AiProvider(provider),
		})
		row = r
		return qerr
	})
	if err != nil {
		return ResolvedCredential{}, mapCredErr(err)
	}
	return s.resolveRow(storedCredential{
		Provider: string(row.Provider), SealedKeyRef: row.SealedKeyRef,
		BaseURL: row.BaseUrl, DefaultModel: row.DefaultModel,
		AllowPrivateBaseURL: row.AllowPrivateBaseUrl,
		MaxConcurrentLanes:  row.MaxConcurrentLanes,
		ChatGPTAccountID:    row.ChatgptAccountID,
	})
}

// mapCredErr converts a query/closure error into a stable service-layer sentinel.
// pgx.ErrNoRows (single-row lookups) → ErrNotFound (no oracle). A unique-constraint
// violation (SQLSTATE 23505 — a duplicate (business_id, provider)) → ErrConflict.
// ErrValidation and other typed sentinels are preserved. Everything else is
// returned wrapped so the HTTP layer logs it server-side and surfaces a generic 500.
func mapCredErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("agents: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("agents: duplicate credential: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("agents: query: %w", err)
	}
}
