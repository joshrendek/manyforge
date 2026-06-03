// Package agents is the agent runtime: agent definitions, the run loop, the
// autonomy gate, the approvals queue, and BYO provider credentials. This file is
// the credential store (Spec 003 US1a): CRUD over ai_provider_credential with the
// API key sealed at rest, plus Resolve to hand the gateway a usable credential.
package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// knownProviders is the closed set accepted at the service boundary (mirrors the
// ai_provider enum). Keep in lockstep with migration 0025.
var knownProviders = map[string]bool{"anthropic": true, "openai": true, "ollama": true, "vllm": true}

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
	Provider     string
	APIKey       string // plaintext; sealed before persistence, never stored/logged raw
	BaseURL      string // optional (openai-compat / self-host)
	DefaultModel string
}

// ResolvedCredential is what the gateway needs to build a Provider.
type ResolvedCredential struct {
	Provider string
	APIKey   string // plaintext, in-memory only
	BaseURL  string
	Model    string
}

// storedCredential is the unsealed-at-rest shape (mirrors the dbgen row; defined
// here so seal/resolve are unit-testable without the DB). Task 8 maps the dbgen
// row into this.
type storedCredential struct {
	Provider     string
	SealedKeyRef *string
	BaseURL      *string
	DefaultModel string
}

func (s *CredentialService) validate(in CreateCredentialInput) error {
	if !knownProviders[in.Provider] {
		return fmt.Errorf("agents: unknown provider %q: %w", in.Provider, errs.ErrValidation)
	}
	if in.DefaultModel == "" {
		return fmt.Errorf("agents: default_model required: %w", errs.ErrValidation)
	}
	return nil
}

// sealAPIKey returns an opaque sealed ref for a plaintext key ("" ⇒ no ref).
func (s *CredentialService) sealAPIKey(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
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
	if row.SealedKeyRef != nil && *row.SealedKeyRef != "" {
		key, err := s.Sealer.Open(*row.SealedKeyRef)
		if err != nil {
			return ResolvedCredential{}, fmt.Errorf("agents: unseal api key: %w", err)
		}
		out.APIKey = string(key)
	}
	return out, nil
}

// Create seals the API key and inserts the credential, returning its id.
func (s *CredentialService) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateCredentialInput) (uuid.UUID, error) {
	if err := s.validate(in); err != nil {
		return uuid.Nil, err
	}
	ref, err := s.sealAPIKey(in.APIKey)
	if err != nil {
		return uuid.Nil, err
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
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, qerr := dbgen.New(tx).InsertAIProviderCredential(ctx, dbgen.InsertAIProviderCredentialParams{
			ID:           id,
			BusinessID:   businessID,
			Provider:     dbgen.AiProvider(in.Provider),
			SealedKeyRef: refArg,
			BaseUrl:      baseArg,
			DefaultModel: in.DefaultModel,
		})
		return qerr
	})
	if err != nil {
		return uuid.Nil, mapCredErr(err)
	}
	return id, nil
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
	})
}

// mapCredErr converts a query/closure error into a stable service-layer sentinel.
// pgx.ErrNoRows (single-row lookups) → ErrNotFound (no oracle). ErrValidation
// and other typed sentinels are preserved. Everything else is returned wrapped so
// the HTTP layer logs it server-side and surfaces a generic 500.
func mapCredErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("agents: not found: %w", errs.ErrNotFound)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("agents: query: %w", err)
	}
}
