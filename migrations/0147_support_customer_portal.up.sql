-- Customer portal credentials and opaque ownership. Email is contact data, never authority.
CREATE TABLE IF NOT EXISTS support_portal_key (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), business_id uuid NOT NULL, tenant_root_id uuid NOT NULL,
 creator_principal_id uuid NOT NULL REFERENCES principal(id), label text NOT NULL CHECK(length(label) BETWEEN 1 AND 100 AND octet_length(label)<=400 AND label !~ '[[:cntrl:]]'),
 token_hash bytea NOT NULL UNIQUE CHECK(octet_length(token_hash)=32),
 created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL, revoked_at timestamptz,
 UNIQUE(id,business_id,tenant_root_id),
 CONSTRAINT portal_key_business_fk FOREIGN KEY(business_id,tenant_root_id) REFERENCES business(id,tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
 CHECK(expires_at > created_at AND expires_at <= created_at + interval '366 days')
);
CREATE INDEX IF NOT EXISTS support_portal_key_business_idx ON support_portal_key(business_id,expires_at) WHERE revoked_at IS NULL;
CREATE TABLE IF NOT EXISTS support_portal_contact (
 business_id uuid NOT NULL, tenant_root_id uuid NOT NULL, external_id text NOT NULL CHECK(external_id ~ '^tph:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
 display_name text NOT NULL CHECK(length(display_name)<=100 AND octet_length(display_name)<=400 AND display_name !~ '[[:cntrl:]]'),
 verified_email text, requester_id uuid,
 pending_email text, challenge_hash bytea, challenge_nonce uuid, challenge_key_id uuid,
 challenge_expires_at timestamptz, attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 6),
 send_nonce uuid, last_sent_at timestamptz, send_times timestamptz[] NOT NULL DEFAULT '{}' CHECK(cardinality(send_times)<=5),
 PRIMARY KEY(business_id,external_id), UNIQUE(business_id,tenant_root_id,external_id),
 CONSTRAINT portal_contact_business_fk FOREIGN KEY(business_id,tenant_root_id) REFERENCES business(id,tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
 CONSTRAINT portal_contact_requester_fk FOREIGN KEY(requester_id,tenant_root_id) REFERENCES requester(id,tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
 CONSTRAINT portal_contact_key_fk FOREIGN KEY(challenge_key_id,business_id,tenant_root_id) REFERENCES support_portal_key(id,business_id,tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
 CHECK((verified_email IS NULL)=(requester_id IS NULL)),
 CHECK(verified_email IS NULL OR octet_length(verified_email)<=254),
 CHECK(pending_email IS NULL OR octet_length(pending_email)<=254)
);
CREATE TABLE IF NOT EXISTS support_portal_ticket (
 ticket_id uuid PRIMARY KEY, business_id uuid NOT NULL, tenant_root_id uuid NOT NULL, external_id text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(ticket_id,business_id,tenant_root_id),
 CONSTRAINT portal_ticket_native_fk FOREIGN KEY(ticket_id,tenant_root_id) REFERENCES ticket(id,tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
 CONSTRAINT portal_ticket_contact_fk FOREIGN KEY(business_id,tenant_root_id,external_id) REFERENCES support_portal_contact(business_id,tenant_root_id,external_id) DEFERRABLE INITIALLY IMMEDIATE,
 CONSTRAINT portal_ticket_business_fk FOREIGN KEY(business_id,tenant_root_id) REFERENCES business(id,tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE
);
CREATE INDEX IF NOT EXISTS support_portal_ticket_owner_idx ON support_portal_ticket(business_id,external_id,created_at DESC);
CREATE TABLE IF NOT EXISTS support_portal_request (
 business_id uuid NOT NULL, tenant_root_id uuid NOT NULL, external_id text NOT NULL, request_id uuid NOT NULL,
 body_hash bytea NOT NULL CHECK(octet_length(body_hash)=32), ticket_id uuid NOT NULL, message_id uuid NOT NULL,
 PRIMARY KEY(business_id,external_id,request_id),
 CONSTRAINT portal_request_contact_fk FOREIGN KEY(business_id,tenant_root_id,external_id) REFERENCES support_portal_contact(business_id,tenant_root_id,external_id) DEFERRABLE INITIALLY IMMEDIATE,
 CONSTRAINT portal_request_ticket_fk FOREIGN KEY(ticket_id,business_id,tenant_root_id) REFERENCES support_portal_ticket(ticket_id,business_id,tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
 CONSTRAINT portal_request_message_fk FOREIGN KEY(message_id,tenant_root_id) REFERENCES ticket_message(id,tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE
);
-- Cross-business parents must agree, not merely share a tenant root.
CREATE OR REPLACE FUNCTION support_portal_parent_scope() RETURNS trigger LANGUAGE plpgsql SET search_path=public,pg_temp AS $$
BEGIN
 -- Only the owner-executed, root-bound cutover may temporarily defer parent
 -- root agreement. Composite FKs revalidate every relationship at its commit.
 IF TG_OP='UPDATE' THEN
  IF NEW.tenant_root_id IS DISTINCT FROM OLD.tenant_root_id THEN
   IF tenant_merge_root_rewrite_allowed(TG_RELID,OLD.tenant_root_id,NEW.tenant_root_id) THEN
    RETURN NEW;
   END IF;
  END IF;
 END IF;
 IF TG_TABLE_NAME='support_portal_ticket'  THEN
  IF NOT EXISTS(SELECT 1 FROM ticket WHERE id=NEW.ticket_id AND business_id=NEW.business_id AND tenant_root_id=NEW.tenant_root_id) THEN RAISE EXCEPTION 'portal parent scope violation'; END IF;
 ELSIF TG_TABLE_NAME='support_portal_request' THEN
  IF NOT EXISTS(SELECT 1 FROM ticket_message WHERE id=NEW.message_id AND ticket_id=NEW.ticket_id AND business_id=NEW.business_id AND tenant_root_id=NEW.tenant_root_id) THEN RAISE EXCEPTION 'portal parent scope violation'; END IF;
 END IF;
 RETURN NEW;
END; $$;
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['support_portal_key','support_portal_contact','support_portal_ticket','support_portal_request'] LOOP
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('DROP POLICY IF EXISTS portal_scope ON %I',t);
  EXECUTE format('CREATE POLICY %I ON %I FOR ALL USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal()))) WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))',t||'_rls',t);
  EXECUTE format('DROP TRIGGER IF EXISTS portal_root_immutable ON %I',t);
  EXECUTE format('CREATE TRIGGER portal_root_immutable BEFORE UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable()',t);
  IF t IN ('support_portal_ticket','support_portal_request') THEN
   EXECUTE format('DROP TRIGGER IF EXISTS portal_parent_scope ON %I',t);
   EXECUTE format('CREATE TRIGGER portal_parent_scope BEFORE INSERT OR UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION support_portal_parent_scope()',t);
  END IF;
 END LOOP;
END $$;
GRANT SELECT,INSERT,UPDATE,DELETE ON support_portal_key,support_portal_contact,support_portal_ticket,support_portal_request TO manyforge_app;
CREATE OR REPLACE FUNCTION resolve_support_portal_key(p_hash bytea)
 RETURNS TABLE(id uuid,business_id uuid,tenant_root_id uuid,creator_principal_id uuid,expires_at timestamptz)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=public,pg_temp AS $$
 SELECT k.id,k.business_id,k.tenant_root_id,k.creator_principal_id,k.expires_at
 FROM support_portal_key k JOIN business b ON b.id=k.business_id AND b.tenant_root_id=k.tenant_root_id
 JOIN principal p ON p.id=k.creator_principal_id AND p.kind='human' JOIN account a ON a.id=p.account_id
 WHERE a.status='active' AND a.deleted_at IS NULL AND b.deleted_at IS NULL AND octet_length(p_hash)=32 AND k.token_hash=p_hash AND k.revoked_at IS NULL AND k.expires_at>now() AND b.status='active' LIMIT 1;
$$;
REVOKE ALL ON FUNCTION resolve_support_portal_key(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION resolve_support_portal_key(bytea) TO manyforge_app;
-- Native requesters are tenant-root unique, including across sibling businesses.
-- Resolve only the notification UUID after proof; do not expose or mutate metadata.
CREATE OR REPLACE FUNCTION resolve_support_portal_requester(p_business uuid,p_root uuid,p_email text,p_name text)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=public,pg_temp AS $$
DECLARE v_id uuid;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM business b WHERE b.id=p_business AND b.tenant_root_id=p_root AND b.id IN(SELECT business_id FROM authorized_businesses(current_principal())))
 OR octet_length(p_email)>254 OR p_email !~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$' OR length(p_name)>100 THEN
  RAISE EXCEPTION 'portal requester scope violation';
 END IF;
 INSERT INTO requester(business_id,tenant_root_id,email,display_name) VALUES(p_business,p_root,p_email,p_name)
 ON CONFLICT(tenant_root_id,email) DO NOTHING;
 SELECT id INTO STRICT v_id FROM requester WHERE tenant_root_id=p_root AND email=p_email::citext;
 RETURN v_id;
END; $$;
REVOKE ALL ON FUNCTION resolve_support_portal_requester(uuid,uuid,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION resolve_support_portal_requester(uuid,uuid,text,text) TO manyforge_app;

-- Preserve portal ownership and credential IDs through the existing fenced,
-- lossless whole-tenant cutover. Business IDs/external customer IDs do not move.
INSERT INTO tenant_merge_manifest(table_name,module,strategy,inventory_version) VALUES
 ('support_portal_key','support','drain_fence_then_rewrite',1),
 ('support_portal_contact','support','drain_fence_then_rewrite',1),
 ('support_portal_ticket','support','drain_fence_then_rewrite',1),
 ('support_portal_request','support','drain_fence_then_rewrite',1);
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['support_portal_key','support_portal_contact','support_portal_ticket','support_portal_request'] LOOP
  EXECUTE format('CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence()',t);
 END LOOP;
END $$;
