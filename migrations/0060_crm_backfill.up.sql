-- migrations/0060_crm_backfill.up.sql
-- Spec 005 Phase A: one-time idempotent backfill of CRM contacts + companies from
-- existing requesters. The free-email denylist below MUST stay in sync with
-- internal/crm/freemail.go (freeEmailDomains). This is a ONE-TIME backfill of
-- pre-existing data; the Go list is the live authority for all new inbound mail
-- (via crm_link_inbound_sender, migration 0059). See the cross-ref note in freemail.go.

-- 1. contacts from requesters missing a contact link (dedup by tenant_root_id+email)
INSERT INTO contact (id, tenant_root_id, primary_email, display_name, created_at, updated_at)
    SELECT gen_random_uuid(), r.tenant_root_id, r.email, NULL, now(), now()
    FROM (SELECT DISTINCT tenant_root_id, email FROM requester WHERE contact_id IS NULL) r
    ON CONFLICT (tenant_root_id, primary_email) WHERE deleted_at IS NULL DO NOTHING;

-- 2. link requesters to their contact
UPDATE requester r
    SET contact_id = c.id, updated_at = now()
    FROM contact c
    WHERE r.contact_id IS NULL
      AND c.tenant_root_id = r.tenant_root_id
      AND c.primary_email = r.email
      AND c.deleted_at IS NULL;

-- 3. companies by domain (skip free-email), dedup by tenant_root_id+domain.
-- Dedup the (tenant_root_id, domain) set in a subquery FIRST, THEN generate one
-- uuid per distinct domain — putting gen_random_uuid() inside the DISTINCT
-- select-list would make every row unique and defeat the dedup.
INSERT INTO company (id, tenant_root_id, name, domain, created_at, updated_at)
    SELECT gen_random_uuid(), d.tenant_root_id, d.domain, d.domain::citext, now(), now()
    FROM (
        SELECT DISTINCT c.tenant_root_id, lower(split_part(c.primary_email::text, '@', 2)) AS domain
        FROM contact c
        WHERE c.company_id IS NULL
          AND split_part(c.primary_email::text, '@', 2) <> ''
          AND lower(split_part(c.primary_email::text, '@', 2)) NOT IN (
              'gmail.com','googlemail.com','outlook.com','hotmail.com','live.com','msn.com',
              'yahoo.com','ymail.com','icloud.com','me.com','mac.com','aol.com','proton.me',
              'protonmail.com','gmx.com','gmx.net','mail.com','zoho.com','yandex.com')
    ) d
    ON CONFLICT (tenant_root_id, domain) WHERE domain IS NOT NULL DO NOTHING;

-- 4. link contacts to companies
UPDATE contact c
    SET company_id = co.id, updated_at = now()
    FROM company co
    WHERE c.company_id IS NULL
      AND co.tenant_root_id = c.tenant_root_id
      AND co.domain = split_part(c.primary_email::text, '@', 2)::citext;
