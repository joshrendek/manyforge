-- Restore the prior validation limit without removing any saved rules.
CREATE OR REPLACE FUNCTION analytics_replace_property_rules(
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
