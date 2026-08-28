DROP FUNCTION IF EXISTS mailing_relay_identity(uuid);
DROP FUNCTION IF EXISTS mailing_s2s_unsubscribe(uuid,citext);
DROP FUNCTION IF EXISTS mailing_unsubscribe(uuid,uuid,text);
DROP FUNCTION IF EXISTS mailing_confirm(bytea);
DROP FUNCTION IF EXISTS mailing_s2s_subscribe(uuid,uuid,uuid,uuid,citext,text,text,jsonb,boolean,bytea,timestamptz);
DROP FUNCTION IF EXISTS mailing_public_subscribe(uuid,uuid,uuid,uuid,citext,text,text,jsonb,inet,text,bytea,timestamptz);
DROP FUNCTION IF EXISTS mailing_key_subscribe(uuid,uuid,uuid,uuid,citext,text,text,jsonb,mailing_consent_source,uuid,inet,text,boolean,bytea,timestamptz);
DROP FUNCTION IF EXISTS mailing_public_list(text);
