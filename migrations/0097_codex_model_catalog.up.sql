-- Seed OpenAI Codex (ChatGPT-subscription) model presets into the system model catalog.
-- Completions run against the flat-rate ChatGPT plan (chatgpt.com/backend-api/codex), not
-- metered api.openai.com, so input/output pricing is 0. *-pro variants are intentionally
-- omitted — the ChatGPT-account backend refuses them with a 403; filterCodexPro in
-- internal/agents/metadata.go also drops any that are ever added, as defense in depth.
INSERT INTO model_pricing
    (model_id, provider, display_name, context_window, input_cents_per_mtok, output_cents_per_mtok, supports_tools)
VALUES
    ('gpt-5-codex', 'openai_codex', 'GPT-5 Codex (ChatGPT)', 400000, 0, 0, true),
    ('gpt-5',       'openai_codex', 'GPT-5 (ChatGPT)',        400000, 0, 0, true)
ON CONFLICT (model_id) DO NOTHING;
