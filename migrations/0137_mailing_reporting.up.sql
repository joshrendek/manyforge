-- Exact consent history begins here, never at a guessed confirmed_at/updated_at.
-- Drain writers before taking the baseline. Trigger writes and their source writes
-- commit together; subscriber and list intervals are independent, so archiving a
-- list cannot race a subscriber activation into a lost membership transition.
LOCK TABLE business, mailing_list, list_subscriber IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE mailing_reporting_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    history_started_at timestamptz NOT NULL,
    identity_key bytea NOT NULL CHECK (octet_length(identity_key) = 32)
);
INSERT INTO mailing_reporting_state VALUES (true, clock_timestamp(), gen_random_bytes(32));
-- The HMAC key is never available to the application (nor through a callable hash
-- oracle). History contains no email, subscriber ID, name, tags, or attributes.
GRANT SELECT (singleton, history_started_at) ON mailing_reporting_state TO manyforge_app;

CREATE TABLE mailing_reporting_business (
    business_id uuid PRIMARY KEY,
    tenant_root_id uuid NOT NULL,
    history_started_at timestamptz NOT NULL,
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business(id, tenant_root_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE
);
INSERT INTO mailing_reporting_business
SELECT b.id, b.tenant_root_id, s.history_started_at
FROM business b CROSS JOIN mailing_reporting_state s;

CREATE TABLE mailing_reporting_membership (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    business_id uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    list_id uuid NOT NULL,
    identity_fingerprint bytea NOT NULL CHECK (octet_length(identity_fingerprint) = 32),
    started_at timestamptz NOT NULL,
    ended_at timestamptz,
    CHECK (ended_at IS NULL OR ended_at >= started_at),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business(id, tenant_root_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE
);
CREATE UNIQUE INDEX mailing_reporting_membership_open_idx
    ON mailing_reporting_membership (list_id, identity_fingerprint) WHERE ended_at IS NULL;
CREATE INDEX mailing_reporting_membership_window_idx
    ON mailing_reporting_membership (business_id, (COALESCE(ended_at, 'infinity'::timestamptz)), started_at);
CREATE INDEX mailing_reporting_membership_retention_idx
    ON mailing_reporting_membership (ended_at) WHERE ended_at IS NOT NULL;

CREATE TABLE mailing_reporting_list (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    business_id uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    list_id uuid NOT NULL,
    started_at timestamptz NOT NULL,
    ended_at timestamptz,
    CHECK (ended_at IS NULL OR ended_at >= started_at),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business(id, tenant_root_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE
);
CREATE UNIQUE INDEX mailing_reporting_list_open_idx
    ON mailing_reporting_list (list_id) WHERE ended_at IS NULL;
CREATE INDEX mailing_reporting_list_window_idx
    ON mailing_reporting_list (list_id, started_at, (COALESCE(ended_at, 'infinity'::timestamptz)));
CREATE INDEX mailing_reporting_list_retention_idx
    ON mailing_reporting_list (ended_at) WHERE ended_at IS NOT NULL;

INSERT INTO mailing_reporting_membership
    (business_id, tenant_root_id, list_id, identity_fingerprint, started_at)
SELECT m.business_id, m.tenant_root_id, m.list_id,
       hmac(convert_to(lower(m.email::text), 'UTF8'), s.identity_key, 'sha256'), s.history_started_at
FROM list_subscriber m CROSS JOIN mailing_reporting_state s WHERE m.status = 'active';
INSERT INTO mailing_reporting_list (business_id, tenant_root_id, list_id, started_at)
SELECT l.business_id, l.tenant_root_id, l.id, s.history_started_at
FROM mailing_list l CROSS JOIN mailing_reporting_state s WHERE l.status = 'active';

-- No list/subscriber FK: deleting a source row is a real departure, not a deletion
-- of its start balance. Closed pseudonymous intervals survive only for reporting's
-- seven-day window, then the existing maintenance worker prunes them.
-- The key deliberately excludes tenant_root_id: a merge rewrites roots exactly
-- once through the manifest and deduplicates both balances in the NEW tenant.
DO $migration$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['mailing_reporting_business', 'mailing_reporting_membership', 'mailing_reporting_list'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY %I ON %I FOR SELECT USING
            (business_id IN (SELECT business_id FROM businesses_with_permission(current_principal(), ''mailing.read'')))', t || '_read', t);
        EXECUTE format('GRANT SELECT ON %I TO manyforge_app', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable()', t || '_troot_immutable', t);
        EXECUTE format('CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence()', t);
    END LOOP;
END;
$migration$;
INSERT INTO tenant_merge_manifest (table_name, module, strategy, inventory_version) VALUES
    ('mailing_reporting_business', 'mailing', 'drain_fence_then_rewrite', 1),
    ('mailing_reporting_membership', 'mailing', 'drain_fence_then_rewrite', 1),
    ('mailing_reporting_list', 'mailing', 'drain_fence_then_rewrite', 1);

CREATE FUNCTION mailing_reporting_business_created() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
    INSERT INTO public.mailing_reporting_business (business_id, tenant_root_id, history_started_at)
    SELECT NEW.id, NEW.tenant_root_id, history_started_at FROM public.mailing_reporting_state;
    RETURN NEW;
END;
$$;
CREATE TRIGGER mailing_reporting_business_created AFTER INSERT ON business
    FOR EACH ROW EXECUTE FUNCTION mailing_reporting_business_created();

CREATE FUNCTION mailing_reporting_membership_changed() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_at timestamptz; v_key bytea;
BEGIN
    -- Root-only merge rewrites must not manufacture lifecycle events or mutate a
    -- second manifest table behind the cutover's exact per-table row accounting.
    IF TG_OP = 'UPDATE' AND OLD.status = NEW.status AND lower(OLD.email::text) = lower(NEW.email::text)
       AND OLD.list_id = NEW.list_id AND OLD.business_id = NEW.business_id THEN
        RETURN NEW;
    END IF;
    v_at := clock_timestamp();
    SELECT identity_key INTO STRICT v_key FROM public.mailing_reporting_state;
    IF TG_OP <> 'INSERT' AND OLD.status = 'active' THEN
        UPDATE public.mailing_reporting_membership SET ended_at = GREATEST(v_at, started_at)
        WHERE list_id = OLD.list_id AND ended_at IS NULL
          AND identity_fingerprint = public.hmac(convert_to(lower(OLD.email::text), 'UTF8'), v_key, 'sha256');
    END IF;
    IF TG_OP <> 'DELETE' AND NEW.status = 'active' THEN
        INSERT INTO public.mailing_reporting_membership
            (business_id, tenant_root_id, list_id, identity_fingerprint, started_at)
        VALUES (NEW.business_id, NEW.tenant_root_id, NEW.list_id,
            public.hmac(convert_to(lower(NEW.email::text), 'UTF8'), v_key, 'sha256'), v_at);
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$;
CREATE TRIGGER mailing_reporting_membership_changed AFTER INSERT OR UPDATE OR DELETE ON list_subscriber
    FOR EACH ROW EXECUTE FUNCTION mailing_reporting_membership_changed();

CREATE FUNCTION mailing_reporting_list_changed() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_at timestamptz;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.status = NEW.status AND OLD.business_id = NEW.business_id THEN
        RETURN NEW;
    END IF;
    v_at := clock_timestamp();
    IF TG_OP <> 'INSERT' AND OLD.status = 'active' THEN
        UPDATE public.mailing_reporting_list SET ended_at = GREATEST(v_at, started_at)
        WHERE list_id = OLD.id AND ended_at IS NULL;
    END IF;
    IF TG_OP <> 'DELETE' AND NEW.status = 'active' THEN
        INSERT INTO public.mailing_reporting_list (business_id, tenant_root_id, list_id, started_at)
        VALUES (NEW.business_id, NEW.tenant_root_id, NEW.id, v_at);
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$;
CREATE TRIGGER mailing_reporting_list_changed AFTER INSERT OR UPDATE OR DELETE ON mailing_list
    FOR EACH ROW EXECUTE FUNCTION mailing_reporting_list_changed();

-- Reuse the existing periodic maintenance process, not a report-time write or a
-- new worker. Closed intervals ending exactly at the start are not in that start
-- balance ([start,end)); pruning at <= cutoff is therefore safe.
CREATE FUNCTION mailing_reporting_prune() RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_count bigint; v_lists bigint; v_cutoff timestamptz := statement_timestamp() - interval '7 days';
BEGIN
    DELETE FROM public.mailing_reporting_membership
    WHERE ended_at <= v_cutoff AND public.tenant_merge_root_write_allowed(tenant_root_id);
    GET DIAGNOSTICS v_count = ROW_COUNT;
    DELETE FROM public.mailing_reporting_list
    WHERE ended_at <= v_cutoff AND public.tenant_merge_root_write_allowed(tenant_root_id);
    GET DIAGNOSTICS v_lists = ROW_COUNT;
    RETURN v_count + v_lists;
END;
$$;

-- The erasure role can remove the remaining pseudonym AFTER source membership
-- erasure. We cannot keep an exact deduplicated historical start after deleting
-- its identity: advance only affected businesses' honest baseline, atomically.
-- Account deletion alone is not mailing consent erasure; no account-email join.
CREATE FUNCTION mailing_reporting_erase(p_tenant_root_id uuid, p_email text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_identity bytea; v_at timestamptz := clock_timestamp();
BEGIN
    -- An infrequent privileged erasure must not race a re-subscription into an
    -- apparently complete but missing baseline. Source writers hold this lock
    -- before their AFTER history triggers, so the ordering is consistent.
    LOCK TABLE public.list_subscriber IN SHARE ROW EXCLUSIVE MODE;
    v_at := clock_timestamp();
    IF EXISTS (SELECT 1 FROM public.list_subscriber
               WHERE tenant_root_id = p_tenant_root_id AND lower(email::text) = lower(p_email)) THEN
        RAISE EXCEPTION 'erase mailing source memberships before reporting history' USING ERRCODE = '55000';
    END IF;
    SELECT public.hmac(convert_to(lower(p_email), 'UTF8'), identity_key, 'sha256')
    INTO STRICT v_identity FROM public.mailing_reporting_state;
    WITH erased AS (
        DELETE FROM public.mailing_reporting_membership
        WHERE tenant_root_id = p_tenant_root_id AND identity_fingerprint = v_identity
        RETURNING business_id
    )
    UPDATE public.mailing_reporting_business SET history_started_at = GREATEST(history_started_at, v_at)
    WHERE business_id IN (SELECT business_id FROM erased);
END;
$$;

REVOKE ALL ON FUNCTION mailing_reporting_business_created(), mailing_reporting_membership_changed(),
    mailing_reporting_list_changed(), mailing_reporting_prune(), mailing_reporting_erase(uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_reporting_prune() TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_reporting_erase(uuid,text) TO manyforge_erasure;
