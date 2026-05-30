package authz

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// Service implements the RBAC use cases: the permission catalog and role CRUD.
type Service struct {
	DB *db.DB
}

// Permission is the API-facing view of a catalog capability.
type Permission struct {
	Key         string `json:"key"`
	Module      string `json:"module"`
	Description string `json:"description"`
}

// ListPermissions returns the global capability catalog, keyset-paginated by key.
// The catalog is a system table (no RLS), readable by any authenticated caller.
// It fetches limit+1 rows to detect whether a further page exists.
func (s *Service) ListPermissions(ctx context.Context, cursor string, limit int) ([]Permission, *string, error) {
	out := make([]Permission, 0, limit)
	var next *string
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := dbgen.New(tx).ListPermissions(ctx, dbgen.ListPermissionsParams{Key: cursor, Limit: int32(limit + 1)})
		if err != nil {
			return err
		}
		if len(rows) > limit {
			last := rows[limit-1].Key
			next = &last
			rows = rows[:limit]
		}
		for _, r := range rows {
			out = append(out, Permission{Key: r.Key, Module: r.Module, Description: r.Description})
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, next, nil
}
