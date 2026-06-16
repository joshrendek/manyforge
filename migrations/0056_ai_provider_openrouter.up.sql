-- Add 'openrouter' to the ai_provider enum. OpenRouter is OpenAI-API-compatible, so it
-- reuses the OpenAICompatProvider; this just makes it a selectable first-class provider.
-- (PG: a newly added enum value cannot be USED in the same tx that adds it; nothing below
-- uses it — credentials/agents reference it post-commit — so this is safe.) Idempotent.
ALTER TYPE ai_provider ADD VALUE IF NOT EXISTS 'openrouter';
