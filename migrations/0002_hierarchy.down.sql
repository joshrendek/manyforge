ALTER TABLE principal DROP CONSTRAINT IF EXISTS principal_home_business_fk;
DROP TABLE IF EXISTS business_closure;
DROP TRIGGER IF EXISTS business_root_guard_trg ON business;
DROP FUNCTION IF EXISTS business_root_guard();
DROP TABLE IF EXISTS business;
