-- 0104: feedback verified-identity tier + exactly-once submit + read path (Spec 006 / saz.5).
-- Extends the 0102 public-ingress DEFINER surface. See
-- docs/superpowers/specs/2026-07-24-feedback-verified-identity-design.md.

BEGIN;

-- 1. Columns ---------------------------------------------------------------------------
ALTER TABLE feedback_post ADD COLUMN identity_verified boolean NOT NULL DEFAULT false;
ALTER TABLE feedback_vote ADD COLUMN identity_verified boolean NOT NULL DEFAULT false;
ALTER TABLE feedback_ingest_key ADD COLUMN sealed_secret text NULL;

-- 2. Backfill legacy PUBLIC identities into the `a:` namespace, leaving INTERNAL
--    principal-UUID rows raw (votes carry no author-kind column, so principal membership is
--    the only discriminator; the a:/v: prefixes never appear in a UUID, so the three
--    namespaces are disjoint going forward). A public anon voter who supplied a real
--    principal's UUID as their identity was ALREADY a collision pre-0104, so leaving such
--    rows raw neither creates nor worsens exposure.
UPDATE feedback_vote
   SET voter_identity = 'a:' || voter_identity
 WHERE voter_identity NOT IN (SELECT id::text FROM principal);

-- feedback_post_author_chk guarantees principal posts have author_identity IS NULL, so
-- author_kind='public' is an exact, safe discriminator (no principal-UUID ambiguity).
UPDATE feedback_post
   SET author_identity = 'a:' || author_identity
 WHERE author_kind = 'public' AND author_identity IS NOT NULL;

-- 3. Consumed-set for exactly-once submit. LOCKED DOWN: RLS on, NO policies, NO app grants —
--    only the SECURITY DEFINER functions (running as owner → bypass RLS) read/write it. Its
--    idem_key values often embed customer-side user/order identifiers, so no principal path
--    may read it (unlike the 0102 feedback tables which are RLS-enabled WITH per-tenant
--    policies + app grants).
CREATE TABLE feedback_ingest_idempotency (
    key_id         uuid        NOT NULL REFERENCES feedback_ingest_key(id) ON DELETE CASCADE,
    idem_key       text        NOT NULL,   -- ALREADY tier-namespaced (a:/v:) by the DEFINER
    business_id    uuid        NOT NULL,
    tenant_root_id uuid        NOT NULL,
    body_sha256    bytea       NOT NULL,
    post_id        uuid        NULL,        -- backfilled after the post is created
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (key_id, idem_key)
);
ALTER TABLE feedback_ingest_idempotency ENABLE ROW LEVEL SECURITY;  -- no policy, no grant
CREATE TRIGGER feedback_ingest_idempotency_tenant_root_immutable
    BEFORE UPDATE ON feedback_ingest_idempotency
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- 4. DEFINER functions: DROP + CREATE (return/arg shapes change). Re-REVOKE/GRANT each. All
--    stay SECURITY DEFINER SET search_path = public and VOLATILE (a STABLE annotation would
--    freeze the claim-first snapshot below).

-- 4a. board lookup now also returns key_id + sealed_secret.
DROP FUNCTION feedback_public_board(text);
CREATE FUNCTION feedback_public_board(p_key text)
RETURNS TABLE(board_id uuid, business_id uuid, tenant_root_id uuid, is_public boolean,
              key_id uuid, sealed_secret text)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT b.id, b.business_id, b.tenant_root_id, b.is_public, k.id, k.sealed_secret
    FROM feedback_ingest_key k
    JOIN feedback_board b ON b.id = k.board_id
    WHERE k.publishable_key = p_key AND k.status = 'enabled' AND b.is_public;
