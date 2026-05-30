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
)
