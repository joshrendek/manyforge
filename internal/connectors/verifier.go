package connectors

import (
	"context"
	"fmt"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// authVerifier is the optional capability a typed connector client exposes to confirm its
// credential authenticates — a cheap, side-effect-free probe (Jira GET /rest/api/3/myself,
// Zendesk GET /api/v2/users/me.json). It is deliberately NOT part of TicketingConnector so
// the many connector fakes in tests don't have to implement it; the registry verifier
// type-asserts to it instead.
type authVerifier interface {
	VerifyAuth(ctx context.Context) error
}

// registryVerifier is a Verifier backed by the typed-client registry: it builds the client
// for a VerifyTarget and runs its live auth probe. Wired into Service.Verify so connector
// create, credential rotation, and the Test action all perform a real credential check.
type registryVerifier struct{ reg *Registry }

// NewRegistryVerifier returns a Verifier that probes credentials via reg's typed clients.
func NewRegistryVerifier(reg *Registry) Verifier { return registryVerifier{reg: reg} }

// Verify builds the connector client for t and calls its live auth probe. A type whose
// client doesn't implement the probe is a validation error (no silent "ok").
func (v registryVerifier) Verify(ctx context.Context, t VerifyTarget) error {
	conn, err := v.reg.BuildSystem(ResolvedConnector{
		Type:                t.Type,
		BaseURL:             t.BaseURL,
		AllowPrivateBaseURL: t.AllowPrivateBaseURL,
		Credential:          t.Credential,
	})
	if err != nil {
		return err
	}
	av, ok := conn.(authVerifier)
	if !ok {
		return fmt.Errorf("connectors: type %q does not support verification: %w", t.Type, errs.ErrValidation)
	}
	return av.VerifyAuth(ctx)
}
