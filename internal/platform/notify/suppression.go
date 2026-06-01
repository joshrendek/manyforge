package notify

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// DBSuppression checks spec-001's email_suppression table. Bounce intake is
// principal-less and email_suppression has no tenant scope, so the lookup runs in a
// plain WithTx (no RLS principal). Implements mailer.SuppressionChecker.
type DBSuppression struct{ DB *db.DB }

// IsSuppressed reports whether email is present in the email_suppression table.
func (s DBSuppression) IsSuppressed(ctx context.Context, email string) (bool, error) {
	var out bool
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		v, qerr := dbgen.New(tx).IsSuppressed(ctx, email)
		out = v
		return qerr
	})
	return out, err
}
