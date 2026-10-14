-- 0145: Work Board (Spec 018, manyforge-jncc) — a light beads replacement.
--
-- Business-scoped tables (like feedback 0102 / mailing_brand 0138): a work project and its
-- tasks/tokens are visible to members of THAT business, so RLS keys on
-- authorized_businesses(current_principal()). Every table carries tenant_root_id, a
-- UNIQUE (id, tenant_root_id) backing tenant-consistent composite FKs, and a
-- support_tenant_root_immutable() trigger.
--
-- work_task.rev is drawn from one global sequence on every insert/update and is the `mf sync`
-- pull cursor. Writers lock the parent work_project row FOR UPDATE before touching its tasks,
-- so within a project rev order equals commit order (a reader can never observe rev N+1
-- committed while rev N is still in flight).
--
-- `mf` authenticates with a project-scoped work token (mfw_…), stored only as a SHA-256 hex
-- hash. Token resolution is principal-less, so it goes through the SECURITY DEFINER
-- work_resolve_token(); the request then runs WithPrincipal(issuer) so RLS and work.write are
-- re-checked on every sync.

CREATE SEQUENCE work_task_rev_seq;

CREATE TABLE work_project (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    slug           text NOT NULL,
    name           text NOT NULL,
    key_prefix     text NOT NULL,
    created_by     uuid REFERENCES principal (id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    CONSTRAINT work_project_slug_uniq UNIQUE (business_id, slug),
    CONSTRAINT work_project_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT work_project_slug_chk CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT work_project_key_prefix_chk CHECK (key_prefix ~ '^[a-z][a-z0-9]{0,11}$'),
    CONSTRAINT work_project_name_chk CHECK (length(name) BETWEEN 1 AND 200)
);
CREATE INDEX work_project_tenant_idx ON work_project (business_id, tenant_root_id);

CREATE TABLE work_task (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    project_id           uuid NOT NULL,
    key                  text NOT NULL,
    title                text NOT NULL,
    description          text NOT NULL DEFAULT '',
    notes                text NOT NULL DEFAULT '',
    lane                 text NOT NULL DEFAULT 'agent',
    status               text NOT NULL DEFAULT 'todo',
    priority             smallint NOT NULL DEFAULT 2,
    type                 text NOT NULL DEFAULT 'task',
    assignee             text NOT NULL DEFAULT '',
    labels               text[] NOT NULL DEFAULT '{}',
    blocked_by           text[] NOT NULL DEFAULT '{}',
    rev                  bigint NOT NULL,
    created_by_principal uuid REFERENCES principal (id) ON DELETE SET NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    closed_at            timestamptz,
    UNIQUE (id, tenant_root_id),
    CONSTRAINT work_task_key_uniq UNIQUE (project_id, key),
    CONSTRAINT work_task_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT work_task_project_fk
        FOREIGN KEY (project_id, tenant_root_id)
        REFERENCES work_project (id, tenant_root_id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT work_task_key_chk CHECK (key ~ '^[a-z][a-z0-9]{0,11}-[a-z0-9]{2,16}(\.[0-9]{1,4}){0,3}$'),
    CONSTRAINT work_task_title_chk CHECK (length(title) BETWEEN 1 AND 500),
    CONSTRAINT work_task_description_len_chk CHECK (length(description) <= 20000),
    CONSTRAINT work_task_notes_len_chk CHECK (length(notes) <= 200000),
    CONSTRAINT work_task_assignee_len_chk CHECK (length(assignee) <= 200),
    CONSTRAINT work_task_lane_chk CHECK (lane IN ('agent', 'operator')),
    CONSTRAINT work_task_status_chk CHECK (status IN ('backlog', 'todo', 'in_progress', 'blocked', 'done', 'canceled')),
    CONSTRAINT work_task_priority_chk CHECK (priority BETWEEN 0 AND 4),
    CONSTRAINT work_task_type_chk CHECK (type IN ('task', 'bug', 'feature', 'chore')),
    CONSTRAINT work_task_labels_chk CHECK (cardinality(labels) <= 50),
    CONSTRAINT work_task_blocked_by_chk CHECK (cardinality(blocked_by) <= 100)
);
CREATE INDEX work_task_project_rev_idx ON work_task (project_id, rev);
CREATE INDEX work_task_lane_status_idx ON work_task (business_id, lane, status);

CREATE TABLE work_token (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    project_id     uuid NOT NULL,
    name           text NOT NULL,
    token_hash     text NOT NULL,
    token_prefix   text NOT NULL,
    principal_id   uuid NOT NULL REFERENCES principal (id) ON DELETE CASCADE,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_used_at   timestamptz,
    revoked_at     timestamptz,
    UNIQUE (id, tenant_root_id),
    CONSTRAINT work_token_hash_uniq UNIQUE (token_hash),
    CONSTRAINT work_token_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT work_token_project_fk
        FOREIGN KEY (project_id, tenant_root_id)
        REFERENCES work_project (id, tenant_root_id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT work_token_name_chk CHECK (length(name) BETWEEN 1 AND 100),
    CONSTRAINT work_token_hash_chk CHECK (token_hash ~ '^[0-9a-f]{64}$')
);
CREATE INDEX work_token_project_idx ON work_token (project_id, tenant_root_id);

-- ---- tenant_root_id immutability guards ----
CREATE TRIGGER work_project_troot_immutable BEFORE UPDATE ON work_project
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER work_task_troot_immutable BEFORE UPDATE ON work_task
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER work_token_troot_immutable BEFORE UPDATE ON work_token
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- ---- app-role grants ----
GRANT SELECT, INSERT, UPDATE, DELETE ON work_project, work_task, work_token TO manyforge_app;
GRANT USAGE ON SEQUENCE work_task_rev_seq TO manyforge_app;

-- ---- RLS: business-scoped ----
ALTER TABLE work_project ENABLE ROW LEVEL SECURITY;
CREATE POLICY work_project_rls ON work_project FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE work_task ENABLE ROW LEVEL SECURITY;
CREATE POLICY work_task_rls ON work_task FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE work_token ENABLE ROW LEVEL SECURITY;
CREATE POLICY work_token_rls ON work_token FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

-- ---- tenant merge classification + write fence ----
INSERT INTO tenant_merge_manifest (table_name, module, strategy, inventory_version) VALUES
    ('work_project', 'work', 'drain_fence_then_rewrite', 1),
    ('work_task', 'work', 'drain_fence_then_rewrite', 1),
    ('work_token', 'work', 'drain_fence_then_rewrite', 1);

CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON work_project
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON work_task
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON work_token
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();

-- Principal-less token resolution for POST /api/v1/work/sync. Returns one row only for a
-- non-revoked token whose project still exists; unknown/revoked → zero rows → uniform 401.
-- The lookup is by the SHA-256 hash (unique index), never by plaintext. last_used_at is
-- refreshed at most once a minute so a chatty agent does not rewrite the row on every sync.
-- Every column reference is qualified: the OUT names shadow work_token's columns.
CREATE FUNCTION work_resolve_token(p_hash text)
RETURNS TABLE(token_id uuid, project_id uuid, business_id uuid, tenant_root_id uuid, principal_id uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE v_last timestamptz;
BEGIN
    SELECT t.id, t.project_id, t.business_id, t.tenant_root_id, t.principal_id, t.last_used_at
      INTO token_id, project_id, business_id, tenant_root_id, principal_id, v_last
      FROM work_token t
      JOIN work_project p ON p.id = t.project_id AND p.tenant_root_id = t.tenant_root_id
     WHERE t.token_hash = p_hash AND t.revoked_at IS NULL;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF v_last IS NULL OR v_last < now() - interval '1 minute' THEN
        UPDATE work_token w SET last_used_at = now() WHERE w.id = token_id;
    END IF;
    RETURN NEXT;
END;
$$;

REVOKE ALL ON FUNCTION work_resolve_token(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION work_resolve_token(text) TO manyforge_app;
