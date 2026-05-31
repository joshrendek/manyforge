// Package account owns human identity: sign-up, email verification, sign-in,
// session refresh, and logout. Account/principal/token tables are auth-internal
// and not RLS-scoped, so these flows run via db.WithTx.
package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/mailer"
)

// ErrInvalidCredentials is the single, generic sign-in failure (no oracle).
var ErrInvalidCredentials = errors.New("invalid credentials")

// Service implements the account use cases.
type Service struct {
	DB         *db.DB
	Ring       *auth.KeyRing
	Mailer     mailer.Mailer
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	TokenTTL   time.Duration
	Now        func() time.Time
}

// TokenPair is the access + refresh token result of sign-in/refresh.
type TokenPair struct {
	Access    string
	Refresh   string
	ExpiresIn int
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Signup creates an account + its human principal and issues a verification
// token (returned for test/handler use; production emails it).
func (s *Service) Signup(ctx context.Context, email, displayName, password string) (accountID uuid.UUID, verifyToken string, err error) {
	if email == "" || displayName == "" || len(password) < 12 {
		return uuid.Nil, "", fmt.Errorf("email, display name, and a 12+ char password are required: %w", errs.ErrValidation)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return uuid.Nil, "", err
	}
	raw, err := auth.NewOpaqueToken()
	if err != nil {
		return uuid.Nil, "", err
	}
	err = s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		acc, err := q.CreateAccount(ctx, dbgen.CreateAccountParams{
			ID: uuid.New(), Email: email, PasswordHash: &hash, DisplayName: displayName,
		})
		if err != nil {
			return err
		}
		accountID = acc.ID
		if _, err := q.CreateHumanPrincipal(ctx, dbgen.CreateHumanPrincipalParams{
			ID: uuid.New(), AccountID: db.PGUUID(acc.ID),
		}); err != nil {
			return err
		}
		_, err = q.CreateOneTimeToken(ctx, dbgen.CreateOneTimeTokenParams{
			ID: uuid.New(), AccountID: db.PGUUID(acc.ID), Email: email,
			Purpose: "verify_email", TokenHash: auth.HashToken(raw), ExpiresAt: s.now().Add(s.TokenTTL),
		})
		return err
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	if s.Mailer != nil {
		_ = s.Mailer.Send(ctx, mailer.Message{To: email, Subject: "Verify your email", Body: "token: " + raw})
	}
	return accountID, raw, nil
}

// VerifyEmail consumes a verification token and marks the account verified.
func (s *Service) VerifyEmail(ctx context.Context, rawToken string) error {
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tok, err := q.ConsumeOneTimeToken(ctx, dbgen.ConsumeOneTimeTokenParams{
			TokenHash: auth.HashToken(rawToken), Purpose: "verify_email",
		})
		if err != nil {
			return fmt.Errorf("invalid or expired token: %w", errs.ErrValidation)
		}
		if !tok.AccountID.Valid {
			return fmt.Errorf("token not bound to an account: %w", errs.ErrValidation)
		}
		return q.MarkEmailVerified(ctx, tok.AccountID.Bytes)
	})
}

// Login authenticates and returns a token pair. Failures are uniform and
// fixed-cost to defeat existence/timing oracles (FR-026).
func (s *Service) Login(ctx context.Context, email, password string) (TokenPair, error) {
	var tp TokenPair
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		acc, err := q.GetAccountByEmail(ctx, email)
		if err != nil {
			auth.DummyVerify(password) // match latency of the real branch
			return ErrInvalidCredentials
		}
		if acc.PasswordHash == nil {
			auth.DummyVerify(password)
			return ErrInvalidCredentials
		}
		if err := auth.VerifyPassword(password, *acc.PasswordHash); err != nil {
			return ErrInvalidCredentials
		}
		// security: a deactivated/suspended account is denied with the same generic
		// failure as a wrong password — no "account disabled" oracle (FR-026).
		if acc.Status != "active" {
			return ErrInvalidCredentials
		}
		prin, err := q.GetPrincipalByAccount(ctx, db.PGUUID(acc.ID))
		if err != nil {
			return err
		}
		access, err := s.Ring.Sign(prin.ID, s.AccessTTL, s.now())
		if err != nil {
			return err
		}
		refresh, err := auth.IssueRefresh(ctx, tx, prin.ID, s.RefreshTTL, s.now())
		if err != nil {
			return err
		}
		tp = TokenPair{Access: access, Refresh: refresh, ExpiresIn: int(s.AccessTTL.Seconds())}
		return nil
	})
	return tp, err
}

// Refresh rotates a refresh token and issues a fresh access token. On detected
// reuse the family-revoke is committed (reuse flag), then ErrRefreshReuse is
// returned to the caller.
func (s *Service) Refresh(ctx context.Context, rawRefresh string) (TokenPair, error) {
	var tp TokenPair
	var reuse bool
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		newRefresh, pid, ru, err := auth.RotateRefresh(ctx, tx, rawRefresh, s.RefreshTTL, s.now())
		if err != nil {
			return err
		}
		if ru {
			reuse = true // commit the family revoke, surface the error after commit
			return nil
		}
		access, err := s.Ring.Sign(pid, s.AccessTTL, s.now())
		if err != nil {
			return err
		}
		tp = TokenPair{Access: access, Refresh: newRefresh, ExpiresIn: int(s.AccessTTL.Seconds())}
		return nil
	})
	if err != nil {
		return TokenPair{}, err
	}
	if reuse {
		return TokenPair{}, auth.ErrRefreshReuse
	}
	return tp, nil
}

// Logout revokes the presented refresh token's family.
func (s *Service) Logout(ctx context.Context, rawRefresh string) error {
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return auth.RevokeRefreshByToken(ctx, tx, rawRefresh)
	})
}

// Profile is the /me view of an account.
type Profile struct {
	ID            uuid.UUID
	Email         string
	DisplayName   string
	EmailVerified bool
	Status        string
}

func toProfile(a dbgen.Account) Profile {
	return Profile{ID: a.ID, Email: a.Email, DisplayName: a.DisplayName, EmailVerified: a.EmailVerifiedAt.Valid, Status: a.Status}
}

// GetProfile returns the account for the given principal.
func (s *Service) GetProfile(ctx context.Context, principalID uuid.UUID) (Profile, error) {
	var p Profile
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		acc, err := dbgen.New(tx).GetAccountByPrincipal(ctx, principalID)
		if err != nil {
			return errs.ErrNotFound
		}
		p = toProfile(acc)
		return nil
	})
	return p, err
}

// UpdateProfile updates the display name of the principal's account.
func (s *Service) UpdateProfile(ctx context.Context, principalID uuid.UUID, displayName string) (Profile, error) {
	if displayName == "" {
		return Profile{}, fmt.Errorf("display name is required: %w", errs.ErrValidation)
	}
	var p Profile
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		acc, err := q.GetAccountByPrincipal(ctx, principalID)
		if err != nil {
			return errs.ErrNotFound
		}
		updated, err := q.UpdateDisplayName(ctx, dbgen.UpdateDisplayNameParams{ID: acc.ID, DisplayName: displayName})
		if err != nil {
			return err
		}
		p = toProfile(updated)
		return nil
	})
	return p, err
}
