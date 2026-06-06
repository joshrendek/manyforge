-- US8 (spec 003): per-credential opt-in to reach a self-hosted Ollama/vLLM on a
-- private/loopback base_url. Default false keeps every existing + new credential
-- locked to public destinations unless an operator explicitly trusts it.
ALTER TABLE ai_provider_credential
    ADD COLUMN allow_private_base_url boolean NOT NULL DEFAULT false;
