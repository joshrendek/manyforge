package mailing

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

const suppressionCursorKind = "mailing-suppression"

func (s *Service) CreateSuppression(ctx context.Context, principalID, businessID uuid.UUID, email, reason string) (Suppression, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return Suppression{}, err
	}
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason == "" {
		reason = "manual"
	}
	if reason != "manual" && reason != "bounce" && reason != "complaint" && reason != "unsubscribe" {
		return Suppression{}, validation("invalid suppression reason")
	}
	var out Suppression
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.InsertMailingSuppression(ctx, dbgen.InsertMailingSuppressionParams{ID: uuid.New(), BusinessID: businessID, TenantRootID: root, Email: email, Reason: dbgen.MailingSuppressionReason(reason), Source: "manual"})
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root, "mailing.suppression.created", "mailing_suppression", row.ID, map[string]any{"reason": reason}); err != nil {
			return err
		}
		out = toSuppression(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) ListSuppressions(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) (Page[Suppression], error) {
	lim := clampLimit(limit)
	var out Page[Suppression]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		var rows []dbgen.MailingSuppression
		if cursor == "" {
			rows, err = q.ListMailingSuppressions(ctx, dbgen.ListMailingSuppressionsParams{BusinessID: businessID, TenantRootID: root, Limit: int32(lim + 1)})
		} else {
			at, id, derr := decodeTimeCursor(suppressionCursorKind, cursor)
			if derr != nil {
				return derr
			}
			rows, err = q.ListMailingSuppressionsAfter(ctx, dbgen.ListMailingSuppressionsAfterParams{BusinessID: businessID, TenantRootID: root, CurCreated: at, CurID: id, Lim: int32(lim + 1)})
		}
		if err != nil {
			return err
		}
		more := len(rows) > lim
		if more {
			rows = rows[:lim]
		}
		out.Items = make([]Suppression, 0, len(rows))
		for _, row := range rows {
			out.Items = append(out.Items, toSuppression(row))
		}
		if more {
			last := rows[len(rows)-1]
			out.NextCursor = strPtr(encodeTimeCursor(suppressionCursorKind, last.CreatedAt, last.ID))
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) DeleteSuppression(ctx context.Context, principalID, businessID, suppressionID uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.DeleteMailingSuppression(ctx, dbgen.DeleteMailingSuppressionParams{ID: suppressionID, TenantRootID: root})
		if err != nil {
			return err
		}
		if row.BusinessID != businessID {
			return pgx.ErrNoRows
		}
		return auditMutation(ctx, tx, principalID, businessID, root, "mailing.suppression.deleted", "mailing_suppression", suppressionID, map[string]any{"reason": row.Reason})
	})
	return mapErr(err)
}

func toSuppression(r dbgen.MailingSuppression) Suppression {
	return Suppression{ID: r.ID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID, Email: r.Email, Reason: string(r.Reason), Source: r.Source, CreatedAt: r.CreatedAt}
}
