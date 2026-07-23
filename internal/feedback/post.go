package feedback

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// maxTitleLen / maxBodyLen bound stored post text (defense-in-depth; the public DEFINER also
// caps the title). Titles over the cap are a validation error rather than a silent truncation.
const (
	maxTitleLen = 300
	maxBodyLen  = 20000
)

// validStatuses is the moderation workflow; SetPostStatus rejects anything else as ErrValidation.
var validStatuses = map[string]dbgen.FeedbackStatus{
	"open":        dbgen.FeedbackStatusOpen,
	"planned":     dbgen.FeedbackStatusPlanned,
	"in_progress": dbgen.FeedbackStatusInProgress,
	"done":        dbgen.FeedbackStatusDone,
	"declined":    dbgen.FeedbackStatusDeclined,
}

// CreatePost records an INTERNAL (principal-authored) submission on a board in the URL business.
func (s *Service) CreatePost(ctx context.Context, principalID, businessID, boardID uuid.UUID, in PostInput) (Post, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Post{}, fmt.Errorf("feedback: title required: %w", errs.ErrValidation)
	}
	if len(title) > maxTitleLen {
		return Post{}, fmt.Errorf("feedback: title too long: %w", errs.ErrValidation)
	}
	if in.Body != nil && len(*in.Body) > maxBodyLen {
		return Post{}, fmt.Errorf("feedback: body too long: %w", errs.ErrValidation)
	}
	var out Post
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		if _, berr := loadBoard(ctx, q, businessID, tenantRoot, boardID); berr != nil {
			return berr
		}
		row, ierr := q.InsertFeedbackPost(ctx, dbgen.InsertFeedbackPostParams{
			ID:                uuid.New(),
			BusinessID:        businessID,
			TenantRootID:      tenantRoot,
			BoardID:           boardID,
			Title:             title,
			Body:              in.Body,
			AuthorPrincipalID: db.PGUUID(principalID),
		})
		if ierr != nil {
			return ierr
		}
		out = toPost(row)
		return nil
	})
	if err != nil {
		return Post{}, mapErr(err)
	}
	return out, nil
}

// GetPost loads a single live post the caller can see in the URL business, or ErrNotFound.
func (s *Service) GetPost(ctx context.Context, principalID, businessID, postID uuid.UUID) (Post, error) {
	var out Post
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, gerr := loadPost(ctx, q, businessID, tenantRoot, postID)
		if gerr != nil {
			return gerr
		}
		out = toPost(row)
		return nil
	})
	if err != nil {
		return Post{}, mapErr(err)
	}
	return out, nil
}

// ListPosts returns a keyset page of a board's live posts, newest-first.
func (s *Service) ListPosts(ctx context.Context, principalID, businessID, boardID uuid.UUID, cursor string, limit int) (Page[Post], error) {
	lim := clampLimit(limit)
	var out Page[Post]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		if _, berr := loadBoard(ctx, q, businessID, tenantRoot, boardID); berr != nil {
			return berr
		}
		var rows []dbgen.FeedbackPost
		var qerr error
		if cursor == "" {
			rows, qerr = q.ListFeedbackPosts(ctx, dbgen.ListFeedbackPostsParams{
				BoardID: boardID, TenantRootID: tenantRoot, Limit: int32(lim + 1),
			})
		} else {
			cAt, cID, perr := decodeTimeCursor(cursorPosts, cursor)
			if perr != nil {
				return perr
			}
			rows, qerr = q.ListFeedbackPostsAfter(ctx, dbgen.ListFeedbackPostsAfterParams{
				BoardID: boardID, TenantRootID: tenantRoot, CurCreated: cAt, CurID: cID, Lim: int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}
		rows, next := trim(rows, lim)
		items := make([]Post, 0, len(rows))
		for _, r := range rows {
			items = append(items, toPost(r))
		}
		out.Items = items
		if next {
			last := rows[len(rows)-1]
			out.NextCursor = ptr(encodeTimeCursor(cursorPosts, last.CreatedAt, last.ID))
		}
		return nil
	})
	if err != nil {
		return Page[Post]{}, mapErr(err)
	}
	return out, nil
}

// SetPostStatus moves a post through the moderation workflow. An unknown status is
// ErrValidation; a vanished / foreign-tenant post is ErrNotFound.
func (s *Service) SetPostStatus(ctx context.Context, principalID, businessID, postID uuid.UUID, status string) (Post, error) {
	st, ok := validStatuses[status]
	if !ok {
		return Post{}, fmt.Errorf("feedback: invalid status %q: %w", status, errs.ErrValidation)
	}
	var out Post
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		if _, gerr := loadPost(ctx, q, businessID, tenantRoot, postID); gerr != nil {
			return gerr
		}
		row, uerr := q.SetFeedbackPostStatus(ctx, dbgen.SetFeedbackPostStatusParams{
			ID: postID, TenantRootID: tenantRoot, Status: st,
		})
		if uerr != nil {
			return uerr
		}
		out = toPost(row)
		return nil
	})
	if err != nil {
		return Post{}, mapErr(err)
	}
	return out, nil
}

