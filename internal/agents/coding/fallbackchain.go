package coding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// resolveFn resolves one agent's reviewbot credential (the runJob closure binds the
// caller's principal/business). Kept as a function type so chooseReviewbot is a pure,
// DB-free unit.
type resolveFn func(ctx context.Context, agentID uuid.UUID) (AICredential, error)

// chooseReviewbot walks the ordered fallback chain and returns the credential of the
// first bot that BOTH resolves and passes the liveness probe. If none is live but some
// resolve, it returns the last resolvable one and lets the real review call fail into the
// worker retry (a briefly-flapping server still gets a shot; the next attempt re-probes).
// If nothing in the chain resolves (every entry is stale/foreign), it errors with
// ErrValidation → a terminal, clearly-messaged failJob.
func chooseReviewbot(ctx context.Context, chain []uuid.UUID, resolve resolveFn, probe reviewbotProber) (AICredential, error) {
	var lastResolvable *AICredential
	for _, id := range chain {
		cred, err := resolve(ctx, id)
		if err != nil {
			slog.Default().WarnContext(ctx, "fallback chain: skipping unresolvable reviewbot", "agent_id", id, "err", err)
			continue
		}
		c := cred
		lastResolvable = &c
		if probe.Live(ctx, cred) {
			return cred, nil
		}
		slog.Default().InfoContext(ctx, "fallback chain: reviewbot not live, trying next", "agent_id", id, "base_url", cred.BaseURL)
	}
	if lastResolvable != nil {
		return *lastResolvable, nil
	}
	return AICredential{}, fmt.Errorf("coding: review fallback chain has no usable reviewbot: %w", errs.ErrValidation)
}

// resolveReviewChain returns the business's configured reviewbot fallback chain (ordered
// agent IDs), or nil when none is set. A resolver failure degrades to nil (the legacy
// single enqueued-agent path) and logs — a transient DB blip must not brick reviews.
func (s *CodeReviewService) resolveReviewChain(ctx context.Context, principalID, businessID uuid.UUID) []uuid.UUID {
	var chain []uuid.UUID
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, err := dbgen.New(tx).GetReviewConfig(ctx, businessID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no config row ⇒ no chain
		}
		if err != nil {
			return err
		}
		chain = row.ReviewAgentChain
		return nil
	}); err != nil {
		slog.Default().WarnContext(ctx, "coding: resolve review chain failed, using enqueued agent",
			"err", err, "business_id", businessID)
		return nil
	}
	return chain
}

// prober returns the configured reviewbot liveness prober, or a default httpProber.
func (s *CodeReviewService) prober() reviewbotProber {
	if s.Prober != nil {
		return s.Prober
	}
	return httpProber{Timeout: defaultProbeTimeout}
}
