package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// serviceDB is the minimal DB surface (satisfied by *db.DB).
type serviceDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// Service creates + resolves per-business connectors with their credential sealed in
// the vault. Verify is an optional live test-call run before persisting (nil = skip).
type Service struct {
	DB     serviceDB
	Vault  *secrets.Vault
	Verify Verifier
}

// Create normalizes + validates input, optionally test-calls the external system,
// then seals the credential into the vault + inserts the connector + audits — all in
// one tx. The audit Inputs carry only non-secret metadata; the api_token/email never
// leave the sealed payload.
func (s *Service) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateConnectorInput) (uuid.UUID, error) {
	in.BaseURL = strings.TrimRight(in.BaseURL, "/")
	if err := validate(in); err != nil {
		return uuid.Nil, err
	}
	// Live test-call BEFORE the tx (never hold a tx open across network I/O).
	if s.Verify != nil {
		if err := s.Verify.Verify(ctx, VerifyTarget{
			Type: in.Type, BaseURL: in.BaseURL, AllowPrivateBaseURL: in.AllowPrivateBaseURL,
			Credential: Credential{Email: in.Email, APIToken: in.APIToken, WebhookSecret: in.WebhookSecret},
		}); err != nil {
			return uuid.Nil, fmt.Errorf("connectors: credential verification failed: %w", errs.ErrValidation)
		}
	}
	credBytes, err := json.Marshal(Credential{Email: in.Email, APIToken: in.APIToken, WebhookSecret: in.WebhookSecret})
	if err != nil {
		return uuid.Nil, fmt.Errorf("connectors: marshal credential: %w", err)
	}
	cfg := in.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return uuid.Nil, fmt.Errorf("connectors: marshal config: %w", errs.ErrValidation)
	}
	id := uuid.New()
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		secretID, perr := s.Vault.Put(ctx, tx, businessID, "connector", credBytes)
		if perr != nil {
			return perr
		}
		if _, perr := dbgen.New(tx).InsertConnector(ctx, dbgen.InsertConnectorParams{
			ID:                  id,
			BusinessID:          businessID,
			Type:                dbgen.ConnectorType(in.Type),
			DisplayName:         in.DisplayName,
			BaseUrl:             in.BaseURL,
			AllowPrivateBaseUrl: in.AllowPrivateBaseURL,
			SecretRef:           secretID,
			Config:              cfgJSON,
			Status:              "enabled",
		}); perr != nil {
			return perr
		}
		// Audit every connector.created (a new external data path) in the SAME tx.
		// Inputs carry only non-secret metadata. Decision flags the trust grant only.
		tt := "connector"
		entry := audit.Entry{
			BusinessID:       &businessID,
			ActorPrincipalID: &principalID,
			Action:           "connector.created",
			TargetType:       &tt,
			TargetID:         &id,
			Inputs:           map[string]any{"type": in.Type, "base_url": in.BaseURL},
		}
		if in.AllowPrivateBaseURL {
			dec := "trust_private_base_url"
			entry.Decision = &dec
		}
		return audit.Write(ctx, tx, entry)
	})
	if err != nil {
		return uuid.Nil, mapErr(err)
	}
	return id, nil
}

// Resolve loads the connector by id (RLS-scoped to business) and unseals its
// credential from the vault, in one tx. Cross-tenant / unknown id → ErrNotFound.
func (s *Service) Resolve(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (ResolvedConnector, error) {
	var out ResolvedConnector
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, qerr := dbgen.New(tx).GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID})
		if qerr != nil {
			return qerr
		}
		credBytes, oerr := s.Vault.Open(ctx, tx, businessID, row.SecretRef)
		if oerr != nil {
			return oerr
		}
		var cred Credential
		if uerr := json.Unmarshal(credBytes, &cred); uerr != nil {
			return fmt.Errorf("connectors: unmarshal credential: %w", uerr)
		}
		var cfg map[string]any
		if len(row.Config) > 0 {
			if uerr := json.Unmarshal(row.Config, &cfg); uerr != nil {
				return fmt.Errorf("connectors: unmarshal config: %w", uerr)
			}
		}
		out = ResolvedConnector{
			ID:                  row.ID.String(),
			Type:                string(row.Type),
			BaseURL:             row.BaseUrl,
			AllowPrivateBaseURL: row.AllowPrivateBaseUrl,
			Config:              cfg,
			Credential:          cred,
		}
		return nil
	})
	if err != nil {
		return ResolvedConnector{}, mapErr(err)
	}
	return out, nil
}

// EnqueueOutboundCreateIssue records a pending create_issue op linking an existing, as-yet-
// unlinked native ticket to a connector. The ownership predicate is pushed into SQL (the
// INSERT...SELECT only matches a ticket owned by businessID and not already linked); a
// no-op (0 rows) means unknown/foreign/already-linked -> ErrNotFound (no oracle). The actual
// Jira issue is created later by the OutboundDispatcher.
func (s *Service) EnqueueOutboundCreateIssue(ctx context.Context, principalID, businessID, ticketID, connectorID uuid.UUID) error {
	return mapErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		// Verify the connector is owned (same business) before enqueuing; the enabled gate is
		// enforced downstream by the dispatcher (connector_outbound_context filters status='enabled').
		if _, gerr := q.GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID}); gerr != nil {
			return gerr // pgx.ErrNoRows -> mapErr -> ErrNotFound
		}
		tag, eerr := q.EnqueueOutboundCreate(ctx, dbgen.EnqueueOutboundCreateParams{
			ID:          ticketID,
			ConnectorID: connectorID,
			Body:        "",
			BusinessID:  businessID,
		})
		if eerr != nil {
			return eerr
		}
		if tag == 0 {
			return fmt.Errorf("ticket not found, foreign, or already linked: %w", errs.ErrNotFound)
		}
		return nil
	}))
}

// mapErr converts DB/sentinel errors to stable service sentinels (mirrors
// agents.mapCredErr): pgx.ErrNoRows→404, SQLSTATE 23505→409.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("connectors: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("connectors: duplicate connector: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("connectors: query: %w", err)
	}
}
