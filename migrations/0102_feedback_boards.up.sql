-- 0102: Feedback / Feature-Request Boards (Spec 006, manyforge-saz.1).
--
-- Business-scoped tables (like the support desk 0013/0014, NOT tenant-wide like CRM 0057):
-- a board and everything under it is visible to members of THAT business, so RLS keys on
-- authorized_businesses(current_principal()). Every table carries tenant_root_id, a
-- UNIQUE (id, tenant_root_id) backing tenant-consistent composite FKs, and a
-- support_tenant_root_immutable() trigger (0013).
--
-- Public ingress (Apple/Android SDKs + a future web portal) is principal-less: it has no
-- manyforge.principal_id GUC, so its reads/writes go through the SECURITY DEFINER functions
-- at the bottom (mirroring the connector webhook, 0042). A board exposes a *publishable*
-- ingest key (feedback_ingest_key.publishable_key) — safe to embed in an app binary; it is
-- NOT a secret. The oracle boundary (Spec 006 regression contract) lives in
-- feedback_public_board(): an unknown/revoked key, or a key on a non-public board, returns
-- zero rows, so the handler answers a uniform 401 and never leaks which businesses/boards
-- exist. Server-to-server signed ingest (elevated trust) is a deferred follow-up (saz.5).

CREATE TYPE feedback_status AS ENUM ('open', 'planned', 'in_progress', 'done', 'declined');

-- ---- tables ----

CREATE TABLE feedback_board (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    slug           text NOT NULL,
    name           text NOT NULL,
    description    text,
    is_public      boolean NOT NULL DEFAULT false,   -- public portal + SDK ingest enabled
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),                      -- backs child composite FKs
    UNIQUE (business_id, slug),                       -- one slug per business
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);

CREATE TABLE feedback_post (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id         uuid NOT NULL,
    tenant_root_id      uuid NOT NULL,
    board_id            uuid NOT NULL,
    title               text NOT NULL,
    body                text,
    status              feedback_status NOT NULL DEFAULT 'open',
    vote_count          int  NOT NULL DEFAULT 0,      -- denormalized; bumped in the vote's own tx
    author_kind         text NOT NULL DEFAULT 'principal',  -- 'principal' (internal) | 'public' (SDK/portal)
    author_principal_id uuid,                          -- set for internal submissions
    author_identity     text,                          -- opaque public identity (SDK-provided), nullable
    ticket_id           uuid,                          -- set once converted to a ticket (Spec 002)
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    deleted_at          timestamptz,
    UNIQUE (id, tenant_root_id),                        -- backs feedback_vote composite FK
    FOREIGN KEY (board_id, tenant_root_id)  REFERENCES feedback_board (id, tenant_root_id),
    FOREIGN KEY (ticket_id, tenant_root_id) REFERENCES ticket (id, tenant_root_id),  -- ticket-link integrity
    FOREIGN KEY (author_principal_id) REFERENCES principal (id),
    CONSTRAINT feedback_post_author_chk CHECK (
        (author_kind = 'principal' AND author_principal_id IS NOT NULL AND author_identity IS NULL) OR
        (author_kind = 'public'    AND author_principal_id IS NULL)
    )
);
CREATE INDEX feedback_post_board_idx
    ON feedback_post (board_id, tenant_root_id) WHERE deleted_at IS NULL;
-- Backs the public "top posts" list (vote_count DESC, created_at DESC).
CREATE INDEX feedback_post_board_rank_idx
    ON feedback_post (board_id, vote_count DESC, created_at DESC) WHERE deleted_at IS NULL;

CREATE TABLE feedback_vote (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    post_id        uuid NOT NULL,
    voter_identity text NOT NULL,   -- principal uuid (internal) OR SDK-provided opaque identity
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (post_id, voter_identity),   -- VOTING INTEGRITY: one vote per identity per post
    FOREIGN KEY (post_id, tenant_root_id) REFERENCES feedback_post (id, tenant_root_id)
);

CREATE TABLE feedback_ingest_key (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    board_id        uuid NOT NULL,
    publishable_key text NOT NULL,                     -- public client key (embeddable), not a secret
    label           text,
    status          text NOT NULL DEFAULT 'enabled',   -- 'enabled' | 'revoked'
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,
    UNIQUE (id, tenant_root_id),
    UNIQUE (publishable_key),                           -- global lookup by key (like connector id)
    FOREIGN KEY (board_id, tenant_root_id) REFERENCES feedback_board (id, tenant_root_id)
);

