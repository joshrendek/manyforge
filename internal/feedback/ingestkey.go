package feedback

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// keyPrefix marks a publishable feedback ingest key. The key is a PUBLIC client token (Sentry
// DSN style) — safe to embed in a mobile app binary; it is not a secret. 24 random bytes →
// 32 base64url chars gives ~192 bits of entropy so keys are unguessable / non-enumerable
// (the oracle boundary depends on this: you cannot discover a board without being handed a key).
const keyPrefix = "fbk_"

// newPublishableKey mints a fresh publishable key. crypto/rand failure is surfaced (never a
// weak/predictable fallback).
func newPublishableKey() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("feedback: key generation: %w", err)
	}
	return keyPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// CreateIngestKey mints a publishable key for a board in the URL business. The plaintext key is
// returned once in the response; it is stored verbatim (publishable, not sealed).
func (s *Service) CreateIngestKey(ctx context.Context, principalID, businessID, boardID uuid.UUID, label *string) (IngestKey, error) {
	pk, kerr := newPublishableKey()
	if kerr != nil {
		return IngestKey{}, kerr
	}
	var out IngestKey
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		if _, berr := loadBoard(ctx, q, businessID, tenantRoot, boardID); berr != nil {
			return berr
		}
		row, ierr := q.InsertFeedbackIngestKey(ctx, dbgen.InsertFeedbackIngestKeyParams{
			ID:             uuid.New(),
			BusinessID:     businessID,
			TenantRootID:   tenantRoot,
			BoardID:        boardID,
			PublishableKey: pk,
			Label:          label,
		})
		if ierr != nil {
			return ierr
		}
		out = toIngestKey(row)
		return nil
	})
	if err != nil {
		return IngestKey{}, mapErr(err)
	}
	return out, nil
}

// ListIngestKeys returns a board's ingest keys (newest-first, capped). Keys are publishable, so
// returning the key value is intentional (an operator needs it to configure the SDK).
func (s *Service) ListIngestKeys(ctx context.Context, principalID, businessID, boardID uuid.UUID, limit int) ([]IngestKey, error) {
	lim := clampLimit(limit)
	var out []IngestKey
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		if _, berr := loadBoard(ctx, q, businessID, tenantRoot, boardID); berr != nil {
			return berr
		}
		rows, qerr := q.ListFeedbackIngestKeys(ctx, dbgen.ListFeedbackIngestKeysParams{
			BoardID: boardID, TenantRootID: tenantRoot, Limit: int32(lim),
		})
		if qerr != nil {
			return qerr
		}
		out = make([]IngestKey, 0, len(rows))
		for _, r := range rows {
			out = append(out, toIngestKey(r))
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// RevokeIngestKey disables a key (SDKs using it then fail the feedback_public_board lookup →
// uniform 401). An already-revoked / unknown / foreign-tenant key is ErrNotFound (no oracle).
func (s *Service) RevokeIngestKey(ctx context.Context, principalID, businessID, keyID uuid.UUID) (IngestKey, error) {
	var out IngestKey
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, rerr := q.RevokeFeedbackIngestKey(ctx, dbgen.RevokeFeedbackIngestKeyParams{
			ID: keyID, TenantRootID: tenantRoot,
		})
		if rerr != nil {
			return rerr
		}
		// Defense-in-depth: the RLS predicate + tenant_root already scope this, but assert the
		// key's business matches the URL business so a sibling-business key can't be revoked here.
		if row.BusinessID != businessID {
			return errs.ErrNotFound
		}
		out = toIngestKey(row)
		return nil
	})
	if err != nil {
		return IngestKey{}, mapErr(err)
	}
	return out, nil
}

func toIngestKey(k dbgen.FeedbackIngestKey) IngestKey {
	return IngestKey{
		ID:             k.ID,
		BusinessID:     k.BusinessID,
		TenantRootID:   k.TenantRootID,
		BoardID:        k.BoardID,
		PublishableKey: k.PublishableKey,
		Label:          k.Label,
		Status:         k.Status,
		CreatedAt:      k.CreatedAt,
		RevokedAt:      pgTimePtr(k.RevokedAt),
	}
}
