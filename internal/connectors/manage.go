package connectors

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
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

// UpdateConnectorInput is a partial (PATCH) update. nil fields are preserved. base_url and
// type are intentionally absent — they are immutable (identity). An empty non-nil config
// pointer replaces config with {}.
type UpdateConnectorInput struct {
	DisplayName *string
	Config      *map[string]any
	Status      *string // "enabled" | "disabled"
}

func validateUpdate(in UpdateConnectorInput) error {
	if in.DisplayName != nil && *in.DisplayName == "" {
		return fmt.Errorf("connectors: display_name cannot be empty: %w", errs.ErrValidation)
	}
	if in.Status != nil && *in.Status != "enabled" && *in.Status != "disabled" {
		return fmt.Errorf("connectors: status must be 'enabled' or 'disabled': %w", errs.ErrValidation)
	}
	return nil
}

// Update applies a partial change scoped to (id, business_id). Omitted fields preserved via
// COALESCE in SQL. No matching row → ErrNotFound (no oracle). Audited in the same tx.
func (s *Service) Update(ctx context.Context, principalID, businessID, connectorID uuid.UUID, in UpdateConnectorInput) (ConnectorView, error) {
	if err := validateUpdate(in); err != nil {
		return ConnectorView{}, err
	}
	params := dbgen.UpdateConnectorParams{ID: connectorID, BusinessID: businessID}
	params.DisplayName = in.DisplayName
	params.Status = in.Status
	if in.Config != nil {
		cfg := *in.Config
		if cfg == nil {
			cfg = map[string]any{}
		}
		b, merr := json.Marshal(cfg)
		if merr != nil {
			return ConnectorView{}, fmt.Errorf("connectors: marshal config: %w", errs.ErrValidation)
		}
		params.Config = b
	}
	var row dbgen.Connector
	var h ConnectorHealth
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		r, qerr := q.UpdateConnector(ctx, params)
		if qerr != nil {
			return qerr
		}
		row = r
		hr, herr := q.GetConnectorHealth(ctx, toPGUUID(connectorID))
		if herr != nil {
			return herr
		}
		h = ConnectorHealth{LinkedTicketCount: hr.LinkedTicketCount, PendingOutboundOps: hr.PendingOps, FailedOutboundOps: hr.FailedOps, LastError: hr.LastError}
		return auditConnector(ctx, tx, businessID, principalID, connectorID, "connector.updated",
			map[string]any{"display_name_changed": in.DisplayName != nil, "config_changed": in.Config != nil, "status": in.Status})
	})
	if err != nil {
		return ConnectorView{}, mapErr(err)
	}
	return connectorToView(row, h), nil
}

// RotateCredentialInput replaces the full sealed credential bundle. Partial (webhook-secret-only)
// rotation is intentionally unsupported (YAGNI) — callers always supply the complete bundle.
type RotateCredentialInput struct {
	Email         string
	APIToken      string
	WebhookSecret string
}

func validateRotate(in RotateCredentialInput) error {
	if in.Email == "" {
		return fmt.Errorf("connectors: email required: %w", errs.ErrValidation)
	}
	if in.APIToken == "" {
		return fmt.Errorf("connectors: api_token required: %w", errs.ErrValidation)
	}
	return nil
}

// RotateCredential seals a new credential bundle and atomically swaps the connector's
// secret_ref, deleting the old sealed secret — mirroring Create's seal/audit discipline.
// When a Verifier is wired, the NEW credential is live-verified BEFORE the tx; a credential
// that fails to authenticate is refused (400) and nothing is persisted. base_url/type are
// read from the existing connector (unchanged).
func (s *Service) RotateCredential(ctx context.Context, principalID, businessID, connectorID uuid.UUID, in RotateCredentialInput) error {
	if err := validateRotate(in); err != nil {
		return err
	}
	// Read connector metadata (and prove ownership) in a short tx; need base_url/type/flag for
	// verification and the old secret_ref for deletion.
	var meta dbgen.Connector
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID})
		meta = r
		return qerr
	}); err != nil {
		return mapErr(err)
	}
	// Live-verify the NEW credential before sealing (only when a Verifier is configured).
	if s.Verify != nil {
		if err := s.Verify.Verify(ctx, VerifyTarget{
			Type: string(meta.Type), BaseURL: meta.BaseUrl, AllowPrivateBaseURL: meta.AllowPrivateBaseUrl,
			Credential: Credential{Email: in.Email, APIToken: in.APIToken, WebhookSecret: in.WebhookSecret},
		}); err != nil {
			return fmt.Errorf("connectors: credential verification failed: %w", errs.ErrValidation)
		}
	}
	credBytes, err := json.Marshal(Credential{Email: in.Email, APIToken: in.APIToken, WebhookSecret: in.WebhookSecret})
	if err != nil {
		return fmt.Errorf("connectors: marshal credential: %w", err)
	}
	return mapErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		newRef, perr := s.Vault.Put(ctx, tx, businessID, "connector", credBytes)
		if perr != nil {
			return perr
		}
		n, uerr := dbgen.New(tx).UpdateConnectorSecretRef(ctx, dbgen.UpdateConnectorSecretRefParams{
			SecretRef: newRef, ID: connectorID, BusinessID: businessID,
		})
		if uerr != nil {
			return uerr
		}
		if n == 0 {
			return fmt.Errorf("connectors: not found: %w", errs.ErrNotFound)
		}
		if derr := s.Vault.Delete(ctx, tx, businessID, meta.SecretRef); derr != nil {
			return derr
		}
		return auditConnector(ctx, tx, businessID, principalID, connectorID, "connector.credential_rotated", map[string]any{})
	}))
}
