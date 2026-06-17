package crm

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

// ActivityService is the timeline surface over activity_entry (spec 005 Phase B). It has
// two faces, mirroring the audit package split:
//
//   - Record is principal-less and takes the CALLER's tx (like audit.Write): the writer
//     already holds an open unit of work — an inbound-email handler, a ticket transition —
//     and threads the activity insert into that same tx so it commits or rolls back atomically
//     with the change it records. The caller supplies a TRUSTED tenant_root_id and runs on an
//     RLS-set (principal-scoped) or RLS-exempt (SECURITY DEFINER) tx; Record performs no
//     ownership resolution of its own.
//   - ListForContact is the authenticated read: it takes the caller's principalID + the
//     target businessID, resolves tenant_root_id inside a WithPrincipal tx (RLS gates
//     visibility), and pushes tenant_root_id into SQL — dual enforcement, mirroring
//     ContactService.List. Unknown / foreign-tenant ids collapse to ErrNotFound (no oracle).
type ActivityService struct {
	DB *db.DB
}

// Record inserts an activity entry on the caller's tx. Kind and SourceType are required
// (ErrValidation). The insert is idempotent: a source_id-bearing event that repeats the
// same (tenant_root_id, source_type, source_id, kind) is a no-op (ON CONFLICT DO NOTHING on
// activity_dedup_idx); a nil SourceID always inserts. tenantRootID is trusted (see the type
// doc) — Record does NOT resolve or verify it.
func (s *ActivityService) Record(ctx context.Context, tx pgx.Tx, tenantRootID uuid.UUID, in ActivityInput) error {
	if strings.TrimSpace(in.Kind) == "" {
		return fmt.Errorf("crm: kind required: %w", errs.ErrValidation)
	}
	if strings.TrimSpace(in.SourceType) == "" {
		return fmt.Errorf("crm: source_type required: %w", errs.ErrValidation)
	}
	err := dbgen.New(tx).InsertActivityEntry(ctx, dbgen.InsertActivityEntryParams{
		ID:           uuid.New(),
		TenantRootID: tenantRootID,
		BusinessID:   in.BusinessID,
		ContactID:    in.ContactID,
		Kind:         in.Kind,
		OccurredAt:   in.OccurredAt,
		Actor:        in.Actor,
		SourceType:   in.SourceType,
		SourceID:     db.PGUUIDPtr(in.SourceID),
		Summary:      in.Summary,
		Metadata:     in.Metadata,
	})
	return mapErr(err)
}

// ListForContact returns a keyset page of a contact's activity, newest-first (occurred_at,
// id DESC). limit is clamped to [1,200] HERE (service boundary). NextCursor is minted from
// the last row's (occurred_at, id) when a further page exists. Mirrors ContactService.List.
func (s *ActivityService) ListForContact(ctx context.Context, principalID, businessID, contactID uuid.UUID, cursor string, limit int) (Page[ActivityEntry], error) {
	lim := clampLimit(limit)
	var out Page[ActivityEntry]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}

		var rows []dbgen.ActivityEntry
		var qerr error
		if cursor == "" {
			rows, qerr = q.ListActivityForContact(ctx, dbgen.ListActivityForContactParams{
				TenantRootID: tenantRoot, ContactID: contactID, Limit: int32(lim + 1),
			})
		} else {
			curOccurred, curID, perr := decodeActivityCursor(cursor)
			if perr != nil {
				return perr
			}
			rows, qerr = q.ListActivityForContactAfter(ctx, dbgen.ListActivityForContactAfterParams{
				TenantRootID: tenantRoot, ContactID: contactID,
				CurOccurred: curOccurred, CurID: curID, Lim: int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}

		rows, next := trim(rows, lim)
		items := make([]ActivityEntry, 0, len(rows))
		for _, r := range rows {
			items = append(items, toActivityEntry(r))
		}
		out.Items = items
		if next {
			last := rows[len(rows)-1]
			out.NextCursor = ptr(encodeActivityCursor(last.OccurredAt, last.ID))
		}
		return nil
	})
	if err != nil {
		return Page[ActivityEntry]{}, mapErr(err)
	}
	return out, nil
}

// toActivityEntry maps a dbgen.ActivityEntry row onto the API view (nullable source_id →
// optional uuid; metadata bytes carried through as raw JSON).
func toActivityEntry(a dbgen.ActivityEntry) ActivityEntry {
	return ActivityEntry{
		ID:           a.ID,
		TenantRootID: a.TenantRootID,
		BusinessID:   a.BusinessID,
		ContactID:    a.ContactID,
		Kind:         a.Kind,
		OccurredAt:   a.OccurredAt,
		Actor:        a.Actor,
		SourceType:   a.SourceType,
		SourceID:     pgUUIDPtr(a.SourceID),
		Summary:      a.Summary,
		Metadata:     a.Metadata,
		CreatedAt:    a.CreatedAt,
	}
}
