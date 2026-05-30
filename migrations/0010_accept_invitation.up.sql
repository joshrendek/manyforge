-- Invitation acceptance is RLS-exempt: the invitee is not yet a member of the
-- target tenant, so the invitation/membership RLS policies (keyed on the caller's
-- authorized businesses via current_principal()) would hide the invitation and
-- block materialising the membership. This SECURITY DEFINER function performs the
-- atomic, trusted accept — verify token + email, enforce single-use under
-- FOR UPDATE, and on success mark the invitation accepted, create the membership,
-- and write the audit entry. The caller's identity (authenticated principal +
-- verified account whose email is passed in) is established in the service BEFORE
-- calling this; the function returns a status the service maps to an HTTP code.
-- Unknown tokens return 'gone' (not a distinct code) so the token is not an oracle.
-- OUT columns are prefixed (out_*) so bare references to invitation columns like
-- `status` inside the body are never ambiguous with the result columns (42702).
CREATE FUNCTION accept_invitation(p_token_hash text, p_principal uuid, p_email citext)
RETURNS TABLE (out_status text, out_business uuid, out_role uuid)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    inv invitation;
    biz business;
BEGIN
    SELECT * INTO inv FROM invitation WHERE token_hash = p_token_hash FOR UPDATE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'gone'::text, NULL::uuid, NULL::uuid;
        RETURN;
    END IF;
    IF inv.email <> p_email THEN
        RETURN QUERY SELECT 'email_mismatch'::text, NULL::uuid, NULL::uuid;
        RETURN;
    END IF;
    IF inv.status <> 'pending' OR inv.expires_at <= now() THEN
        RETURN QUERY SELECT 'gone'::text, NULL::uuid, NULL::uuid;
        RETURN;
    END IF;
    SELECT * INTO biz FROM business WHERE id = inv.business_id AND deleted_at IS NULL;
    IF NOT FOUND OR biz.status = 'archived' THEN
        RETURN QUERY SELECT 'gone'::text, NULL::uuid, NULL::uuid;
        RETURN;
    END IF;

    -- Single-use: only one transaction can flip pending -> accepted.
    UPDATE invitation SET status = 'accepted', accepted_at = now()
        WHERE id = inv.id AND status = 'pending';
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'gone'::text, NULL::uuid, NULL::uuid;
        RETURN;
    END IF;

    -- Materialise the membership (idempotent: an existing member keeps their role).
    INSERT INTO membership (principal_id, business_id, tenant_root_id, role_id, granted_by)
        VALUES (p_principal, inv.business_id, inv.tenant_root_id, inv.role_id, inv.created_by)
        ON CONFLICT (principal_id, business_id) DO NOTHING;

    INSERT INTO audit_entry (id, business_id, tenant_root_id, actor_principal_id, action, target_type, target_id, new_value)
        VALUES (gen_random_uuid(), inv.business_id, inv.tenant_root_id, p_principal, 'invitation.accepted', 'membership', p_principal,
                jsonb_build_object('invitation_id', inv.id, 'role_id', inv.role_id));

    RETURN QUERY SELECT 'ok'::text, inv.business_id, inv.role_id;
END;
$$;

REVOKE ALL ON FUNCTION accept_invitation(text, uuid, citext) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION accept_invitation(text, uuid, citext) TO manyforge_app;
