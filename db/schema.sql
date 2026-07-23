-- sqlc schema input: TABLE definitions only, mirroring migrations/ (the runtime
-- source of truth). Triggers, RLS policies, roles, and functions live in the
-- migrations and are intentionally excluded here so sqlc's parser stays happy.
-- Keep this in sync with migrations/0001..0062 when table shapes change.

CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE account (
    id                uuid PRIMARY KEY,
    email             citext NOT NULL UNIQUE,
    email_verified_at timestamptz,
    password_hash     text,
    display_name      text NOT NULL,
    status            text NOT NULL,
    deleted_at        timestamptz,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL
);

CREATE TABLE principal (
    id               uuid PRIMARY KEY,
    kind             text NOT NULL,
    account_id       uuid,
    home_business_id uuid,
    tenant_root_id   uuid,
    created_at       timestamptz NOT NULL
);

CREATE TABLE business (
    id             uuid PRIMARY KEY,
    parent_id      uuid,
    tenant_root_id uuid NOT NULL,
    name           text NOT NULL,
    status         text NOT NULL,
    deleted_at     timestamptz,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL
);

CREATE TABLE business_closure (
    ancestor_id    uuid NOT NULL,
    descendant_id  uuid NOT NULL,
    depth          int  NOT NULL,
    tenant_root_id uuid NOT NULL,
    PRIMARY KEY (ancestor_id, descendant_id)
);

CREATE TABLE permission (
    key         text PRIMARY KEY,
    module      text NOT NULL,
    description text NOT NULL
);

CREATE TABLE role (
    id             uuid PRIMARY KEY,
    tenant_root_id uuid,
    key            text NOT NULL,
    name           text NOT NULL,
    is_locked      boolean NOT NULL,
    created_at     timestamptz NOT NULL
);

CREATE TABLE role_permission (
    role_id        uuid NOT NULL,
    permission_key text NOT NULL,
    PRIMARY KEY (role_id, permission_key)
);

CREATE TABLE membership (
    id             uuid PRIMARY KEY,
    principal_id   uuid NOT NULL,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    role_id        uuid NOT NULL,
    granted_by     uuid,
    granted_at     timestamptz NOT NULL
);

CREATE TABLE invitation (
    id             uuid PRIMARY KEY,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    email          citext NOT NULL,
    role_id        uuid NOT NULL,
    token_hash     text NOT NULL,
    status         text NOT NULL,
    created_by     uuid,
    expires_at     timestamptz NOT NULL,
    accepted_at    timestamptz,
    created_at     timestamptz NOT NULL
);

CREATE TABLE refresh_token (
    id           uuid PRIMARY KEY,
    principal_id uuid NOT NULL,
    token_hash   text NOT NULL UNIQUE,
    family_id    uuid NOT NULL,
    parent_id    uuid,
    used_at      timestamptz,
    revoked_at   timestamptz,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL
);

