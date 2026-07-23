-- 0102 down: drop the feedback module (functions, tables, enum). Policies + triggers drop
-- with their tables.

DROP FUNCTION IF EXISTS convert_feedback_post_to_ticket(uuid,uuid,uuid);
DROP FUNCTION IF EXISTS feedback_public_list_posts(uuid,int);
DROP FUNCTION IF EXISTS feedback_public_vote(uuid,uuid,uuid,uuid,text);
DROP FUNCTION IF EXISTS feedback_public_submit(uuid,uuid,uuid,text,text,text);
DROP FUNCTION IF EXISTS feedback_public_board(text);

DROP TABLE IF EXISTS feedback_ingest_key;
DROP TABLE IF EXISTS feedback_vote;
DROP TABLE IF EXISTS feedback_post;
DROP TABLE IF EXISTS feedback_board;

DROP TYPE IF EXISTS feedback_status;