-- ---- tenant_root_id immutability guards (reuse the generic fn from 0013) ----
CREATE TRIGGER feedback_board_troot_immutable BEFORE UPDATE ON feedback_board
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER feedback_post_troot_immutable BEFORE UPDATE ON feedback_post
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER feedback_vote_troot_immutable BEFORE UPDATE ON feedback_vote
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER feedback_ingest_key_troot_immutable BEFORE UPDATE ON feedback_ingest_key
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- ---- app-role grants ----
GRANT SELECT, INSERT, UPDATE, DELETE ON
    feedback_board, feedback_post, feedback_vote, feedback_ingest_key TO manyforge_app;

-- ---- enable RLS + business-scoped policies ----
ALTER TABLE feedback_board ENABLE ROW LEVEL SECURITY;
CREATE POLICY feedback_board_rls ON feedback_board FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE feedback_post ENABLE ROW LEVEL SECURITY;
CREATE POLICY feedback_post_rls ON feedback_post FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE feedback_vote ENABLE ROW LEVEL SECURITY;
CREATE POLICY feedback_vote_rls ON feedback_vote FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE feedback_ingest_key ENABLE ROW LEVEL SECURITY;
CREATE POLICY feedback_ingest_key_rls ON feedback_ingest_key FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

-- ======================================================================================
-- SECURITY DEFINER functions for principal-less public ingress (SDK / portal).
-- All BYPASS RLS, so each SETs search_path = public and is REVOKEd from PUBLIC then granted
-- only to manyforge_app (mirrors 0042/0024).
-- ======================================================================================

-- Auth + tenancy lookup for a publishable key. Returns a row ONLY for an enabled key on a
-- PUBLIC board — the oracle boundary. Unknown/revoked key or a private board → zero rows →
-- the handler answers a uniform 401 (no business/board existence oracle).
CREATE FUNCTION feedback_public_board(p_key text)
RETURNS TABLE(board_id uuid, business_id uuid, tenant_root_id uuid, is_public boolean)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT b.id, b.business_id, b.tenant_root_id, b.is_public
    FROM feedback_ingest_key k
    JOIN feedback_board b ON b.id = k.board_id
    WHERE k.publishable_key = p_key AND k.status = 'enabled' AND b.is_public;
$$;

