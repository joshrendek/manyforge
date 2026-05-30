// Package invitations owns the invitation lifecycle (create/list/revoke/resend)
// and the auth-bound, single-use acceptance flow.
package invitations

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/mailer"
)

const defaultTTL = 7 * 24 * time.Hour

// Service implements the invitation use cases.
type Service struct {
	DB     *db.DB
	Mailer mailer.Mailer
	TTL    time.Duration // invitation validity; defaults to 7 days
}

func (s *Service) ttl() time.Duration {
	if s.TTL <= 0 {
		return defaultTTL
	}
	return s.TTL
}

// Invitation is the API-facing view of a pending/expired/etc. invitation.
type Invitation struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
	Role      RoleRef   `json:"role"`
}

// RoleRef is a compact role reference embedded in an invitation.
type RoleRef struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

// Create issues an invitation. Requires members.manage at the business, the role
// to be assignable in the tenant, and — per FR-023 — the role to grant no
// permission the inviter does not itself hold (no escalation). The token is
// emailed; the response is uniform (the caller never learns whether the address
// is already a member), so only authority/role errors surface.
func (s *Service) Create(ctx context.Context, inviterID, businessID, roleID uuid.UUID, email string) error {
	email = strings.TrimSpace(email)
	if email == "" || !strings.Contains(email, "@") {
		return fmt.Errorf("a valid email is required: %w", errs.ErrValidation)
	}
	raw := randomToken()
	err := s.DB.WithPrincipal(ctx, inviterID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		biz, err := q.GetBusiness(ctx, businessID)
		if err != nil {
			return errs.ErrNotFound
		}
		perms, err := authz.Resolve(ctx, tx, inviterID, businessID)
		if err != nil {
			return err
		}
		if !perms.Has("members.manage") {
			return errs.ErrNotFound
		}
		if _, err := q.RoleVisibleInTenant(ctx, dbgen.RoleVisibleInTenantParams{ID: roleID, TenantRootID: db.PGUUID(biz.TenantRootID)}); err != nil {
			return fmt.Errorf("unknown role: %w", errs.ErrConflict)
		}
		rolePerms, err := q.GetRolePermissions(ctx, roleID)
		if err != nil {
			return err
		}
		for _, p := range rolePerms {
			if !perms.Has(p) {
				return fmt.Errorf("cannot invite with a role above your own: %w", errs.ErrConflict)
			}
		}
		return q.CreateInvitation(ctx, dbgen.CreateInvitationParams{
			ID: uuid.New(), BusinessID: businessID, TenantRootID: biz.TenantRootID,
			Email: email, RoleID: roleID, TokenHash: auth.HashToken(raw),
			CreatedBy: db.PGUUID(inviterID), ExpiresAt: time.Now().Add(s.ttl()),
		})
	})
	if err != nil {
		return err
	}
	s.email(ctx, email, raw)
	return nil
}

// List returns the business's invitations (requires members.manage).
func (s *Service) List(ctx context.Context, principalID, businessID uuid.UUID) ([]Invitation, error) {
	var out []Invitation
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.GetBusiness(ctx, businessID); err != nil {
			return errs.ErrNotFound
		}
		perms, err := authz.Resolve(ctx, tx, principalID, businessID)
		if err != nil {
			return err
		}
		if !perms.Has("members.manage") {
			return errs.ErrNotFound
		}
		rows, err := q.ListInvitations(ctx, businessID)
		if err != nil {
			return err
		}
		out = make([]Invitation, 0, len(rows))
		for _, r := range rows {
			out = append(out, Invitation{
				ID: r.ID.String(), Email: r.Email, Status: r.Status, ExpiresAt: r.ExpiresAt,
				Role: RoleRef{ID: r.RoleID.String(), Key: r.RoleKey, Name: r.RoleName},
			})
		}
		return nil
	})
	return out, err
}

// Revoke cancels a pending invitation (requires members.manage). Unknown or
// non-pending invitations are not-found.
func (s *Service) Revoke(ctx context.Context, principalID, businessID, invID uuid.UUID) error {
	return s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := requireManage(ctx, q, tx, principalID, businessID); err != nil {
			return err
		}
		if _, err := q.RevokeInvitation(ctx, dbgen.RevokeInvitationParams{ID: invID, BusinessID: businessID}); err != nil {
			return errs.ErrNotFound
		}
		return nil
	})
}

// Resend rotates a pending invitation's token, extends its expiry, and re-emails
// it (requires members.manage).
func (s *Service) Resend(ctx context.Context, principalID, businessID, invID uuid.UUID) error {
	raw := randomToken()
	var to string
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := requireManage(ctx, q, tx, principalID, businessID); err != nil {
			return err
		}
		inv, err := q.GetPendingInvitation(ctx, dbgen.GetPendingInvitationParams{ID: invID, BusinessID: businessID})
		if err != nil {
			return errs.ErrNotFound
		}
		if _, err := q.RotateInvitationToken(ctx, dbgen.RotateInvitationTokenParams{
			ID: invID, BusinessID: businessID, TokenHash: auth.HashToken(raw), ExpiresAt: time.Now().Add(s.ttl()),
		}); err != nil {
			return errs.ErrNotFound
		}
		to = inv.Email
		return nil
	})
	if err != nil {
		return err
	}
	s.email(ctx, to, raw)
	return nil
}

// AcceptResult carries the outcome the handler maps to an HTTP status.
type AcceptResult struct {
	Status     string // ok | gone | email_mismatch | unverified
	BusinessID string
	RoleID     string
}

// Accept consumes an invitation token for the authenticated principal. It runs
// outside RLS (the invitee is not yet a member) via the accept_invitation
// SECURITY DEFINER function, after confirming the caller's account is verified
// and supplying its email for the match. Single-use: a second accept is 'gone'.
func (s *Service) Accept(ctx context.Context, principalID uuid.UUID, token string) (AcceptResult, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return AcceptResult{}, fmt.Errorf("token is required: %w", errs.ErrValidation)
	}
	var res AcceptResult
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		acct, err := dbgen.New(tx).GetAccountByPrincipal(ctx, principalID)
		if err != nil {
			return errs.ErrNotFound
		}
		if !acct.EmailVerifiedAt.Valid {
			res = AcceptResult{Status: "unverified"}
			return nil
		}
		var status string
		var bid, rid pgtype.UUID
		row := tx.QueryRow(ctx, "SELECT out_status, out_business, out_role FROM accept_invitation($1, $2, $3::citext)",
			auth.HashToken(token), principalID, acct.Email)
		if err := row.Scan(&status, &bid, &rid); err != nil {
			return err
		}
		res = AcceptResult{Status: status, BusinessID: uuidString(bid), RoleID: uuidString(rid)}
		return nil
	})
	if err != nil {
		return AcceptResult{}, err
	}
	return res, nil
}

func requireManage(ctx context.Context, q *dbgen.Queries, tx pgx.Tx, principalID, businessID uuid.UUID) error {
	if _, err := q.GetBusiness(ctx, businessID); err != nil {
		return errs.ErrNotFound
	}
	perms, err := authz.Resolve(ctx, tx, principalID, businessID)
	if err != nil {
		return err
	}
	if !perms.Has("members.manage") {
		return errs.ErrNotFound
	}
	return nil
}

func (s *Service) email(ctx context.Context, to, rawToken string) {
	if s.Mailer == nil {
		return
	}
	// Dev convention: body carries "token: <raw>" so local flows are completable.
	_ = s.Mailer.Send(ctx, mailer.Message{
		To:      to,
		Subject: "You've been invited to a ManyForge business",
		Body:    "token: " + rawToken,
	})
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}
