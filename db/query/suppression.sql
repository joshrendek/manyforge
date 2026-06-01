-- Email suppression queries (spec 001 email_suppression table, US2 outbound safety).
-- Table: email_suppression (email citext PK, reason text, created_at timestamptz).

-- name: IsSuppressed :one
SELECT EXISTS (SELECT 1 FROM email_suppression WHERE email = $1);

-- name: InsertSuppression :exec
INSERT INTO email_suppression (email, reason) VALUES ($1, $2)
ON CONFLICT (email) DO NOTHING;
