-- Reverse 0038_model_pricing.
REVOKE SELECT ON model_pricing FROM manyforge_app;
DROP TABLE IF EXISTS model_pricing;
