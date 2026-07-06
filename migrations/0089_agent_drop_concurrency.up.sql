-- 0089: the review concurrency cap moved from the agent to the credential/endpoint
-- (manyforge-azy supersedes k8e's 0085); drop the now-unused agent column.
ALTER TABLE agent DROP COLUMN max_concurrent_lanes;
