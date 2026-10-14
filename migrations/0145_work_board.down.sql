DROP FUNCTION work_resolve_token(text);
DROP TRIGGER tenant_merge_write_fence ON work_token;
DROP TRIGGER tenant_merge_write_fence ON work_task;
DROP TRIGGER tenant_merge_write_fence ON work_project;
DELETE FROM tenant_merge_manifest WHERE table_name IN ('work_project', 'work_task', 'work_token');
DROP TABLE work_token;
DROP TABLE work_task;
DROP TABLE work_project;
DROP SEQUENCE work_task_rev_seq;
