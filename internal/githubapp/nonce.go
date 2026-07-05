package githubapp

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// NonceService enforces single-use setup/link state via github_setup_nonce
// (migrations/0081): tenantless, no RLS, INSERT ... ON CONFLICT DO NOTHING so
// "first use vs replay" is DB-enforced rather than a check-then-act race.
type NonceService struct{ DB txRunner }

// Consume returns true the FIRST time a nonce is seen, false on replay.
func (s *NonceService) Consume(ctx context.Context, nonce string) (bool, error) {
	var first bool
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, "INSERT INTO github_setup_nonce (nonce) VALUES ($1) ON CONFLICT (nonce) DO NOTHING", nonce)
		if err != nil {
			return err
		}
		first = ct.RowsAffected() > 0
		return nil
	})
	return first, err
}
