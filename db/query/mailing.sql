-- Mailing-list core queries (Spec 013, migration 0124). All tables are business-scoped.
-- Every query runs inside db.WithPrincipal (RLS) and every ID-taking query also carries
-- tenant_root_id. Services additionally assert business/list containment so a sibling-business,
-- foreign-tenant, archived, or unknown identifier collapses to ErrNotFound.

-- ---- lists ----

-- name: InsertMailingList :one
INSERT INTO mailing_list (
    id, business_id, tenant_root_id, slug, name, description, double_opt_in,
    status, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', now(), now())
RETURNING *;

-- name: GetMailingList :one
SELECT * FROM mailing_list
WHERE id = $1 AND tenant_root_id = $2;

-- name: ListMailingLists :many
SELECT * FROM mailing_list
WHERE business_id = $1 AND tenant_root_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListMailingListsAfter :many
SELECT * FROM mailing_list
WHERE business_id = sqlc.arg('business_id')
  AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND (created_at, id) < (sqlc.arg('cur_created')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: UpdateMailingList :one
UPDATE mailing_list SET
    name = COALESCE(NULLIF(sqlc.narg('name')::text, ''), name),
    description = CASE
        WHEN sqlc.narg('set_description')::boolean THEN sqlc.narg('description')::text
        ELSE description
    END,
    double_opt_in = COALESCE(sqlc.narg('double_opt_in')::boolean, double_opt_in),
    updated_at = now()
WHERE id = sqlc.arg('id') AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND status = 'active'
RETURNING *;

-- name: ArchiveMailingList :one
UPDATE mailing_list SET status = 'archived', updated_at = now()
WHERE id = $1 AND tenant_root_id = $2 AND status = 'active'
RETURNING *;

-- ---- subscribers ----

-- name: InsertListSubscriber :one
INSERT INTO list_subscriber (
    id, business_id, tenant_root_id, list_id, email, first_name, last_name,
    attributes, status, contact_id, consent_source, consent_attested_by,
    consent_at, confirmed_at, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
    now(), CASE WHEN $9::mailing_subscriber_status = 'active' THEN now() END,
    now(), now()
)
RETURNING *;

-- InsertSubscribersBatch is the atomic import primitive. Go validates equal-length arrays
-- before calling because multi-array unnest pads short arrays with NULL. Duplicate emails on
-- the list are skipped and the returned rows are the imported set.
-- name: InsertSubscribersBatch :many
INSERT INTO list_subscriber (
    id, business_id, tenant_root_id, list_id, email, first_name, last_name,
    attributes, status, contact_id, consent_source, consent_attested_by,
    consent_at, confirmed_at, created_at, updated_at
)
SELECT
    x.id, sqlc.arg('business_id')::uuid, sqlc.arg('tenant_root_id')::uuid,
    sqlc.arg('list_id')::uuid, x.email::citext, NULLIF(x.first_name, ''), NULLIF(x.last_name, ''),
    x.attributes, sqlc.arg('status')::mailing_subscriber_status,
    NULLIF(x.contact_id, '00000000-0000-0000-0000-000000000000'::uuid),
    sqlc.arg('consent_source')::mailing_consent_source,
    sqlc.arg('consent_attested_by')::uuid, now(),
    CASE WHEN sqlc.arg('status')::mailing_subscriber_status = 'active' THEN now() END,
    now(), now()
FROM (
    SELECT
        unnest(sqlc.arg('ids')::uuid[]) AS id,
        unnest(sqlc.arg('emails')::text[]) AS email,
        unnest(sqlc.arg('first_names')::text[]) AS first_name,
        unnest(sqlc.arg('last_names')::text[]) AS last_name,
        unnest(sqlc.arg('attributes')::jsonb[]) AS attributes,
        unnest(sqlc.arg('contact_ids')::uuid[]) AS contact_id
) AS x
ON CONFLICT (list_id, email) DO NOTHING
RETURNING *;

-- name: GetListSubscriber :one
SELECT * FROM list_subscriber
WHERE id = $1 AND tenant_root_id = $2;

-- name: ListListSubscribers :many
SELECT s.* FROM list_subscriber s
WHERE s.list_id = sqlc.arg('list_id')
  AND s.tenant_root_id = sqlc.arg('tenant_root_id')
  AND (sqlc.narg('q')::text IS NULL OR s.email ILIKE '%' || sqlc.narg('q')::text || '%')
  AND (sqlc.narg('status')::text IS NULL OR s.status::text = sqlc.narg('status')::text)
  AND (sqlc.narg('tag')::text IS NULL OR EXISTS (
      SELECT 1 FROM subscriber_tag st
      WHERE st.subscriber_id = s.id AND st.tenant_root_id = s.tenant_root_id
        AND st.tag = sqlc.narg('tag')::citext
  ))
ORDER BY s.email ASC, s.id ASC
LIMIT sqlc.arg('lim');

-- name: ListListSubscribersAfter :many
SELECT s.* FROM list_subscriber s
WHERE s.list_id = sqlc.arg('list_id')
  AND s.tenant_root_id = sqlc.arg('tenant_root_id')
  AND (s.email, s.id) > (sqlc.arg('cur_email')::citext, sqlc.arg('cur_id')::uuid)
  AND (sqlc.narg('q')::text IS NULL OR s.email ILIKE '%' || sqlc.narg('q')::text || '%')
  AND (sqlc.narg('status')::text IS NULL OR s.status::text = sqlc.narg('status')::text)
  AND (sqlc.narg('tag')::text IS NULL OR EXISTS (
      SELECT 1 FROM subscriber_tag st
      WHERE st.subscriber_id = s.id AND st.tenant_root_id = s.tenant_root_id
        AND st.tag = sqlc.narg('tag')::citext
  ))
ORDER BY s.email ASC, s.id ASC
LIMIT sqlc.arg('lim');

-- name: ListSubscribersForExport :many
SELECT s.* FROM list_subscriber s
WHERE s.list_id = $1 AND s.tenant_root_id = $2
ORDER BY s.email ASC, s.id ASC
LIMIT $3;

-- name: ListSubscribersForExportAfter :many
SELECT s.* FROM list_subscriber s
WHERE s.list_id = sqlc.arg('list_id')
  AND s.tenant_root_id = sqlc.arg('tenant_root_id')
  AND (s.email, s.id) > (sqlc.arg('cur_email')::citext, sqlc.arg('cur_id')::uuid)
ORDER BY s.email ASC, s.id ASC
LIMIT sqlc.arg('lim');

-- name: UpdateListSubscriber :one
UPDATE list_subscriber SET
    first_name = CASE
        WHEN sqlc.narg('set_first_name')::boolean THEN sqlc.narg('first_name')::text
        ELSE first_name
    END,
    last_name = CASE
        WHEN sqlc.narg('set_last_name')::boolean THEN sqlc.narg('last_name')::text
        ELSE last_name
    END,
    attributes = COALESCE(sqlc.narg('attributes')::jsonb, attributes),
    status = COALESCE(sqlc.narg('status')::mailing_subscriber_status, status),
    confirmed_at = CASE
        WHEN sqlc.narg('status')::text = 'active' AND confirmed_at IS NULL THEN now()
        ELSE confirmed_at
    END,
    unsubscribed_at = CASE
        WHEN sqlc.narg('status')::text = 'unsubscribed' THEN now()
        WHEN sqlc.narg('status')::text = 'active' THEN NULL
        ELSE unsubscribed_at
    END,
    status_reason = CASE
        WHEN sqlc.narg('set_status_reason')::boolean THEN sqlc.narg('status_reason')::text
        ELSE status_reason
    END,
    updated_at = now()
WHERE id = sqlc.arg('id') AND tenant_root_id = sqlc.arg('tenant_root_id')
RETURNING *;

-- name: UnsubscribeListSubscriber :one
UPDATE list_subscriber SET
    status = 'unsubscribed', unsubscribed_at = COALESCE(unsubscribed_at, now()),
    status_reason = COALESCE(NULLIF(sqlc.arg('status_reason')::text, ''), status_reason), updated_at = now()
WHERE id = sqlc.arg('id') AND tenant_root_id = sqlc.arg('tenant_root_id')
RETURNING *;

-- name: ListSubscriberTags :many
SELECT * FROM subscriber_tag
WHERE subscriber_id = $1 AND tenant_root_id = $2
ORDER BY tag ASC, id ASC;

-- name: DeleteSubscriberTags :exec
DELETE FROM subscriber_tag
WHERE subscriber_id = $1 AND tenant_root_id = $2;

-- name: InsertSubscriberTag :one
INSERT INTO subscriber_tag (
    id, business_id, tenant_root_id, list_id, subscriber_id, tag, created_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (subscriber_id, tag) DO UPDATE SET tag = EXCLUDED.tag
RETURNING *;

-- name: GetContactsByIDs :many
SELECT * FROM contact
WHERE id = ANY(sqlc.arg('ids')::uuid[])
  AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND deleted_at IS NULL
ORDER BY id;

-- ---- list keys ----

-- name: InsertMailingListKey :one
INSERT INTO mailing_list_key (
    id, business_id, tenant_root_id, list_id, publishable_key, sealed_secret,
    label, status, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, 'enabled', now())
RETURNING *;

-- name: ListMailingListKeys :many
SELECT * FROM mailing_list_key
WHERE list_id = $1 AND tenant_root_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: RevokeMailingListKey :one
UPDATE mailing_list_key SET status = 'revoked', revoked_at = now()
WHERE id = $1 AND tenant_root_id = $2 AND status = 'enabled'
RETURNING *;

-- ---- sending profile ----

-- name: GetMailingSendingProfile :one
SELECT * FROM mailing_sending_profile
WHERE business_id = $1 AND tenant_root_id = $2;

-- name: InsertMailingSendingProfile :one
INSERT INTO mailing_sending_profile (
    id, business_id, tenant_root_id, mode, from_email, from_name, reply_to,
    postal_address, email_domain_id, secret_ref, ses_region,
    ses_configuration_set, sns_topic_arn, status, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
    'unverified', now(), now()
)
RETURNING *;

-- name: UpdateMailingSendingProfile :one
UPDATE mailing_sending_profile SET
    mode = $3,
    from_email = $4,
    from_name = $5,
    reply_to = $6,
    postal_address = $7,
    email_domain_id = $8,
    secret_ref = $9,
    ses_region = $10,
    ses_configuration_set = $11,
    sns_topic_arn = $12,
    status = 'unverified', last_verified_at = NULL, verify_error = NULL,
    updated_at = now()
WHERE business_id = $1 AND tenant_root_id = $2
RETURNING *;

-- name: DeleteMailingSendingProfile :one
DELETE FROM mailing_sending_profile
WHERE business_id = $1 AND tenant_root_id = $2
RETURNING *;

-- ---- templates ----

-- name: InsertMailingTemplate :one
INSERT INTO mailing_template (
    id, business_id, tenant_root_id, name, subject, preheader, body_markdown,
    track_opens, track_clicks, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), now())
RETURNING *;

-- name: GetMailingTemplate :one
SELECT * FROM mailing_template
WHERE id = $1 AND tenant_root_id = $2;

-- name: ListMailingTemplates :many
SELECT * FROM mailing_template
WHERE business_id = $1 AND tenant_root_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListMailingTemplatesAfter :many
SELECT * FROM mailing_template
WHERE business_id = sqlc.arg('business_id')
  AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND (created_at, id) < (sqlc.arg('cur_created')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: UpdateMailingTemplate :one
UPDATE mailing_template SET
    name = COALESCE(NULLIF(sqlc.narg('name')::text, ''), name),
    subject = COALESCE(NULLIF(sqlc.narg('subject')::text, ''), subject),
    preheader = CASE
        WHEN sqlc.narg('set_preheader')::boolean THEN sqlc.narg('preheader')::text
        ELSE preheader
    END,
    body_markdown = COALESCE(sqlc.narg('body_markdown')::text, body_markdown),
    track_opens = COALESCE(sqlc.narg('track_opens')::boolean, track_opens),
    track_clicks = COALESCE(sqlc.narg('track_clicks')::boolean, track_clicks),
    updated_at = now()
WHERE id = sqlc.arg('id') AND tenant_root_id = sqlc.arg('tenant_root_id')
RETURNING *;

-- name: DeleteMailingTemplate :one
DELETE FROM mailing_template
WHERE id = $1 AND tenant_root_id = $2
RETURNING *;

-- ---- suppressions ----

-- name: InsertMailingSuppression :one
INSERT INTO mailing_suppression (
    id, business_id, tenant_root_id, email, reason, source, created_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
RETURNING *;

-- name: ListMailingSuppressions :many
SELECT * FROM mailing_suppression
WHERE business_id = $1 AND tenant_root_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListMailingSuppressionsAfter :many
SELECT * FROM mailing_suppression
WHERE business_id = sqlc.arg('business_id')
  AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND (created_at, id) < (sqlc.arg('cur_created')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: DeleteMailingSuppression :one
DELETE FROM mailing_suppression
WHERE id = $1 AND tenant_root_id = $2
RETURNING *;
