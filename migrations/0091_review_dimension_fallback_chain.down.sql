ALTER TABLE review_dimension ADD COLUMN fallback_provider ai_provider;
ALTER TABLE review_dimension ADD COLUMN fallback_model    text NOT NULL DEFAULT '';

-- Restore the FIRST chain entry into the scalar columns (lossy for N>1).
UPDATE review_dimension
SET fallback_provider = (fallback_chain->0->>'provider')::ai_provider,
    fallback_model     = COALESCE(fallback_chain->0->>'model', '')
WHERE jsonb_array_length(fallback_chain) > 0;

ALTER TABLE review_dimension DROP COLUMN fallback_chain;
