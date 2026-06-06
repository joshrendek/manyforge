-- 0038: model_pricing — system catalog for model metadata + pricing (Spec 003 US7).
-- Single source of truth for the ai.Registry (replaces the hardcoded seed.go list in
-- prod; seed.go stays the test fixture). Pricing is integer cents per MILLION tokens.
-- security: system catalog, no tenant scoping (like permission in 0003) — no RLS,
-- SELECT-only grant; writes happen via migration, never from the app.
CREATE TABLE model_pricing (
    model_id              text PRIMARY KEY,
    provider              text NOT NULL,
    display_name          text NOT NULL,
    context_window        integer NOT NULL,
    input_cents_per_mtok  bigint NOT NULL,
    output_cents_per_mtok bigint NOT NULL,
    supports_tools        boolean NOT NULL DEFAULT true,
    enabled               boolean NOT NULL DEFAULT true,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT ON model_pricing TO manyforge_app;

-- Seed mirrors internal/platform/ai/seed.go RegisterDefaults (kept in sync; pinned by
-- TestPin_ModelPricingSeedMatchesDefaults in internal/security_regression).
INSERT INTO model_pricing
    (model_id, provider, display_name, context_window, input_cents_per_mtok, output_cents_per_mtok, supports_tools)
VALUES
    ('claude-sonnet-4-5', 'anthropic', 'Claude Sonnet 4.5', 200000, 300, 1500, true),
    ('claude-opus-4-1',   'anthropic', 'Claude Opus 4.1',   200000, 1500, 7500, true),
    ('claude-haiku-4-5',  'anthropic', 'Claude Haiku 4.5',  200000, 100, 500, true),
    ('gpt-4o',            'openai',    'GPT-4o',             128000, 250, 1000, true),
    ('gpt-4o-mini',       'openai',    'GPT-4o mini',        128000, 15, 60, true);
