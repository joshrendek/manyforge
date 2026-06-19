package connectors

import (
	"context"
	"encoding/json"
	"errors"
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
	ID                          string
	BusinessID                  string
	Type                        string
	DisplayName                 string
	BaseURL                     string
	AllowPrivateBaseURL         bool
	SuppressNativeNotifications bool
	Config                      map[string]any
	Status                      string
	LastReconciledAt            *string // RFC3339, nil if never reconciled
	CreatedAt                   string
	UpdatedAt                   string
	Health                      ConnectorHealth
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
		// Stored config is always well-formed (written via json.Marshal); on the read path a
		// corrupt config renders as {} intentionally rather than failing the list/get.
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
		ID:                          row.ID.String(),
		BusinessID:                  row.BusinessID.String(),
		Type:                        string(row.Type),
		DisplayName:                 row.DisplayName,
		BaseURL:                     row.BaseUrl,
		AllowPrivateBaseURL:         row.AllowPrivateBaseUrl,
		SuppressNativeNotifications: row.SuppressNativeNotifications,
		Config:                      cfg,
		Status:                      row.Status,
		LastReconciledAt:            lastRec,
		CreatedAt:                   row.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:                   row.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Health:                      h,
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
		// GetConnector is the ownership gate (scoped to id + business_id) and MUST precede
		// GetConnectorHealth, which is a pure-aggregate query that succeeds for any id and
		// would otherwise leak counts for connectors the caller doesn't own.
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
	DisplayName                 *string
	Config                      *map[string]any
	Status                      *string // "enabled" | "disabled"
	SuppressNativeNotifications *bool   // nil = preserve
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
	params.SuppressNativeNotifications = in.SuppressNativeNotifications
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
			map[string]any{"display_name_changed": in.DisplayName != nil, "config_changed": in.Config != nil, "status": in.Status,
				"suppress_native_notifications_changed": in.SuppressNativeNotifications != nil})
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

// TestResult reports a live connection test. Detail is a short, non-leaking status string
// (never the credential or an upstream response body).
type TestResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Test resolves the stored credential and runs a live verify against the external system.
// Unknown/foreign id → ErrNotFound. A configured-but-failing credential returns {ok:false}
// with a safe detail (HTTP 200 — a test result is not an API error). If no Verifier is wired
// (dev without the connector master key), returns {ok:false, detail:"verification unavailable"}.
func (s *Service) Test(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (TestResult, error) {
	rc, err := s.Resolve(ctx, principalID, businessID, connectorID)
	if err != nil {
		return TestResult{}, err // already mapped (ErrNotFound on unknown)
	}
	if s.Verify == nil {
		return TestResult{OK: false, Detail: "verification unavailable"}, nil
	}
	if verr := s.Verify.Verify(ctx, VerifyTarget{
		Type: rc.Type, BaseURL: rc.BaseURL, AllowPrivateBaseURL: rc.AllowPrivateBaseURL, Credential: rc.Credential,
	}); verr != nil {
		return TestResult{OK: false, Detail: "credential verification failed"}, nil
	}
	return TestResult{OK: true, Detail: "ok"}, nil
}

// Delete is the terminal connector removal. In one tx it: confirms ownership (and reads
// secret_ref), detaches linked tickets + messages to native (NULL connector_id, PRESERVING
// external_id/external_url), cascade-deletes the sync/webhook/outbound bookkeeping, deletes
// the connector row, then deletes the sealed secret — and audits. Order matters: tickets and
// bookkeeping clear their FKs into connector BEFORE the connector row is deleted; the secret
// is deleted LAST (the connector references it until then). Unknown/foreign id → ErrNotFound.
func (s *Service) Delete(ctx context.Context, principalID, businessID, connectorID uuid.UUID) error {
	return mapErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, gerr := q.GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID})
		if gerr != nil {
			return gerr // pgx.ErrNoRows → ErrNotFound
		}
		// Capture the linked-ticket count for the audit before detaching.
		linked, herr := q.GetConnectorHealth(ctx, toPGUUID(connectorID))
		if herr != nil {
			return herr
		}
		if _, derr := q.DetachTicketsFromConnector(ctx, toPGUUID(connectorID)); derr != nil {
			return derr
		}
		if _, derr := q.DetachTicketMessagesFromConnector(ctx, toPGUUID(connectorID)); derr != nil {
			return derr
		}
		if _, derr := q.DeleteConnectorSyncState(ctx, connectorID); derr != nil {
			return derr
		}
		if _, derr := q.DeleteConnectorWebhookDeliveries(ctx, connectorID); derr != nil {
			return derr
		}
		if _, derr := q.DeleteConnectorOutboundOps(ctx, connectorID); derr != nil {
			return derr
		}
		n, derr := q.DeleteConnectorRow(ctx, dbgen.DeleteConnectorRowParams{ID: connectorID, BusinessID: businessID})
		if derr != nil {
			return derr
		}
		if n == 0 {
			return fmt.Errorf("connectors: not found: %w", errs.ErrNotFound)
		}
		if verr := s.Vault.Delete(ctx, tx, businessID, row.SecretRef); verr != nil && !errors.Is(verr, errs.ErrNotFound) {
			return verr
		}
		return auditConnector(ctx, tx, businessID, principalID, connectorID, "connector.deleted",
			map[string]any{"type": string(row.Type), "base_url": row.BaseUrl, "detached_tickets": linked.LinkedTicketCount})
	}))
}

