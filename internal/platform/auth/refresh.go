package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

var (
	// ErrRefreshInvalid is returned for an unknown, expired, or revoked token.
	ErrRefreshInvalid = errors.New("invalid refresh token")
	// ErrRefreshReuse is returned when an already-used token is presented again;
	// the whole family is revoked as a precaution (Constitution Principle II).
	ErrRefreshReuse = errors.New("refresh token reuse detected")
)

// NewOpaqueToken returns a 256-bit URL-safe random secret.
func NewOpaqueToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the SHA-256 hex digest used to store opaque tokens at rest.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// IssueRefresh creates a new refresh-token family root for principal and returns
// the raw token (the only time it exists in plaintext).
func IssueRefresh(ctx context.Context, tx pgx.Tx, principalID uuid.UUID, ttl time.Duration, now time.Time) (string, error) {
	raw, err := NewOpaqueToken()
	if err != nil {
		return "", err
	}
	_, err = dbgen.New(tx).CreateRefreshToken(ctx, dbgen.CreateRefreshTokenParams{
		ID:          uuid.New(),
		PrincipalID: principalID,
		TokenHash:   HashToken(raw),
		FamilyID:    uuid.New(),
		ParentID:    db.PGUUIDPtr(nil),
		ExpiresAt:   now.Add(ttl),
	})
	return raw, err
}

// RotateRefresh validates raw and issues a child token. Presenting an
// already-used token is reuse: the entire family is revoked and reuse=true is
// returned WITHOUT an error, so the caller's transaction COMMITS the revoke
// (returning an error here would roll it back). The caller surfaces
// ErrRefreshReuse after commit.
func RotateRefresh(ctx context.Context, tx pgx.Tx, raw string, ttl time.Duration, now time.Time) (newRaw string, principalID uuid.UUID, reuse bool, err error) {
	q := dbgen.New(tx)
	rt, err := q.GetRefreshTokenByHashForUpdate(ctx, HashToken(raw))
	if err != nil {
		return "", uuid.Nil, false, ErrRefreshInvalid
	}
	if rt.RevokedAt.Valid || now.After(rt.ExpiresAt) {
		return "", uuid.Nil, false, ErrRefreshInvalid
	}
	if rt.UsedAt.Valid {
		if err := q.RevokeRefreshFamily(ctx, rt.FamilyID); err != nil {
			return "", uuid.Nil, false, err
		}
		return "", uuid.Nil, true, nil
	}
	if err := q.MarkRefreshTokenUsed(ctx, rt.ID); err != nil {
		return "", uuid.Nil, false, err
	}
	newRaw, err = NewOpaqueToken()
	if err != nil {
		return "", uuid.Nil, false, err
	}
	if _, err := q.CreateRefreshToken(ctx, dbgen.CreateRefreshTokenParams{
		ID:          uuid.New(),
		PrincipalID: rt.PrincipalID,
		TokenHash:   HashToken(newRaw),
		FamilyID:    rt.FamilyID,
		ParentID:    db.PGUUID(rt.ID),
		ExpiresAt:   now.Add(ttl),
	}); err != nil {
		return "", uuid.Nil, false, err
	}
	return newRaw, rt.PrincipalID, false, nil
}

// RevokeRefreshByToken revokes the presented token's whole family (logout). It
// is idempotent: an unknown token is a no-op.
func RevokeRefreshByToken(ctx context.Context, tx pgx.Tx, raw string) error {
	q := dbgen.New(tx)
	rt, err := q.GetRefreshTokenByHashForUpdate(ctx, HashToken(raw))
	if err != nil {
		return nil
	}
	return q.RevokeRefreshFamily(ctx, rt.FamilyID)
}
