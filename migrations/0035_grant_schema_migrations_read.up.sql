-- 0035: let the app role read golang-migrate's version table.
--
-- The server's startup schema-drift guard (db.VerifySchemaCurrent) reads schema_migrations
-- to refuse serving a database behind the code. Production connects as the non-superuser
-- manyforge_app (Constitution Principle I), which has no privilege on golang-migrate's
-- internal table by default — so without this grant the guard itself errors at startup.
-- SELECT only: the app reads the version; it never writes it (migrations do).
GRANT SELECT ON schema_migrations TO manyforge_app;
