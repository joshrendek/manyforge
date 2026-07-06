ALTER TABLE agent
    ADD COLUMN max_concurrent_lanes int NOT NULL DEFAULT 4
        CHECK (max_concurrent_lanes BETWEEN 1 AND 16);
