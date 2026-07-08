package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// ReviewDimensionService is the RLS-scoped CRUD for a business's configured review panel
// (spec 008 Slice 2): the per-dimension rows and the panel-level config. It is the write/read
// twin of the worker's resolvePanel — the Review Setup UI drives this; the worker reads the
// same rows to fan out. No secrets here (provider/model reference the credential vault by
// provider), so it needs only the DB.
type ReviewDimensionService struct {
	DB serviceDB
}

// ReviewDimensionView is the public view of one configured dimension row.
type ReviewDimensionView struct {
	ID            string          `json:"id"`
	Dimension     string          `json:"dimension"`
	Provider      string          `json:"provider,omitempty"` // "" ⇒ use the review's default resolved credential
	Model         string          `json:"model"`
	FallbackChain []FallbackEntry `json:"fallback_chain"` // ordered fallback (provider, model) pairs; empty ⇒ no fallback for this lane
	Prompt        string          `json:"prompt"`
	ScopeGlobs    []string        `json:"scope_globs"`
	MinSeverity   string          `json:"min_severity"`
	Enabled       bool            `json:"enabled"`
	SortOrder     int             `json:"sort_order"`
}

// ReviewDimensionInput is the upsert payload for one dimension (the Setup page "save row").
type ReviewDimensionInput struct {
	Dimension     string          `json:"dimension"`
	Provider      string          `json:"provider"`
	Model         string          `json:"model"`
	FallbackChain []FallbackEntry `json:"fallback_chain"`
	Prompt        string          `json:"prompt"`
	ScopeGlobs    []string        `json:"scope_globs"`
	MinSeverity   string          `json:"min_severity"`
	Enabled       bool            `json:"enabled"`
	SortOrder     int             `json:"sort_order"`
}

// ReviewConfigView is the public view of a business's panel-level config.
type ReviewConfigView struct {
	Dedupe         bool   `json:"dedupe"`
	VerifyEnabled  bool   `json:"verify_enabled"`
	VerifyProvider string `json:"verify_provider,omitempty"`
	VerifyModel    string `json:"verify_model"`
	CiteRules      bool   `json:"cite_rules"`
	PostMode       string `json:"post_mode"`
	// ReviewAgentChain is the ordered reviewbot fallback chain (agent UUIDs, primary
	// first). Empty ⇒ no fallback (the review uses its single enqueued agent).
	ReviewAgentChain []string `json:"review_agent_chain"`
}

// ReviewConfigInput is the PUT payload; same shape as the view.
type ReviewConfigInput = ReviewConfigView

var (
	knownDimensionKeys = map[string]bool{
		"security": true, "correctness": true, "performance": true,
		"ui": true, "docs": true, "tests": true, "general": true,
	}
	knownAIProviders = map[string]bool{
		"anthropic": true, "openai": true, "ollama": true, "vllm": true, "openrouter": true,
	}
	knownSeverities = map[string]bool{"info": true, "warning": true, "error": true}
	knownPostModes  = map[string]bool{"single": true, "per_dimension": true}
)

// maxDimensionPromptBytes bounds a per-dimension prompt so a giant blob can't be stored or
// then blown into every review's system message.
const maxDimensionPromptBytes = 20000

// maxFallbackChainEntries bounds a dimension's fallback chain so an unbounded list can't be
// stored or then walked entry-by-entry (one sandbox invocation per hop) at review time.
const maxFallbackChainEntries = 8

// validateDimensionInput enforces the service-boundary invariants (spec 008 plan): dimension +
// severity in their sets; provider (when set) known AND accompanied by a model; prompt bounded.
func validateDimensionInput(in ReviewDimensionInput) error {
	if !knownDimensionKeys[in.Dimension] {
		return fmt.Errorf("coding: unknown dimension %q: %w", in.Dimension, errs.ErrValidation)
	}
	if !knownSeverities[in.MinSeverity] {
		return fmt.Errorf("coding: min_severity must be info|warning|error: %w", errs.ErrValidation)
	}
	if in.Provider != "" {
		if !knownAIProviders[in.Provider] {
			return fmt.Errorf("coding: unknown provider %q: %w", in.Provider, errs.ErrValidation)
		}
		if strings.TrimSpace(in.Model) == "" {
			return fmt.Errorf("coding: model required when provider is set: %w", errs.ErrValidation)
		}
	}
	if len(in.FallbackChain) > maxFallbackChainEntries {
		return fmt.Errorf("coding: fallback_chain exceeds %d entries: %w", maxFallbackChainEntries, errs.ErrValidation)
	}
	for i, fb := range in.FallbackChain {
		if fb.Provider == "" || !knownAIProviders[fb.Provider] {
			return fmt.Errorf("coding: fallback[%d] has unknown/empty provider %q: %w", i, fb.Provider, errs.ErrValidation)
		}
		if strings.TrimSpace(fb.Model) == "" {
			return fmt.Errorf("coding: fallback[%d] model required: %w", i, errs.ErrValidation)
		}
	}
	if len(in.Prompt) > maxDimensionPromptBytes {
		return fmt.Errorf("coding: prompt exceeds %d bytes: %w", maxDimensionPromptBytes, errs.ErrValidation)
	}
	return nil
}

