-- 0067 (manyforge-67i): reaper DEFINER for orphaned 'running' agent runs. The runner sets a run
-- 'running' (agent_run.updated_at = start time) then executes a loop capped at 120s wall-clock
-- (internal/agents/runner.go defaultWallClock). If the worker goroutine dies — backend restart
-- or crash — the row never reaches a terminal state and is stuck 'running' forever, so any
-- "agent working" indicator built on it would lie.
--
-- reap_stale_agent_runs marks 'running' runs whose updated_at is older than p_stale_seconds as
-- 'failed' and returns the count. The caller passes a window WELL above the 120s cap (the worker
-- wires 10 min), so a genuinely-live run is never reaped. It is staleness-based rather than a
-- startup "reap every running row" so it stays correct if the app is ever run as multiple
-- replicas — a run stale past the window is dead regardless of which instance owned it (the app
-- is single-instance in v1 but the agent drainer already uses FOR UPDATE SKIP LOCKED for future
-- horizontal scaling). p_stale_seconds=0 reaps all running rows (the semantics a single-instance
-- startup sweep would want), which the periodic worker never uses.
--
-- Principal-less background sweep → SECURITY DEFINER, granted to the app role only (mirrors
-- expire_stale_approvals and the 0045/0047 outbound DEFINERs).
CREATE FUNCTION reap_stale_agent_runs(p_stale_seconds double precision)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_n integer;
BEGIN
    UPDATE agent_run
    SET status = 'failed',
        error = COALESCE(NULLIF(error, ''), 'run abandoned: worker stopped before completion (reaped)'),
        updated_at = now()
    WHERE status = 'running'
      AND updated_at < now() - make_interval(secs => p_stale_seconds);
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$;

REVOKE ALL ON FUNCTION reap_stale_agent_runs(double precision) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION reap_stale_agent_runs(double precision) TO manyforge_app;
