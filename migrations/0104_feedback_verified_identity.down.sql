-- Recreate original 0102 function signatures verbatim.
DROP FUNCTION feedback_public_list_posts(uuid,int,text,text);
CREATE FUNCTION feedback_public_list_posts(p_board_id uuid, p_limit int)
RETURNS TABLE(id uuid, title text, body text, status feedback_status, vote_count int, created_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT fp.id, fp.title, fp.body, fp.status, fp.vote_count, fp.created_at
    FROM feedback_post fp
    WHERE fp.board_id = p_board_id AND fp.deleted_at IS NULL
    ORDER BY fp.vote_count DESC, fp.created_at DESC, fp.id DESC
    LIMIT LEAST(GREATEST(coalesce(p_limit, 20), 1), 100);
$$;
REVOKE ALL ON FUNCTION feedback_public_list_posts(uuid,int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_list_posts(uuid,int) TO manyforge_app;

DROP FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text,boolean);
CREATE FUNCTION feedback_public_vote(
    p_board_id uuid, p_business_id uuid, p_tenant_root uuid, p_post_id uuid, p_voter_identity text
) RETURNS TABLE(accepted boolean, out_votes int)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_rows int; v_count int;
BEGIN
    PERFORM 1 FROM feedback_post WHERE id = p_post_id AND board_id = p_board_id AND deleted_at IS NULL;
    IF NOT FOUND THEN accepted := false; out_votes := NULL; RETURN NEXT; RETURN; END IF;
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
    out_votes := v_count; RETURN NEXT;
END;
$$;
REVOKE ALL ON FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text) TO manyforge_app;

DROP FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text,boolean,uuid,text,bytea);
CREATE FUNCTION feedback_public_submit(
    p_board_id uuid, p_business_id uuid, p_tenant_root uuid, p_title text, p_body text, p_author_identity text
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_id uuid;
BEGIN
    INSERT INTO feedback_post (id, business_id, tenant_root_id, board_id, title, body, status,
        vote_count, author_kind, author_identity, created_at, updated_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, p_board_id,
        left(btrim(p_title), 300), NULLIF(btrim(coalesce(p_body, '')), ''),
        'open', 0, 'public', NULLIF(btrim(coalesce(p_author_identity, '')), ''), now(), now())
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$;
REVOKE ALL ON FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text) TO manyforge_app;

DROP FUNCTION feedback_public_board(text);
CREATE FUNCTION feedback_public_board(p_key text)
RETURNS TABLE(board_id uuid, business_id uuid, tenant_root_id uuid, is_public boolean)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT b.id, b.business_id, b.tenant_root_id, b.is_public
    FROM feedback_ingest_key k JOIN feedback_board b ON b.id = k.board_id
    WHERE k.publishable_key = p_key AND k.status = 'enabled' AND b.is_public;
$$;
REVOKE ALL ON FUNCTION feedback_public_board(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_board(text) TO manyforge_app;

-- Strip the a:/v: identity prefixes (best-effort; a legacy raw identity that genuinely began
-- 'a:'/'v:' would be over-stripped — vanishingly unlikely, acceptable for a down path).
--
-- Rollback is lossy where one raw identity voted in BOTH tiers on a post: drop the anon
-- duplicate first so stripping the prefix can't violate UNIQUE (post_id, voter_identity).
DELETE FROM feedback_vote a
 WHERE a.voter_identity LIKE 'a:%'
   AND EXISTS (SELECT 1 FROM feedback_vote v
               WHERE v.post_id = a.post_id
                 AND v.voter_identity = 'v:' || substring(a.voter_identity from 3));
UPDATE feedback_vote SET voter_identity = substring(voter_identity from 3)
 WHERE voter_identity LIKE 'a:%' OR voter_identity LIKE 'v:%';
UPDATE feedback_post SET author_identity = substring(author_identity from 3)
 WHERE author_identity LIKE 'a:%' OR author_identity LIKE 'v:%';

DROP TABLE feedback_ingest_idempotency;
ALTER TABLE feedback_ingest_key DROP COLUMN sealed_secret;
ALTER TABLE feedback_vote DROP COLUMN identity_verified;
ALTER TABLE feedback_post DROP COLUMN identity_verified;