// nullProvider maps a provider string ("" ⇒ default) to the nullable ai_provider column type.
func nullProvider(p string) dbgen.NullAiProvider {
	if p == "" {
		return dbgen.NullAiProvider{}
	}
	return dbgen.NullAiProvider{AiProvider: dbgen.AiProvider(p), Valid: true}
}

// mustMarshalChain serializes a fallback chain to the jsonb column's []byte form. An empty
// chain marshals to "[]" (not "null") so a cleared fallback round-trips as an empty list, never
// a NULL the column doesn't allow.
func mustMarshalChain(c []FallbackEntry) []byte {
	if len(c) == 0 {
		return []byte("[]")
	}
	b, _ := json.Marshal(c)
	return b
}

// ListPanel returns the business's configured dimensions (enabled + disabled) in panel order.
func (s *ReviewDimensionService) ListPanel(ctx context.Context, principalID, businessID uuid.UUID) ([]ReviewDimensionView, error) {
	var out []ReviewDimensionView
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, qerr := dbgen.New(tx).ListReviewDimensions(ctx, businessID)
		if qerr != nil {
			return qerr
		}
		out = make([]ReviewDimensionView, 0, len(rows))
		for _, r := range rows {
			out = append(out, dimensionViewFromRow(r))
		}
		return nil
	})
	if err != nil {
		return nil, mapReviewCfgErr(err)
	}
	return out, nil
}

// UpsertDimension validates and inserts-or-updates one dimension row (keyed on the business +
// dimension), auditing the change. Returns the persisted view.
func (s *ReviewDimensionService) UpsertDimension(ctx context.Context, principalID, businessID uuid.UUID, in ReviewDimensionInput) (ReviewDimensionView, error) {
	if err := validateDimensionInput(in); err != nil {
		return ReviewDimensionView{}, err
	}
	globs := in.ScopeGlobs
	if globs == nil {
		globs = []string{}
	}
	var view ReviewDimensionView
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, qerr := dbgen.New(tx).UpsertReviewDimension(ctx, dbgen.UpsertReviewDimensionParams{
			ID:            uuid.New(),
			BusinessID:    businessID,
			Dimension:     in.Dimension,
			Provider:      nullProvider(in.Provider),
			Model:         in.Model,
			FallbackChain: mustMarshalChain(in.FallbackChain),
			Prompt:        in.Prompt,
			ScopeGlobs:    globs,
			MinSeverity:   in.MinSeverity,
			Enabled:       in.Enabled,
			SortOrder:     int32(in.SortOrder),
		})
		if qerr != nil {
			return qerr
		}
		view = dimensionViewFromRow(row)
		tt := "review_dimension"
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			ActorPrincipalID: &principalID,
			Action:           "review_dimension.upserted",
			TargetType:       &tt,
			TargetID:         &row.ID,
			Inputs:           map[string]any{"dimension": in.Dimension, "enabled": in.Enabled, "provider": in.Provider, "model": in.Model, "fallback_chain": in.FallbackChain},
		})
	})
	if err != nil {
		return ReviewDimensionView{}, mapReviewCfgErr(err)
	}
	return view, nil
}

// DeleteDimension removes one dimension row, scoped to the business (RLS + business_id
// predicate). Not found / cross-tenant ⇒ ErrNotFound.
func (s *ReviewDimensionService) DeleteDimension(ctx context.Context, principalID, businessID, id uuid.UUID) error {
	return mapReviewCfgErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		n, err := dbgen.New(tx).DeleteReviewDimension(ctx, dbgen.DeleteReviewDimensionParams{ID: id, BusinessID: businessID})
		if err != nil {
			return err
		}
		if n == 0 {
			return errs.ErrNotFound
		}
		tt := "review_dimension"
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			ActorPrincipalID: &principalID,
			Action:           "review_dimension.deleted",
			TargetType:       &tt,
			TargetID:         &id,
		})
	}))
}

// GetConfig returns the business's panel config, or the built-in defaults when no row exists
// (dedupe on, verify off, single post) — the UI always gets a usable config to render.
func (s *ReviewDimensionService) GetConfig(ctx context.Context, principalID, businessID uuid.UUID) (ReviewConfigView, error) {
	out := defaultReviewConfigView()
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, qerr := dbgen.New(tx).GetReviewConfig(ctx, businessID)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return nil // no row → defaults
		}
		if qerr != nil {
			return qerr
		}
		out = configViewFromRow(row)
		return nil
	})
	if err != nil {
		return ReviewConfigView{}, mapReviewCfgErr(err)
	}
	return out, nil
}