-- Insert a public submission. Tenancy is passed in (already resolved via
-- feedback_public_board in the same tx), mirroring ingest_connector_webhook. Title is
-- assumed non-empty (the handler validates); capped defensively at 300 chars.
CREATE FUNCTION feedback_public_submit(
    p_board_id uuid, p_business_id uuid, p_tenant_root uuid,
    p_title text, p_body text, p_author_identity text
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_id uuid;
BEGIN
    INSERT INTO feedback_post (
        id, business_id, tenant_root_id, board_id, title, body, status, vote_count,
        author_kind, author_identity, created_at, updated_at)
    VALUES (
        gen_random_uuid(), p_business_id, p_tenant_root, p_board_id,
        left(btrim(p_title), 300), NULLIF(btrim(coalesce(p_body, '')), ''),
        'open', 0, 'public', NULLIF(btrim(coalesce(p_author_identity, '')), ''),
        now(), now())
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$;

-- Record a public vote (one per identity per post) and return the fresh count. accepted is
-- false on replay (ON CONFLICT DO NOTHING) — voting integrity holds even on this RLS-bypass
-- path. A post_id not on this board → (false, NULL) so the handler answers 404 (no oracle).
-- out_votes (not vote_count) as the OUT column so it does not shadow the feedback_post.vote_count
-- column inside the UPDATE ... SET vote_count = vote_count + 1 RETURNING vote_count (which would
-- be an ambiguous reference in plpgsql). Go selects "accepted, out_votes".
CREATE FUNCTION feedback_public_vote(
    p_board_id uuid, p_business_id uuid, p_tenant_root uuid,
    p_post_id uuid, p_voter_identity text
) RETURNS TABLE(accepted boolean, out_votes int)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_rows int; v_count int;
BEGIN
    PERFORM 1 FROM feedback_post
        WHERE id = p_post_id AND board_id = p_board_id AND deleted_at IS NULL;
    IF NOT FOUND THEN
        accepted := false; out_votes := NULL; RETURN NEXT; RETURN;
    END IF;

    INSERT INTO feedback_vote (id, business_id, tenant_root_id, post_id, voter_identity, created_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, p_post_id, p_voter_identity, now())
    ON CONFLICT (post_id, voter_identity) DO NOTHING;
    GET DIAGNOSTICS v_rows = ROW_COUNT;

    IF v_rows > 0 THEN
        UPDATE feedback_post SET vote_count = vote_count + 1, updated_at = now()
            WHERE id = p_post_id RETURNING vote_count INTO v_count;
        accepted := true;
    ELSE
        SELECT fp.vote_count INTO v_count FROM feedback_post fp WHERE fp.id = p_post_id;
        accepted := false;
    END IF;
    out_votes := v_count;
    RETURN NEXT;
END;
$$;

-- Top public posts for a board (vote_count DESC), for the SDK/portal to render. board_id is
-- already the authenticated scope; limit is clamped to [1,100].
CREATE FUNCTION feedback_public_list_posts(p_board_id uuid, p_limit int)
RETURNS TABLE(id uuid, title text, body text, status feedback_status, vote_count int, created_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT fp.id, fp.title, fp.body, fp.status, fp.vote_count, fp.created_at
    FROM feedback_post fp
    WHERE fp.board_id = p_board_id AND fp.deleted_at IS NULL
    ORDER BY fp.vote_count DESC, fp.created_at DESC, fp.id DESC
    LIMIT LEAST(GREATEST(coalesce(p_limit, 20), 1), 100);
$$;

-- Convert a feedback post to a support ticket (Spec 002). Idempotent: if already linked,
-- returns the existing ticket_id. Invoked from the AUTHENTICATED service AFTER an RLS-bound
-- fetch has confirmed the caller can see the post, so the passed (business_id, tenant_root)
-- are trusted. Creates a synthetic requester keyed on the post id (public authors have no
-- real email), then the ticket, then links feedback_post.ticket_id — all in one tx.
CREATE FUNCTION convert_feedback_post_to_ticket(
    p_post_id uuid, p_business_id uuid, p_tenant_root uuid
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_existing uuid; v_title text; v_requester uuid; v_ticket uuid;
        v_email citext := ('feedback+' || p_post_id::text || '@feedback.local')::citext;
BEGIN
    SELECT ticket_id, title INTO v_existing, v_title
    FROM feedback_post
    WHERE id = p_post_id AND business_id = p_business_id
      AND tenant_root_id = p_tenant_root AND deleted_at IS NULL;
    IF v_title IS NULL THEN
        RAISE EXCEPTION 'feedback post % not found in business %', p_post_id, p_business_id;
    END IF;
    IF v_existing IS NOT NULL THEN
        RETURN v_existing;  -- idempotent
    END IF;

    INSERT INTO requester (id, business_id, tenant_root_id, email, display_name,
                           first_seen_at, last_seen_at, created_at, updated_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, v_email, 'Feedback Author',
            now(), now(), now(), now())
    ON CONFLICT (tenant_root_id, email) DO UPDATE
        SET last_seen_at = now(), updated_at = now()
    RETURNING id INTO v_requester;

    INSERT INTO ticket (id, business_id, tenant_root_id, requester_id, subject, status, priority,
                        reply_token, last_message_at, created_at, updated_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, v_requester,
            left(v_title, 300), 'open', 'normal',
            'fb:' || p_post_id::text, now(), now(), now())
    RETURNING id INTO v_ticket;

    UPDATE feedback_post SET ticket_id = v_ticket, updated_at = now() WHERE id = p_post_id;
    RETURN v_ticket;
END;
$$;

REVOKE ALL ON FUNCTION feedback_public_board(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_board(text) TO manyforge_app;
REVOKE ALL ON FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text) TO manyforge_app;
REVOKE ALL ON FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text) TO manyforge_app;
REVOKE ALL ON FUNCTION feedback_public_list_posts(uuid,int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_list_posts(uuid,int) TO manyforge_app;
REVOKE ALL ON FUNCTION convert_feedback_post_to_ticket(uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION convert_feedback_post_to_ticket(uuid,uuid,uuid) TO manyforge_app;
