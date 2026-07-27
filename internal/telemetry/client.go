package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	appdb "github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// keyPrefix marks a PUBLISHABLE telemetry key. Like a Sentry DSN, it is a public client token —
// safe to embed in an app binary or a web page. 24 random bytes → 32 base64url chars gives ~192
// bits of entropy, which is what makes keys non-enumerable: without being handed one you cannot
// discover that a client exists.
const keyPrefix = "mfk_"

// secretPrefix marks a telemetry signing SECRET. Server-to-server only — deliberately a distinct
// prefix from mfk_ so the two can never be confused in a config file. It is returned once at
// creation and only its sealed form is persisted.
const secretPrefix = "mfs_"

// keyBodyLen is the base64url length of a 24-byte publishable key body.
const keyBodyLen = 32

func newPublishableKey() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("telemetry: key generation: %w", err)
	}
	return keyPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("telemetry: secret generation: %w", err)
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Service owns telemetry client registration, revocation, and analytics-site moves.
type Service struct {
	DB *appdb.DB
	// Sealer seals the mfs_ signing secret at rest. Nil ⇒ the signed tier is not configured for
	// this deployment; clients are still minted, just without a secret (HasSecret false).
	Sealer *crypto.Sealer
}

func NewService(database *appdb.DB, sealer *crypto.Sealer) *Service {
	return &Service{DB: database, Sealer: sealer}
}

const maxClientsPerPage = 200

func clampLimit(n int) int {
	if n <= 0 {
		return 50
	}
	if n > maxClientsPerPage {
		return maxClientsPerPage
	}
	return n
}

func validKind(kind string) bool { return kind == KindAnalytics || kind == KindCrash }

// CreateClient registers a telemetry source under the URL business and mints its keys. The
// plaintext secret is returned once here and never again.
// requireSignature opts the client into mandatory HMAC ingest. Leave it false for anything that
// runs on a device or in a browser: those authenticate with the embeddable mfk_ key alone, and the
// mfs_ secret must never ship inside a client binary. Set it only for a server-to-server sender.
func (s *Service) CreateClient(ctx context.Context, principalID, businessID uuid.UUID, kind, name string, requireSignature bool) (Client, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Client{}, fmt.Errorf("telemetry: name is required: %w", errs.ErrValidation)
	}
	if !validKind(kind) {
		return Client{}, fmt.Errorf("telemetry: kind must be %q or %q: %w",
			KindAnalytics, KindCrash, errs.ErrValidation)
	}

	pk, err := newPublishableKey()
	if err != nil {
		return Client{}, err
	}
	// A secret is minted ONLY for a signing client. Issuing one to every client would be dead
	// credential surface, and (worse) an ingest path that keys "must sign" off "has a secret"
	// would then silently lock out every embeddable SDK key.
	var secretPlain string
	var sealed *string
	if requireSignature {
		if s.Sealer == nil {
			return Client{}, fmt.Errorf(
				"telemetry: signed clients require a configured master key: %w", errs.ErrValidation)
		}
		sec, serr := newSecret()
		if serr != nil {
			return Client{}, serr
		}
		blob, berr := s.Sealer.Seal([]byte(sec))
		if berr != nil {
			return Client{}, fmt.Errorf("telemetry: seal secret: %w", berr)
		}
		secretPlain = sec
		sealed = &blob
	}

	var out Client
	txErr := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, ierr := q.InsertTelemetryClient(ctx, dbgen.InsertTelemetryClientParams{
			ID:               uuid.New(),
			BusinessID:       businessID,
			TenantRootID:     tenantRoot,
			Kind:             kind,
			Name:             name,
			PublishableKey:   pk,
			RequireSignature: requireSignature,
			SealedSecret:     sealed,
		})
		if ierr != nil {
			return ierr
		}
		out = toClient(row)
		return nil
	})
	if txErr != nil {
		return Client{}, mapErr(txErr)
	}
	// Write-once: toClient never sets Secret, so list/revoke can only ever report HasSecret.
	out.Secret = secretPlain
	return out, nil
}

