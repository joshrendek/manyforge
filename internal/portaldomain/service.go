package portaldomain

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

const (
	challengePrefix = "_manyforge-portal."
	tokenPrefix     = "mf-portal-verify="

	cursorKind   = "p"
	cursorSep    = "|"
	defaultLimit = 50
	maxLimit     = 100
)

// TXTResolver looks up DNS TXT records. Production wraps net.DefaultResolver with a
// timeout; tests inject a deterministic stub.
type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// TXTRecord is the DNS record a business publishes to prove it controls a hostname.
type TXTRecord struct{ Name, Value string }

// Domain is a business's custom portal hostname.
type Domain struct {
	ID, BusinessID uuid.UUID
	Hostname       string
	VerifiedAt     *time.Time
	Verification   TXTRecord // name "_manyforge-portal."+Hostname
	CreatedAt      time.Time
}

// Service manages portal hostnames. A hostname routes to a business only once
// verified, and a verified hostname is globally unique.
type Service struct {
	DB           *db.DB
	Resolver     TXTResolver
	InstanceHost string            // host of the public base URL; "" when unset
	Reserved     []string          // platform mail domains; a hostname equal to or under any of these is rejected
	Rand         io.Reader         // nil means crypto/rand
	Forget       func(host string) // optional cache invalidation hook
}

func (s *Service) rand() io.Reader {
	if s.Rand != nil {
		return s.Rand
	}
	return rand.Reader
}

func (s *Service) forget(host string) {
	if s.Forget != nil {
		s.Forget(host)
	}
}

func (s *Service) validateNew(hostname string) error {
	bad := func(why string) error {
		return fmt.Errorf("portaldomain: hostname %s: %w", why, errs.ErrValidation)
	}
	if !ValidHostname(hostname) {
		return bad("is not a valid fully qualified name")
	}
	if IsInternalHost(hostname) {
		return bad("is an internal name")
	}
	if inst := NormalizeHost(s.InstanceHost); inst != "" && hostname == inst {
		return bad("is the platform's own host")
	}
	for _, r := range s.Reserved {
		r = NormalizeHost(r)
		if r != "" && (hostname == r || strings.HasSuffix(hostname, "."+r)) {
			return bad("is reserved")
		}
	}
	return nil
}

// Create registers a pending hostname for the business and returns the DNS challenge.
func (s *Service) Create(ctx context.Context, principalID, businessID uuid.UUID, hostname string) (Domain, error) {
	// Not NormalizeHost: a pasted port must fail validation, not be silently dropped.
	hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	if err := s.validateNew(hostname); err != nil {
		return Domain{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := io.ReadFull(s.rand(), tokenBytes); err != nil {
		return Domain{}, fmt.Errorf("portaldomain: generate verify token: %w", err)
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(tokenBytes)

	var row dbgen.PortalDomain
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, ierr := dbgen.New(tx).InsertPortalDomain(ctx, dbgen.InsertPortalDomainParams{
			ID: uuid.New(), BusinessID: businessID, Hostname: hostname, VerifyToken: token,
		})
		if ierr != nil {
			return ierr
		}
		row = r
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			TenantRootID:     &row.TenantRootID,
			ActorPrincipalID: &principalID,
			Action:           "portal_domain.created",
			TargetType:       &targetType,
			TargetID:         &row.ID,
			NewValue:         map[string]any{"hostname": hostname},
		})
	})
	if err != nil {
		return Domain{}, mapErr(err)
	}
	return toDomain(row), nil
}

// List returns a keyset page of the business's hostnames, oldest first.
func (s *Service) List(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) ([]Domain, string, error) {
	lim := clampLimit(limit)
	var out []Domain
	var next string
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var rows []dbgen.PortalDomain
		var qerr error
		if cursor == "" {
			rows, qerr = q.ListPortalDomains(ctx, dbgen.ListPortalDomainsParams{BusinessID: businessID, Lim: int32(lim + 1)})
		} else {
			ts, id, derr := decodeCursor(cursor)
			if derr != nil {
				return derr
			}
			rows, qerr = q.ListPortalDomainsAfter(ctx, dbgen.ListPortalDomainsAfterParams{
				BusinessID: businessID, CurCreatedAt: ts, CurID: id, Lim: int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}
		if len(rows) > lim {
			rows = rows[:lim]
			last := rows[len(rows)-1]
			next = encodeCursor(last.CreatedAt, last.ID)
		}
		out = make([]Domain, 0, len(rows))
		for _, r := range rows {
			out = append(out, toDomain(r))
		}
		return nil
	})
	if err != nil {
		return nil, "", mapErr(err)
	}
	return out, next, nil
}

