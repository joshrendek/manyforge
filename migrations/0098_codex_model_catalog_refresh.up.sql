-- Refresh the OpenAI Codex (ChatGPT-subscription) model catalog to OpenAI's current lineup.
-- Migration 0097 seeded gpt-5-codex + gpt-5, but OpenAI retired both slugs: the "-codex" suffix
-- scheme was dropped and the line moved on to the GPT-5.6 Sol/Terra/Luna family, so the model
-- picker was offering models the ChatGPT Codex backend no longer serves (users saw a wrong list).
-- Source of truth: the official codex CLI's bundled catalog
-- (openai/codex, codex-rs/models-manager/models.json), which we mirror here. Completions run
-- against the flat-rate ChatGPT plan (chatgpt.com/backend-api/codex), so input/output pricing is 0.
-- The lineup drifts often; a follow-up replaces this static seed with a live per-plan catalog
-- fetch (GET chatgpt.com/backend-api/codex/models with the credential's OAuth token).
DELETE FROM model_pricing WHERE provider = 'openai_codex' AND model_id IN ('gpt-5-codex', 'gpt-5');
INSERT INTO model_pricing
    (model_id, provider, display_name, context_window, input_cents_per_mtok, output_cents_per_mtok, supports_tools)
VALUES
    ('gpt-5.6-sol',   'openai_codex', 'GPT-5.6-Sol (ChatGPT)',   272000, 0, 0, true),
    ('gpt-5.6-terra', 'openai_codex', 'GPT-5.6-Terra (ChatGPT)', 272000, 0, 0, true),
    ('gpt-5.6-luna',  'openai_codex', 'GPT-5.6-Luna (ChatGPT)',  272000, 0, 0, true)
ON CONFLICT (model_id) DO NOTHING;
