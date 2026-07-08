-- 0091: dimension fallback becomes an ORDERED CHAIN (manyforge-7lx), not a single (provider,
-- model) pair. fallback_chain is a jsonb array of {"provider","model"} objects, tried in order
-- after the primary. This migration is behavior-preserving: every existing single fallback
-- becomes a 1-element chain, so no operator loses their configured fallback.
ALTER TABLE review_dimension ADD COLUMN fallback_chain jsonb NOT NULL DEFAULT '[]';

-- Preserve existing single fallbacks as 1-element chains (no config lost).
UPDATE review_dimension
SET fallback_chain = jsonb_build_array(
    jsonb_build_object('provider', fallback_provider::text, 'model', fallback_model))
WHERE fallback_provider IS NOT NULL;

ALTER TABLE review_dimension DROP COLUMN fallback_provider;
ALTER TABLE review_dimension DROP COLUMN fallback_model;
