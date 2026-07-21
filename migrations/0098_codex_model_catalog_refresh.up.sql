-- Refresh the OpenAI Codex (ChatGPT-subscription) model catalog to OpenAI's current lineup.
-- Migration 0097 seeded gpt-5-codex + gpt-5, which OpenAI retired. The authoritative list is
-- OpenAI's per-account catalog (GET chatgpt.com/backend-api/codex/models) cross-checked with
-- learn.chatgpt.com/docs/models. Verified against a real ChatGPT Plus token: the returned set is
-- gated by BOTH plan and the client_version query param — an old client_version hides the newest
-- models (0.99.0 -> only gpt-5.4/-mini; 1.0.0+ -> the full gpt-5.6 family). With a current client
-- version a Plus account returns: gpt-5.6-sol/terra/luna, gpt-5.5, gpt-5.4, gpt-5.4-mini.
--
-- Seeded below (default gpt-5.6-sol, the flagship). Omitted on purpose: codex-auto-review (an
-- internal auto-review model, not user-facing) and gpt-5.3-codex-spark (Pro-only; a Plus account
-- does not receive it). Completions run against the flat-rate ChatGPT plan, so pricing is 0.
--
-- Because the real list is plan- AND client_version-gated, a static seed is inherently a stopgap;
-- the durable fix is a live per-plan fetch (current client_version) keyed on the credential token.
-- The DELETE also clears any wrong slugs an earlier revision of this migration may have seeded.
DELETE FROM model_pricing WHERE provider = 'openai_codex'
    AND model_id IN ('gpt-5-codex', 'gpt-5', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna',
                     'gpt-5.5', 'gpt-5.4', 'gpt-5.4-mini');
INSERT INTO model_pricing
    (model_id, provider, display_name, context_window, input_cents_per_mtok, output_cents_per_mtok, supports_tools)
VALUES
    ('gpt-5.6-sol',   'openai_codex', 'GPT-5.6-Sol (ChatGPT)',   272000, 0, 0, true),
    ('gpt-5.6-terra', 'openai_codex', 'GPT-5.6-Terra (ChatGPT)', 272000, 0, 0, true),
    ('gpt-5.6-luna',  'openai_codex', 'GPT-5.6-Luna (ChatGPT)',  272000, 0, 0, true),
    ('gpt-5.5',       'openai_codex', 'GPT-5.5 (ChatGPT)',       272000, 0, 0, true),
    ('gpt-5.4',       'openai_codex', 'GPT-5.4 (ChatGPT)',       272000, 0, 0, true),
    ('gpt-5.4-mini',  'openai_codex', 'GPT-5.4-Mini (ChatGPT)',  272000, 0, 0, true)
ON CONFLICT (model_id) DO NOTHING;
