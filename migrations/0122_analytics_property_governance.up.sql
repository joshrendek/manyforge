-- manyforge-hu5a.4.2.1 — governed custom-event property collection.
--
-- Existing raw props are deliberately left untouched: analytics_event partitions expire after
-- 90 days. From this migration onward, analytics_collect persists only explicitly configured
-- scalar event/property pairs. A later rollup migration uses enabled_at as the non-retroactive
-- boundary, so enabling a rule never turns older free-form JSON into a reported aggregate.

CREATE FUNCTION analytics_property_key_prohibited(p_key text) RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE SET search_path = public AS $$
    WITH normalized AS (
        SELECT regexp_replace(lower(p_key), '[^a-z0-9]', '', 'g') AS compact
    )
    SELECT compact = ANY(ARRAY[
               'ip', 'ipaddress', 'ssn', 'dob', 'birthdate', 'dateofbirth', 'name',
               'userid', 'customerid', 'accountid', 'sessionid', 'deviceid', 'fingerprint'
           ])
        OR compact ~ '(email|phone|password|passwd|secret|token|bearer|cookie|address|street|postalcode|zipcode|useragent|firstname|lastname|fullname|creditcard|cardnumber)'
    FROM normalized;
$$;
REVOKE ALL ON FUNCTION analytics_property_key_prohibited(text) FROM PUBLIC;

CREATE TABLE analytics_property_rule (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_root_id  uuid        NOT NULL,
    business_id     uuid        NOT NULL,
    client_id       uuid        NOT NULL,
    event_name      text        NOT NULL,
    property_key    text        NOT NULL,
    label           text        NOT NULL,
    enabled_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT analytics_property_rule_pair_unique UNIQUE (client_id, event_name, property_key),
    CONSTRAINT analytics_property_rule_client_fk
        FOREIGN KEY (client_id, tenant_root_id)
        REFERENCES telemetry_client(id, tenant_root_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT analytics_property_rule_event_valid CHECK (
        char_length(event_name) BETWEEN 1 AND 64
        AND event_name <> 'pageview'
        AND event_name ~ '^[A-Za-z0-9_:.-]+$'
    ),
    CONSTRAINT analytics_property_rule_key_valid CHECK (
        char_length(property_key) BETWEEN 1 AND 32
        AND property_key ~ '^[A-Za-z0-9_:.-]+$'
        AND NOT analytics_property_key_prohibited(property_key)
    ),
    CONSTRAINT analytics_property_rule_label_valid CHECK (
        char_length(label) BETWEEN 1 AND 64
        AND label = btrim(label)
        AND label !~ '[[:cntrl:]]'
    )
);

COMMENT ON TABLE analytics_property_rule IS
    'Bounded allowlist of custom-event properties with reporting purpose. enabled_at prevents '
    'historical raw props from being retained or aggregated retroactively.';

ALTER TABLE analytics_property_rule ENABLE ROW LEVEL SECURITY;
CREATE POLICY analytics_property_rule_rls ON analytics_property_rule FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

-- Configuration is mutated only through analytics_replace_property_rules, which reasserts the
-- telemetry.write permission and serializes against collection, moves, and revocation.
GRANT SELECT ON analytics_property_rule TO manyforge_app;

INSERT INTO tenant_merge_manifest (table_name, module, strategy, inventory_version)
VALUES ('analytics_property_rule', 'telemetry', 'drain_fence_then_rewrite', 1);

CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON analytics_property_rule
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();

CREATE FUNCTION analytics_replace_property_rules(
    p_business_id uuid,
    p_client_id uuid,
    p_rules jsonb
) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE c telemetry_client%ROWTYPE;
BEGIN
    IF p_rules IS NULL OR jsonb_typeof(p_rules) IS DISTINCT FROM 'array' THEN
        RETURN 'invalid';
    END IF;

    IF jsonb_array_length(p_rules) > 20
       OR EXISTS (
           SELECT 1
           FROM jsonb_array_elements(p_rules) AS items(rule)
           WHERE jsonb_typeof(rule) IS DISTINCT FROM 'object'
              OR CASE WHEN jsonb_typeof(rule) = 'object'
                      THEN (SELECT count(*) FROM jsonb_object_keys(rule)) <> 3
                      ELSE false
                 END
              OR jsonb_typeof(rule->'event_name') IS DISTINCT FROM 'string'
              OR jsonb_typeof(rule->'property_key') IS DISTINCT FROM 'string'
              OR jsonb_typeof(rule->'label') IS DISTINCT FROM 'string'
              OR char_length(rule->>'event_name') NOT BETWEEN 1 AND 64
              OR rule->>'event_name' = 'pageview'
              OR rule->>'event_name' !~ '^[A-Za-z0-9_:.-]+$'
              OR char_length(rule->>'property_key') NOT BETWEEN 1 AND 32
              OR rule->>'property_key' !~ '^[A-Za-z0-9_:.-]+$'
              OR analytics_property_key_prohibited(rule->>'property_key')
              OR char_length(rule->>'label') NOT BETWEEN 1 AND 64
              OR rule->>'label' <> btrim(rule->>'label')
              OR rule->>'label' ~ '[[:cntrl:]]'
       )
       OR jsonb_array_length(p_rules) <> (
           SELECT count(DISTINCT (rule->>'event_name', rule->>'property_key'))
           FROM jsonb_array_elements(p_rules) AS items(rule)
       )
       OR EXISTS (
           SELECT 1
           FROM jsonb_array_elements(p_rules) AS items(rule)
           GROUP BY rule->>'event_name'
           HAVING count(*) > 6
       ) THEN
        RETURN 'invalid';
    END IF;

    -- Match site-move lock ordering: the dimension rollup lock precedes the client row lock.
    -- The row lock conflicts with analytics_collect's FOR SHARE and makes configuration races
    -- binary: an event either commits under the old complete rule set or waits for the new one.
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));
    SELECT * INTO c
    FROM telemetry_client
    WHERE id = p_client_id
      AND business_id = p_business_id
      AND kind = 'analytics'
      AND status = 'active'
      AND revoked_at IS NULL
      AND business_id IN (
          SELECT business_id
          FROM businesses_with_permission(current_principal(), 'telemetry.write')
      )
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN 'not_found';
    END IF;

    -- Remove any future property-dimension rows tied to rules being deleted. Migration 0122 does
    -- not create such rows yet, but keeping replacement cleanup here makes the stable rule IDs a
    -- safe contract for the dependent rollup slice.
    DELETE FROM analytics_dimension_daily d
    USING analytics_property_rule r
    WHERE r.client_id = p_client_id
      AND d.client_id = r.client_id
      AND d.dimension = 'property:' || r.id::text
      AND NOT EXISTS (
          SELECT 1
          FROM jsonb_array_elements(p_rules) AS items(rule)
          WHERE rule->>'event_name' = r.event_name
            AND rule->>'property_key' = r.property_key
      );

    DELETE FROM analytics_property_rule r
    WHERE r.client_id = p_client_id
      AND NOT EXISTS (
          SELECT 1
          FROM jsonb_array_elements(p_rules) AS items(rule)
          WHERE rule->>'event_name' = r.event_name
            AND rule->>'property_key' = r.property_key
      );

    INSERT INTO analytics_property_rule (
        tenant_root_id, business_id, client_id, event_name, property_key, label
    )
    SELECT c.tenant_root_id, c.business_id, c.id,
           rule->>'event_name', rule->>'property_key', rule->>'label'
    FROM jsonb_array_elements(p_rules) AS items(rule)
    ON CONFLICT (client_id, event_name, property_key) DO UPDATE
        SET label = EXCLUDED.label, updated_at = now();

    RETURN 'updated';