// ListClients returns the business's registered clients, newest first. Publishable keys are
// returned deliberately — an operator needs them to configure an SDK.
func (s *Service) ListClients(ctx context.Context, principalID, businessID uuid.UUID, limit int) ([]Client, error) {
	var out []Client
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		rows, qerr := q.ListTelemetryClients(ctx, dbgen.ListTelemetryClientsParams{
			BusinessID:   businessID,
			TenantRootID: tenantRoot,
			Limit:        int32(clampLimit(limit)),
		})
		if qerr != nil {
			return qerr
		}
		out = make([]Client, 0, len(rows))
		for _, r := range rows {
			out = append(out, toClient(r))
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// RevokeClient disables a client; ingest with its key then fails the resolve and returns the same
// 401 as an unknown key. An unknown, already-revoked, or foreign-tenant id is ErrNotFound — the
// three are deliberately indistinguishable so the endpoint is not a UUID-existence oracle.
func (s *Service) RevokeClient(ctx context.Context, principalID, businessID, clientID uuid.UUID) (Client, error) {
	var out Client
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, rerr := q.RevokeTelemetryClient(ctx, dbgen.RevokeTelemetryClientParams{
			ID: clientID, TenantRootID: tenantRoot,
		})
		if rerr != nil {
			return rerr
		}
		// Defense in depth: RLS and the tenant_root predicate already scope this, but assert the
		// client's business matches the URL business so a sibling business's client cannot be
		// revoked through this route.
		if row.BusinessID != businessID {
			return errs.ErrNotFound
		}
		out = toClient(row)
		return nil
	})
	if err != nil {
		return Client{}, mapErr(err)
	}
	return out, nil
}

// MoveTargets returns eligible destinations for an active analytics client. It validates the
// source/client pair in the same permission-aware shape as MoveClient, so an unknown client, a
// foreign client, and a source where the caller lacks telemetry.write are indistinguishable.
func (s *Service) MoveTargets(ctx context.Context, principalID, sourceBusinessID, clientID uuid.UUID) ([]MoveTarget, error) {
	out := []MoveTarget{}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		var sourceTenantRoot uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT c.tenant_root_id
			   FROM telemetry_client c
			  WHERE c.id = $1
			    AND c.business_id = $2
			    AND c.kind = 'analytics'
			    AND c.status = 'active'
			    AND c.revoked_at IS NULL
			    AND c.business_id IN (
			        SELECT business_id
			          FROM businesses_with_permission(current_principal(), 'telemetry.write')
			    )`,
			clientID, sourceBusinessID).Scan(&sourceTenantRoot)
		if err != nil {
			return err
		}

		rows, err := tx.Query(ctx,
			`SELECT b.id, b.tenant_root_id, b.name
			   FROM business b
			  WHERE b.tenant_root_id = $1
			    AND b.id <> $2
			    AND b.status = 'active'
			    AND b.id IN (
			        SELECT business_id
			          FROM businesses_with_permission(current_principal(), 'telemetry.write')
			    )
			  ORDER BY lower(b.name), b.id`,
			sourceTenantRoot, sourceBusinessID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var target MoveTarget
			if err := rows.Scan(&target.ID, &target.TenantRootID, &target.Name); err != nil {
				return err
			}
			out = append(out, target)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// MoveClient atomically changes an active analytics client's owning business while preserving its
// identity and full history. telemetry_move_analytics_client performs the target permission check
// and takes the client lock that serializes this transaction with every ingest path.
func (s *Service) MoveClient(ctx context.Context, principalID, sourceBusinessID, clientID, targetBusinessID uuid.UUID) (Client, error) {
	var out Client
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		var outcome string
		if err := tx.QueryRow(ctx,
			`SELECT telemetry_move_analytics_client($1, $2, $3)`,
			sourceBusinessID, clientID, targetBusinessID).Scan(&outcome); err != nil {
			return err
		}
		switch outcome {
		case "moved":
			row, err := dbgen.New(tx).GetTelemetryClient(ctx, dbgen.GetTelemetryClientParams{
				ID: clientID, BusinessID: targetBusinessID,
			})
			if err != nil {
				return err
			}
			out = toClient(row)
			return nil
		case "not_found":
			return errs.ErrNotFound
		case "conflict":
			return errs.ErrConflict
		default:
			return fmt.Errorf("telemetry: unexpected move outcome %q", outcome)
		}
	})
	if err != nil {
		return Client{}, mapErr(err)
	}
	return out, nil
}

func resolveTenantRoot(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID) (uuid.UUID, error) {
	b, err := q.GetBusiness(ctx, businessID)
	if err != nil {
		return uuid.Nil, err
	}
	return b.TenantRootID, nil
}
