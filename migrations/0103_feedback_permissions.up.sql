-- 0103: Feedback permission catalog (Spec 006 — feedback boards). feedback.write gates all
-- board/post/key mutations (create board, moderate status, create/revoke ingest keys,
-- convert a post to a ticket); feedback.read gates viewing them. Mirrors the CRM catalog
-- (0058): the mutator is owner + admin, the reader is member + viewer (plus the mutators).
-- Key/module are authoritative and shared verbatim with the OpenAPI contract — do not rename.
-- The PUBLIC SDK/portal ingress path carries no principal and no permission (it authenticates
-- by a publishable board key), so it is deliberately absent from this catalog.

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('feedback.read',  'feedback', 'View feedback boards, posts, and ingest keys'),
    ('feedback.write', 'feedback', 'Manage boards, moderate posts, manage ingest keys, convert to tickets');

-- owner + admin ⇒ feedback.write (and, transitively, the full feedback surface).
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN ('feedback.read', 'feedback.write')
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin');

-- member + viewer ⇒ feedback.read (read-only access).
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'feedback.read'
    WHERE r.tenant_root_id IS NULL AND r.key IN ('member', 'viewer');
