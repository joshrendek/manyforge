package connectors

import (
	"context"

	"github.com/google/uuid"
)

// AgentGateway is the narrow surface Spec-003 agent tools use to read external ticket state
// and enqueue gated external writes. Composes Service (ownership-scoped DB ops) + Registry
// (live connector resolve). Construction: NewAgentGateway(connSvc, connReg).
type AgentGateway struct {
	svc *Service
	reg *Registry
}

// NewAgentGateway builds an AgentGateway from a Service and Registry.
func NewAgentGateway(svc *Service, reg *Registry) *AgentGateway {
	return &AgentGateway{svc: svc, reg: reg}
}

// ReadTicketExternal returns the external issue (with comments) for a connector-linked ticket
// the caller owns. Unlinked, unknown, or foreign ticket → ErrNotFound.
func (g *AgentGateway) ReadTicketExternal(ctx context.Context, principalID, businessID, ticketID uuid.UUID) (ExternalIssue, error) {
	connID, extID, err := g.svc.TicketConnectorRef(ctx, principalID, businessID, ticketID)
	if err != nil {
		return ExternalIssue{}, err
	}
	conn, err := g.reg.Resolve(ctx, principalID, businessID, connID)
	if err != nil {
		return ExternalIssue{}, err
	}
	// connector errors are already sentinel/no-body-leak
	return conn.FetchIssue(ctx, extID)
}

// EnqueueComment enqueues a 'comment' outbound op for a connector-linked ticket the caller owns,
// anchored to messageID (for external-id write-back + inbound dedup).
func (g *AgentGateway) EnqueueComment(ctx context.Context, principalID, businessID, ticketID, messageID uuid.UUID, body string) error {
	return g.svc.EnqueueOutboundComment(ctx, principalID, businessID, ticketID, messageID, body)
}

// EnqueueTransition enqueues a 'transition' outbound op (target status) for a connector-linked
// ticket the caller owns; dedups identical in-flight transitions.
func (g *AgentGateway) EnqueueTransition(ctx context.Context, principalID, businessID, ticketID uuid.UUID, status string) error {
	return g.svc.EnqueueOutboundTransition(ctx, principalID, businessID, ticketID, status)
}
