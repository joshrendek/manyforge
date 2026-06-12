package connectors

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// ConnectorHealth is the moderate sync-health summary surfaced per connector.
type ConnectorHealth struct {
	State              string  `json:"state"` // healthy | degraded | disabled
	LinkedTicketCount  int64   `json:"linked_ticket_count"`
	PendingOutboundOps int64   `json:"pending_outbound_ops"`
	FailedOutboundOps  int64   `json:"failed_outbound_ops"`
	LastError          *string `json:"last_error"`
}

// ConnectorView is a connector as returned to management callers. It deliberately carries
// NO credential fields (email/api_token/webhook_secret) — credentials are write-only.
type ConnectorView struct {
	ID                  string
	BusinessID          string
	Type                string
	DisplayName         string
	BaseURL             string
	AllowPrivateBaseURL bool
	Config              map[string]any
	Status              string
	LastReconciledAt    *string // RFC3339, nil if never reconciled
	CreatedAt           string
	UpdatedAt           string
	Health              ConnectorHealth
}

// healthState derives the rollup pill from status + failure counts. A disabled connector is
// "disabled" regardless of counts; any failed outbound op makes it "degraded"; else "healthy".
// Pending ops alone are normal queue depth, not degradation.
func healthState(status string, failedOps int64) string {
	switch {
	case status == "disabled":
		return "disabled"
	case failedOps > 0:
		return "degraded"
	default:
		return "healthy"
	}
}

// connectorToView maps a dbgen.Connector + its health aggregates into a ConnectorView.
func connectorToView(row dbgen.Connector, h ConnectorHealth) ConnectorView {
	var cfg map[string]any
	if len(row.Config) > 0 {
		_ = json.Unmarshal(row.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	var lastRec *string
	if row.LastReconciledAt.Valid {
		s := row.LastReconciledAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		lastRec = &s
	}
	h.State = healthState(row.Status, h.FailedOutboundOps)
	return ConnectorView{
		ID:                  row.ID.String(),
		BusinessID:          row.BusinessID.String(),
		Type:                string(row.Type),
		DisplayName:         row.DisplayName,
		BaseURL:             row.BaseUrl,
		AllowPrivateBaseURL: row.AllowPrivateBaseUrl,
		Config:              cfg,
		Status:              row.Status,
		LastReconciledAt:    lastRec,
		CreatedAt:           row.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:           row.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Health:              h,
	}
}

// toPGUUID converts uuid.UUID to pgtype.UUID for dbgen queries that require it.
func toPGUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}
}

// List returns all connectors for a business with health, ordered by display name. RLS +
// the business_id predicate scope this to the caller's tenant.
func (s *Service) List(ctx context.Context, principalID, businessID uuid.UUID) ([]ConnectorView, error) {
	var rows []dbgen.Connector
	health := map[uuid.UUID]ConnectorHealth{}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		r, qerr := q.ListConnectors(ctx, businessID)
		if qerr != nil {
			return qerr
		}
		rows = r
		hr, herr := q.ListConnectorHealth(ctx, businessID)
		if herr != nil {
			return herr
		}
		for _, h := range hr {
			health[h.ConnectorID] = ConnectorHealth{
				LinkedTicketCount:  h.LinkedTicketCount,
				PendingOutboundOps: h.PendingOps,
				FailedOutboundOps:  h.FailedOps,
				LastError:          h.LastError,
			}
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]ConnectorView, 0, len(rows))
	for _, row := range rows {
		out = append(out, connectorToView(row, health[row.ID]))
	}
	return out, nil
}

// Get loads one connector with health by (id, business_id). Unknown/foreign id → ErrNotFound.
func (s *Service) Get(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (ConnectorView, error) {
	var row dbgen.Connector
	var h ConnectorHealth
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		r, qerr := q.GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID})
		if qerr != nil {
			return qerr
		}
		row = r
		hr, herr := q.GetConnectorHealth(ctx, toPGUUID(connectorID))
		if herr != nil {
			return herr
		}
		h = ConnectorHealth{
			LinkedTicketCount:  hr.LinkedTicketCount,
			PendingOutboundOps: hr.PendingOps,
			FailedOutboundOps:  hr.FailedOps,
			LastError:          hr.LastError,
		}
		return nil
	})
	if err != nil {
		return ConnectorView{}, mapErr(err)
	}
	return connectorToView(row, h), nil
}

// auditConnector is a small helper for the management mutations (update/rotate/delete) to
// write a same-tx audit row with non-secret metadata only.
func auditConnector(ctx context.Context, tx pgx.Tx, businessID, principalID, connectorID uuid.UUID, action string, inputs map[string]any) error {
	tt := "connector"
	return audit.Write(ctx, tx, audit.Entry{
		BusinessID:       &businessID,
		ActorPrincipalID: &principalID,
		Action:           action,
		TargetType:       &tt,
		TargetID:         &connectorID,
		Inputs:           inputs,
	})
}
