-- 0099: model_pricing pricing key is provider-aware (manyforge-6fx.2).
-- The sole model_id PRIMARY KEY (0038) let one model_id be claimed globally: seed 0097/0098
-- add ('gpt-5','openai_codex',0,0), so a metered same-named model of another provider would
-- be dropped by its ON CONFLICT (model_id) DO NOTHING and any run with that id would resolve
-- to the $0 codex row (suppressing the 'unpriced model cost=0' log). Widen the PK to
-- (provider, model_id) so each provider owns its own pricing row; ai.Registry keys the same
-- way. Safe: model_id was globally unique, so (provider, model_id) is trivially unique too.
-- security: system catalog, no tenant scoping (like 0038) — SELECT-only grant unchanged.
ALTER TABLE model_pricing DROP CONSTRAINT model_pricing_pkey;
ALTER TABLE model_pricing ADD CONSTRAINT model_pricing_pkey PRIMARY KEY (provider, model_id);
