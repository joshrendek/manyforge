-- 0103 down: remove the feedback permission catalog. role_permission rows referencing these
-- keys are removed first (no ON DELETE CASCADE assumed).
DELETE FROM role_permission WHERE permission_key IN ('feedback.read', 'feedback.write');
DELETE FROM permission WHERE key IN ('feedback.read', 'feedback.write');
