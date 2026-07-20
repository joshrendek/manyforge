-- Refresh the OpenAI Codex (ChatGPT-subscription) model catalog to OpenAI's current lineup.
-- Migration 0097 seeded gpt-5-codex + gpt-5, which OpenAI retired. The authoritative per-account
-- list comes from OpenAI's live per-plan endpoint (GET chatgpt.com/backend-api/codex/models),
-- verified against a real ChatGPT Plus token: gpt-5.4 (default) and gpt-5.4-mini, both 272k ctx.
-- (codex-auto-review is also returned, but it is OpenAI's specialized auto-review model rather than
-- a general coding pick, so it is intentionally omitted here.) Completions run against the flat-rate
-- ChatGPT plan (chatgpt.com/backend-api/codex), so input/output pricing is 0. The lineup is
-- per-plan and drifts, so a follow-up replaces this static seed with a live per-plan fetch keyed on
-- the connected credential's OAuth token (proven working). The DELETE also clears the wrong slugs
-- an earlier revision of this migration may have seeded.
DELETE FROM model_pricing WHERE provider = 'openai_codex'
    AND model_id IN ('gpt-5-codex', 'gpt-5', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna');
INSERT INTO model_pricing
    (model_id, provider, display_name, context_window, input_cents_per_mtok, output_cents_per_mtok, supports_tools)
VALUES
    ('gpt-5.4',      'openai_codex', 'GPT-5.4 (ChatGPT)',      272000, 0, 0, true),
    ('gpt-5.4-mini', 'openai_codex', 'GPT-5.4-Mini (ChatGPT)', 272000, 0, 0, true)
ON CONFLICT (model_id) DO NOTHING;
