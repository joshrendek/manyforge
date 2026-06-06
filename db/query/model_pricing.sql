-- name: ListModelPricing :many
SELECT model_id, provider, context_window, input_cents_per_mtok, output_cents_per_mtok, supports_tools
FROM model_pricing
WHERE enabled = true
ORDER BY model_id;
