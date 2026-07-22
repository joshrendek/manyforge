-- Revert to the provider-blind sole-model_id PK. NOTE: this fails if two providers now
-- claim the same model_id (exactly the state the up-migration permits) — resolve the
-- duplicate (drop/rename one row) before rolling back.
ALTER TABLE model_pricing DROP CONSTRAINT model_pricing_pkey;
ALTER TABLE model_pricing ADD CONSTRAINT model_pricing_pkey PRIMARY KEY (model_id);
