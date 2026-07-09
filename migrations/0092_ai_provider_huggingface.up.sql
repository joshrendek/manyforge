-- Add 'huggingface' to the ai_provider enum. This is the HF Inference Providers router
-- (https://router.huggingface.co/v1), an OpenAI-compatible gateway that routes to partner
-- providers on a single hf_ token, so it reuses the OpenAICompatProvider and defaults its
-- base_url exactly the way openrouter does. Model ids pin the partner: "org/model:groq".
-- (PG: a newly added enum value cannot be USED in the same tx that adds it; nothing below
-- uses it — credentials/agents reference it post-commit — so this is safe.) Idempotent.
ALTER TYPE ai_provider ADD VALUE IF NOT EXISTS 'huggingface';
