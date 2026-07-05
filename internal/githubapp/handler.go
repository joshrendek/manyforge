package githubapp

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// appConfigStore is the subset of ConfigStore the Handler needs.
type appConfigStore interface {
	Get(ctx context.Context) (AppConfig, error)
	Save(ctx context.Context, c AppCreds) error
}

// installOps is the subset of the (not-yet-built, Task 4) installation
// service the Handler needs to react to webhook events and link an
// installation to a business/agent.
type installOps interface {
	UpsertFromEvent(ctx context.Context, id int64, login, accountType string) error
	MarkDeleted(ctx context.Context, id int64) error
	SetSuspended(ctx context.Context, id int64, suspended bool) error
	Link(ctx context.Context, id int64, businessID, agentID uuid.UUID) error
}

// nonceConsumer is the subset of NonceService the Handler needs.
type nonceConsumer interface {
	Consume(ctx context.Context, nonce string) (bool, error)
}

// permChecker is the subset of the permissions service the Handler needs to
// authorize a link request against the caller's business.
type permChecker interface {
	Has(ctx context.Context, principalID, businessID uuid.UUID, perm string) (bool, error)
}

// prReviewOps is the subset of the (Task 3) PR review path the Handler needs
// to react to a pull_request webhook: resolve the installation's linked
// business/agent, then atomically ingest the review request. Satisfied by
// *PRReviewEnqueuer.
type prReviewOps interface {
	ResolveInstallation(ctx context.Context, installationID int64) (InstallationContext, bool, error)
	IngestPRReview(ctx context.Context, in PRReviewInput) (uuid.UUID, bool, error)
}

// Handler is the HTTP surface for the instance-level GitHub App integration:
// operator-only App setup (manifest creation/conversion) and, in later
// tasks, the webhook receiver and per-business installation linking.
type Handler struct {
	Store             appConfigStore
	Installs          installOps
	API               GitHubAPI
	Nonces            nonceConsumer
	Perms             permChecker
	PRReviews         prReviewOps
	OperatorPrincipal uuid.UUID
	PublicBaseURL     string
	StateKey          []byte
	Now               func() time.Time
	Logger            *slog.Logger
}

func (h *Handler) log(ctx context.Context, msg string, err error) {
	if h.Logger != nil {
		h.Logger.ErrorContext(ctx, msg, "err", err)
	}
}

// info logs routine, expected skip/filter reasons (draft/bot/fork/rate-cap/
// dup) at Info level — distinct from log (Error level) so normal pull_request
// filtering doesn't spam ERROR (fable m4).
func (h *Handler) info(ctx context.Context, msg string, args ...any) {
	if h.Logger != nil {
		h.Logger.InfoContext(ctx, msg, args...)
	}
}

// operatorOnly gates a route on the config-pinned instance operator
// principal. Non-operator (including an unset/uuid.Nil OperatorPrincipal,
// which would otherwise let an unauthenticated caller with zero-value
// pid match) gets the same 404 as a nonexistent route — no oracle.
func (h *Handler) operatorOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pid, ok := httpx.PrincipalFromContext(r.Context())
		if !ok || h.OperatorPrincipal == uuid.Nil || pid != h.OperatorPrincipal {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// OperatorRoutes registers the instance-operator-only GitHub App setup
// routes: manifest generation (this task) and manifest conversion (Task 6).
func (h *Handler) OperatorRoutes(r chi.Router) {
	r.Group(func(g chi.Router) {
		g.Use(h.operatorOnly)
		g.Get("/github/app/manifest", h.renderManifest)
		g.Post("/github/app/manifest/convert", h.convertManifest) // implemented in Task 6
	})
}
