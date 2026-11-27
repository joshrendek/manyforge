-- Business-bound confirmation for custom portal hostnames. A confirmation token
-- presented on a business's own hostname may only confirm a subscriber of THAT
-- business. When the pending subscription belongs to another business (or there is
-- none) nothing changes and 0 is returned, so the token stays usable and the caller
-- cannot tell the cases apart. Otherwise the existing mailing_confirm does the work.
CREATE FUNCTION mailing_confirm_for_business(p_token_hash bytea, p_business_id uuid)
RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    v_owner uuid;
BEGIN
    SELECT s.business_id INTO v_owner
      FROM list_subscriber s
     WHERE s.confirm_token_hash = p_token_hash
       AND s.status = 'pending';
    IF v_owner IS NULL OR v_owner IS DISTINCT FROM p_business_id THEN
        RETURN 0;
    END IF;
    RETURN mailing_confirm(p_token_hash);
END;
$$;
REVOKE ALL ON FUNCTION mailing_confirm_for_business(bytea, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_confirm_for_business(bytea, uuid) TO manyforge_app;
