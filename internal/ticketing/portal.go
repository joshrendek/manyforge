package ticketing

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

var (
	ErrPortalInvalid      = errors.New("invalid portal request")
	ErrPortalUnauthorized = errors.New("invalid portal credentials")
	ErrPortalConflict     = errors.New("portal conflict")
	ErrPortalRate         = errors.New("portal rate limit")
	ErrPortalUnavailable  = errors.New("portal unavailable")
)

// PortalSupportMailer is satisfied by mailing.Service; production must inject that
// service, never a log sender or a general-purpose SMTP sender.
type PortalSupportMailer interface {
	SendSupportReply(context.Context, uuid.UUID, notify.Mail) error
}

type PortalService struct {
	DB            *db.DB
	Resolve       httpx.PermissionResolver
	Mailer        PortalSupportMailer
	ReplyTokenKey []byte
	SystemDomain  string
}

type PortalScope struct {
	KeyID        uuid.UUID
	BusinessID   uuid.UUID
	TenantRootID uuid.UUID
	PrincipalID  uuid.UUID
	ExternalID   string
	DisplayName  string
	token        string
}

type PortalKey struct {
	ID        uuid.UUID  `json:"id"`
	Label     string     `json:"label"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	Token     string     `json:"token,omitempty"`
}

type PortalContact struct {
	Email    string `json:"email"`
	Verified bool   `json:"verified"`
}

func portalUUID(v string) (uuid.UUID, error) {
	id, err := uuid.Parse(v)
	if err != nil || id == uuid.Nil || id.String() != v {
		return uuid.Nil, ErrPortalInvalid
	}
	return id, nil
}

func portalIdentity(external, name string) bool {
	if len(external) != 40 || !strings.HasPrefix(external, "tph:") {
		return false
	}
	if _, err := portalUUID(strings.TrimPrefix(external, "tph:")); err != nil {
		return false
	}
	if len(name) > 400 || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 100 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *PortalService) allowed(ctx context.Context, tx pgx.Tx, pid, bid uuid.UUID, owner bool) error {
	// Recheck the human account, retaining the lock until this operation commits.
	// Memberships survive deactivation/erasure and cannot establish eligibility.
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT true FROM principal p JOIN account a ON a.id=p.account_id JOIN business b ON b.id=$2 WHERE p.id=$1 AND p.kind='human' AND a.status='active' AND a.deleted_at IS NULL AND b.status='active' AND b.deleted_at IS NULL FOR SHARE OF p,a`, pid, bid).Scan(&eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.ErrNotFound
	}
	if err != nil {
		return err
	}
	if s.Resolve == nil {
		return ErrPortalUnavailable
	}
	perms, err := s.Resolve(ctx, tx, pid, bid)
	if err != nil {
		return err
	}
	if !perms.Has("tickets.write") {
		return errs.ErrNotFound
	}
	if owner {
		var ok bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM membership m JOIN role r ON r.id=m.role_id JOIN business_closure bc ON bc.ancestor_id=m.business_id WHERE m.principal_id=$1 AND bc.descendant_id=$2 AND r.key='owner' AND r.is_locked AND r.tenant_root_id IS NULL)`, pid, bid).Scan(&ok)
		if err != nil {
			return err
		}
		if !ok {
			return errs.ErrNotFound
		}
	}
	return nil
}

func (s *PortalService) CreateKey(ctx context.Context, pid, bid uuid.UUID, label string, days int) (PortalKey, error) {
	label = strings.TrimSpace(label)
	if label == "" || utf8.RuneCountInString(label) > 100 || len(label) > 400 {
		return PortalKey{}, ErrPortalInvalid
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return PortalKey{}, ErrPortalInvalid
		}
	}
	if days == 0 {
		days = 90
	}
	if days < 1 || days > 365 {
		return PortalKey{}, ErrPortalInvalid
	}
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return PortalKey{}, ErrPortalUnavailable
	}
	raw := "mfsp_" + base64.RawURLEncoding.EncodeToString(entropy[:])
	hash := sha256.Sum256([]byte(raw))
	out := PortalKey{ID: uuid.New(), Label: label}
	err := s.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		if err := s.allowed(ctx, tx, pid, bid, true); err != nil {
			return err
		}
		var root uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT tenant_root_id FROM business WHERE id=$1 AND status='active' FOR UPDATE`, bid).Scan(&root); err != nil {
			return portalNotFound(err)
		}
		var count, total int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE revoked_at IS NULL AND expires_at>now()),count(*) FROM support_portal_key WHERE business_id=$1`, bid).Scan(&count, &total); err != nil {
			return err
		}
		if count >= 10 || total >= 100 {
			return ErrPortalConflict
		}
		if err := tx.QueryRow(ctx, `INSERT INTO support_portal_key(id,business_id,tenant_root_id,creator_principal_id,label,token_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,now()+make_interval(days=>$7)) RETURNING created_at,expires_at`, out.ID, bid, root, pid, label, hash[:], days).Scan(&out.CreatedAt, &out.ExpiresAt); err != nil {
			return err
		}
		return portalAudit(ctx, tx, pid, bid, root, "support.portal_key.created", out.ID)
	})
	if err == nil {
		out.Token = raw
	}
	return out, err
}

func (s *PortalService) ListKeys(ctx context.Context, pid, bid uuid.UUID) ([]PortalKey, error) {
	out := make([]PortalKey, 0)
	err := s.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		if err := s.allowed(ctx, tx, pid, bid, true); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,label,created_at,expires_at,revoked_at FROM support_portal_key WHERE business_id=$1 ORDER BY created_at DESC LIMIT 100`, bid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k PortalKey
			if err := rows.Scan(&k.ID, &k.Label, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt); err != nil {
				return err
			}
			out = append(out, k)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PortalService) RevokeKey(ctx context.Context, pid, bid, kid uuid.UUID) error {
	return s.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		if err := s.allowed(ctx, tx, pid, bid, true); err != nil {
			return err
		}
		var root uuid.UUID
		if err := tx.QueryRow(ctx, `UPDATE support_portal_key SET revoked_at=now() WHERE id=$1 AND business_id=$2 AND revoked_at IS NULL RETURNING tenant_root_id`, kid, bid).Scan(&root); err != nil {
			return portalNotFound(err)
		}
		return portalAudit(ctx, tx, pid, bid, root, "support.portal_key.revoked", kid)
	})
}

