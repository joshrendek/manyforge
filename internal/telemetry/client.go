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

// Service owns telemetry client registration.
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
func (s *Service) CreateClient(ctx context.Context, principalID, businessID uuid.UUID, kind, name string) (Client, error) {
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
	var secretPlain string
	var sealed *string
	if s.Sealer != nil {
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
			ID:             uuid.New(),
			BusinessID:     businessID,
			TenantRootID:   tenantRoot,
			Kind:           kind,
			Name:           name,
			PublishableKey: pk,
			SealedSecret:   sealed,
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

func resolveTenantRoot(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID) (uuid.UUID, error) {
	b, err := q.GetBusiness(ctx, businessID)
	if err != nil {
		return uuid.Nil, err
	}
	return b.TenantRootID, nil
}
