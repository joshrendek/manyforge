-- Native Support Desk (spec 002) — schema.
-- Seven tenant-owned tables, each carrying business_id + immutable tenant_root_id
-- and a composite FK (business_id, tenant_root_id) -> business(id, tenant_root_id)
-- so "same tenant" is a database invariant (mirrors spec 001). RLS, the ingestion
-- SECURITY DEFINER function, and the permission catalog land in 0014/0015; the
-- outbox/notification infra in 0016. Indexes lead with the scope column so the
-- app predicate AND the RLS EXISTS both ride the same index (SC-010).

-- ---- enums (data-model.md §Enums) ----
CREATE TYPE inbound_address_kind       AS ENUM ('system', 'custom');
CREATE TYPE email_domain_mode          AS ENUM ('forward_in', 'subdomain_mx', 'provider_route');
CREATE TYPE email_domain_spf_state     AS ENUM ('unknown', 'pending', 'pass', 'fail');
CREATE TYPE ticket_status              AS ENUM ('new', 'open', 'pending', 'solved', 'closed');
CREATE TYPE ticket_priority            AS ENUM ('low', 'normal', 'high', 'urgent');
CREATE TYPE ticket_message_direction   AS ENUM ('inbound', 'outbound', 'note');

-- ---- shared tenant_root_id immutability guard (reused by every table below) ----
CREATE FUNCTION support_tenant_root_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.tenant_root_id <> OLD.tenant_root_id THEN
        RAISE EXCEPTION 'tenant_root_id is immutable';
    END IF;
    RETURN NEW;
END;
$$;

-- ---- email_domain — custom domain / sending identity (FR-012/FR-013) ----
CREATE TABLE email_domain (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    domain               citext NOT NULL,
    mode                 email_domain_mode NOT NULL,
    verify_token         text NOT NULL,
    verified_at          timestamptz,
    dkim_selector        text,
    dkim_public_key      text,
    dkim_private_key_ref text,                          -- opaque secret-store ref; never the raw key
    spf_state            email_domain_spf_state NOT NULL DEFAULT 'unknown',
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_root_id, domain),
    UNIQUE (id, tenant_root_id),                        -- backs inbound_address composite FK
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    CHECK (verified_at IS NULL OR verify_token IS NOT NULL)
);
CREATE INDEX email_domain_business_idx   ON email_domain (business_id, tenant_root_id);
CREATE INDEX email_domain_unverified_idx ON email_domain (verified_at) WHERE verified_at IS NULL;
CREATE TRIGGER email_domain_troot_immutable BEFORE UPDATE ON email_domain
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- ---- inbound_address — recipient -> business routing (FR-001/FR-003) ----
CREATE TABLE inbound_address (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    address         citext NOT NULL,                    -- normalized; plus/VERP token stripped before store
    kind            inbound_address_kind NOT NULL,
    email_domain_id uuid,                               -- NULL for system; the verified domain for custom
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_root_id, address),                   -- dedup + the resolution lookup key
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (email_domain_id, tenant_root_id) REFERENCES email_domain (id, tenant_root_id),
    CHECK ((kind = 'system' AND email_domain_id IS NULL)
        OR (kind = 'custom' AND email_domain_id IS NOT NULL))
);
CREATE INDEX inbound_address_business_idx ON inbound_address (business_id, tenant_root_id);
CREATE INDEX inbound_address_domain_idx   ON inbound_address (email_domain_id);
CREATE TRIGGER inbound_address_troot_immutable BEFORE UPDATE ON inbound_address
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- ---- requester — external sender, tenant-scoped, deduped by email (FR-006) ----
CREATE TABLE requester (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id    uuid NOT NULL,                       -- origin business; tickets carry their own business_id
    tenant_root_id uuid NOT NULL,
    email          citext NOT NULL,
    display_name   text,
    contact_id     uuid,                                -- CRM seam (spec 005): nullable, NO FK yet
    first_seen_at  timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz NOT NULL DEFAULT now(),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_root_id, email),                     -- dedup within the tenant (never across tenants)
    UNIQUE (id, tenant_root_id),                        -- backs ticket.requester_id composite FK
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX requester_business_idx ON requester (business_id, tenant_root_id);
CREATE INDEX requester_contact_idx  ON requester (contact_id) WHERE contact_id IS NOT NULL;
CREATE TRIGGER requester_troot_immutable BEFORE UPDATE ON requester
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- ---- ticket — support conversation, business-scoped (FR-004/FR-010/FR-011) ----
CREATE TABLE ticket (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id           uuid NOT NULL,
    tenant_root_id        uuid NOT NULL,
    requester_id          uuid NOT NULL,
    subject               text NOT NULL,                -- normalized empty-string, never NULL
    status                ticket_status   NOT NULL DEFAULT 'new',
    priority              ticket_priority NOT NULL DEFAULT 'normal',
    assignee_principal_id uuid,                          -- eligibility checked in SQL before persist (FR-011)
    reply_token           text NOT NULL,                -- HMAC VERP/plus-address token for threading fallback
    last_message_at       timestamptz NOT NULL DEFAULT now(),  -- denormalized; updated in-tx w/ each message
    redacted_at           timestamptz,                  -- soft-delete/redact (tickets.delete), DB-only
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),                        -- backs child composite FKs
    UNIQUE (tenant_root_id, reply_token),              -- threading-fallback lookup
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (requester_id, tenant_root_id) REFERENCES requester (id, tenant_root_id),
    FOREIGN KEY (assignee_principal_id) REFERENCES principal (id)  -- eligibility is a service+SQL check, not the FK
);
-- SC-010 default inbox list: a business's active tickets, status-filtered, newest activity first.
CREATE INDEX ticket_list_idx ON ticket (business_id, status, last_message_at DESC)
    WHERE redacted_at IS NULL;
