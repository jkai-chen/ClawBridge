-- Schema-3 additive DDL copied verbatim from internal/bus/taskboard.go at
-- 8aef0f97802169efdf6895eb84bf468a3a9f05c8. Apply over the historical v2 PG
-- fixture. This is source-derived DDL, not a dump from a historical PG runtime.
BEGIN;
CREATE TABLE taskboard_bindings(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision BIGINT NOT NULL CHECK(revision>0),mapping TEXT,updated_at TEXT NOT NULL);
INSERT INTO clawbridge_schema(version) VALUES(3);
COMMIT;
