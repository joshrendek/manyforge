-- Add 'huggingface' to the ai_provider enum. A huggingface credential points at a
-- user-hosted ZeroGPU Space serving an OpenAI-compatible /v1/chat/completions, so it
-- reuses the OpenAICompatProvider and requires a caller-supplied base_url (there is no
-- sensible default: the Space URL is per-user, e.g. https://<user>-<space>.hf.space/v1).
-- (PG: a newly added enum value cannot be USED in the same tx that adds it; nothing below
-- uses it — credentials/agents reference it post-commit — so this is safe.) Idempotent.
ALTER TYPE ai_provider ADD VALUE IF NOT EXISTS 'huggingface';
