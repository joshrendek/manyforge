-- RLS smoke test — run against a freshly migrated database, connected as a
-- SUPERUSER (so it can SET ROLE manyforge_app). The authoritative, asserting
-- version is the Go RLS matrix in internal/security_regression (task T064);
-- this script is the fast manual/CI check. Expected values are noted inline.
--
--   make db-smoke   (MANYFORGE_DATABASE_URL must point at the migrated DB)
--
-- Re-runnable: it resets tenant data first (presets + permission catalog kept).

-- Surgical reset (preserves built-in presets + permission catalog; child rows first).
-- NB: do NOT TRUNCATE business CASCADE — it would also truncate the role table.
DELETE FROM membership;
DELETE FROM invitation;
DELETE FROM business_closure;
DELETE FROM role WHERE tenant_root_id IS NOT NULL;  -- custom roles only
DELETE FROM refresh_token;
DELETE FROM audit_entry;
DELETE FROM business;
DELETE FROM principal;
DELETE FROM account;

BEGIN;
INSERT INTO account (id,email,display_name,email_verified_at) VALUES
  ('00000000-0000-0000-0000-0000000000a1','a1@x.test','A1',now()),
  ('00000000-0000-0000-0000-0000000000a2','a2@x.test','A2',now());
INSERT INTO principal (id,kind,account_id) VALUES
  ('00000000-0000-0000-0000-0000000000f1','human','00000000-0000-0000-0000-0000000000a1'),
  ('00000000-0000-0000-0000-0000000000f2','human','00000000-0000-0000-0000-0000000000a2');
-- tenant 1: master + sub
INSERT INTO business (id,parent_id,tenant_root_id,name) VALUES
  ('00000000-0000-0000-0000-0000000000b1',NULL,'00000000-0000-0000-0000-0000000000b1','Acme'),
  ('00000000-0000-0000-0000-00000000b1f5','00000000-0000-0000-0000-0000000000b1','00000000-0000-0000-0000-0000000000b1','Acme-Sub');
INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES
  ('00000000-0000-0000-0000-0000000000b1','00000000-0000-0000-0000-0000000000b1',0,'00000000-0000-0000-0000-0000000000b1'),
  ('00000000-0000-0000-0000-00000000b1f5','00000000-0000-0000-0000-00000000b1f5',0,'00000000-0000-0000-0000-0000000000b1'),
  ('00000000-0000-0000-0000-0000000000b1','00000000-0000-0000-0000-00000000b1f5',1,'00000000-0000-0000-0000-0000000000b1');
INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id) VALUES
  ('00000000-0000-0000-0000-0000000000f1','00000000-0000-0000-0000-0000000000b1','00000000-0000-0000-0000-0000000000b1',(SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'));
INSERT INTO role (tenant_root_id,key,name) VALUES ('00000000-0000-0000-0000-0000000000b1','ops','Ops');
-- tenant 2: master
INSERT INTO business (id,parent_id,tenant_root_id,name) VALUES
  ('00000000-0000-0000-0000-0000000000b2',NULL,'00000000-0000-0000-0000-0000000000b2','Globex');
INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES
  ('00000000-0000-0000-0000-0000000000b2','00000000-0000-0000-0000-0000000000b2',0,'00000000-0000-0000-0000-0000000000b2');
INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id) VALUES
  ('00000000-0000-0000-0000-0000000000f2','00000000-0000-0000-0000-0000000000b2','00000000-0000-0000-0000-0000000000b2',(SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'));
COMMIT;

-- fail closed (no principal context) → expect 0 / 0
BEGIN; SET ROLE manyforge_app; SET LOCAL manyforge.principal_id = '';
SELECT 'failclosed_business='||count(*) FROM business;
SELECT 'failclosed_membership='||count(*) FROM membership;
COMMIT; RESET ROLE;

-- p1 → expect business=2, sees_b2=0, roles_visible=5 (4 presets + 1 custom)
BEGIN; SET ROLE manyforge_app; SET LOCAL manyforge.principal_id = '00000000-0000-0000-0000-0000000000f1';
SELECT 'p1_business='||count(*) FROM business;
SELECT 'p1_sees_b2='||count(*) FROM business WHERE id='00000000-0000-0000-0000-0000000000b2';
SELECT 'p1_roles_visible='||count(*) FROM role;
COMMIT; RESET ROLE;

-- p2 → expect business=1, sees_b1=0, roles_visible=4 (presets only; tenant 1's custom role hidden)
BEGIN; SET ROLE manyforge_app; SET LOCAL manyforge.principal_id = '00000000-0000-0000-0000-0000000000f2';
SELECT 'p2_business='||count(*) FROM business;
SELECT 'p2_sees_b1='||count(*) FROM business WHERE id='00000000-0000-0000-0000-0000000000b1';
SELECT 'p2_roles_visible='||count(*) FROM role;
COMMIT; RESET ROLE;
