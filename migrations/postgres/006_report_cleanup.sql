CREATE TABLE report_file_cleanup (
    path TEXT PRIMARY KEY,
    enqueued_at BIGINT NOT NULL DEFAULT (floor(extract(epoch FROM statement_timestamp()))::bigint),
    available_at BIGINT NOT NULL DEFAULT (floor(extract(epoch FROM statement_timestamp()))::bigint),
    claim_token TEXT,
    attempts INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX report_file_cleanup_due ON report_file_cleanup(available_at, path);

CREATE FUNCTION enqueue_retired_report() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO report_file_cleanup(path) VALUES (OLD.report_path) ON CONFLICT(path) DO NOTHING;
    RETURN NULL;
END;
$$;

CREATE TRIGGER reports_cleanup_delete AFTER DELETE ON reports
FOR EACH ROW EXECUTE FUNCTION enqueue_retired_report();

CREATE TRIGGER reports_cleanup_replace AFTER UPDATE OF report_path ON reports
FOR EACH ROW WHEN (OLD.report_path <> NEW.report_path) EXECUTE FUNCTION enqueue_retired_report();

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
