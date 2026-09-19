-- Spec 016: one mailing brand per business. Campaign, template, automation and
-- confirmation mail inherit it at render time; nothing is snapshotted.
CREATE TABLE mailing_brand (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id       uuid NOT NULL,
    tenant_root_id    uuid NOT NULL,
    name              text NOT NULL,
    logo_blob_key     text,
    logo_content_type text,
    logo_sha256       bytea,
    logo_width        integer NOT NULL DEFAULT 160,
    color_background  text NOT NULL DEFAULT '#f4f6f8',
    color_surface     text NOT NULL DEFAULT '#ffffff',
    color_text        text NOT NULL DEFAULT '#17212b',
    color_accent      text NOT NULL DEFAULT '#1769aa',
    color_header_bg   text NOT NULL DEFAULT '#ffffff',
    color_header_text text NOT NULL DEFAULT '#17212b',
    font_stack        text NOT NULL DEFAULT 'system',
    footer_markdown   text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    CONSTRAINT mailing_brand_business_uniq UNIQUE (business_id),
    CONSTRAINT mailing_brand_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_brand_name_chk CHECK (length(name) BETWEEN 1 AND 200),
    CONSTRAINT mailing_brand_color_chk CHECK (
        color_background ~ '^#[0-9a-f]{6}$' AND color_surface ~ '^#[0-9a-f]{6}$' AND color_text ~ '^#[0-9a-f]{6}$'
        AND color_accent ~ '^#[0-9a-f]{6}$' AND color_header_bg ~ '^#[0-9a-f]{6}$' AND color_header_text ~ '^#[0-9a-f]{6}$'),
    CONSTRAINT mailing_brand_font_chk CHECK (font_stack IN ('system', 'serif', 'mono')),
    CONSTRAINT mailing_brand_footer_chk CHECK (length(footer_markdown) <= 4096),
    CONSTRAINT mailing_brand_logo_width_chk CHECK (logo_width BETWEEN 40 AND 600),
    CONSTRAINT mailing_brand_logo_chk CHECK (
        (logo_blob_key IS NULL AND logo_content_type IS NULL AND logo_sha256 IS NULL)
        OR (logo_blob_key IS NOT NULL AND logo_content_type IS NOT NULL AND logo_sha256 IS NOT NULL))
);
CREATE INDEX mailing_brand_tenant_idx ON mailing_brand (business_id, tenant_root_id);

CREATE TRIGGER mailing_brand_troot_immutable
    BEFORE UPDATE ON mailing_brand
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

GRANT SELECT, INSERT, UPDATE, DELETE ON mailing_brand TO manyforge_app;

ALTER TABLE mailing_brand ENABLE ROW LEVEL SECURITY;
CREATE POLICY mailing_brand_rls ON mailing_brand FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

INSERT INTO tenant_merge_manifest (table_name, module, strategy, inventory_version) VALUES
    ('mailing_brand', 'mailing', 'drain_fence_then_rewrite', 1);

CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON mailing_brand
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();

-- Principal-less render path (send worker, confirmation mail, public brand-by-key).
-- Column order is load-bearing: Go scans positionally.
CREATE FUNCTION mailing_brand_context(p_business_id uuid)
RETURNS TABLE(
    brand_id uuid, updated_at timestamptz, name text,
    logo_blob_key text, logo_content_type text, logo_sha256 bytea, logo_width integer,
    color_background text, color_surface text, color_text text, color_accent text,
    color_header_bg text, color_header_text text, font_stack text, footer_markdown text
)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT b.id, b.updated_at, b.name,
           b.logo_blob_key, b.logo_content_type, b.logo_sha256, b.logo_width,
           b.color_background, b.color_surface, b.color_text, b.color_accent,
           b.color_header_bg, b.color_header_text, b.font_stack, b.footer_markdown
    FROM mailing_brand b
    WHERE b.business_id = p_business_id;
$$;

-- Public logo bytes lookup by brand id. The brand id is the only capability an email
-- client holds; rows without a logo are invisible so the route cannot probe brand existence.
CREATE FUNCTION mailing_public_brand_logo(p_brand_id uuid)
RETURNS TABLE(logo_blob_key text, logo_content_type text, logo_sha256 bytea)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT b.logo_blob_key, b.logo_content_type, b.logo_sha256
    FROM mailing_brand b
    WHERE b.id = p_brand_id AND b.logo_blob_key IS NOT NULL;
$$;

REVOKE ALL ON FUNCTION mailing_brand_context(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_public_brand_logo(uuid) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION mailing_brand_context(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_public_brand_logo(uuid) TO manyforge_app;
