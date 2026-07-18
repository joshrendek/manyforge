-- ChatGPT-Account-Id header value for openai_codex credentials. Non-secret (an account
-- identifier, not a token); NULL for every other provider. The sealed access token continues
-- to live in sealed_key_ref. Sent as a request header by the sandbox entrypoint's openai_codex arm.
ALTER TABLE ai_provider_credential ADD COLUMN chatgpt_account_id text;
