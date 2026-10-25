-- Signed server-to-server pageviews use canonical route templates and a random tab-session
-- UUID, not a visitor IP/User-Agent or an authenticated account identity. Only the HTTP handler
-- after successful HMAC verification calls this entry point. The legacy unsigned function and
-- its historical event semantics are unchanged.

CREATE FUNCTION analytics_signed_pageview_valid(p_path text, p_session text)
RETURNS boolean LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE SET search_path = public AS $$
    SELECT length(p_path) BETWEEN 1 AND 512
       AND (p_path = '/' OR p_path ~ '^/([A-Za-z0-9_-]+|:[A-Za-z_][A-Za-z0-9_-]*)(/([A-Za-z0-9_-]+|:[A-Za-z_][A-Za-z0-9_-]*))*$')
       AND p_session ~ '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
       AND position(p_session IN p_path) = 0;
$$;
REVOKE ALL ON FUNCTION analytics_signed_pageview_valid(text,text) FROM PUBLIC;

CREATE FUNCTION telemetry_ingest_analytics_signed(p_key text, p_events jsonb)
RETURNS int LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE c record; s bytea; today date; n int; enhanced int;
BEGIN
    SELECT id, business_id, tenant_root_id INTO c
    FROM telemetry_client
    WHERE publishable_key = p_key AND kind = 'analytics'
      AND status = 'active' AND revoked_at IS NULL AND sealed_secret IS NOT NULL
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN -1;
    END IF;
    IF p_events IS NULL OR jsonb_typeof(p_events) IS DISTINCT FROM 'array' THEN
        RETURN 0;
    END IF;
    IF jsonb_array_length(p_events) > 1000 THEN
        RETURN 0;
    END IF;

    -- Signed clients may continue sending old generic events in the same batch. New path/session
    -- fields are not free-form properties and cannot silently fall back to an ordinary pageview.
    INSERT INTO analytics_event (tenant_root_id, business_id, client_id, occurred_at, name, props)
    SELECT c.tenant_root_id, c.business_id, c.id, e.occurred_at, e.name,
           coalesce(e.props, '{}'::jsonb)
    FROM jsonb_to_recordset(p_events)
         AS e(occurred_at timestamptz, name text, props jsonb, path text, session_id text)
    WHERE coalesce(e.path, '') = '' AND coalesce(e.session_id, '') = '';
    GET DIAGNOSTICS n = ROW_COUNT;

    IF NOT EXISTS (
        SELECT 1 FROM jsonb_to_recordset(p_events) AS e(name text, path text, session_id text)
        WHERE e.name = 'pageview' AND analytics_signed_pageview_valid(e.path, e.session_id)
    ) THEN
        RETURN n;
    END IF;

    today := (now() AT TIME ZONE 'UTC')::date;
    SELECT salt INTO s FROM analytics_salt WHERE day = today;
    IF s IS NULL THEN
        INSERT INTO analytics_salt (day, salt) VALUES (today, gen_random_bytes(32))
        ON CONFLICT (day) DO NOTHING;
        SELECT salt INTO s FROM analytics_salt WHERE day = today;
    END IF;

    INSERT INTO analytics_event
        (tenant_root_id, business_id, client_id, occurred_at, name, props, path, visitor_hash, is_bot)
    SELECT c.tenant_root_id, c.business_id, c.id, e.occurred_at, 'pageview', '{}'::jsonb, e.path,
           substring(hmac(convert_to(
               'manyforge:analytics:tab-session:v1:' || c.tenant_root_id::text || ':' ||
               c.business_id::text || ':' || c.id::text || ':' || today::text || ':' || e.session_id,
               'UTF8'), s, 'sha256') from 1 for 16),
           false
    FROM jsonb_to_recordset(p_events)
         AS e(occurred_at timestamptz, name text, path text, session_id text)
    WHERE e.name = 'pageview' AND analytics_signed_pageview_valid(e.path, e.session_id);
    GET DIAGNOSTICS enhanced = ROW_COUNT;
    RETURN n + enhanced;
END;
$$;
REVOKE ALL ON FUNCTION telemetry_ingest_analytics_signed(text,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION telemetry_ingest_analytics_signed(text,jsonb) TO manyforge_app;

COMMENT ON COLUMN analytics_event.visitor_hash IS
    'Daily, client-scoped visitor digest. Browser collection hashes transient IP/UA with the daily salt. Signed pageviews use domain-separated HMAC-SHA256 of tenant/business/client/day plus a transient random tab-session UUID, truncated to 16 bytes. No raw session UUID, IP, or UA is stored by signed pageviews.';
