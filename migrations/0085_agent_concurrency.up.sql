-- 0085: per-agent review concurrency cap (fallback-chain epic). How many dimension
-- lanes may run at once when THIS agent is the review's resolved reviewbot. Default 4
-- reproduces the prior hard-coded maxConcurrentLanes constant, so existing agents are
-- unchanged; a single-GPU self-host sets it to 1.
ALTER TABLE agent
    ADD COLUMN max_concurrent_lanes int NOT NULL DEFAULT 4
        CHECK (max_concurrent_lanes BETWEEN 1 AND 16);
