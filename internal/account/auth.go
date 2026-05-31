package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/mailer"
)

// issueOneTimeToken mints a single-use opaque token for purpose, mails it, and
// returns the raw value (for handler/test use). new_email is stored only for the
// email-change purpose.
func (s *Service) issueOneTimeToken(ctx context.Context, q *dbgen.Queries, accountID uuid.UUID, email, purpose string, newEmail *string, subject string) (string, error) {
	raw, err := auth.NewOpaqueToken()
	if err != nil {
		return "", err
	}
	if _, err := q.CreateOneTimeToken(ctx, dbgen.CreateOneTimeTokenParams{
		ID: uuid.New(), AccountID: db.PGUUID(accountID), Email: email,
		Purpose: purpose, TokenHash: auth.HashToken(raw), NewEmail: newEmail,
		ExpiresAt: s.now().Add(s.TokenTTL),
	}); err != nil {
		return "", err
	}
	if s.Mailer != nil {
		to := email
		if newEmail != nil {
			to = *newEmail
		}
		_ = s.Mailer.Send(ctx, mailer.Message{To: to, Subject: subject, Body: "token: " + raw})
	}
	return raw, nil
}

// RequestPasswordReset issues a single-use reset token if the email maps to an
// account. The outcome is uniform regardless of existence (FR-026): the caller
// cannot distinguish a hit from a miss. The raw token is returned for handler/test
// use only — the handler always responds the same way and never echoes it.
func (s *Service) RequestPasswordReset(ctx context.Context, email string) (string, error) {
	var raw string
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		acc, err := q.GetAccountByEmail(ctx, email)
		if err != nil {
			return nil // no existence oracle: silent success
		}
		raw, err = s.issueOneTimeToken(ctx, q, acc.ID, email, "password_reset", nil, "Reset your password")
		return err
	})
	return raw, err
}

// ConfirmPasswordReset consumes a reset token and sets a new password, then
// revokes every existing session for the account's principal.
func (s *Service) ConfirmPasswordReset(ctx context.Context, rawToken, newPassword string) error {
	if len(newPassword) < 12 {
		return fmt.Errorf("a 12+ char password is required: %w", errs.ErrValidation)
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tok, err := q.ConsumeOneTimeToken(ctx, dbgen.ConsumeOneTimeTokenParams{
			TokenHash: auth.HashToken(rawToken), Purpose: "password_reset",
		})
		if err != nil || !tok.AccountID.Valid {
			return fmt.Errorf("invalid or expired token: %w", errs.ErrValidation)
		}
		accountID := uuid.UUID(tok.AccountID.Bytes)
		if err := q.UpdatePasswordHash(ctx, dbgen.UpdatePasswordHashParams{ID: accountID, PasswordHash: &hash}); err != nil {
			return err
		}
		prin, err := q.GetPrincipalByAccount(ctx, db.PGUUID(accountID))
		if err != nil {
			return err
		}
		return q.RevokeAllRefreshForPrincipal(ctx, prin.ID)
	})
}

// RequestEmailChange issues a single-use token to move the login identity to
// newEmail. The caller is authenticated, so this is not enumeration-sensitive;
// verification is sent to the NEW address and the change applies only on confirm.
func (s *Service) RequestEmailChange(ctx context.Context, principalID uuid.UUID, newEmail string) (string, error) {
	newEmail = strings.TrimSpace(newEmail)
	if newEmail == "" || !strings.Contains(newEmail, "@") {
		return "", fmt.Errorf("a valid new email is required: %w", errs.ErrValidation)
	}
	var raw string
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		acc, err := q.GetAccountByPrincipal(ctx, principalID)
		if err != nil {
			return errs.ErrNotFound
		}
		raw, err = s.issueOneTimeToken(ctx, q, acc.ID, newEmail, "email_change", &newEmail, "Confirm your new email")
		return err
	})
	return raw, err
}

// ConfirmEmailChange consumes an email-change token and applies the new address.
// A collision with an existing account surfaces as a validation error.
func (s *Service) ConfirmEmailChange(ctx context.Context, rawToken string) error {
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tok, err := q.ConsumeOneTimeToken(ctx, dbgen.ConsumeOneTimeTokenParams{
			TokenHash: auth.HashToken(rawToken), Purpose: "email_change",
		})
		if err != nil || !tok.AccountID.Valid || tok.NewEmail == nil {
			return fmt.Errorf("invalid or expired token: %w", errs.ErrValidation)
		}
		err = q.UpdateEmail(ctx, dbgen.UpdateEmailParams{ID: uuid.UUID(tok.AccountID.Bytes), Email: *tok.NewEmail})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("that email is not available: %w", errs.ErrValidation)
		}
		return err
	})
}

// RequestMagicLink issues a single-use login token if the email maps to an
// account. Uniform regardless of existence (FR-026).
func (s *Service) RequestMagicLink(ctx context.Context, email string) (string, error) {
	var raw string
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		acc, err := q.GetAccountByEmail(ctx, email)
		if err != nil {
			return nil // no existence oracle: silent success
		}
		raw, err = s.issueOneTimeToken(ctx, q, acc.ID, email, "magic_link", nil, "Your sign-in link")
		return err
	})
	return raw, err
}

// ConsumeMagicLink consumes a magic-link token and issues a session token pair
// (works for password-less accounts). A non-active or deleted account is denied.
func (s *Service) ConsumeMagicLink(ctx context.Context, rawToken string) (TokenPair, error) {
	var tp TokenPair
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tok, err := q.ConsumeOneTimeToken(ctx, dbgen.ConsumeOneTimeTokenParams{
			TokenHash: auth.HashToken(rawToken), Purpose: "magic_link",
		})
		if err != nil || !tok.AccountID.Valid {
			return fmt.Errorf("invalid or expired token: %w", errs.ErrValidation)
		}
		accountID := uuid.UUID(tok.AccountID.Bytes)
		acc, err := q.GetAccountByID(ctx, accountID)
		if err != nil || acc.Status != "active" {
			return fmt.Errorf("invalid or expired token: %w", errs.ErrValidation)
		}
		prin, err := q.GetPrincipalByAccount(ctx, db.PGUUID(accountID))
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
