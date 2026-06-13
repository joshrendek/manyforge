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
	"github.com/jackc/pgx/v5/pgtype"

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
		// Re-adopt orphaned tickets from a previously-deleted connector to the same provider host
		// (manyforge-7zx): relink the newest detached ticket per external_id + its messages, so a
		// recreated connector resumes instead of re-importing duplicates. Bounded by existing
		// detached rows (never imports). Same tx → atomic with the create.
		q := dbgen.New(tx)
		candidates, perr := q.CountReadoptableTickets(ctx, dbgen.CountReadoptableTicketsParams{
			BusinessID: businessID, BaseUrl: in.BaseURL,
		})
		if perr != nil {
			return perr
		}
		readoptedIDs, perr := q.ReadoptDetachedTickets(ctx, dbgen.ReadoptDetachedTicketsParams{
			BusinessID: businessID, BaseUrl: in.BaseURL, ConnectorID: id,
		})
		if perr != nil {
			return perr
		}
		if len(readoptedIDs) > 0 {
			if perr := q.RelinkReadoptedMessages(ctx, dbgen.RelinkReadoptedMessagesParams{
				BusinessID: businessID, ConnectorID: id, TicketIds: readoptedIDs,
			}); perr != nil {
				return perr
			}
			rtt := "connector"
			if werr := audit.Write(ctx, tx, audit.Entry{
				BusinessID:       &businessID,
				ActorPrincipalID: &principalID,
				Action:           "connector.tickets_readopted",
				TargetType:       &rtt,
				TargetID:         &id,
				Inputs: map[string]any{
					"readopted_count":         len(readoptedIDs),
					"skipped_duplicate_count": int(candidates) - len(readoptedIDs),
				},
			}); werr != nil {
				return werr
			}
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

// TicketConnectorRef returns the connector id + external id of a connector-linked ticket the
// caller owns. Unlinked, unknown, or foreign → ErrNotFound (no 403/404 oracle).
func (s *Service) TicketConnectorRef(ctx context.Context, principalID, businessID, ticketID uuid.UUID) (uuid.UUID, string, error) {
	var connID uuid.UUID
	var extID string
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		ref, qerr := dbgen.New(tx).GetTicketConnectorRef(ctx, dbgen.GetTicketConnectorRefParams{
			ID:         ticketID,
			BusinessID: businessID,
		})
		if qerr != nil {
			return qerr // pgx.ErrNoRows -> mapErr -> ErrNotFound
		}
		if !ref.ConnectorID.Valid {
			return fmt.Errorf("ticket connector_id is NULL: %w", errs.ErrNotFound)
		}
		if ref.ExternalID == nil {
			return fmt.Errorf("ticket external_id is NULL: %w", errs.ErrNotFound)
		}
		connID = uuid.UUID(ref.ConnectorID.Bytes)
		extID = *ref.ExternalID
		return nil
	})
	if err != nil {
		return uuid.Nil, "", mapErr(err)
	}
	return connID, extID, nil
}

// EnqueueOutboundComment enqueues a 'comment' outbound op for a connector-linked ticket the
// caller owns, anchored to messageID (for external-id write-back + inbound dedup). Ownership
// and connector-linkage are enforced by the TicketConnectorRef pre-check: a not-found, foreign,
// or unlinked ticket → ErrNotFound. The dbgen insert is :exec (a 0-row INSERT returns nil), so
// the pre-check — not the insert — is the not-found gate.
func (s *Service) EnqueueOutboundComment(ctx context.Context, principalID, businessID, ticketID, messageID uuid.UUID, body string) error {
	// Pre-check confirms the ticket is owned + connector-linked; also gives consistent ErrNotFound semantics.
	if _, _, err := s.TicketConnectorRef(ctx, principalID, businessID, ticketID); err != nil {
		return err
	}
	return mapErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		return dbgen.New(tx).EnqueueOutboundComment(ctx, dbgen.EnqueueOutboundCommentParams{
			ID:         ticketID,
			MessageID:  pgtype.UUID{Bytes: [16]byte(messageID), Valid: true},
			Body:       &body,
			BusinessID: businessID,
		})
	}))
}

// EnqueueOutboundTransition enqueues a 'transition' outbound op (target status in body) for a
// connector-linked ticket the caller owns. Ownership and connector-linkage are enforced by the
// TicketConnectorRef pre-check: a not-found, foreign, or unlinked ticket → ErrNotFound. The
// dbgen insert is :exec with a NOT EXISTS dedup guard, so an identical in-flight transition is
// intentionally a no-op (0 rows inserted → nil), not an error.
func (s *Service) EnqueueOutboundTransition(ctx context.Context, principalID, businessID, ticketID uuid.UUID, status string) error {
	// Pre-check confirms the ticket is owned + connector-linked; also gives consistent ErrNotFound semantics.
	if _, _, err := s.TicketConnectorRef(ctx, principalID, businessID, ticketID); err != nil {
		return err
	}
	return mapErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		return dbgen.New(tx).EnqueueOutboundTransition(ctx, dbgen.EnqueueOutboundTransitionParams{
			ID:         ticketID,
			BusinessID: businessID,
			Status:     status,
		})
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
