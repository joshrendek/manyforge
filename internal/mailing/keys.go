package mailing

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

func (s *Service) CreateListKey(ctx context.Context, principalID, businessID, listID uuid.UUID, label *string) (ListKey, error) {
	reader := s.Rand
	if reader == nil {
		reader = rand.Reader
	}
	public, err := randomToken(reader, "mlk_")
	if err != nil {
		return ListKey{}, fmt.Errorf("mailing: generate publishable key: %w", err)
	}
	var secret string
	var sealed *string
	if s.Sealer != nil {
		secret, err = randomToken(reader, "mls_")
		if err != nil {
			return ListKey{}, fmt.Errorf("mailing: generate secret: %w", err)
		}
		blob, err := s.Sealer.Seal([]byte(secret))
		if err != nil {
			return ListKey{}, fmt.Errorf("mailing: seal secret: %w", err)
		}
		sealed = &blob
	}
	label = cleanOptional(label)
	if label != nil && len(*label) > 120 {
		return ListKey{}, validation("label must not exceed 120 characters")
	}
	var out ListKey
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = loadActiveList(ctx, q, businessID, root, listID); err != nil {
			return err
		}
		row, err := q.InsertMailingListKey(ctx, dbgen.InsertMailingListKeyParams{
			ID: uuid.New(), BusinessID: businessID, TenantRootID: root,
			ListID: listID, PublishableKey: public, SealedSecret: sealed, Label: label,
		})
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root,
			"mailing.list_key.created", "mailing_list_key", row.ID,
			map[string]any{"list_id": listID, "has_secret": sealed != nil}); err != nil {
			return err
		}
		out = toListKey(row)
		out.Secret = secret
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) ListListKeys(ctx context.Context, principalID, businessID, listID uuid.UUID, limit int) ([]ListKey, error) {
	lim := clampLimit(limit)
	var out []ListKey
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = loadList(ctx, q, businessID, root, listID); err != nil {
			return err
		}
		rows, err := q.ListMailingListKeys(ctx, dbgen.ListMailingListKeysParams{ListID: listID, TenantRootID: root, Limit: int32(lim)})
		if err != nil {
			return err
		}
		out = make([]ListKey, 0, len(rows))
		for _, row := range rows {
			if row.BusinessID != businessID {
				return pgx.ErrNoRows
			}
			out = append(out, toListKey(row))
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) RevokeListKey(ctx context.Context, principalID, businessID, listID, keyID uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = loadList(ctx, q, businessID, root, listID); err != nil {
			return err
		}
		row, err := q.RevokeMailingListKey(ctx, dbgen.RevokeMailingListKeyParams{ID: keyID, TenantRootID: root})
		if err != nil {
			return err
		}
		if row.BusinessID != businessID || row.ListID != listID {
			return pgx.ErrNoRows
		}
		return auditMutation(ctx, tx, principalID, businessID, root, "mailing.list_key.revoked", "mailing_list_key", keyID, map[string]any{"list_id": listID})
	})
	return mapErr(err)
}

func randomToken(r io.Reader, prefix string) (string, error) {
	var raw [32]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func toListKey(r dbgen.MailingListKey) ListKey {
	return ListKey{
		ID: r.ID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID,
		ListID: r.ListID, PublishableKey: r.PublishableKey, Label: r.Label,
		Status: r.Status, CreatedAt: r.CreatedAt, RevokedAt: timePtr(r.RevokedAt),
		HasSecret: r.SealedSecret != nil,
	}
}
