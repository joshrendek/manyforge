DROP FUNCTION IF EXISTS fail_code_review(uuid, text);
DROP FUNCTION IF EXISTS requeue_code_review(uuid, int, text);
DROP FUNCTION IF EXISTS claim_code_reviews(int, int);
