-- name: AccountingSummaryByAgent :many
-- Per-agent usage rollup for a business over [from_ts, to_ts). LEFT JOIN so agents
-- with zero runs in the window still appear (with zeros). RLS on agent + agent_run
-- (under WithPrincipal) scopes to the caller's businesses; the business_id arg narrows.
SELECT
    a.id AS agent_id,
    a.name,
    a.monthly_budget_cents,
    COUNT(r.id) AS run_count,
    COALESCE(SUM(r.tokens_in), 0)::bigint  AS tokens_in,
    COALESCE(SUM(r.tokens_out), 0)::bigint AS tokens_out,
    COALESCE(SUM(r.cost_cents), 0)::bigint AS cost_cents
FROM agent a
LEFT JOIN agent_run r
    ON r.agent_id = a.id
    AND r.business_id = a.business_id
    AND r.created_at >= sqlc.arg('from_ts')::timestamptz
    AND r.created_at <  sqlc.arg('to_ts')::timestamptz
WHERE a.business_id = sqlc.arg('business_id')::uuid
GROUP BY a.id, a.name, a.monthly_budget_cents
ORDER BY cost_cents DESC, a.name;
