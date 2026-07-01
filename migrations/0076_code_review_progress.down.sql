DROP FUNCTION IF EXISTS renew_code_review_lease(uuid, int, jsonb);
ALTER TABLE code_review DROP COLUMN IF EXISTS progress;
