CREATE TABLE reports (
    id TEXT NOT NULL PRIMARY KEY,
    machine_id TEXT NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    name TEXT NOT NULL,
    configuration_revision TEXT NOT NULL,
    received_at TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT NOT NULL,
    outcome TEXT NOT NULL,
    operation TEXT,
    input_hash TEXT,
    error_kind TEXT,
    report_path TEXT NOT NULL CHECK (length(report_path) > 0),
    upload_id TEXT NOT NULL,
    result_sha256 BYTEA CHECK (result_sha256 IS NULL OR octet_length(result_sha256) = 32),
    UNIQUE (machine_id, sequence),
    UNIQUE (machine_id, name, upload_id)
);
CREATE INDEX reports_configuration_latest ON reports(machine_id, name, sequence DESC);
CREATE INDEX reports_configuration_revision_latest ON reports(machine_id, name, configuration_revision, sequence DESC);
CREATE INDEX reports_path ON reports(report_path);

INSERT INTO reports
    (id, machine_id, sequence, name, configuration_revision, received_at, started_at,
     finished_at, outcome, operation, input_hash, error_kind, report_path, upload_id)
SELECT gen_random_uuid()::text,
       machine_id, ROW_NUMBER() OVER (PARTITION BY machine_id ORDER BY replace(received_at, 'Z', ''), name),
       name, configuration_revision, received_at, started_at, finished_at, outcome,
       operation, input_hash, error_kind, report_path, last_upload_id
FROM configuration_status;

DROP TABLE configuration_status;
