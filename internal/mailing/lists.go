package mailing

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

const listCursorKind = "mailing-list"

func (s *Service) CreateList(ctx context.Context, principalID, businessID uuid.UUID, in ListInput) (List, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 200 {
		return List{}, validation("name is required and must not exceed 200 characters")
	}
	slug := slugify(in.Slug)
	if slug == "" {
		slug = slugify(name)
	}
	if slug == "" || len(slug) > 100 {
		return List{}, validation("slug is invalid")
	}
	var out List
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.InsertMailingList(ctx, dbgen.InsertMailingListParams{
			ID: uuid.New(), BusinessID: businessID, TenantRootID: root, Slug: slug,
			Name: name, Description: in.Description, DoubleOptIn: in.DoubleOptIn,
		})
		if err != nil {
			return err
		}
		if err := auditMutation(ctx, tx, principalID, businessID, root, "mailing.list.created", "mailing_list", row.ID,
			map[string]any{"slug": slug, "double_opt_in": in.DoubleOptIn}); err != nil {
			return err
		}
		out = toList(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) GetList(ctx context.Context, principalID, businessID, listID uuid.UUID) (List, error) {
	var out List
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := loadList(ctx, q, businessID, root, listID)
		if err != nil {
			return err
		}
		out = toList(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) ListLists(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) (Page[List], error) {
	lim := clampLimit(limit)
	var out Page[List]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		var rows []dbgen.MailingList
		if cursor == "" {
			rows, err = q.ListMailingLists(ctx, dbgen.ListMailingListsParams{BusinessID: businessID, TenantRootID: root, Limit: int32(lim + 1)})
		} else {
			at, id, derr := decodeTimeCursor(listCursorKind, cursor)
			if derr != nil {
				return derr
			}
			rows, err = q.ListMailingListsAfter(ctx, dbgen.ListMailingListsAfterParams{BusinessID: businessID, TenantRootID: root, CurCreated: at, CurID: id, Lim: int32(lim + 1)})
		}
		if err != nil {
			return err
		}
		more := len(rows) > lim
		if more {
			rows = rows[:lim]
		}
		out.Items = make([]List, 0, len(rows))
		for _, row := range rows {
			out.Items = append(out.Items, toList(row))
		}
		if more {
			last := rows[len(rows)-1]
			out.NextCursor = strPtr(encodeTimeCursor(listCursorKind, last.CreatedAt, last.ID))
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) UpdateList(ctx context.Context, principalID, businessID, listID uuid.UUID, in ListUpdate) (List, error) {
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 200 {
			return List{}, validation("name is required and must not exceed 200 characters")
		}
		in.Name = &name
	}
	var out List
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err := loadList(ctx, q, businessID, root, listID); err != nil {
			return err
		}
		setDescription := in.SetDescription
		row, err := q.UpdateMailingList(ctx, dbgen.UpdateMailingListParams{
			Name: in.Name, SetDescription: &setDescription, Description: in.Description,
			DoubleOptIn: in.DoubleOptIn, ID: listID, TenantRootID: root,
		})
		if err != nil {
			return err
		}
		if err := auditMutation(ctx, tx, principalID, businessID, root, "mailing.list.updated", "mailing_list", listID,
			map[string]any{"double_opt_in": row.DoubleOptIn}); err != nil {
			return err
		}
		out = toList(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) ArchiveList(ctx context.Context, principalID, businessID, listID uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err := loadList(ctx, q, businessID, root, listID); err != nil {
			return err
		}
		if _, err := q.ArchiveMailingList(ctx, dbgen.ArchiveMailingListParams{ID: listID, TenantRootID: root}); err != nil {
			return err
		}
		return auditMutation(ctx, tx, principalID, businessID, root, "mailing.list.archived", "mailing_list", listID, map[string]any{})
	})
	return mapErr(err)
}

func loadList(ctx context.Context, q *dbgen.Queries, businessID, root, listID uuid.UUID) (dbgen.MailingList, error) {
	row, err := q.GetMailingList(ctx, dbgen.GetMailingListParams{ID: listID, TenantRootID: root})
	if err != nil {
		return dbgen.MailingList{}, err
	}
	if row.BusinessID != businessID {
		return dbgen.MailingList{}, pgx.ErrNoRows
	}
	return row, nil
}

func loadActiveList(ctx context.Context, q *dbgen.Queries, businessID, root, listID uuid.UUID) (dbgen.MailingList, error) {
	row, err := loadList(ctx, q, businessID, root, listID)
	if err != nil {
		return dbgen.MailingList{}, err
	}
	if row.Status != "active" {
		return dbgen.MailingList{}, pgx.ErrNoRows
	}
	return row, nil
}
