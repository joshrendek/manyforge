-- Add 'openai_codex' to the ai_provider enum. This is a ChatGPT-subscription credential
-- ("Sign in with ChatGPT"): the sealed key is a short-lived OAuth access token and completions
-- go to the ChatGPT backend (https://chatgpt.com/backend-api/codex, Responses wire) via opencode's
-- built-in openai provider, NOT api.openai.com. See specs .../2026-07-11-codex-...-design.md.
-- (PG: a newly added enum value cannot be USED in the same tx that adds it; nothing below uses
-- it — credentials reference it post-commit — so this is safe.) Idempotent.
ALTER TYPE ai_provider ADD VALUE IF NOT EXISTS 'openai_codex';