CREATE TABLE email_suppression (
    email      citext PRIMARY KEY,
    reason     text NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE one_time_token (
    id          uuid PRIMARY KEY,
    account_id  uuid,
    email       citext NOT NULL,
    purpose     text NOT NULL,
    token_hash  text NOT NULL UNIQUE,
    new_email   citext,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL
);

CREATE TABLE account_erasure (
    account_id   uuid PRIMARY KEY,
    requested_at timestamptz NOT NULL,
    purge_after  timestamptz NOT NULL,
    purged_at    timestamptz,
    created_at   timestamptz NOT NULL
);

CREATE TABLE audit_entry (
    id                 uuid PRIMARY KEY,
    business_id        uuid,
    tenant_root_id     uuid,
    actor_principal_id uuid,
    action             text NOT NULL,
    target_type        text,
    target_id          uuid,
    inputs             jsonb,
    outputs            jsonb,
    decision           text,
    correlation_id     text,
    old_value          jsonb,
    new_value          jsonb,
    created_at         timestamptz NOT NULL
);

-- ============================================================================
-- Native Support Desk (spec 002) — mirrors migrations/0013..0016.
-- ============================================================================

CREATE TYPE inbound_address_kind     AS ENUM ('system', 'custom');
CREATE TYPE email_domain_mode        AS ENUM ('forward_in', 'subdomain_mx', 'provider_route');
CREATE TYPE email_domain_spf_state   AS ENUM ('unknown', 'pending', 'pass', 'fail');
CREATE TYPE ticket_status            AS ENUM ('new', 'open', 'pending', 'solved', 'closed');
CREATE TYPE ticket_priority          AS ENUM ('low', 'normal', 'high', 'urgent');
CREATE TYPE ticket_message_direction AS ENUM ('inbound', 'outbound', 'note');
CREATE TYPE message_delivery_state   AS ENUM ('pending', 'sent', 'failed');

CREATE TABLE email_domain (
    id                   uuid PRIMARY KEY,
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    domain               citext NOT NULL,
    mode                 email_domain_mode NOT NULL,
    verify_token         text NOT NULL,
    verified_at          timestamptz,
    dkim_selector        text,
    dkim_public_key      text,
    dkim_private_key_ref text,
    spf_state            email_domain_spf_state NOT NULL,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    UNIQUE (tenant_root_id, domain),
    UNIQUE (id, tenant_root_id)
);

CREATE TABLE inbound_address (
    id              uuid PRIMARY KEY,
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    address         citext NOT NULL,
    kind            inbound_address_kind NOT NULL,
    email_domain_id uuid,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    UNIQUE (tenant_root_id, address)
);

CREATE TABLE requester (
    id             uuid PRIMARY KEY,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    email          citext NOT NULL,
    display_name   text,
    contact_id     uuid,
    first_seen_at  timestamptz NOT NULL,
    last_seen_at   timestamptz NOT NULL,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    UNIQUE (tenant_root_id, email),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (contact_id, tenant_root_id) REFERENCES contact (id, tenant_root_id)
);

CREATE TABLE ticket (
    id                    uuid PRIMARY KEY,
    business_id           uuid NOT NULL,
    tenant_root_id        uuid NOT NULL,
    requester_id          uuid NOT NULL,
    subject               text NOT NULL,
    status                ticket_status NOT NULL,
    priority              ticket_priority NOT NULL,
    assignee_principal_id uuid,
    reply_token           text NOT NULL,
    last_message_at       timestamptz NOT NULL,
    redacted_at           timestamptz,
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    connector_id          uuid,
    external_id           text,
    external_url          text,
    UNIQUE (id, tenant_root_id),
    UNIQUE (tenant_root_id, reply_token),
    CONSTRAINT ticket_connector_external_chk CHECK (connector_id IS NULL OR external_id IS NOT NULL)
);

CREATE TABLE ticket_tag (
    ticket_id      uuid NOT NULL,
    tag            citext NOT NULL,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    created_at     timestamptz NOT NULL,
    PRIMARY KEY (ticket_id, tag)
);

CREATE TABLE ticket_message (
    id                  uuid PRIMARY KEY,
    ticket_id           uuid NOT NULL,
    business_id         uuid NOT NULL,
    tenant_root_id      uuid NOT NULL,
    direction           ticket_message_direction NOT NULL,
    author_principal_id uuid,
    message_id          text NOT NULL,
    in_reply_to         text,
    "references"        text[] NOT NULL,
    body_text           text,
    body_html           text,
    auth_results        jsonb,
    is_auto_reply       boolean NOT NULL,
    created_at          timestamptz NOT NULL,
    delivery_state      message_delivery_state,
    delivery_error      text,
    source_approval_item_id uuid,
    connector_id        uuid,
    external_id         text,
    UNIQUE (tenant_root_id, message_id),
    UNIQUE (id, tenant_root_id),
    CONSTRAINT ticket_message_connector_external_chk CHECK (connector_id IS NULL OR external_id IS NOT NULL)
);
CREATE UNIQUE INDEX ticket_external_idx ON ticket (connector_id, external_id) WHERE connector_id IS NOT NULL;
CREATE UNIQUE INDEX ticket_message_external_idx ON ticket_message (connector_id, external_id) WHERE connector_id IS NOT NULL;
CREATE UNIQUE INDEX ticket_message_source_approval_idx ON ticket_message (source_approval_item_id) WHERE source_approval_item_id IS NOT NULL;

CREATE TABLE attachment (
    id                uuid PRIMARY KEY,
    ticket_message_id uuid NOT NULL,
    business_id       uuid NOT NULL,
    tenant_root_id    uuid NOT NULL,
    blob_key          text NOT NULL,
    filename          text,
    content_type      text NOT NULL,
    size              bigint NOT NULL,
    created_at        timestamptz NOT NULL,
    UNIQUE (tenant_root_id, blob_key)
);

CREATE TABLE outbox (
    id             uuid PRIMARY KEY,
    tenant_root_id uuid NOT NULL,
    topic          text NOT NULL,
    payload        jsonb NOT NULL,
    available_at   timestamptz NOT NULL,
    processed_at   timestamptz,
    attempts       int NOT NULL,
    created_at     timestamptz NOT NULL
);

CREATE TABLE notification (
    id             uuid PRIMARY KEY,
    tenant_root_id uuid NOT NULL,
    principal_id   uuid NOT NULL,
    kind           text NOT NULL,
    ref            jsonb NOT NULL,
    read_at        timestamptz,
    created_at     timestamptz NOT NULL
);

-- ============================================================================
-- Agent runtime (spec 003) — mirrors migrations/0025.
-- ============================================================================

CREATE TYPE ai_provider AS ENUM ('anthropic', 'openai', 'ollama', 'vllm', 'openrouter', 'huggingface', 'openai_codex');

CREATE TABLE ai_provider_credential (
    id              uuid PRIMARY KEY,
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    provider        ai_provider NOT NULL,
    sealed_key_ref  text,
    base_url        text,
    default_model   text NOT NULL,
    allow_private_base_url boolean NOT NULL,
    max_concurrent_lanes integer NOT NULL,
    chatgpt_account_id text,
    oauth_refresh_token text,
    oauth_access_expiry timestamptz,
    chatgpt_plan text,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    UNIQUE (business_id, provider),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);

CREATE TABLE agent (
    id                   uuid PRIMARY KEY,
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    principal_id         uuid NOT NULL,
    name                 text NOT NULL,
    provider             ai_provider NOT NULL,
    model                text NOT NULL,
    system_prompt        text NOT NULL,
    allowed_tools        text[] NOT NULL,
    autonomy_mode        smallint NOT NULL,
    enabled              boolean NOT NULL,
    monthly_budget_cents integer NOT NULL,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    allowed_mcp_servers  uuid[] NOT NULL,
    retriage_on_reply    boolean NOT NULL,
    web_allowed_domains  text[] NOT NULL,
    UNIQUE (business_id, name),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (principal_id) REFERENCES principal (id)
);

CREATE TABLE agent_run (
    id             uuid PRIMARY KEY,
    agent_id       uuid NOT NULL,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    trigger        text NOT NULL,
    target_type    text,
    target_id      uuid,
    status         text NOT NULL,
    tokens_in      integer NOT NULL,
    tokens_out     integer NOT NULL,
    cost_cents     bigint NOT NULL,
    correlation_id text NOT NULL,
    error          text,
    trigger_dedup_key text,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (agent_id, tenant_root_id) REFERENCES agent (id, tenant_root_id),
    CONSTRAINT agent_run_trigger_check CHECK (trigger = ANY (ARRAY['event', 'manual', 'reply', 'code_review']))
);
CREATE UNIQUE INDEX agent_run_trigger_dedup_idx
    ON agent_run (agent_id, trigger_dedup_key)
    WHERE trigger_dedup_key IS NOT NULL;

CREATE TABLE approval_item (
    id                      uuid PRIMARY KEY,
    agent_run_id            uuid NOT NULL,
    business_id             uuid NOT NULL,
    tenant_root_id          uuid NOT NULL,
    tool                    text NOT NULL,
    args                    jsonb NOT NULL,
    effect_class            smallint NOT NULL,
    state                   text NOT NULL,
    decided_by_principal_id uuid,
    decided_at              timestamptz,
    executed_at             timestamptz,
    expires_at              timestamptz NOT NULL,
    error                   text,
    created_at              timestamptz NOT NULL,
    updated_at              timestamptz NOT NULL,
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (agent_run_id, tenant_root_id) REFERENCES agent_run (id, tenant_root_id)
);

CREATE TABLE mcp_server (
    id              uuid PRIMARY KEY,
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    name            text NOT NULL,
    url             text NOT NULL,
    sealed_auth_ref text,
    enabled         boolean NOT NULL,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    UNIQUE (business_id, name),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX mcp_server_business_idx ON mcp_server (business_id, tenant_root_id);

CREATE TABLE mcp_tool_policy (
    mcp_server_id  uuid     NOT NULL,
    business_id    uuid     NOT NULL,
    tenant_root_id uuid     NOT NULL,
    tool_name      text     NOT NULL,
    effect         smallint NOT NULL,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    PRIMARY KEY (mcp_server_id, tool_name),
    FOREIGN KEY (mcp_server_id, tenant_root_id) REFERENCES mcp_server (id, tenant_root_id) ON DELETE CASCADE,
    FOREIGN KEY (business_id, tenant_root_id)   REFERENCES business (id, tenant_root_id)
);
CREATE INDEX mcp_tool_policy_business_idx ON mcp_tool_policy (business_id, tenant_root_id);

-- security: system catalog, no tenant scoping (like permission in 0003) — no RLS,
-- SELECT-only grant; writes happen via migration, never from the app.
CREATE TABLE model_pricing (
    model_id              text NOT NULL,
    provider              text NOT NULL,
    display_name          text NOT NULL,
    context_window        integer NOT NULL,
    input_cents_per_mtok  bigint NOT NULL,
    output_cents_per_mtok bigint NOT NULL,
    supports_tools        boolean NOT NULL,
    enabled               boolean NOT NULL,
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    -- Provider-aware PK (manyforge-6fx.2): a $0 openai_codex 'gpt-5' must not claim the
    -- model_id globally and shadow a metered same-named model of another provider.
    PRIMARY KEY (provider, model_id)
);

-- ============================================================================
-- External connectors + secret vault (spec 004) — mirrors migrations/0040.
-- ============================================================================

CREATE TYPE connector_type AS ENUM ('jira', 'zendesk');

CREATE TABLE secret (
    id              uuid PRIMARY KEY,
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    scope           text NOT NULL,
    sealed_value    text NOT NULL,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX secret_business_idx ON secret (business_id, tenant_root_id);

CREATE TABLE connector (
    id                      uuid PRIMARY KEY,
    business_id             uuid NOT NULL,
    tenant_root_id          uuid NOT NULL,
    type                    connector_type NOT NULL,
    display_name            text NOT NULL,
    base_url                text NOT NULL,
    allow_private_base_url  boolean NOT NULL,
    suppress_native_notifications boolean NOT NULL DEFAULT false,
    secret_ref              uuid NOT NULL,
    config                  jsonb NOT NULL,
    status                  text NOT NULL,
    last_reconciled_at      timestamptz,
    created_at              timestamptz NOT NULL,
    updated_at              timestamptz NOT NULL,
    UNIQUE (id, tenant_root_id),
    CONSTRAINT connector_status_chk CHECK (status IN ('enabled', 'disabled')),
    UNIQUE (business_id, type, base_url),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (secret_ref, tenant_root_id) REFERENCES secret (id, tenant_root_id)
);
CREATE INDEX connector_business_idx ON connector (business_id, tenant_root_id);

CREATE TABLE connector_sync_state (
    ticket_id           uuid PRIMARY KEY,
    business_id         uuid NOT NULL,
    tenant_root_id      uuid NOT NULL,
    connector_id        uuid NOT NULL,
    external_id         text NOT NULL,
    snapshot            jsonb NOT NULL,
    external_updated_at timestamptz NOT NULL,
    synced_at           timestamptz NOT NULL,
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (ticket_id, tenant_root_id) REFERENCES ticket (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id)
);
CREATE INDEX connector_sync_state_business_idx ON connector_sync_state (business_id, tenant_root_id);

CREATE TABLE connector_webhook_delivery (
    id                   uuid PRIMARY KEY,
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    connector_id         uuid NOT NULL,
    external_delivery_id text NOT NULL,
    received_at          timestamptz NOT NULL,
    UNIQUE (connector_id, external_delivery_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id)
);
CREATE INDEX connector_webhook_delivery_business_idx ON connector_webhook_delivery (business_id, tenant_root_id);

CREATE TYPE connector_outbound_op_type   AS ENUM ('comment', 'create_issue', 'transition');
CREATE TYPE connector_outbound_op_status AS ENUM ('pending', 'in_progress', 'done', 'failed', 'dismissed');

CREATE TABLE connector_outbound_op (
    id             uuid PRIMARY KEY,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    connector_id   uuid NOT NULL,
    ticket_id      uuid NOT NULL,
    message_id     uuid,
    op_type        connector_outbound_op_type NOT NULL,
    status         connector_outbound_op_status NOT NULL,
    attempts       int NOT NULL,
    body           text,
    last_error     text,
    internal       boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id),
    FOREIGN KEY (ticket_id, tenant_root_id) REFERENCES ticket (id, tenant_root_id)
);
CREATE INDEX connector_outbound_op_business_idx ON connector_outbound_op (business_id, tenant_root_id);

-- Spec 005 CRM: tenant-wide contacts + companies (migrations/0057).
CREATE TABLE company (
    id             uuid PRIMARY KEY,
    tenant_root_id uuid NOT NULL,
    name           text NOT NULL,
    domain         citext,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    UNIQUE (id, tenant_root_id)
);
CREATE UNIQUE INDEX company_tenant_domain_uq ON company (tenant_root_id, domain) WHERE domain IS NOT NULL;

CREATE TABLE contact (
    id             uuid PRIMARY KEY,
    tenant_root_id uuid NOT NULL,
    primary_email  citext NOT NULL,
    display_name   text,
    company_id     uuid,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    deleted_at     timestamptz,
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (company_id, tenant_root_id) REFERENCES company (id, tenant_root_id)
);
CREATE UNIQUE INDEX contact_tenant_email_uq ON contact (tenant_root_id, primary_email) WHERE deleted_at IS NULL;
CREATE INDEX contact_company_idx ON contact (company_id, tenant_root_id);

-- Spec 005 CRM Phase B: tenant-wide activity timeline (migrations/0062).
CREATE TABLE activity_entry (
    id             uuid PRIMARY KEY,
    tenant_root_id uuid NOT NULL,
    business_id    uuid NOT NULL,
    contact_id     uuid NOT NULL,
    kind           text NOT NULL,
    occurred_at    timestamptz NOT NULL,
    actor          text,
    source_type    text NOT NULL,
    source_id      uuid,
    summary        text NOT NULL,
    metadata       jsonb,
    created_at     timestamptz NOT NULL,
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (contact_id, tenant_root_id) REFERENCES contact (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX activity_contact_time_idx  ON activity_entry (contact_id, occurred_at DESC, id DESC);
CREATE INDEX activity_business_time_idx ON activity_entry (business_id, occurred_at DESC);
CREATE UNIQUE INDEX activity_dedup_idx ON activity_entry (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL;

-- Spec 007 code-review agent: per-business GitHub repo connector (migrations/0070).
CREATE TABLE repo_connector (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id            uuid NOT NULL,
    tenant_root_id         uuid NOT NULL,
    type                   text NOT NULL DEFAULT 'github',
    display_name           text NOT NULL,
    base_url               text NOT NULL,
    repo                   text NOT NULL,
    allow_private_base_url boolean NOT NULL DEFAULT false,
    secret_ref             uuid,
    config                 jsonb NOT NULL DEFAULT '{}'::jsonb,
    status                 text NOT NULL DEFAULT 'enabled',
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (secret_ref, tenant_root_id) REFERENCES secret (id, tenant_root_id),
    -- migrations/0083: app-backed connectors (type='github_app') store no PAT — secret_ref
    -- is NULL and config carries the installation_id instead.
    CONSTRAINT repo_connector_type_chk CHECK (type IN ('github', 'github_app')),
    CONSTRAINT repo_connector_secret_ref_chk CHECK (
        (type = 'github'     AND secret_ref IS NOT NULL) OR
        (type = 'github_app' AND secret_ref IS NULL AND config ? 'installation_id')
    )
);
-- One app-backed connector per (business, repo) — migrations/0083.
CREATE UNIQUE INDEX repo_connector_github_app_repo_uq ON repo_connector (business_id, repo) WHERE type = 'github_app';

-- Spec 007 code-review agent: one review of one PR, linked to an agent_run (migrations/0071).
-- Spec 007 slice 2: durable work-queue columns added in migrations/0072.
CREATE TABLE code_review (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id        uuid NOT NULL,
    tenant_root_id     uuid NOT NULL,
    agent_run_id       uuid,
    repo_connector_id  uuid NOT NULL,
    pr_number          integer NOT NULL,
    head_sha           text NOT NULL DEFAULT '',
    status             text NOT NULL DEFAULT 'pending',
    summary            text NOT NULL DEFAULT '',
    findings           jsonb NOT NULL DEFAULT '[]'::jsonb,
    external_review_ref text NOT NULL DEFAULT '',
    posted_at          timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    principal_id       uuid,
    agent_id           uuid,
    attempts           integer NOT NULL DEFAULT 0,
    run_after          timestamptz NOT NULL DEFAULT now(),
    lease_expires_at   timestamptz,
    last_error         text NOT NULL DEFAULT '',
    model              text NOT NULL DEFAULT '',
    tokens_in          integer NOT NULL DEFAULT 0,
    tokens_out         integer NOT NULL DEFAULT 0,
    cost_cents         bigint NOT NULL DEFAULT 0,
    progress           jsonb,
    dimension_runs     jsonb NOT NULL DEFAULT '[]'::jsonb,
    force              boolean NOT NULL DEFAULT false,
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (repo_connector_id, tenant_root_id) REFERENCES repo_connector (id, tenant_root_id),
    -- migrations/0084: 'superseded' added when a new push cancels an unstarted review.
    CONSTRAINT code_review_status_chk CHECK (status IN ('pending','running','succeeded','failed','superseded'))
);

CREATE TABLE review_dimension (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    dimension       text NOT NULL,
    provider        ai_provider,
    model           text NOT NULL DEFAULT '',
    fallback_chain  jsonb NOT NULL DEFAULT '[]',
    prompt          text NOT NULL DEFAULT '',
    scope_globs     text[] NOT NULL DEFAULT '{}',
    min_severity    text NOT NULL DEFAULT 'info',
    enabled         boolean NOT NULL DEFAULT true,
    sort_order      integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, dimension),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    CONSTRAINT review_dimension_dimension_chk
        CHECK (dimension IN ('security', 'correctness', 'performance', 'ui', 'docs', 'tests', 'general')),
    CONSTRAINT review_dimension_min_severity_chk
        CHECK (min_severity IN ('info', 'warning', 'error'))
);

-- Cross-iteration finding tracking (Spec 008 Slice 4, manyforge-e54.1). See migration 0100.
CREATE TABLE code_review_finding_seen (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    repo            text NOT NULL,
    pr_number       integer NOT NULL,
    fingerprint     text NOT NULL,
    first_seen_sha  text NOT NULL,
    last_seen_sha   text NOT NULL,
    status          text NOT NULL DEFAULT 'open',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, repo, pr_number, fingerprint),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    CONSTRAINT code_review_finding_seen_status_chk
        CHECK (status IN ('open', 'resolved'))
);

CREATE TABLE review_config (
    business_id     uuid PRIMARY KEY,
    tenant_root_id  uuid NOT NULL,
    dedupe          boolean NOT NULL DEFAULT true,
    verify_enabled  boolean NOT NULL DEFAULT false,
    verify_provider ai_provider,
    verify_model    text NOT NULL DEFAULT '',
    cite_rules      boolean NOT NULL DEFAULT false,
    post_mode       text NOT NULL DEFAULT 'single',
    review_agent_chain uuid[] NOT NULL DEFAULT '{}',
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);

CREATE INDEX code_review_claim_idx ON code_review (status, run_after);

CREATE TABLE github_app_config (
    id                    integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    app_id                bigint  NOT NULL,
    slug                  text    NOT NULL,
    client_id             text    NOT NULL,
    sealed_client_secret  text    NOT NULL,
    sealed_private_key    text    NOT NULL,
    sealed_webhook_secret text    NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE github_setup_nonce (
    nonce       text PRIMARY KEY,
    consumed_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE github_app_installation (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id bigint NOT NULL UNIQUE,
    account_login   text   NOT NULL,
    account_type    text   NOT NULL DEFAULT 'Organization',
    business_id     uuid,
    tenant_root_id  uuid,
    agent_id        uuid,
    enabled         boolean NOT NULL DEFAULT true,
    config          jsonb   NOT NULL DEFAULT '{}'::jsonb,
    suspended_at    timestamptz,
    deleted_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Webhook delivery dedup (tenantless — installation is the key pre-link). migrations/0084.
CREATE TABLE github_webhook_delivery (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id bigint NOT NULL,
    external_delivery_id text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (installation_id, external_delivery_id)
);

-- Codex OAuth pending state. migrations/0095.
CREATE TABLE codex_oauth_pending (
    jti                  uuid PRIMARY KEY,
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    flow                 text NOT NULL,
    sealed_device_code   text,
    sealed_pkce_verifier text,
    default_model        text NOT NULL,
    base_url             text,
    max_concurrent_lanes integer NOT NULL,
    status               text NOT NULL DEFAULT 'pending',
    created_at           timestamptz NOT NULL DEFAULT now(),
    expires_at           timestamptz NOT NULL,
    UNIQUE (jti, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