// DeletePost soft-deletes a post. A vanished / foreign-tenant / already-deleted post is
// ErrNotFound (the Get + delete share one tx, so there is no TOCTOU window).
func (s *Service) DeletePost(ctx context.Context, principalID, businessID, postID uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		if _, gerr := loadPost(ctx, q, businessID, tenantRoot, postID); gerr != nil {
			return gerr
		}
		return q.SoftDeleteFeedbackPost(ctx, dbgen.SoftDeleteFeedbackPostParams{ID: postID, TenantRootID: tenantRoot})
	})
	return mapErr(err)
}

// Vote records an INTERNAL vote (voter identity = the caller's principal id), enforcing one
// vote per principal per post. Returns whether this call recorded a new vote and the fresh
// count. A replay (already voted) returns (false, current count) — not an error.
func (s *Service) Vote(ctx context.Context, principalID, businessID, postID uuid.UUID) (bool, int, error) {
	var voted bool
	var count int
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		post, gerr := loadPost(ctx, q, businessID, tenantRoot, postID)
		if gerr != nil {
			return gerr
		}
		n, ierr := q.InsertFeedbackVote(ctx, dbgen.InsertFeedbackVoteParams{
			ID:            uuid.New(),
			BusinessID:    businessID,
			TenantRootID:  tenantRoot,
			PostID:        postID,
			VoterIdentity: principalID.String(),
		})
		if ierr != nil {
			return ierr
		}
		if n > 0 {
			c, uerr := q.IncrementFeedbackPostVoteCount(ctx, dbgen.IncrementFeedbackPostVoteCountParams{
				ID: postID, TenantRootID: tenantRoot,
			})
			if uerr != nil {
				return uerr
			}
			voted, count = true, int(c)
		} else {
			voted, count = false, int(post.VoteCount)
		}
		return nil
	})
	if err != nil {
		return false, 0, mapErr(err)
	}
	return voted, count, nil
}

// ConvertToTicket converts a post to a support ticket (spec 002) and links it, idempotently.
// The post is loaded under RLS first (confirms the caller can see it in the URL business), then
// the SECURITY DEFINER convert_feedback_post_to_ticket runs in the same tx with the trusted
// (business_id, tenant_root). Returns the (new or existing) ticket id.
func (s *Service) ConvertToTicket(ctx context.Context, principalID, businessID, postID uuid.UUID) (uuid.UUID, error) {
	var ticketID uuid.UUID
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		if _, gerr := loadPost(ctx, q, businessID, tenantRoot, postID); gerr != nil {
			return gerr
		}
		return tx.QueryRow(ctx,
			`SELECT convert_feedback_post_to_ticket($1, $2, $3)`,
			postID, businessID, tenantRoot,
		).Scan(&ticketID)
	})
	if err != nil {
		return uuid.Nil, mapErr(err)
	}
	return ticketID, nil
}

// loadPost fetches a live post scoped to (id, tenant_root) and asserts it belongs to the URL
// business, so a sibling-business post under the same tenant collapses to ErrNotFound.
func loadPost(ctx context.Context, q *dbgen.Queries, businessID, tenantRoot, postID uuid.UUID) (dbgen.FeedbackPost, error) {
	row, err := q.GetFeedbackPost(ctx, dbgen.GetFeedbackPostParams{ID: postID, TenantRootID: tenantRoot})
	if err != nil {
		return dbgen.FeedbackPost{}, err
	}
	if row.BusinessID != businessID {
		return dbgen.FeedbackPost{}, pgx.ErrNoRows
	}
	return row, nil
}

func toPost(p dbgen.FeedbackPost) Post {
	return Post{
		ID:                p.ID,
		BusinessID:        p.BusinessID,
		TenantRootID:      p.TenantRootID,
		BoardID:           p.BoardID,
		Title:             p.Title,
		Body:              p.Body,
		Status:            string(p.Status),
		VoteCount:         int(p.VoteCount),
		AuthorKind:        p.AuthorKind,
		AuthorPrincipalID: pgUUIDPtr(p.AuthorPrincipalID),
		AuthorIdentity:    p.AuthorIdentity,
		TicketID:          pgUUIDPtr(p.TicketID),
		CreatedAt:         p.CreatedAt,
		UpdatedAt:         p.UpdatedAt,
	}
}
