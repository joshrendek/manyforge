package coding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
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

// laneCandidates returns the ordered fallback candidates for a dimension: the primary
// (provider, model) first, then each fallback-chain entry.
func laneCandidates(dim Dimension) []FallbackEntry {
	out := make([]FallbackEntry, 0, len(dim.FallbackChain)+1)
	out = append(out, FallbackEntry{Provider: dim.Provider, Model: dim.Model})
	out = append(out, dim.FallbackChain...)
	return out
}

// resolveLaneCred picks the reviewbot credential for one dimension lane (manyforge-azy, extended
// to walk the whole chain by manyforge-7lx T2): it probes [primary, ...FallbackChain] in order
// and returns the first one that resolves AND passes the liveness probe, plus the not-yet-tried
// tail (the remaining candidates after the chosen one) so a caller can keep going down the chain
// at runtime if the chosen lane's real call still fails (T3). A blank provider inherits the
// review's default resolved cred (def). If nothing in the chain is live but some candidates
// resolve, it returns the first resolvable one (the retry path — a briefly-flapping endpoint
// still gets a shot). Returns a non-empty reason only when NOTHING in the chain resolves — the
// lane is then skipped (recorded in dimension_runs, never silently misrouted).
func (s *CodeReviewService) resolveLaneCred(ctx context.Context, principalID, businessID uuid.UUID, def AICredential, dim Dimension) (AICredential, []FallbackEntry, string) {
	cands := laneCandidates(dim)
	var firstResolvable AICredential
	firstResolvableIdx := -1
	for i, c := range cands {
		lc, err := s.laneCredFor(ctx, principalID, businessID, def, c.Provider, c.Model)
		if err != nil {
			slog.Default().InfoContext(ctx, "coding: lane candidate unresolvable", "dimension", dim.Key, "provider", c.Provider, "err", err)
			continue
		}
		if firstResolvableIdx < 0 {
			firstResolvable, firstResolvableIdx = lc, i
		}
		if s.prober().Live(ctx, lc) {
			return lc, cands[i+1:], "" // live start; runtime may continue down the tail
		}
		slog.Default().InfoContext(ctx, "coding: lane candidate not live, trying next", "dimension", dim.Key, "provider", c.Provider)
	}
	if firstResolvableIdx >= 0 {
		return firstResolvable, cands[firstResolvableIdx+1:], "" // none live; try the first resolvable (retry path)
	}
	return AICredential{}, nil, fmt.Sprintf("no reachable reviewbot for %q (check its provider credentials and fallbacks)", dim.Key)
}

// privateBaseURLBlocked reports whether a base-URL host must be refused for a sandbox
// lane given the credential's AllowPrivateBaseURL opt-in. Only IP-LITERAL hosts are
// classified: a DNS hostname or a public IP returns false (governed solely by the egress
// allowlist, checked separately in laneCredFor/runJob). A private/ULA IP is permitted
// only with the opt-in; loopback is always permitted; cloud-metadata/link-local stay
// blocked even with the opt-in. Every provider — local and cloud alike — reaches its
// endpoint via the same sandbox path, gated by this guard plus the allowlist-gated
// egress proxy (manyforge-9er Tasks 3-4).
func privateBaseURLBlocked(host string, allowPrivate bool) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false // DNS name / not an IP literal — egress allowlist governs it
	}
	return netsafe.IsBlocked(ip, netsafe.Options{AllowLoopback: true, AllowPrivate: allowPrivate})
}

// laneCredFor resolves (provider, model) into a lane credential: a blank/same provider inherits
// the review default (model overridden); a different provider resolves its own BYO credential.
// It then requires a usable host, membership in the sandbox egress allowlist (checked per-lane
// now that lanes may span endpoints — local providers now route through the sandbox too, so this
// applies to every provider), and — for an IP-literal private/link-local host — the credential's
// AllowPrivateBaseURL opt-in.
func (s *CodeReviewService) laneCredFor(ctx context.Context, principalID, businessID uuid.UUID, def AICredential, provider, model string) (AICredential, error) {
	lc := def
	if provider != "" && !strings.EqualFold(provider, def.Provider) {
		rc, err := s.Creds.ResolveProvider(ctx, principalID, businessID, provider, model)
		if err != nil {
			return AICredential{}, err
		}
		lc = rc
	} else if strings.TrimSpace(model) != "" {
		lc.Model = model
	}
	if lc.Host() == "" {
		return AICredential{}, fmt.Errorf("no usable base url for provider %q", lc.Provider)
	}
	if !s.EgressAllow.Allows(lc.Host()) {
		return AICredential{}, fmt.Errorf("provider host %q not in sandbox egress allowlist", lc.Host())
	}
	// A private/RFC1918/ULA (or metadata/link-local) IP host is permitted only with the
	// credential's explicit AllowPrivateBaseURL opt-in; DNS + public hosts pass unchanged.
	if privateBaseURLBlocked(lc.Host(), lc.AllowPrivateBaseURL) {
		return AICredential{}, fmt.Errorf("host %q requires allow_private_base_url", lc.Host())
	}
	return lc, nil
}