// UpsertConfig validates and inserts-or-updates the business's panel config (one row per
// business), auditing the change.
func (s *ReviewDimensionService) UpsertConfig(ctx context.Context, principalID, businessID uuid.UUID, in ReviewConfigInput) (ReviewConfigView, error) {
	if in.PostMode == "" {
		in.PostMode = "single"
	}
	if !knownPostModes[in.PostMode] {
		return ReviewConfigView{}, fmt.Errorf("coding: post_mode must be single|per_dimension: %w", errs.ErrValidation)
	}
	if in.VerifyProvider != "" && !knownAIProviders[in.VerifyProvider] {
		return ReviewConfigView{}, fmt.Errorf("coding: unknown verify provider %q: %w", in.VerifyProvider, errs.ErrValidation)
	}
	// Parse the fallback chain agent IDs up front (malformed id = client error);
	// existence/visibility is validated inside the tx below so RLS scopes it.
	chain, perr := parseAgentChain(in.ReviewAgentChain)
	if perr != nil {
		return ReviewConfigView{}, perr
	}
	// A chain is an ordered set — dedupe (first-seen order) before validating/storing so a
	// repeated id isn't persisted (chooseReviewbot would just probe it twice).
	chain = distinctUUIDs(chain)
	var view ReviewConfigView
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		// Reject a chain that names an agent this caller can't see (unknown or foreign): a
		// count mismatch means at least one id doesn't resolve under RLS.
		if len(chain) > 0 {
			n, cerr := q.CountAgentsInBusiness(ctx, dbgen.CountAgentsInBusinessParams{Ids: chain, BusinessID: businessID})
			if cerr != nil {
				return cerr
			}
			if int(n) != len(chain) {
				return fmt.Errorf("coding: review_agent_chain references an unknown agent: %w", errs.ErrValidation)
			}
		}
		row, qerr := q.UpsertReviewConfig(ctx, dbgen.UpsertReviewConfigParams{
			BusinessID:       businessID,
			Dedupe:           in.Dedupe,
			VerifyEnabled:    in.VerifyEnabled,
			VerifyProvider:   nullProvider(in.VerifyProvider),
			VerifyModel:      in.VerifyModel,
			CiteRules:        in.CiteRules,
			PostMode:         in.PostMode,
			ReviewAgentChain: chain,
		})
		if qerr != nil {
			return qerr
		}
		view = configViewFromRow(row)
		tt := "review_config"
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			ActorPrincipalID: &principalID,
			Action:           "review_config.upserted",
			TargetType:       &tt,
			TargetID:         &businessID,
			Inputs:           map[string]any{"dedupe": in.Dedupe, "verify_enabled": in.VerifyEnabled, "post_mode": in.PostMode},
		})
	})
	if err != nil {
		return ReviewConfigView{}, mapReviewCfgErr(err)
	}
	return view, nil
}

func dimensionViewFromRow(r dbgen.ReviewDimension) ReviewDimensionView {
	v := ReviewDimensionView{
		ID:          r.ID.String(),
		Dimension:   r.Dimension,
		Model:       r.Model,
		Prompt:      r.Prompt,
		ScopeGlobs:  r.ScopeGlobs,
		MinSeverity: r.MinSeverity,
		Enabled:     r.Enabled,
		SortOrder:   int(r.SortOrder),
	}
	if v.ScopeGlobs == nil {
		v.ScopeGlobs = []string{}
	}
	if len(r.FallbackChain) > 0 {
		_ = json.Unmarshal(r.FallbackChain, &v.FallbackChain)
	}
	if v.FallbackChain == nil {
		v.FallbackChain = []FallbackEntry{}
	}
	if r.Provider.Valid {
		v.Provider = string(r.Provider.AiProvider)
	}
	return v
}

func defaultReviewConfigView() ReviewConfigView {
	return ReviewConfigView{Dedupe: true, VerifyEnabled: false, CiteRules: false, PostMode: "single"}
}

func configViewFromRow(r dbgen.ReviewConfig) ReviewConfigView {
	v := ReviewConfigView{
		Dedupe:           r.Dedupe,
		VerifyEnabled:    r.VerifyEnabled,
		VerifyModel:      r.VerifyModel,
		CiteRules:        r.CiteRules,
		PostMode:         r.PostMode,
		ReviewAgentChain: uuidsToStrings(r.ReviewAgentChain),
	}
	if r.VerifyProvider.Valid {
		v.VerifyProvider = string(r.VerifyProvider.AiProvider)
	}
	return v
}

// parseAgentChain converts the API's string agent IDs into UUIDs, preserving order. A
// malformed id is a client error (ErrValidation); existence/visibility is checked
// separately under RLS by the caller.
func parseAgentChain(in []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(in))
	for _, s := range in {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("coding: review_agent_chain has an invalid agent id %q: %w", s, errs.ErrValidation)
		}
		out = append(out, id)
	}
	return out, nil
}

// distinctUUIDs returns the unique ids preserving first-seen order (used to validate the
// chain's agent existence without double-counting a repeated entry).
func distinctUUIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// uuidsToStrings renders stored agent-chain UUIDs for the API view (nil ⇒ empty slice so
// the JSON is [] not null).
func uuidsToStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// mapReviewCfgErr converts DB/sentinel errors to stable service sentinels (mirrors mapRepoErr).
func mapReviewCfgErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("coding: review config not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("coding: duplicate: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound), errors.Is(err, errs.ErrConflict):
		return err
	default:
		return fmt.Errorf("coding: review config query: %w", err)
	}
}