$$;
REVOKE ALL ON FUNCTION feedback_public_board(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_board(text) TO manyforge_app;

-- 4b. submit: tier-prefix author identity from p_verified; exactly-once via claim-first when
--     p_idem_key non-empty. Returns (post_id, deduped); RAISEs FB409 on same-key-different-body.
DROP FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text);
CREATE FUNCTION feedback_public_submit(
    p_board_id uuid, p_business_id uuid, p_tenant_root uuid,
    p_title text, p_body text, p_author_identity text,
    p_verified boolean, p_key_id uuid, p_idem_key text, p_body_sha256 bytea
) RETURNS TABLE(post_id uuid, deduped boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_id            uuid;
    v_ik            text;
    v_rows          int;
    v_existing_post uuid;
    v_existing_hash bytea;
    v_prefix        text := CASE WHEN p_verified THEN 'v:' ELSE 'a:' END;
    v_raw_identity  text := NULLIF(btrim(coalesce(p_author_identity, '')), '');
    v_identity      text;  -- NULL author stays NULL (no bare-prefix row)
BEGIN
    IF v_raw_identity IS NOT NULL THEN
        v_identity := v_prefix || left(v_raw_identity, 200);
    END IF;

    IF NULLIF(btrim(coalesce(p_idem_key, '')), '') IS NULL THEN
        INSERT INTO feedback_post (id, business_id, tenant_root_id, board_id, title, body,
            status, vote_count, author_kind, author_identity, identity_verified, created_at, updated_at)
        VALUES (gen_random_uuid(), p_business_id, p_tenant_root, p_board_id,
            left(btrim(p_title), 300), NULLIF(btrim(coalesce(p_body, '')), ''),
            'open', 0, 'public', v_identity, p_verified, now(), now())
        RETURNING id INTO v_id;
        post_id := v_id; deduped := false; RETURN NEXT; RETURN;
    END IF;

    v_ik := v_prefix || left(p_idem_key, 255);

    INSERT INTO feedback_ingest_idempotency (key_id, idem_key, business_id, tenant_root_id, body_sha256)
    VALUES (p_key_id, v_ik, p_business_id, p_tenant_root, p_body_sha256)
    ON CONFLICT (key_id, idem_key) DO NOTHING;
    GET DIAGNOSTICS v_rows = ROW_COUNT;

    IF v_rows = 0 THEN
        -- replay. The ON CONFLICT above blocked on the PK until any concurrent first claimant
        -- committed, so this read (READ COMMITTED, fresh snapshot) sees the backfilled post_id.
        SELECT fi.post_id, fi.body_sha256 INTO v_existing_post, v_existing_hash
        FROM feedback_ingest_idempotency fi
        WHERE fi.key_id = p_key_id AND fi.idem_key = v_ik;
        IF v_existing_hash IS DISTINCT FROM p_body_sha256 THEN
            RAISE EXCEPTION 'feedback idempotency key reused with different body'
                USING ERRCODE = 'FB409';
        END IF;
        post_id := v_existing_post; deduped := true; RETURN NEXT; RETURN;
    END IF;

    -- claimed: create the post, backfill post_id (all one tx → no committed NULL post_id).
    INSERT INTO feedback_post (id, business_id, tenant_root_id, board_id, title, body,
        status, vote_count, author_kind, author_identity, identity_verified, created_at, updated_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, p_board_id,
        left(btrim(p_title), 300), NULLIF(btrim(coalesce(p_body, '')), ''),
        'open', 0, 'public', v_identity, p_verified, now(), now())
    RETURNING id INTO v_id;
    UPDATE feedback_ingest_idempotency SET post_id = v_id
        WHERE key_id = p_key_id AND idem_key = v_ik;
    post_id := v_id; deduped := false; RETURN NEXT;
END;
$$;
REVOKE ALL ON FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text,boolean,uuid,text,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text,boolean,uuid,text,bytea) TO manyforge_app;

-- 4c. vote: tier-prefix voter identity from p_verified; set the vote's identity_verified.
DROP FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text);
CREATE FUNCTION feedback_public_vote(
    p_board_id uuid, p_business_id uuid, p_tenant_root uuid,
    p_post_id uuid, p_voter_identity text, p_verified boolean
) RETURNS TABLE(accepted boolean, out_votes int)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_rows  int;
    v_count int;
    v_vid   text := (CASE WHEN p_verified THEN 'v:' ELSE 'a:' END)
                    || left(btrim(coalesce(p_voter_identity, '')), 200);
BEGIN
    PERFORM 1 FROM feedback_post
        WHERE id = p_post_id AND board_id = p_board_id AND deleted_at IS NULL;
    IF NOT FOUND THEN
        accepted := false; out_votes := NULL; RETURN NEXT; RETURN;
    END IF;

    INSERT INTO feedback_vote (id, business_id, tenant_root_id, post_id, voter_identity,
                              identity_verified, created_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, p_post_id, v_vid, p_verified, now())
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
REVOKE ALL ON FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text,boolean) TO manyforge_app;

-- 4d. list: add viewer_voted + identity_verified; optional author filter. Handler passes
--     already-namespaced p_viewer / p_author.
DROP FUNCTION feedback_public_list_posts(uuid,int);
CREATE FUNCTION feedback_public_list_posts(p_board_id uuid, p_limit int, p_viewer text, p_author text)
RETURNS TABLE(id uuid, title text, body text, status feedback_status, vote_count int,
              created_at timestamptz, viewer_voted boolean, identity_verified boolean)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT fp.id, fp.title, fp.body, fp.status, fp.vote_count, fp.created_at,
           (p_viewer IS NOT NULL AND EXISTS (
               SELECT 1 FROM feedback_vote v WHERE v.post_id = fp.id AND v.voter_identity = p_viewer)),
           fp.identity_verified
    FROM feedback_post fp
    WHERE fp.board_id = p_board_id AND fp.deleted_at IS NULL
      AND (p_author IS NULL OR fp.author_identity = p_author)
    ORDER BY fp.vote_count DESC, fp.created_at DESC, fp.id DESC
    LIMIT LEAST(GREATEST(coalesce(p_limit, 20), 1), 100);
$$;
REVOKE ALL ON FUNCTION feedback_public_list_posts(uuid,int,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_list_posts(uuid,int,text,text) TO manyforge_app;

COMMIT;