// Verify polls the ownership TXT record. A pending domain whose token is published
// is marked verified; anything else (no record, wrong value, lookup error) leaves it
// pending and returns no error. If another business holds the hostname verified but
// no longer publishes its own token while the claimant does, the incumbent is
// demoted (audited by the DEFINER function) so the claim can proceed; otherwise the
// unique index rejects the claim with ErrConflict.
func (s *Service) Verify(ctx context.Context, principalID, businessID, domainID uuid.UUID) (Domain, error) {
	var out Domain
	changed := false
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, gerr := q.GetPortalDomain(ctx, dbgen.GetPortalDomainParams{ID: domainID, BusinessID: businessID})
		if gerr != nil {
			return gerr
		}
		out = toDomain(row)
		if row.VerifiedAt.Valid {
			return nil
		}
		// A lookup error is a pending poll, never a verification or a displacement.
		recs, lerr := s.Resolver.LookupTXT(ctx, challengeName(row.Hostname))
		if lerr != nil || !containsExact(recs, row.VerifyToken) {
			return nil
		}
		var demoted bool
		if terr := tx.QueryRow(ctx, `SELECT portal_domain_takeover_stale($1, $2, $3, $4)`,
			row.ID, businessID, recs, principalID).Scan(&demoted); terr != nil {
			return terr
		}
		n, merr := q.MarkPortalDomainVerified(ctx, dbgen.MarkPortalDomainVerifiedParams{ID: row.ID, BusinessID: businessID})
		if merr != nil {
			return merr
		}
		if n == 0 {
			return nil
		}
		updated, rerr := q.GetPortalDomain(ctx, dbgen.GetPortalDomainParams{ID: domainID, BusinessID: businessID})
		if rerr != nil {
			return rerr
		}
		out = toDomain(updated)
		changed = true
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			TenantRootID:     &updated.TenantRootID,
			ActorPrincipalID: &principalID,
			Action:           "portal_domain.verified",
			TargetType:       &targetType,
			TargetID:         &updated.ID,
			NewValue:         map[string]any{"hostname": updated.Hostname},
		})
	})
	if err != nil {
		return Domain{}, mapErr(err)
	}
	if changed {
		s.forget(out.Hostname)
	}
	return out, nil
}

// Delete removes a hostname. Unknown and foreign ids are both ErrNotFound.
func (s *Service) Delete(ctx context.Context, principalID, businessID, domainID uuid.UUID) error {
	var hostname string
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, gerr := q.GetPortalDomain(ctx, dbgen.GetPortalDomainParams{ID: domainID, BusinessID: businessID})
		if gerr != nil {
			return gerr
		}
		n, derr := q.DeletePortalDomain(ctx, dbgen.DeletePortalDomainParams{ID: domainID, BusinessID: businessID})
		if derr != nil {
			return derr
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		hostname = row.Hostname
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			TenantRootID:     &row.TenantRootID,
			ActorPrincipalID: &principalID,
			Action:           "portal_domain.deleted",
			TargetType:       &targetType,
			TargetID:         &row.ID,
			OldValue:         map[string]any{"hostname": row.Hostname, "verified": row.VerifiedAt.Valid},
		})
	})
	if err != nil {
		return mapErr(err)
	}
	s.forget(hostname)
	return nil
}

// BaseURLFor returns "https://"+hostname of the business's earliest verified
// domain, or ("", false) when it has none (or the lookup fails).
func (s *Service) BaseURLFor(ctx context.Context, businessID uuid.UUID) (string, bool) {
	var host string
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		h, qerr := dbgen.New(tx).PrimaryPortalDomain(ctx, businessID)
		if qerr != nil {
			return qerr
		}
		host = h
		return nil
	})
	if err != nil || host == "" {
		return "", false
	}
	return "https://" + host, true
}

var targetType = "portal_domain"

func challengeName(hostname string) string { return challengePrefix + hostname }

func toDomain(r dbgen.PortalDomain) Domain {
	d := Domain{
		ID: r.ID, BusinessID: r.BusinessID, Hostname: r.Hostname, CreatedAt: r.CreatedAt,
		Verification: TXTRecord{Name: challengeName(r.Hostname), Value: r.VerifyToken},
	}
	if r.VerifiedAt.Valid {
		t := r.VerifiedAt.Time
		d.VerifiedAt = &t
	}
	return d
}

func containsExact(records []string, want string) bool {
	for _, r := range records {
		if r == want {
			return true
		}
	}
	return false
}

func clampLimit(requested int) int {
	switch {
	case requested <= 0:
		return defaultLimit
	case requested > maxLimit:
		return maxLimit
	default:
		return requested
	}
}

// Cursors are opaque: base64url("p|<rfc3339nano>|<uuid>"). A malformed token is a
// validation error, never a 500.
func encodeCursor(ts time.Time, id uuid.UUID) string {
	raw := cursorKind + cursorSep + ts.UTC().Format(time.RFC3339Nano) + cursorSep + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(token string) (time.Time, uuid.UUID, error) {
	bad := func() (time.Time, uuid.UUID, error) {
		return time.Time{}, uuid.Nil, fmt.Errorf("portaldomain: invalid cursor: %w", errs.ErrValidation)
	}
	dec, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return bad()
	}
	parts := strings.SplitN(string(dec), cursorSep, 3)
	if len(parts) != 3 || parts[0] != cursorKind {
		return bad()
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return bad()
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return bad()
	}
	return ts, id, nil
}

// mapErr converts query errors into the service sentinels: no rows (unknown or
// foreign id, or an unauthorized business) is not found, a unique violation is a
// conflict, everything else is wrapped for a generic 500.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("portaldomain: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("portaldomain: duplicate: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrTenantMaintenance):
		return err
	default:
		return fmt.Errorf("portaldomain: query: %w", err)
	}
}
