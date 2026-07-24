package feedback

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Service is the authenticated feedback surface (boards, posts, votes, ingest keys, convert).
// Every method takes the caller's principalID and the target businessID: queries run inside
// db.WithPrincipal (RLS scopes rows to the caller's authorized businesses) AND push the
// tenant_root_id predicate into SQL (dual enforcement). The tenant_root_id is resolved from
// businessID inside the same tx via the RLS-bound GetBusiness — a business the principal
// cannot see yields ErrNotFound (no oracle).
type Service struct {
	DB *db.DB
}

// CreateBoard inserts a board under the URL business. Slug is derived from Name when omitted;
// a duplicate (business_id, slug) violates the unique index → ErrConflict.
func (s *Service) CreateBoard(ctx context.Context, principalID, businessID uuid.UUID, in BoardInput) (Board, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Board{}, fmt.Errorf("feedback: name required: %w", errs.ErrValidation)
	}
	slug := slugify(in.Slug)
	if slug == "" {
		slug = slugify(name)
	}
	if slug == "" {
		return Board{}, fmt.Errorf("feedback: slug could not be derived from name: %w", errs.ErrValidation)
	}
	var out Board
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, ierr := q.InsertFeedbackBoard(ctx, dbgen.InsertFeedbackBoardParams{
			ID:           uuid.New(),
			BusinessID:   businessID,
			TenantRootID: tenantRoot,
			Slug:         slug,
			Name:         name,
			Description:  in.Description,
			IsPublic:     in.IsPublic,
		})
		if ierr != nil {
			return ierr
		}
		out = toBoard(row)
		return nil
	})
	if err != nil {
		return Board{}, mapErr(err)
	}
	return out, nil
}

// GetBoard loads a single board the caller can see in the URL business, or ErrNotFound.
func (s *Service) GetBoard(ctx context.Context, principalID, businessID, boardID uuid.UUID) (Board, error) {
	var out Board
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, berr := loadBoard(ctx, q, businessID, tenantRoot, boardID)
		if berr != nil {
			return berr
		}
		out = toBoard(row)
		return nil
	})
	if err != nil {
		return Board{}, mapErr(err)
	}
	return out, nil
}

// ListBoards returns a keyset page of the business's boards, newest-first.
func (s *Service) ListBoards(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) (Page[Board], error) {
	lim := clampLimit(limit)
	var out Page[Board]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		var rows []dbgen.FeedbackBoard
		var qerr error
		if cursor == "" {
			rows, qerr = q.ListFeedbackBoards(ctx, dbgen.ListFeedbackBoardsParams{
				BusinessID: businessID, TenantRootID: tenantRoot, Limit: int32(lim + 1),
			})
		} else {
			cAt, cID, perr := decodeTimeCursor(cursorBoards, cursor)
			if perr != nil {
				return perr
			}
			rows, qerr = q.ListFeedbackBoardsAfter(ctx, dbgen.ListFeedbackBoardsAfterParams{
				BusinessID: businessID, TenantRootID: tenantRoot, CurCreated: cAt, CurID: cID, Lim: int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}
		rows, next := trim(rows, lim)
		items := make([]Board, 0, len(rows))
		for _, r := range rows {
			items = append(items, toBoard(r))
		}
		out.Items = items
		if next {
			last := rows[len(rows)-1]
			out.NextCursor = ptr(encodeTimeCursor(cursorBoards, last.CreatedAt, last.ID))
		}
		return nil
	})
	if err != nil {
		return Page[Board]{}, mapErr(err)
	}
	return out, nil
}

// UpdateBoard applies a partial update (name / description / is_public). A foreign-tenant /
// unknown board matches zero rows ⇒ ErrNotFound (no oracle).
func (s *Service) UpdateBoard(ctx context.Context, principalID, businessID, boardID uuid.UUID, in BoardUpdate) (Board, error) {
	var out Board
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		// Confirm the board is in THIS business first (a board in a sibling business under the
		// same tenant would otherwise be updatable via the tenant-scoped id predicate).
		if _, berr := loadBoard(ctx, q, businessID, tenantRoot, boardID); berr != nil {
			return berr
		}
		row, uerr := q.UpdateFeedbackBoard(ctx, dbgen.UpdateFeedbackBoardParams{
			ID:           boardID,
			TenantRootID: tenantRoot,
			Name:         in.Name,
			Description:  in.Description,
			IsPublic:     in.IsPublic,
		})
		if uerr != nil {
			return uerr
		}
		out = toBoard(row)
		return nil
	})
	if err != nil {
		return Board{}, mapErr(err)
	}
	return out, nil
}

// loadBoard fetches a board scoped to (id, tenant_root) and asserts it belongs to the URL
// business, so a sibling-business board under the same tenant collapses to ErrNotFound.
func loadBoard(ctx context.Context, q *dbgen.Queries, businessID, tenantRoot, boardID uuid.UUID) (dbgen.FeedbackBoard, error) {
	row, err := q.GetFeedbackBoard(ctx, dbgen.GetFeedbackBoardParams{ID: boardID, TenantRootID: tenantRoot})
	if err != nil {
		return dbgen.FeedbackBoard{}, err
	}
	if row.BusinessID != businessID {
		return dbgen.FeedbackBoard{}, pgx.ErrNoRows // no oracle — behaves like unknown board
	}
	return row, nil
}

// resolveTenantRoot loads the tenant_root_id for the URL business via the RLS-bound GetBusiness
// query — a business the principal cannot see collapses to ErrNoRows ⇒ ErrNotFound (no oracle).
func resolveTenantRoot(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID) (uuid.UUID, error) {
	b, err := q.GetBusiness(ctx, businessID)
	if err != nil {
		return uuid.Nil, err
	}
	return b.TenantRootID, nil
}

func toBoard(b dbgen.FeedbackBoard) Board {
	return Board{
		ID:           b.ID,
		BusinessID:   b.BusinessID,
		TenantRootID: b.TenantRootID,
		Slug:         b.Slug,
		Name:         b.Name,
		Description:  b.Description,
		IsPublic:     b.IsPublic,
		CreatedAt:    b.CreatedAt,
		UpdatedAt:    b.UpdatedAt,
	}
}

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// slugify lowercases and replaces runs of non-alphanumerics with a single hyphen, trimming
// leading/trailing hyphens. An all-symbol input yields "" (the caller then rejects it).
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugNonAlnum.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