CREATE INDEX ticket_business_idx ON ticket (business_id, tenant_root_id);
CREATE INDEX ticket_requester_idx ON ticket (requester_id);
CREATE INDEX ticket_assignee_idx ON ticket (assignee_principal_id) WHERE assignee_principal_id IS NOT NULL;
CREATE TRIGGER ticket_troot_immutable BEFORE UPDATE ON ticket
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- ---- ticket_tag — free-form tags (FR-011) ----
CREATE TABLE ticket_tag (
    ticket_id      uuid NOT NULL,
    tag            citext NOT NULL,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ticket_id, tag),                       -- idempotent tagging
    FOREIGN KEY (ticket_id, tenant_root_id) REFERENCES ticket (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX ticket_tag_facet_idx  ON ticket_tag (business_id, tag);  -- "tickets with tag X in this business"
CREATE INDEX ticket_tag_tenant_idx ON ticket_tag (tenant_root_id);
CREATE TRIGGER ticket_tag_troot_immutable BEFORE UPDATE ON ticket_tag
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- ---- ticket_message — one entry in a thread (FR-004/FR-005/FR-008/FR-009) ----
CREATE TABLE ticket_message (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id           uuid NOT NULL,
    business_id         uuid NOT NULL,
    tenant_root_id      uuid NOT NULL,
    direction           ticket_message_direction NOT NULL,
    author_principal_id uuid,                            -- NULL for inbound; set for outbound/note
    message_id          text NOT NULL,                  -- RFC822 Message-ID (synthetic if header-less)
    in_reply_to         text,
    "references"        text[] NOT NULL DEFAULT '{}',   -- RFC822 References chain (reserved word, quoted)
    body_text           text,
    body_html           text,                           -- sanitized before store
    auth_results        jsonb,                          -- SPF/DKIM/DMARC for inbound (FR-019; flagged, not rejected)
    is_auto_reply       boolean NOT NULL DEFAULT false, -- loop guard (FR-018)
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_root_id, message_id),               -- idempotency (FR-005) + threading lookup
    UNIQUE (id, tenant_root_id),                        -- backs attachment composite FK
    FOREIGN KEY (ticket_id, tenant_root_id) REFERENCES ticket (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (author_principal_id) REFERENCES principal (id),
    CHECK ((direction = 'inbound'  AND author_principal_id IS NULL)
        OR (direction IN ('outbound', 'note') AND author_principal_id IS NOT NULL)),
    CHECK (body_text IS NOT NULL OR body_html IS NOT NULL)
);
CREATE INDEX ticket_message_thread_idx   ON ticket_message (ticket_id, created_at);  -- SC-010 thread load
CREATE INDEX ticket_message_business_idx ON ticket_message (business_id, tenant_root_id);
CREATE TRIGGER ticket_message_troot_immutable BEFORE UPDATE ON ticket_message
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- ---- attachment — file on a message; bytes in object storage (FR-007) ----
CREATE TABLE attachment (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_message_id uuid NOT NULL,
    business_id       uuid NOT NULL,
    tenant_root_id    uuid NOT NULL,
    blob_key          text NOT NULL,                    -- tenant-scoped object-storage key
    filename          text,                             -- display only; never trusted for type
    content_type      text NOT NULL,                    -- SNIFFED MIME type (first 512 bytes), allowlisted
    size              bigint NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_root_id, blob_key),
    FOREIGN KEY (ticket_message_id, tenant_root_id) REFERENCES ticket_message (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    CHECK (size > 0)
);
CREATE INDEX attachment_message_idx  ON attachment (ticket_message_id);
CREATE INDEX attachment_business_idx ON attachment (business_id, tenant_root_id);
CREATE TRIGGER attachment_troot_immutable BEFORE UPDATE ON attachment
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
