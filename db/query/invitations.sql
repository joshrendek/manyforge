-- Invitation lifecycle queries. Create/list/revoke/resend run under the inviter's
-- principal (a member with members.manage), so RLS scopes them to the business.
-- Acceptance is handled by the accept_invitation() SECURITY DEFINER function
-- (invitees are not yet members), invoked via raw SQL in the service.

-- name: RoleVisibleInTenant :one
-- A role assignable within the tenant: a preset (NULL tenant) or the tenant's own.
SELECT id FROM role WHERE id = $1 AND (tenant_root_id IS NULL OR tenant_root_id = $2);

-- name: CreateInvitation :exec
INSERT INTO invitation (id, business_id, tenant_root_id, email, role_id, token_hash, created_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListInvitations :many
SELECT i.id, i.email, i.status, i.expires_at, i.created_at,
       r.id AS role_id, r.key AS role_key, r.name AS role_name
FROM invitation i
JOIN role r ON r.id = i.role_id
WHERE i.business_id = $1
ORDER BY i.created_at DESC, i.id;

-- name: RevokeInvitation :one
UPDATE invitation SET status = 'revoked'
WHERE id = $1 AND business_id = $2 AND status = 'pending'
RETURNING id;

-- name: GetPendingInvitation :one
SELECT id, email, role_id FROM invitation
WHERE id = $1 AND business_id = $2 AND status = 'pending';

-- name: RotateInvitationToken :one
UPDATE invitation SET token_hash = $3, expires_at = $4
WHERE id = $1 AND business_id = $2 AND status = 'pending'
RETURNING id;
