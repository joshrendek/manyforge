// Package errs defines the typed error sentinels used at the service-layer
// boundary. Handlers branch on these with errors.Is and map them to stable HTTP
// responses; wrapped errors are logged server-side and never returned raw to
// clients (Constitution Principle II).
package errs

import "errors"

var (
	// ErrNotFound is returned for a missing resource — and, deliberately, for a
	// resource the caller is not authorized to see, so the two are
	// indistinguishable to clients (no existence oracle; FR-011/FR-026).
	ErrNotFound = errors.New("not found")

	// ErrForbidden is for authenticated-but-not-permitted actions that are NOT
	// tenant-resource lookups (those collapse to ErrNotFound). It exists for
	// non-tenant cases such as an invite-accept email mismatch.
	ErrForbidden = errors.New("forbidden")

	// ErrValidation marks caller-input errors; its message is safe to surface.
	ErrValidation = errors.New("validation")

	// ErrConflict marks a state conflict: last-owner protection, hierarchy
	// cycle, role-in-use, non-root ownership transfer, or a concurrent mutation.
	ErrConflict = errors.New("conflict")

	// ErrRateLimited marks an action refused because a rate/abuse limit was hit
	// (e.g. outbound reply volume per business/recipient). Maps to HTTP 429.
	ErrRateLimited = errors.New("rate limited")

	// ErrTenantMaintenance marks a tenant mutation paused by a durable
	// maintenance operation such as a whole-tenant merge. It is retryable and
	// maps to a stable HTTP 503 response.
	ErrTenantMaintenance = errors.New("tenant maintenance")

	// ErrReauthenticationRequired marks a high-risk action whose fresh
	// credential verification failed. It deliberately does not distinguish a
	// missing password, passwordless account, disabled account, or mismatch.
	ErrReauthenticationRequired = errors.New("reauthentication required")

	// ErrStalePrecondition marks an operation whose previously reviewed state
	// no longer matches current data. Callers must rerun preflight and review
	// the new result rather than retrying the mutation blindly.
	ErrStalePrecondition = errors.New("stale precondition")

	// ErrUpstream marks a failed call to an external provider (e.g. auth.openai.com). Maps to
	// HTTP 502; the upstream body is logged server-side and never surfaced to the client.
	ErrUpstream = errors.New("upstream")

	// ErrCodexDisconnected marks an openai_codex credential whose refresh token is dead
	// (revoked/rotated-out). The user must reconnect their ChatGPT account. Maps to HTTP 409.
	ErrCodexDisconnected = errors.New("codex disconnected")
)