func (s *PortalService) Authenticate(ctx context.Context, raw, external, name string) (PortalScope, error) {
	var out PortalScope
	if len(raw) != 48 || !strings.HasPrefix(raw, "mfsp_") || !portalIdentity(external, name) {
		return out, ErrPortalUnauthorized
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw[5:])
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != raw[5:] {
		return out, ErrPortalUnauthorized
	}
	hash := sha256.Sum256([]byte(raw))
	err = s.DB.Pool().QueryRow(ctx, `SELECT id,business_id,tenant_root_id,creator_principal_id FROM resolve_support_portal_key($1)`, hash[:]).Scan(&out.KeyID, &out.BusinessID, &out.TenantRootID, &out.PrincipalID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PortalScope{}, ErrPortalUnauthorized
		}
		return PortalScope{}, ErrPortalUnavailable
	}
	out.ExternalID = external
	out.DisplayName = name
	out.token = raw
	err = s.withScope(ctx, out, func(pgx.Tx) error { return nil })
	return out, err
}

func (s *PortalService) withScope(ctx context.Context, p PortalScope, fn func(pgx.Tx) error) error {
	if p.token == "" || !portalIdentity(p.ExternalID, p.DisplayName) {
		return ErrPortalUnauthorized
	}
	return s.DB.WithPrincipal(ctx, p.PrincipalID, func(tx pgx.Tx) error {
		if err := s.allowed(ctx, tx, p.PrincipalID, p.BusinessID, false); err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				return ErrPortalUnauthorized
			}
			return err
		}
		var ok bool
		hash := sha256.Sum256([]byte(p.token))
		// Lock the credential through commit: revocation cannot race an accepted write.
		err := tx.QueryRow(ctx, `SELECT true FROM support_portal_key WHERE id=$1 AND business_id=$2 AND tenant_root_id=$3 AND creator_principal_id=$4 AND token_hash=$5 AND revoked_at IS NULL AND expires_at>now() FOR SHARE`, p.KeyID, p.BusinessID, p.TenantRootID, p.PrincipalID, hash[:]).Scan(&ok)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPortalUnauthorized
		}
		if err != nil {
			return err
		}
		return fn(tx)
	})
}

func portalNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.ErrNotFound
	}
	return err
}
func portalEmail(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	a, err := mail.ParseAddress(v)
	if err != nil || a.Address != v || !validSendAs(v) || len(v) > 254 {
		return "", ErrPortalInvalid
	}
	return v, nil
}
func portalChallengeHash(token, external, email string, nonce uuid.UUID, code string) []byte {
	h := hmac.New(sha256.New, []byte(token))
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", external, email, nonce.String(), code)
	return h.Sum(nil)
}
func portalAudit(ctx context.Context, tx pgx.Tx, pid, bid, root uuid.UUID, action string, target uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_entry(id,business_id,tenant_root_id,actor_principal_id,action,target_type,target_id,inputs) VALUES(gen_random_uuid(),$1,$2,$3,$4,'support_portal',$5,'{}')`, bid, root, pid, action, target)
	return err
}
