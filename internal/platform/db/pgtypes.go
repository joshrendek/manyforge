package db

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// PGUUID wraps a uuid.UUID as a non-null pgtype.UUID (sqlc represents nullable
// uuid columns as pgtype.UUID in pgx mode).
func PGUUID(u uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte(u), Valid: true}
}

// PGUUIDPtr converts an optional uuid.UUID to pgtype.UUID (nil -> NULL).
func PGUUIDPtr(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return PGUUID(*u)
}
