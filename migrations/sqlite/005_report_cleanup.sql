CREATE TABLE report_file_cleanup (
    path TEXT NOT NULL PRIMARY KEY,
    enqueued_at INTEGER NOT NULL DEFAULT (unixepoch()),
    available_at INTEGER NOT NULL DEFAULT (unixepoch()),
    claim_token TEXT,
    attempts INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX report_file_cleanup_due ON report_file_cleanup(available_at, path);

CREATE TRIGGER reports_cleanup_delete AFTER DELETE ON reports
BEGIN
    INSERT INTO report_file_cleanup(path) VALUES (OLD.report_path) ON CONFLICT(path) DO NOTHING;
END;

CREATE TRIGGER reports_cleanup_replace AFTER UPDATE OF report_path ON reports
WHEN OLD.report_path <> NEW.report_path
BEGIN
    INSERT INTO report_file_cleanup(path) VALUES (OLD.report_path) ON CONFLICT(path) DO NOTHING;
END;

-- Historical rows are already invisible to every API. Preserve each exposed
-- receipt, its sequence and its file, and use the outbox to retire the rest.
DELETE FROM reports AS old WHERE old.id IN (
    SELECT id FROM (
        SELECT id, ROW_NUMBER() OVER (PARTITION BY machine_id, name ORDER BY sequence DESC) AS rank
        FROM reports
    ) ranked WHERE rank > 1
);
CREATE UNIQUE INDEX reports_configuration ON reports(machine_id, name);
DROP INDEX reports_configuration_latest;
DROP INDEX reports_configuration_revision_latest;
