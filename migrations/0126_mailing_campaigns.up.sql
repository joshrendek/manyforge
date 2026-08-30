-- 0126: Mailing campaigns, delivery queue, and tracking (Spec 013, manyforge-m2hh.7).
--
-- Authenticated campaign CRUD remains RLS-bound. Principal-less workers and public tracking
-- enter only through the narrowly-scoped SECURITY DEFINER functions below. Provider I/O is
-- deliberately outside these functions: claims and writebacks are short transactions.

CREATE TYPE campaign_status AS ENUM (
    'draft', 'scheduled', 'sending', 'sent', 'cancelled', 'failed'
);
CREATE TYPE mailing_delivery_status AS ENUM (
    'queued', 'sending', 'sent', 'delivered', 'bounced', 'complained',
    'failed', 'suppressed', 'cancelled'
);
CREATE TYPE mailing_track_kind AS ENUM (
    'open', 'click', 'unsubscribe', 'delivered', 'bounce', 'complaint'
);
CREATE TYPE mailing_delivery_source AS ENUM ('campaign', 'automation');

CREATE TABLE campaign (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id        uuid NOT NULL,
    tenant_root_id     uuid NOT NULL,
    list_id            uuid NOT NULL,
    profile_id         uuid,
    name               text NOT NULL,
    subject            text NOT NULL,
    preheader          text,
    body_markdown      text NOT NULL,
    tag_filter         text[] NOT NULL DEFAULT '{}',
    track_opens        boolean NOT NULL DEFAULT true,
    track_clicks       boolean NOT NULL DEFAULT true,
    status             campaign_status NOT NULL DEFAULT 'draft',
    scheduled_at       timestamptz,
    started_at         timestamptz,
    completed_at       timestamptz,
    fanout_cursor      uuid,
    fanout_done        boolean NOT NULL DEFAULT false,
    recipient_count    integer NOT NULL DEFAULT 0,
    sent_count         integer NOT NULL DEFAULT 0,
    delivered_count    integer NOT NULL DEFAULT 0,
    bounced_count      integer NOT NULL DEFAULT 0,
    complained_count   integer NOT NULL DEFAULT 0,
    opened_count       integer NOT NULL DEFAULT 0,
    clicked_count      integer NOT NULL DEFAULT 0,
    unsubscribed_count integer NOT NULL DEFAULT 0,
    failed_count       integer NOT NULL DEFAULT 0,
    last_error         text,
    created_by         uuid REFERENCES principal(id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    CONSTRAINT campaign_business_fk FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT campaign_list_fk FOREIGN KEY (list_id, tenant_root_id)
        REFERENCES mailing_list(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT campaign_profile_fk FOREIGN KEY (profile_id, tenant_root_id)
        REFERENCES mailing_sending_profile(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT campaign_nonnegative_counts_chk CHECK (
        recipient_count >= 0 AND sent_count >= 0 AND delivered_count >= 0 AND
        bounced_count >= 0 AND complained_count >= 0 AND opened_count >= 0 AND
        clicked_count >= 0 AND unsubscribed_count >= 0 AND failed_count >= 0
    )
);
CREATE INDEX campaign_business_created_idx
    ON campaign (business_id, tenant_root_id, created_at DESC, id DESC);
CREATE INDEX campaign_scheduled_idx ON campaign (scheduled_at) WHERE status = 'scheduled';
CREATE INDEX campaign_sending_idx ON campaign (status) WHERE status = 'sending';

CREATE TABLE mailing_delivery (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id         uuid NOT NULL,
    tenant_root_id      uuid NOT NULL,
    source_kind         mailing_delivery_source NOT NULL,
    source_id           uuid NOT NULL,
    campaign_id         uuid,
    template_id         uuid,
    subscriber_id       uuid NOT NULL,
    email               citext NOT NULL,
    status              mailing_delivery_status NOT NULL DEFAULT 'queued',
    attempts            integer NOT NULL DEFAULT 0,
    claim_generation    integer NOT NULL DEFAULT 0,
    not_before          timestamptz NOT NULL DEFAULT now(),
    lease_until         timestamptz,
    message_id          text NOT NULL UNIQUE,
    provider_message_id text,
    opened_at           timestamptz,
    first_clicked_at    timestamptz,
    last_error          text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (source_kind, source_id, subscriber_id),
    CONSTRAINT mailing_delivery_business_fk FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_delivery_campaign_fk FOREIGN KEY (campaign_id, tenant_root_id)
        REFERENCES campaign(id, tenant_root_id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_delivery_template_fk FOREIGN KEY (template_id, tenant_root_id)
        REFERENCES mailing_template(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_delivery_subscriber_fk FOREIGN KEY (subscriber_id, tenant_root_id)
        REFERENCES list_subscriber(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_delivery_content_chk CHECK (
        (source_kind = 'campaign' AND campaign_id IS NOT NULL AND template_id IS NULL)
        OR (source_kind = 'automation' AND campaign_id IS NULL AND template_id IS NOT NULL)
    ),
    CONSTRAINT mailing_delivery_attempts_chk CHECK (attempts >= 0),
    CONSTRAINT mailing_delivery_generation_chk CHECK (claim_generation >= 0)
);
CREATE INDEX mailing_delivery_due_idx
    ON mailing_delivery (not_before, created_at) WHERE status = 'queued';
CREATE INDEX mailing_delivery_lease_idx
    ON mailing_delivery (lease_until) WHERE status = 'sending';
CREATE INDEX mailing_delivery_provider_idx
    ON mailing_delivery (provider_message_id) WHERE provider_message_id IS NOT NULL;
CREATE INDEX mailing_delivery_campaign_status_idx ON mailing_delivery (campaign_id, status);
CREATE INDEX mailing_delivery_business_created_idx
    ON mailing_delivery (business_id, tenant_root_id, created_at DESC, id DESC);

CREATE TABLE mailing_tracking_event (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id      uuid NOT NULL,
    tenant_root_id   uuid NOT NULL,
    campaign_id      uuid,
    delivery_id      uuid,
    subscriber_id    uuid,
    kind             mailing_track_kind NOT NULL,
    url              text,
    ip               inet,
    user_agent       text,
    provider_payload jsonb,
    occurred_at      timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    CONSTRAINT mailing_tracking_business_fk FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_tracking_campaign_fk FOREIGN KEY (campaign_id, tenant_root_id)
        REFERENCES campaign(id, tenant_root_id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_tracking_delivery_fk FOREIGN KEY (delivery_id, tenant_root_id)
        REFERENCES mailing_delivery(id, tenant_root_id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_tracking_subscriber_fk FOREIGN KEY (subscriber_id, tenant_root_id)
        REFERENCES list_subscriber(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE
);
CREATE INDEX mailing_tracking_campaign_kind_idx
    ON mailing_tracking_event (campaign_id, kind, occurred_at DESC);
CREATE INDEX mailing_tracking_delivery_kind_idx ON mailing_tracking_event (delivery_id, kind);

CREATE TABLE mailing_provider_webhook_delivery (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id       uuid NOT NULL,
    tenant_root_id    uuid NOT NULL,
    profile_id        uuid NOT NULL,
    provider          text NOT NULL,
    external_event_id text NOT NULL,
    received_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (profile_id, external_event_id),
    CONSTRAINT mailing_webhook_business_fk FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_webhook_profile_fk FOREIGN KEY (profile_id, tenant_root_id)
        REFERENCES mailing_sending_profile(id, tenant_root_id) ON DELETE CASCADE
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_webhook_provider_chk CHECK (provider IN ('resend', 'ses'))
);

-- Authenticated CRUD tables are RLS protected. The webhook idempotency table intentionally has
-- no table grant; webhook code reaches it only through its DEFINER.
GRANT SELECT, INSERT, UPDATE, DELETE ON campaign, mailing_delivery, mailing_tracking_event TO manyforge_app;

ALTER TABLE campaign ENABLE ROW LEVEL SECURITY;
CREATE POLICY campaign_rls ON campaign FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
ALTER TABLE mailing_delivery ENABLE ROW LEVEL SECURITY;
CREATE POLICY mailing_delivery_rls ON mailing_delivery FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
ALTER TABLE mailing_tracking_event ENABLE ROW LEVEL SECURITY;
CREATE POLICY mailing_tracking_event_rls ON mailing_tracking_event FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
ALTER TABLE mailing_provider_webhook_delivery ENABLE ROW LEVEL SECURITY;
CREATE POLICY mailing_provider_webhook_delivery_rls ON mailing_provider_webhook_delivery FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

CREATE TRIGGER campaign_troot_immutable BEFORE UPDATE ON campaign
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER mailing_delivery_troot_immutable BEFORE UPDATE ON mailing_delivery
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER mailing_tracking_event_troot_immutable BEFORE UPDATE ON mailing_tracking_event
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER mailing_provider_webhook_delivery_troot_immutable
    BEFORE UPDATE ON mailing_provider_webhook_delivery
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

INSERT INTO tenant_merge_manifest (table_name, module, strategy, inventory_version) VALUES
    ('campaign', 'mailing', 'drain_fence_then_rewrite', 1),
    ('mailing_delivery', 'mailing', 'drain_fence_then_rewrite', 1),
    ('mailing_tracking_event', 'mailing', 'drain_fence_then_rewrite', 1),
    ('mailing_provider_webhook_delivery', 'mailing', 'drain_fence_then_rewrite', 1);

CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON campaign
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON mailing_delivery
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON mailing_tracking_event
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON mailing_provider_webhook_delivery
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();

CREATE FUNCTION mailing_claim_campaigns_for_fanout(p_limit integer)
RETURNS TABLE(campaign_id uuid)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    WITH candidates AS (
        SELECT c.id
        FROM campaign c
        WHERE (
                (c.status = 'scheduled' AND c.scheduled_at <= now())
                OR (c.status = 'sending' AND NOT c.fanout_done)
              )
          AND tenant_merge_root_write_allowed(c.tenant_root_id)
        ORDER BY CASE WHEN c.status = 'sending' THEN 0 ELSE 1 END, c.scheduled_at, c.id
        FOR UPDATE SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 10), 100))
    ), claimed AS (
        UPDATE campaign c
        SET status = 'sending', started_at = COALESCE(c.started_at, now()), updated_at = now()
        FROM candidates x
        WHERE c.id = x.id
        RETURNING c.id
    )
    SELECT id FROM claimed;
$$;

CREATE FUNCTION mailing_fanout_batch(p_campaign_id uuid, p_batch integer, p_message_domain text)
RETURNS TABLE(inserted_count integer, fanout_done boolean)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_campaign campaign%ROWTYPE;
    v_count integer := 0;
    v_last uuid;
    v_domain text := lower(btrim(p_message_domain));
BEGIN
    IF v_domain = '' OR v_domain IS NULL OR v_domain !~ '^[a-z0-9.-]+$' THEN
        RAISE EXCEPTION 'invalid mailing message domain' USING ERRCODE = '22023';
    END IF;
    SELECT * INTO v_campaign FROM campaign WHERE id = p_campaign_id FOR UPDATE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 0, true;
        RETURN;
    END IF;
    IF v_campaign.status <> 'sending' OR v_campaign.fanout_done THEN
        RETURN QUERY SELECT 0, true;
        RETURN;
    END IF;
    IF NOT tenant_merge_root_write_allowed(v_campaign.tenant_root_id) THEN
        RETURN QUERY SELECT 0, true;
        RETURN;
    END IF;

    WITH eligible AS (
        SELECT s.id, s.email
        FROM list_subscriber s
        WHERE s.list_id = v_campaign.list_id
          AND s.tenant_root_id = v_campaign.tenant_root_id
          AND s.status = 'active'
          AND (v_campaign.fanout_cursor IS NULL OR s.id > v_campaign.fanout_cursor)
          AND (
              cardinality(v_campaign.tag_filter) = 0 OR EXISTS (
                  SELECT 1 FROM subscriber_tag st
                  WHERE st.subscriber_id = s.id
                    AND st.tenant_root_id = s.tenant_root_id
                    AND lower(st.tag::text) = ANY(v_campaign.tag_filter)
              )
          )
          AND NOT EXISTS (
              SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = v_campaign.business_id AND ms.email = s.email
          )
          AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = s.email)
        ORDER BY s.id
        LIMIT GREATEST(1, LEAST(COALESCE(p_batch, 1000), 5000))
    ), prepared AS (
        SELECT gen_random_uuid() AS id, e.id AS subscriber_id, e.email FROM eligible e
    ), inserted AS (
        INSERT INTO mailing_delivery (
            id, business_id, tenant_root_id, source_kind, source_id, campaign_id,
            subscriber_id, email, status, not_before, message_id
        )
        SELECT p.id, v_campaign.business_id, v_campaign.tenant_root_id, 'campaign',
               v_campaign.id, v_campaign.id, p.subscriber_id, p.email, 'queued', now(),
               p.id::text || '@' || v_domain
        FROM prepared p
        ON CONFLICT (source_kind, source_id, subscriber_id) DO NOTHING
        RETURNING subscriber_id
    )
    SELECT count(*)::integer INTO v_count FROM inserted;

    -- Advance over the selected eligible page even if replay dedupe made inserts no-ops.
    SELECT s.id INTO v_last
    FROM (
        SELECT ls.id
        FROM list_subscriber ls
        WHERE ls.list_id = v_campaign.list_id
          AND ls.tenant_root_id = v_campaign.tenant_root_id
          AND ls.status = 'active'
          AND (v_campaign.fanout_cursor IS NULL OR ls.id > v_campaign.fanout_cursor)
          AND (cardinality(v_campaign.tag_filter) = 0 OR EXISTS (
              SELECT 1 FROM subscriber_tag st WHERE st.subscriber_id = ls.id
                AND st.tenant_root_id = ls.tenant_root_id
                AND lower(st.tag::text) = ANY(v_campaign.tag_filter)))
          AND NOT EXISTS (SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = v_campaign.business_id AND ms.email = ls.email)
          AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = ls.email)
        ORDER BY ls.id
        LIMIT GREATEST(1, LEAST(COALESCE(p_batch, 1000), 5000))
    ) s
    ORDER BY s.id DESC
    LIMIT 1;

    UPDATE campaign c SET
        fanout_cursor = COALESCE(v_last, c.fanout_cursor),
        fanout_done = v_last IS NULL OR NOT EXISTS (
            SELECT 1 FROM list_subscriber ls
            WHERE ls.list_id = c.list_id AND ls.tenant_root_id = c.tenant_root_id
              AND ls.status = 'active' AND (v_last IS NULL OR ls.id > v_last)
              AND (cardinality(c.tag_filter) = 0 OR EXISTS (
                  SELECT 1 FROM subscriber_tag st WHERE st.subscriber_id = ls.id
                    AND st.tenant_root_id = ls.tenant_root_id
                    AND lower(st.tag::text) = ANY(c.tag_filter)))
              AND NOT EXISTS (SELECT 1 FROM mailing_suppression ms
                  WHERE ms.business_id = c.business_id AND ms.email = ls.email)
              AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = ls.email)
        ),
        recipient_count = (SELECT count(*) FROM mailing_delivery d WHERE d.campaign_id = c.id),
        updated_at = now()
    WHERE c.id = v_campaign.id
    RETURNING c.fanout_done INTO fanout_done;
    inserted_count := v_count;
    RETURN NEXT;
END;
$$;

CREATE FUNCTION mailing_claim_deliveries(p_limit integer, p_lease interval)
RETURNS TABLE(
    delivery_id uuid, business_id uuid, tenant_root_id uuid, source_id uuid,
    campaign_id uuid, template_id uuid, content_updated_at timestamptz,
    subscriber_id uuid, email citext, attempts integer, claim_generation integer, message_id text,
    subject text, preheader text, body_markdown text, track_opens boolean,
    track_clicks boolean, list_name text, first_name text, last_name text,
    profile_id uuid, profile_updated_at timestamptz, profile_mode mailing_send_mode,
    from_email citext, from_name text, reply_to citext, postal_address text,
    email_domain_id uuid, secret_ref uuid, ses_region text, ses_configuration_set text
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    WITH cancelled_candidates AS (
        SELECT d.id
        FROM mailing_delivery d
        JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE d.status = 'sending' AND d.lease_until <= now()
          AND c.status = 'cancelled'
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.lease_until, d.id
        FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), cancelled AS (
        UPDATE mailing_delivery d
        SET status = 'cancelled', lease_until = NULL,
            last_error = 'campaign cancelled while delivery was in flight', updated_at = now()
        FROM cancelled_candidates x
        WHERE d.id = x.id
        RETURNING d.id
    ), ineligible_candidates AS (
        SELECT d.id
        FROM mailing_delivery d
        JOIN list_subscriber s
          ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE (
                (d.status = 'queued' AND d.not_before <= now())
                OR (d.status = 'sending' AND d.lease_until <= now())
              )
          AND (d.source_kind = 'automation' OR c.status = 'sending')
          AND (
              s.status <> 'active'
              OR EXISTS (SELECT 1 FROM mailing_suppression ms
                  WHERE ms.business_id = d.business_id AND ms.email = d.email)
              OR EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = d.email)
          )
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.not_before, d.created_at, d.id
        FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), suppressed AS (
        UPDATE mailing_delivery d
        SET status = 'suppressed', lease_until = NULL,
            last_error = 'subscriber became ineligible before send', updated_at = now()
        FROM ineligible_candidates x
        WHERE d.id = x.id
        RETURNING d.id
    ), candidates AS (
        SELECT d.id
        FROM mailing_delivery d
        JOIN list_subscriber s
          ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE (
                (d.status = 'queued' AND d.not_before <= now())
                OR (d.status = 'sending' AND d.lease_until <= now())
              )
          AND (d.source_kind = 'automation' OR c.status = 'sending')
          AND s.status = 'active'
          AND NOT EXISTS (SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = d.business_id AND ms.email = d.email)
          AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = d.email)
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.not_before, d.created_at, d.id
        FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), claimed AS (
        UPDATE mailing_delivery d
        SET status = 'sending', attempts = d.attempts + 1,
            claim_generation = d.claim_generation + 1,
            lease_until = now() + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            updated_at = now()
        FROM candidates x
        WHERE d.id = x.id
        RETURNING d.*
    )
    SELECT d.id, d.business_id, d.tenant_root_id, d.source_id, d.campaign_id,
           d.template_id, COALESCE(c.updated_at, t.updated_at), d.subscriber_id,
           d.email, d.attempts, d.claim_generation, d.message_id,
           COALESCE(c.subject, t.subject), COALESCE(c.preheader, t.preheader),
           COALESCE(c.body_markdown, t.body_markdown),
           COALESCE(c.track_opens, t.track_opens), COALESCE(c.track_clicks, t.track_clicks),
           l.name, s.first_name, s.last_name,
           p.id, p.updated_at, p.mode, p.from_email, p.from_name, p.reply_to,
           p.postal_address, p.email_domain_id, p.secret_ref, p.ses_region,
           p.ses_configuration_set
    FROM claimed d
    LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
    LEFT JOIN mailing_template t ON t.id = d.template_id AND t.tenant_root_id = d.tenant_root_id
    JOIN list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
    JOIN mailing_list l ON l.id = s.list_id AND l.tenant_root_id = s.tenant_root_id
    JOIN mailing_sending_profile p
     ON p.tenant_root_id = d.tenant_root_id
     AND p.business_id = d.business_id
     AND p.id = COALESCE(c.profile_id, p.id);
$$;

CREATE FUNCTION mailing_release_delivery(p_id uuid, p_generation integer, p_not_before timestamptz)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH changed AS (
        UPDATE mailing_delivery SET status = 'queued', attempts = GREATEST(attempts - 1, 0),
            not_before = GREATEST(COALESCE(p_not_before, now()), now()), lease_until = NULL,
            updated_at = now()
        WHERE id = p_id AND status = 'sending' AND claim_generation = p_generation
          AND tenant_merge_root_write_allowed(tenant_root_id)
        RETURNING 1
    ) SELECT EXISTS(SELECT 1 FROM changed);
$$;

CREATE FUNCTION mailing_renew_delivery(p_id uuid, p_generation integer, p_lease interval)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH changed AS (
        UPDATE mailing_delivery SET
            lease_until = now() + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            updated_at = now()
        WHERE id = p_id AND status = 'sending' AND claim_generation = p_generation
          AND tenant_merge_root_write_allowed(tenant_root_id)
          AND (source_kind = 'automation' OR EXISTS (
              SELECT 1 FROM campaign c
              WHERE c.id = campaign_id AND c.tenant_root_id = mailing_delivery.tenant_root_id
                AND c.status = 'sending'
          ))
          AND EXISTS (
              SELECT 1 FROM list_subscriber s
              WHERE s.id = subscriber_id AND s.tenant_root_id = mailing_delivery.tenant_root_id
                AND s.status = 'active'
          )
          AND NOT EXISTS (
              SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = mailing_delivery.business_id AND ms.email = mailing_delivery.email
          )
          AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = mailing_delivery.email)
        RETURNING 1
    ) SELECT EXISTS(SELECT 1 FROM changed);
$$;

CREATE FUNCTION mailing_complete_delivery(p_id uuid, p_generation integer, p_provider_message_id text)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH changed AS (
        UPDATE mailing_delivery SET status = 'sent', provider_message_id = p_provider_message_id,
            lease_until = NULL, last_error = NULL, updated_at = now()
        WHERE id = p_id AND status = 'sending' AND claim_generation = p_generation
          AND tenant_merge_root_write_allowed(tenant_root_id)
        RETURNING 1
    ) SELECT EXISTS(SELECT 1 FROM changed);
$$;

CREATE FUNCTION mailing_fail_delivery(
    p_id uuid, p_generation integer, p_error text, p_status text, p_not_before timestamptz
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_status mailing_delivery_status;
BEGIN
    IF p_status = 'retry' THEN v_status := 'queued';
    ELSIF p_status IN ('failed', 'suppressed') THEN v_status := p_status::mailing_delivery_status;
    ELSE RAISE EXCEPTION 'invalid delivery failure status' USING ERRCODE = '22023';
    END IF;
    UPDATE mailing_delivery SET status = v_status,
        not_before = CASE WHEN v_status = 'queued' THEN GREATEST(COALESCE(p_not_before, now()), now()) ELSE not_before END,
        lease_until = NULL, last_error = left(COALESCE(p_error, ''), 2000), updated_at = now()
    WHERE id = p_id AND status = 'sending' AND claim_generation = p_generation
      AND tenant_merge_root_write_allowed(tenant_root_id);
    RETURN FOUND;
END;
$$;

CREATE FUNCTION mailing_cancel_campaign(p_campaign_id uuid)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_changed boolean;
BEGIN
    UPDATE campaign c SET status = 'cancelled', completed_at = now(), updated_at = now()
    WHERE c.id = p_campaign_id AND c.status IN ('scheduled', 'sending')
      AND c.business_id IN (SELECT business_id FROM authorized_businesses(current_principal()));
    v_changed := FOUND;
    IF v_changed THEN
        UPDATE mailing_delivery SET status = 'cancelled', updated_at = now()
        WHERE campaign_id = p_campaign_id AND status = 'queued';
    END IF;
    RETURN v_changed;
END;
$$;

CREATE FUNCTION mailing_rollup_campaigns()
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_count integer;
BEGIN
    WITH aggregates AS (
        SELECT c.id,
               count(d.id)::integer AS recipients,
               count(*) FILTER (WHERE d.status IN ('sent','delivered','bounced','complained'))::integer AS sent,
               count(*) FILTER (WHERE d.status = 'delivered')::integer AS delivered,
               count(*) FILTER (WHERE d.status = 'bounced')::integer AS bounced,
               count(*) FILTER (WHERE d.status = 'complained')::integer AS complained,
               count(*) FILTER (WHERE d.opened_at IS NOT NULL)::integer AS opened,
               count(*) FILTER (WHERE d.first_clicked_at IS NOT NULL)::integer AS clicked,
               count(*) FILTER (WHERE d.status IN ('failed','suppressed'))::integer AS failed,
               NOT EXISTS (SELECT 1 FROM mailing_delivery x WHERE x.campaign_id = c.id AND x.status IN ('queued','sending')) AS drained
        FROM campaign c LEFT JOIN mailing_delivery d ON d.campaign_id = c.id
        WHERE c.status IN ('sending', 'sent', 'cancelled')
          AND tenant_merge_root_write_allowed(c.tenant_root_id)
        GROUP BY c.id
    ), changed AS (
        UPDATE campaign c SET recipient_count = a.recipients, sent_count = a.sent,
            delivered_count = a.delivered, bounced_count = a.bounced,
            complained_count = a.complained, opened_count = a.opened,
            clicked_count = a.clicked, failed_count = a.failed,
            unsubscribed_count = (SELECT count(DISTINCT e.subscriber_id)::integer
                FROM mailing_tracking_event e WHERE e.campaign_id = c.id AND e.kind = 'unsubscribe'),
            status = CASE WHEN c.status = 'sending' AND c.fanout_done AND a.drained THEN 'sent'::campaign_status ELSE c.status END,
            completed_at = CASE WHEN c.status = 'sending' AND c.fanout_done AND a.drained THEN COALESCE(c.completed_at, now()) ELSE c.completed_at END,
            updated_at = now()
        FROM aggregates a WHERE c.id = a.id RETURNING 1
    ) SELECT count(*) INTO v_count FROM changed;
    RETURN v_count;
END;
$$;

CREATE FUNCTION mailing_profile_context(p_profile_id uuid)
RETURNS TABLE(
    profile_id uuid, business_id uuid, tenant_root_id uuid, updated_at timestamptz,
    mode mailing_send_mode, from_email citext, from_name text, reply_to citext,
    postal_address text, email_domain_id uuid, secret_ref uuid, credential_sealed text, ses_region text,
    ses_configuration_set text, sns_topic_arn text
)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT p.id, p.business_id, p.tenant_root_id, p.updated_at, p.mode, p.from_email,
           p.from_name, p.reply_to, p.postal_address, p.email_domain_id, p.secret_ref, s.sealed_value,
           p.ses_region, p.ses_configuration_set, p.sns_topic_arn
    FROM mailing_sending_profile p
    LEFT JOIN secret s ON s.id = p.secret_ref AND s.tenant_root_id = p.tenant_root_id
    WHERE p.id = p_profile_id AND p.status = 'verified';
$$;

CREATE FUNCTION mailing_business_profile_context(p_business_id uuid)
RETURNS TABLE(
    profile_id uuid, business_id uuid, tenant_root_id uuid, updated_at timestamptz,
    mode mailing_send_mode, from_email citext, from_name text, reply_to citext,
    postal_address text, email_domain_id uuid, secret_ref uuid, credential_sealed text, ses_region text,
    ses_configuration_set text, sns_topic_arn text
)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT p.id, p.business_id, p.tenant_root_id, p.updated_at, p.mode, p.from_email,
           p.from_name, p.reply_to, p.postal_address, p.email_domain_id, p.secret_ref, s.sealed_value,
           p.ses_region, p.ses_configuration_set, p.sns_topic_arn
    FROM mailing_sending_profile p
    LEFT JOIN secret s ON s.id = p.secret_ref AND s.tenant_root_id = p.tenant_root_id
    WHERE p.business_id = p_business_id AND p.status = 'verified';
$$;

CREATE FUNCTION mailing_record_track(
    p_delivery_id uuid, p_kind mailing_track_kind, p_url text, p_ip inet, p_ua text
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_delivery mailing_delivery%ROWTYPE;
BEGIN
    SELECT * INTO v_delivery FROM mailing_delivery WHERE id = p_delivery_id;
    IF NOT FOUND OR p_kind NOT IN ('open', 'click') THEN RETURN false; END IF;
    INSERT INTO mailing_tracking_event (
        business_id, tenant_root_id, campaign_id, delivery_id, subscriber_id,
        kind, url, ip, user_agent, occurred_at
    ) VALUES (
        v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.campaign_id,
        v_delivery.id, v_delivery.subscriber_id, p_kind, p_url, p_ip,
        left(COALESCE(p_ua, ''), 1000), now()
    );
    IF p_kind = 'open' THEN
        UPDATE mailing_delivery SET opened_at = COALESCE(opened_at, now()), updated_at = now()
        WHERE id = v_delivery.id;
    ELSE
        UPDATE mailing_delivery SET first_clicked_at = COALESCE(first_clicked_at, now()), updated_at = now()
        WHERE id = v_delivery.id;
    END IF;
    RETURN true;
END;
$$;

CREATE FUNCTION mailing_record_unsubscribe(p_subscriber_id uuid, p_campaign_id uuid)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_delivery mailing_delivery%ROWTYPE;
BEGIN
    SELECT * INTO v_delivery FROM mailing_delivery
    WHERE campaign_id = p_campaign_id AND subscriber_id = p_subscriber_id;
    IF NOT FOUND THEN RETURN false; END IF;
    IF NOT EXISTS (SELECT 1 FROM mailing_tracking_event
        WHERE delivery_id = v_delivery.id AND kind = 'unsubscribe') THEN
        INSERT INTO mailing_tracking_event (
            business_id, tenant_root_id, campaign_id, delivery_id, subscriber_id, kind
        ) VALUES (v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.campaign_id,
                  v_delivery.id, v_delivery.subscriber_id, 'unsubscribe');
    END IF;
    RETURN true;
END;
$$;

CREATE FUNCTION mailing_mark_bounced(p_message_id text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_delivery mailing_delivery%ROWTYPE;
BEGIN
    SELECT * INTO v_delivery FROM mailing_delivery
    WHERE message_id = trim(both '<>' from btrim(p_message_id)) FOR UPDATE;
    IF NOT FOUND THEN RETURN false; END IF;
    UPDATE mailing_delivery SET status = 'bounced', lease_until = NULL,
        last_error = 'relay hard bounce', updated_at = now()
    WHERE id = v_delivery.id AND status NOT IN ('bounced', 'complained');
    INSERT INTO mailing_suppression (business_id, tenant_root_id, email, reason, source)
    VALUES (v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.email, 'bounce', 'relay')
    ON CONFLICT (business_id, email) DO UPDATE SET
        reason = CASE WHEN mailing_suppression.reason = 'complaint' THEN mailing_suppression.reason ELSE 'bounce' END,
        source = CASE WHEN mailing_suppression.reason = 'complaint' THEN mailing_suppression.source ELSE 'relay' END;
    UPDATE list_subscriber SET status = 'bounced', status_reason = 'relay hard bounce', updated_at = now()
    WHERE id = v_delivery.subscriber_id AND status NOT IN ('complained');
    IF NOT EXISTS (SELECT 1 FROM mailing_tracking_event
        WHERE delivery_id = v_delivery.id AND kind = 'bounce') THEN
        INSERT INTO mailing_tracking_event (
            business_id, tenant_root_id, campaign_id, delivery_id, subscriber_id, kind
        ) VALUES (v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.campaign_id,
                  v_delivery.id, v_delivery.subscriber_id, 'bounce');
    END IF;
    RETURN true;
END;
$$;

CREATE FUNCTION mailing_enqueue_delivery(
    p_business_id uuid, p_tenant_root_id uuid, p_source_id uuid, p_template_id uuid,
    p_subscriber_id uuid, p_not_before timestamptz, p_message_domain text
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_id uuid := gen_random_uuid(); v_email citext; v_existing uuid;
BEGIN
    IF p_message_domain IS NULL OR lower(btrim(p_message_domain)) !~ '^[a-z0-9.-]+$' THEN
        RAISE EXCEPTION 'invalid mailing message domain' USING ERRCODE = '22023';
    END IF;
    SELECT email INTO v_email FROM list_subscriber
    WHERE id = p_subscriber_id AND business_id = p_business_id AND tenant_root_id = p_tenant_root_id;
    IF NOT FOUND THEN RETURN NULL; END IF;
    PERFORM 1 FROM mailing_template
    WHERE id = p_template_id AND business_id = p_business_id AND tenant_root_id = p_tenant_root_id;
    IF NOT FOUND THEN RETURN NULL; END IF;
    INSERT INTO mailing_delivery (
        id, business_id, tenant_root_id, source_kind, source_id, template_id,
        subscriber_id, email, not_before, message_id
    ) VALUES (v_id, p_business_id, p_tenant_root_id, 'automation', p_source_id,
              p_template_id, p_subscriber_id, v_email, COALESCE(p_not_before, now()),
              v_id::text || '@' || lower(btrim(p_message_domain)))
    ON CONFLICT (source_kind, source_id, subscriber_id) DO NOTHING
    RETURNING id INTO v_existing;
    IF v_existing IS NULL THEN
        SELECT id INTO v_existing FROM mailing_delivery
        WHERE source_kind = 'automation' AND source_id = p_source_id AND subscriber_id = p_subscriber_id;
    END IF;
    RETURN v_existing;
END;
$$;

CREATE FUNCTION mailing_delivery_engagement(p_delivery_id uuid)
RETURNS TABLE(opened_at timestamptz, first_clicked_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT d.opened_at, d.first_clicked_at FROM mailing_delivery d WHERE d.id = p_delivery_id;
$$;

CREATE FUNCTION mailing_webhook_context(p_profile_id uuid)
RETURNS TABLE(
    profile_id uuid, business_id uuid, tenant_root_id uuid, provider text,
    secret_ref uuid, credential_sealed text, sns_topic_arn text
) LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT p.id, p.business_id, p.tenant_root_id, p.mode::text, p.secret_ref,
           s.sealed_value, p.sns_topic_arn
    FROM mailing_sending_profile p
    LEFT JOIN secret s ON s.id = p.secret_ref AND s.tenant_root_id = p.tenant_root_id
    WHERE p.id = p_profile_id AND p.status = 'verified' AND p.mode IN ('resend', 'ses');
$$;

CREATE FUNCTION mailing_record_webhook(p_profile_id uuid, p_provider text, p_event_id text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_profile mailing_sending_profile%ROWTYPE;
BEGIN
    SELECT * INTO v_profile FROM mailing_sending_profile
    WHERE id = p_profile_id AND status = 'verified' AND mode::text = p_provider;
    IF NOT FOUND THEN RETURN false; END IF;
    INSERT INTO mailing_provider_webhook_delivery (
        business_id, tenant_root_id, profile_id, provider, external_event_id
    ) VALUES (v_profile.business_id, v_profile.tenant_root_id, v_profile.id, p_provider, p_event_id)
    ON CONFLICT (profile_id, external_event_id) DO NOTHING;
    RETURN FOUND;
END;
$$;

CREATE FUNCTION mailing_apply_provider_event(
    p_profile_id uuid, p_provider_message_id text, p_recipient citext,
    p_kind mailing_track_kind, p_occurred_at timestamptz, p_payload jsonb
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_delivery mailing_delivery%ROWTYPE; v_status mailing_delivery_status;
BEGIN
    SELECT d.* INTO v_delivery FROM mailing_delivery d
    JOIN mailing_sending_profile p
      ON p.id = p_profile_id AND p.business_id = d.business_id
     AND p.tenant_root_id = d.tenant_root_id
    LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
    WHERE (d.campaign_id IS NULL OR c.profile_id = p.id)
      AND d.provider_message_id = p_provider_message_id
      AND d.email = p_recipient FOR UPDATE OF d;
    IF NOT FOUND OR p_kind NOT IN ('delivered','bounce','complaint') THEN RETURN false; END IF;
    v_status := CASE p_kind WHEN 'delivered' THEN 'delivered'::mailing_delivery_status
        WHEN 'bounce' THEN 'bounced'::mailing_delivery_status
        ELSE 'complained'::mailing_delivery_status END;
    -- Complaint/bounce are terminal and must not be downgraded by a late delivery event.
    UPDATE mailing_delivery SET status = CASE
        WHEN status = 'complained' THEN status
        WHEN status = 'bounced' AND v_status = 'delivered' THEN status
        ELSE v_status END, updated_at = now()
    WHERE id = v_delivery.id;
    INSERT INTO mailing_tracking_event (
        business_id, tenant_root_id, campaign_id, delivery_id, subscriber_id,
        kind, provider_payload, occurred_at
    ) VALUES (v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.campaign_id,
              v_delivery.id, v_delivery.subscriber_id, p_kind, p_payload,
              COALESCE(p_occurred_at, now()));
    IF p_kind IN ('bounce','complaint') THEN
        INSERT INTO mailing_suppression (business_id, tenant_root_id, email, reason, source)
        VALUES (v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.email,
                CASE WHEN p_kind = 'complaint' THEN 'complaint'::mailing_suppression_reason
                     ELSE 'bounce'::mailing_suppression_reason END, 'provider_webhook')
        ON CONFLICT (business_id, email) DO UPDATE SET reason = EXCLUDED.reason, source = EXCLUDED.source;
        UPDATE list_subscriber SET status = CASE WHEN p_kind = 'complaint'
            THEN 'complained'::mailing_subscriber_status ELSE 'bounced'::mailing_subscriber_status END,
            status_reason = 'provider ' || p_kind::text, updated_at = now()
        WHERE id = v_delivery.subscriber_id
          AND (p_kind = 'complaint' OR status <> 'complained');
    END IF;
    INSERT INTO activity_entry (
        id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor,
        source_type, source_id, summary, metadata, created_at
    ) SELECT gen_random_uuid(), v_delivery.tenant_root_id, v_delivery.business_id,
             s.contact_id, 'mailing.' || p_kind::text, COALESCE(p_occurred_at, now()),
             'system', 'mailing_delivery', v_delivery.id,
             'Mailing delivery ' || p_kind::text, '{}'::jsonb, now()
      FROM list_subscriber s WHERE s.id = v_delivery.subscriber_id AND s.contact_id IS NOT NULL
    ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING;
    RETURN true;
END;
$$;

REVOKE ALL ON FUNCTION mailing_claim_campaigns_for_fanout(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_fanout_batch(uuid,integer,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_claim_deliveries(integer,interval) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_release_delivery(uuid,integer,timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_renew_delivery(uuid,integer,interval) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_complete_delivery(uuid,integer,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_fail_delivery(uuid,integer,text,text,timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_cancel_campaign(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_rollup_campaigns() FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_profile_context(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_business_profile_context(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_record_track(uuid,mailing_track_kind,text,inet,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_record_unsubscribe(uuid,uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_mark_bounced(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_enqueue_delivery(uuid,uuid,uuid,uuid,uuid,timestamptz,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_delivery_engagement(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_webhook_context(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_record_webhook(uuid,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_apply_provider_event(uuid,text,citext,mailing_track_kind,timestamptz,jsonb) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION mailing_claim_campaigns_for_fanout(integer) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_fanout_batch(uuid,integer,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_claim_deliveries(integer,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_release_delivery(uuid,integer,timestamptz) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_renew_delivery(uuid,integer,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_complete_delivery(uuid,integer,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_fail_delivery(uuid,integer,text,text,timestamptz) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_cancel_campaign(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_rollup_campaigns() TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_profile_context(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_business_profile_context(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_record_track(uuid,mailing_track_kind,text,inet,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_record_unsubscribe(uuid,uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_mark_bounced(text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_enqueue_delivery(uuid,uuid,uuid,uuid,uuid,timestamptz,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_delivery_engagement(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_webhook_context(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_record_webhook(uuid,text,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_apply_provider_event(uuid,text,citext,mailing_track_kind,timestamptz,jsonb) TO manyforge_app;
