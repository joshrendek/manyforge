package connectors

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// Factory builds a live TicketingConnector from a resolved connector (unsealed
// credential + base_url + trust flag). US3/US4 register the real Jira/Zendesk factories;
// each builds its own SSRF-safe HTTP client honoring rc.AllowPrivateBaseURL.
type Factory func(rc ResolvedConnector) (TicketingConnector, error)

// Registry maps a connector type to its Factory and resolves a connector row (via the
// US1 credential Service, RLS-scoped) into a live, credential-bound client.
type Registry struct {
	svc       *Service
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry builds an empty registry over the credential service.
func NewRegistry(svc *Service) *Registry {
	return &Registry{svc: svc, factories: map[string]Factory{}}
}

// Register binds a connector type (e.g. "jira") to its factory. Intended to be called
// at startup. Panics if the type is already registered (a wiring bug).
func (r *Registry) Register(connectorType string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[connectorType]; exists {
		panic(fmt.Sprintf("connectors: factory for type %q already registered", connectorType))
	}
	r.factories[connectorType] = f
}

// Resolve loads the connector (RLS-scoped to business) and builds its live client.
// Cross-tenant / unknown connector → ErrNotFound (from the Service). An enabled
// connector whose type has no registered factory is a server-config error (not a
// client fault), returned as a plain wrapped error (→ 500 at the handler).
func (r *Registry) Resolve(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (TicketingConnector, error) {
	rc, err := r.svc.Resolve(ctx, principalID, businessID, connectorID)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	f, ok := r.factories[rc.Type]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("connectors: no factory registered for type %q", rc.Type)
	}
	return f(rc)
}
