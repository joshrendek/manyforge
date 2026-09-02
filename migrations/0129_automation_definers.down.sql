DROP FUNCTION IF EXISTS automation_step_delivery(uuid,text);
DROP FUNCTION IF EXISTS automation_event_exists(uuid,citext,text,timestamptz,interval);
DROP FUNCTION IF EXISTS automation_exit_for_subscriber(uuid,uuid,text);
DROP FUNCTION IF EXISTS automation_enroll_for_trigger(uuid,uuid,text,text,uuid,uuid,timestamptz);
DROP FUNCTION IF EXISTS automation_fail_step(uuid,integer,text,boolean,timestamptz);
DROP FUNCTION IF EXISTS automation_record_step(uuid,integer,text,text,automation_step_outcome,text,timestamptz,automation_enrollment_status,uuid,jsonb,timestamptz);
DROP FUNCTION IF EXISTS automation_claim_due(timestamptz,integer,interval);
