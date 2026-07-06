-- 0088: per-endpoint review concurrency cap (manyforge-azy). The cap lives on the
-- credential because a credential IS the endpoint (provider + base_url); the review
-- fan-out serializes lanes per endpoint by this value. Default 4 preserves prior behavior.
ALTER TABLE ai_provider_credential
    ADD COLUMN max_concurrent_lanes int NOT NULL DEFAULT 4
        CHECK (max_concurrent_lanes BETWEEN 1 AND 16);
