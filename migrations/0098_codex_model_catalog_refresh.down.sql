-- Restore the 0097 seed (retired gpt-5-codex + gpt-5) on rollback.
DELETE FROM model_pricing WHERE provider = 'openai_codex'
    AND model_id IN ('gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna', 'gpt-5.5', 'gpt-5.4', 'gpt-5.4-mini');
INSERT INTO model_pricing
    (model_id, provider, display_name, context_window, input_cents_per_mtok, output_cents_per_mtok, supports_tools)
VALUES
    ('gpt-5-codex', 'openai_codex', 'GPT-5 Codex (ChatGPT)', 400000, 0, 0, true),
    ('gpt-5',       'openai_codex', 'GPT-5 (ChatGPT)',        400000, 0, 0, true)
ON CONFLICT (model_id) DO NOTHING;
