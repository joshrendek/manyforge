package ticketing

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// AssignableMember is a human principal eligible to be a ticket assignee (FR-011):
// a member of the business, surfaced to power the assignee picker. The id is the
// principal id used for ticket.assignee_principal_id.
type AssignableMember struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
}

// ListAssignableMembers returns the business's human, active members ordered by
// display name (FR-011) — the candidate assignees for the picker. The result is a
// single server-capped page (a support team is small; the caller can still assign an
// ancestor member or an overflow member by principal id, which the service-layer
// eligibility check validates). Runs in the caller's RLS context, so it only ever
// returns members of a business the caller is authorized over; the route is gated on
// tickets.assign so a caller who cannot assign gets a no-oracle 404 before reaching
// here.
func (s *Service) ListAssignableMembers(ctx context.Context, principalID, businessID uuid.UUID, limit int) ([]AssignableMember, error) {
	lim := clampLimit(limit)
	var out []AssignableMember
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, qerr := dbgen.New(tx).ListAssignableMembers(ctx, dbgen.ListAssignableMembersParams{
			BusinessID: businessID,
			Lim:        int32(lim),
		})
		if qerr != nil {
			return qerr
		}
		out = make([]AssignableMember, 0, len(rows))
		for _, r := range rows {
			out = append(out, AssignableMember{ID: r.ID, Email: r.Email, DisplayName: r.DisplayName})
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}
