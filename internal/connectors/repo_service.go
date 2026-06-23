package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// RepoConnectorSummary is the public (credential-free) view of a repo connector
// returned by List. It intentionally omits secret_ref and any APIToken field.
type RepoConnectorSummary struct {
	ID                  string
	Type                string
	DisplayName         string
	BaseURL             string
	Repo                string
	AllowPrivateBaseURL bool
	Status              string
	CreatedAt           time.Time
}

// RepoConnectorService creates + resolves per-business repo connectors with
// their credential (APIToken) sealed in the vault. Mirrors connectors.Service.
type RepoConnectorService struct {
	DB    serviceDB
	Vault *secrets.Vault
}

// knownRepoConnectorTypes gates the type enum at the service boundary.
var knownRepoConnectorTypes = map[string]bool{"github": true}

// Create validates input, seals the credential into the vault, and inserts the
// repo_connector row — all in one transaction. Returns the new connector's UUID.
func (s *RepoConnectorService) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateRepoConnectorInput) (uuid.UUID, error) {
	in.BaseURL = strings.TrimRight(in.BaseURL, "/")
	if err := validateRepoConnector(in); err != nil {
		return uuid.Nil, err
	}

	credBytes, err := json.Marshal(Credential{APIToken: in.APIToken})
	if err != nil {
		return uuid.Nil, fmt.Errorf("repo_connectors: marshal credential: %w", err)
	}

	id := uuid.New()
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		secretID, perr := s.Vault.Put(ctx, tx, businessID, "repo_connector", credBytes)
		if perr != nil {
			return perr
		}
		_, perr = dbgen.New(tx).InsertRepoConnector(ctx, dbgen.InsertRepoConnectorParams{
			ID:                  id,
			Type:                in.Type,
			DisplayName:         in.DisplayName,
			BaseUrl:             in.BaseURL,
			Repo:                in.Repo,
			AllowPrivateBaseUrl: in.AllowPrivateBaseURL,
			SecretRef:           secretID,
			Config:              []byte("{}"),
			Status:              "enabled",
			BusinessID:          businessID,
		})
		if perr != nil {
			return perr
		}
		tt := "repo_connector"
		entry := audit.Entry{
			BusinessID:       &businessID,
			ActorPrincipalID: &principalID,
			Action:           "repo_connector.created",
			TargetType:       &tt,
			TargetID:         &id,
			Inputs:           map[string]any{"type": in.Type, "base_url": in.BaseURL, "repo": in.Repo},
		}
		if in.AllowPrivateBaseURL {
			dec := "trust_private_base_url"
			entry.Decision = &dec
		}
		return audit.Write(ctx, tx, entry)
	})
	if err != nil {
		return uuid.Nil, mapRepoErr(err)
	}
	return id, nil
}

// Resolve loads the repo connector by id (RLS-scoped to principal's business)
// and unseals its credential from the vault, in one transaction.
// Cross-tenant or unknown id → ErrNotFound (no 403/404 oracle).
func (s *RepoConnectorService) Resolve(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (ResolvedRepoConnector, error) {
	var out ResolvedRepoConnector
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, qerr := dbgen.New(tx).GetRepoConnector(ctx, connectorID)
		if qerr != nil {
			return qerr
		}
		credBytes, oerr := s.Vault.Open(ctx, tx, businessID, row.SecretRef)
		if oerr != nil {
			return oerr
		}
		var cred Credential
		if uerr := json.Unmarshal(credBytes, &cred); uerr != nil {
			return fmt.Errorf("repo_connectors: unmarshal credential: %w", uerr)
		}
		var cfg map[string]any
		if len(row.Config) > 0 {
			if uerr := json.Unmarshal(row.Config, &cfg); uerr != nil {
				return fmt.Errorf("repo_connectors: unmarshal config: %w", uerr)
			}
		}
		out = ResolvedRepoConnector{
			ID:                  row.ID.String(),
			Type:                row.Type,
			BaseURL:             row.BaseUrl,
			Repo:                row.Repo,
			AllowPrivateBaseURL: row.AllowPrivateBaseUrl,
			Config:              cfg,
			Credential:          cred,
		}
		return nil
	})
	if err != nil {
		return ResolvedRepoConnector{}, mapRepoErr(err)
	}
	return out, nil
}

// validateRepoConnector checks required fields and the base_url SSRF guard.
func validateRepoConnector(in CreateRepoConnectorInput) error {
	if !knownRepoConnectorTypes[in.Type] {
		return fmt.Errorf("repo_connectors: unknown type %q: %w", in.Type, errs.ErrValidation)
	}
	if in.DisplayName == "" {
		return fmt.Errorf("repo_connectors: display_name required: %w", errs.ErrValidation)
	}
	if in.BaseURL == "" {
		return fmt.Errorf("repo_connectors: base_url required: %w", errs.ErrValidation)
	}
	if err := validateBaseURL(in.BaseURL, in.AllowPrivateBaseURL); err != nil {
		return err
	}
	if in.Repo == "" {
		return fmt.Errorf("repo_connectors: repo required: %w", errs.ErrValidation)
	}
	if !strings.Contains(in.Repo, "/") {
		return fmt.Errorf("repo_connector: repo must be owner/name: %w", errs.ErrValidation)
	}
	if in.APIToken == "" {
		return fmt.Errorf("repo_connectors: api_token required: %w", errs.ErrValidation)
	}
	return nil
}

// List returns all repo connectors for the given business, newest-first.
// The returned summaries contain NO credential or secret_ref fields.
// Cross-tenant or missing data → ErrNotFound via RLS enforcement.
func (s *RepoConnectorService) List(ctx context.Context, principalID, businessID uuid.UUID) ([]RepoConnectorSummary, error) {
	var out []RepoConnectorSummary
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, err := dbgen.New(tx).ListRepoConnectors(ctx, businessID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, RepoConnectorSummary{
				ID:                  r.ID.String(),
				Type:                r.Type,
				DisplayName:         r.DisplayName,
				BaseURL:             r.BaseUrl,
				Repo:                r.Repo,
				AllowPrivateBaseURL: r.AllowPrivateBaseUrl,
				Status:              r.Status,
				CreatedAt:           r.CreatedAt,
			})
		}
		return nil
	})
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return out, nil
}

// Delete removes a single repo connector by id, scoped to the business (RLS +
// explicit business_id predicate). Returns ErrNotFound when the connector does
// not exist, belongs to a different tenant, or was already deleted.
func (s *RepoConnectorService) Delete(ctx context.Context, principalID, businessID, id uuid.UUID) error {
	return mapRepoErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		n, err := dbgen.New(tx).DeleteRepoConnector(ctx, dbgen.DeleteRepoConnectorParams{
			ID:         id,
			BusinessID: businessID,
		})
		if err != nil {
			return fmt.Errorf("connectors: delete repo connector: %w", err)
		}
		if n == 0 {
			return errs.ErrNotFound
		}
		return nil
	}))
}

// mapRepoErr converts DB/sentinel errors to stable service sentinels.
// Mirrors connectors.mapErr: pgx.ErrNoRows→ErrNotFound, SQLSTATE 23505→ErrConflict.
func mapRepoErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("repo_connectors: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("repo_connectors: duplicate: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("repo_connectors: query: %w", err)
	}
}
