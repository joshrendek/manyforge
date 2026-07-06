-- 0087: per-dimension reviewbot fallback (manyforge-azy). Each dimension gets a fallback
-- (provider, model) tried when its primary endpoint fails the liveness probe. NULL
-- fallback_provider ⇒ no fallback (the primary's failure just funnels to the worker retry).
ALTER TABLE review_dimension
    ADD COLUMN fallback_provider ai_provider,
    ADD COLUMN fallback_model    text NOT NULL DEFAULT '';