END; $$;

REVOKE ALL ON FUNCTION analytics_replace_property_rules(uuid,uuid,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION analytics_replace_property_rules(uuid,uuid,jsonb) TO manyforge_app;

-- Replace the 0121 collector without changing its rolling-compatible signature. p_props remains
-- a trailing-default argument for old pods, but free-form values are now filtered only after the
-- active analytics client and its exact rule set resolve under the same transaction lock.
CREATE OR REPLACE FUNCTION analytics_collect(
    p_key           text,
    p_path          text,
    p_referrer_host text,
    p_ip            text,
    p_ua            text,
    p_is_bot        boolean,
    p_utm_source    text,
    p_utm_medium    text,
    p_utm_campaign  text,
    p_device_type   text,
    p_browser       text,
    p_country       text,
    p_name          text  DEFAULT NULL,
    p_props         jsonb DEFAULT NULL,
    p_origin        text  DEFAULT NULL
) RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE c record; s bytea; today date; filtered_props jsonb := '{}'::jsonb;
BEGIN
    SELECT id, business_id, tenant_root_id, allowed_origins INTO c
    FROM telemetry_client
    WHERE publishable_key = p_key
      AND status = 'active' AND revoked_at IS NULL
      AND kind = 'analytics'
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN 0;
    END IF;

    IF cardinality(c.allowed_origins) > 0
       AND (p_origin IS NULL OR NOT (p_origin = ANY(c.allowed_origins))) THEN
        RETURN -1;
    END IF;

    IF p_name IS NOT NULL AND p_name <> ''
       AND jsonb_typeof(p_props) = 'object' THEN
        SELECT coalesce(
                   jsonb_object_agg(
                       r.property_key,
                       to_jsonb(left(p_props->>r.property_key, 128))
                   ),
                   '{}'::jsonb
               )
        INTO filtered_props
        FROM analytics_property_rule r
        WHERE r.client_id = c.id
          AND r.event_name = p_name
          AND NOT analytics_property_key_prohibited(r.property_key)
          AND p_props ? r.property_key
          AND jsonb_typeof(p_props->r.property_key) IN ('string', 'number', 'boolean');
    END IF;

    today := (now() AT TIME ZONE 'UTC')::date;
    SELECT salt INTO s FROM analytics_salt WHERE day = today;
    IF s IS NULL THEN
        INSERT INTO analytics_salt (day, salt) VALUES (today, gen_random_bytes(32))
        ON CONFLICT (day) DO NOTHING;
        SELECT salt INTO s FROM analytics_salt WHERE day = today;
    END IF;

    INSERT INTO analytics_event
        (tenant_root_id, business_id, client_id, occurred_at, name, props,
         path, referrer_host, visitor_hash, is_bot,
         utm_source, utm_medium, utm_campaign, device_type, browser, country)
    VALUES (
        c.tenant_root_id, c.business_id, c.id, now(),
        coalesce(nullif(p_name, ''), 'pageview'),
        filtered_props,
        p_path,
        nullif(p_referrer_host, ''),
        substring(
            sha256(s || convert_to(c.id::text || coalesce(p_ip, '') || coalesce(p_ua, ''), 'UTF8'))
            from 1 for 16),
        coalesce(p_is_bot, false),
        nullif(p_utm_source, ''),
        nullif(p_utm_medium, ''),
        nullif(p_utm_campaign, ''),
        nullif(p_device_type, ''),
        nullif(p_browser, ''),
        nullif(p_country, ''));
    RETURN 1;
END; $$;

-- Site moves preserve stable rule IDs and activation boundaries. The new composite FK is deferred
-- while both the rule and its client serialization row change roots atomically.
CREATE OR REPLACE FUNCTION telemetry_move_analytics_client(
    p_source_business_id uuid,
    p_client_id          uuid,
    p_target_business_id uuid
) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    c telemetry_client%ROWTYPE;
    target_tenant_root_id uuid;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM businesses_with_permission(current_principal(), 'telemetry.write')
        WHERE business_id = p_source_business_id
    ) THEN RETURN 'not_found'; END IF;

    SELECT * INTO c FROM telemetry_client
    WHERE id = p_client_id AND business_id = p_source_business_id
      AND kind = 'analytics' AND status = 'active' AND revoked_at IS NULL;
    IF NOT FOUND THEN RETURN 'not_found'; END IF;
    IF p_target_business_id = p_source_business_id THEN RETURN 'conflict'; END IF;

    SELECT b.tenant_root_id INTO target_tenant_root_id
    FROM business b
    WHERE b.id = p_target_business_id AND b.status = 'active'
      AND b.id IN (
          SELECT business_id
          FROM businesses_with_permission(current_principal(), 'telemetry.write')
      );
    IF NOT FOUND THEN RETURN 'not_found'; END IF;

    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_daily'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));

    SELECT * INTO c FROM telemetry_client
    WHERE id = p_client_id AND business_id = p_source_business_id
      AND kind = 'analytics' AND status = 'active' AND revoked_at IS NULL
    FOR UPDATE;
    IF NOT FOUND THEN RETURN 'not_found'; END IF;

    SET CONSTRAINTS analytics_property_rule_client_fk DEFERRED;

    UPDATE analytics_event SET business_id = p_target_business_id,
        tenant_root_id = target_tenant_root_id
    WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_event_daily SET business_id = p_target_business_id,
        tenant_root_id = target_tenant_root_id
    WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_daily SET business_id = p_target_business_id,
        tenant_root_id = target_tenant_root_id
    WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_page_daily SET business_id = p_target_business_id,
        tenant_root_id = target_tenant_root_id
    WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_referrer_daily SET business_id = p_target_business_id,
        tenant_root_id = target_tenant_root_id
    WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_dimension_daily SET business_id = p_target_business_id,
        tenant_root_id = target_tenant_root_id
    WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_property_rule SET business_id = p_target_business_id,
        tenant_root_id = target_tenant_root_id, updated_at = now()
    WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE telemetry_client SET business_id = p_target_business_id,
        tenant_root_id = target_tenant_root_id
    WHERE id = p_client_id;

    RETURN 'moved';
END; $$;