// failedOpsAction is the shared scaffold for the two failed-op recovery mutations (xfj). In one
// tx it: confirms ownership via GetConnector (scoped to id + business_id — unknown/foreign id →
// ErrNoRows → ErrNotFound, no oracle), runs the supplied bulk mutation over the connector's
// failed ops, then writes a same-tx audit row recording how many were affected. The ownership
// gate MUST precede the mutation: the mutation is business-scoped but would silently match 0 rows
// for a connector the caller can't see, so without the gate an unowned id would look like a 0-op
// success instead of a 404. Returns the number of ops the mutation touched.
func (s *Service) failedOpsAction(ctx context.Context, principalID, businessID, connectorID uuid.UUID, action string, mutate func(q *dbgen.Queries) (int64, error)) (int64, error) {
	var n int64
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, gerr := q.GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID}); gerr != nil {
			return gerr // pgx.ErrNoRows → ErrNotFound
		}
		c, merr := mutate(q)
		if merr != nil {
			return merr
		}
		n = c
		return auditConnector(ctx, tx, businessID, principalID, connectorID, action,
			map[string]any{"failed_ops_affected": c})
	})
	if err != nil {
		return 0, mapErr(err)
	}
	return n, nil
}

// RetryFailedOps re-enqueues all of the connector's terminally-failed outbound ops (failed →
// pending, attempts reset, last_error cleared) so the dispatcher claims them again — the sole
// exit from the terminal 'failed' state that otherwise pins a connector 'degraded' forever
// (xfj). Unknown/foreign id → ErrNotFound. Returns the count re-enqueued (0 is valid).
func (s *Service) RetryFailedOps(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (int64, error) {
	return s.failedOpsAction(ctx, principalID, businessID, connectorID, "connector.failed_ops_retried",
		func(q *dbgen.Queries) (int64, error) {
			return q.RetryFailedOps(ctx, dbgen.RetryFailedOpsParams{ConnectorID: connectorID, BusinessID: businessID})
		})
}

// DismissFailedOps marks all of the connector's failed outbound ops dismissed (failed →
// dismissed), clearing 'degraded' without retrying while preserving the rows for audit (xfj).
// Same ownership gate + audit as RetryFailedOps. Returns the count dismissed (0 is valid).
func (s *Service) DismissFailedOps(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (int64, error) {
	return s.failedOpsAction(ctx, principalID, businessID, connectorID, "connector.failed_ops_dismissed",
		func(q *dbgen.Queries) (int64, error) {
			return q.DismissFailedOps(ctx, dbgen.DismissFailedOpsParams{ConnectorID: connectorID, BusinessID: businessID})
		})
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
	// When a Verifier is wired, live-verify the NEW credential before any write. The connector's
	// immutable base_url/type/flag are read here (ownership also proven); base_url/type can't change.
	if s.Verify != nil {
		var meta dbgen.Connector
		if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
			r, qerr := dbgen.New(tx).GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID})
			meta = r
			return qerr
		}); err != nil {
			return mapErr(err)
		}
		if err := s.Verify.Verify(ctx, VerifyTarget{
			Type: string(meta.Type), BaseURL: meta.BaseUrl, AllowPrivateBaseURL: meta.AllowPrivateBaseUrl,
			Credential: Credential(in),
		}); err != nil {
			return fmt.Errorf("connectors: credential verification failed: %w", errs.ErrValidation)
		}
	}
	credBytes, err := json.Marshal(Credential(in))
	if err != nil {
		return fmt.Errorf("connectors: marshal credential: %w", err)
	}
	return mapErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		newRef, perr := s.Vault.Put(ctx, tx, businessID, "connector", credBytes)
		if perr != nil {
			return perr
		}
		oldRef, uerr := q.RotateConnectorSecretRef(ctx, dbgen.RotateConnectorSecretRefParams{
			ID: connectorID, BusinessID: businessID, NewSecretRef: newRef,
		})
		if uerr != nil {
			return uerr // pgx.ErrNoRows → ErrNotFound (connector gone)
		}
		// Delete the displaced secret. A secret already absent is not an error here.
		if derr := s.Vault.Delete(ctx, tx, businessID, oldRef); derr != nil && !errors.Is(derr, errs.ErrNotFound) {
			return derr
		}
		return auditConnector(ctx, tx, businessID, principalID, connectorID, "connector.credential_rotated", map[string]any{})
	}))
}
