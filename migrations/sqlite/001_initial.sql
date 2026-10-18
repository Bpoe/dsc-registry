CREATE TABLE machines (
    id TEXT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL,
    configuration_revision INTEGER NOT NULL DEFAULT 0 CHECK (configuration_revision >= 0),
    created_at TEXT NOT NULL
);

CREATE TABLE configurations (
    machine_id TEXT NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    revision TEXT NOT NULL,
    template_json TEXT NOT NULL CHECK (json_valid(template_json) AND json_type(template_json) = 'object'),
    parameters_json TEXT CHECK (parameters_json IS NULL OR (json_valid(parameters_json) AND json_type(parameters_json) = 'object')),
    content_sha256 BLOB NOT NULL CHECK (length(content_sha256) = 32),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (machine_id, name)
);

CREATE TABLE configuration_status (
    machine_id TEXT NOT NULL,
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
    last_upload_id TEXT NOT NULL,
    PRIMARY KEY (machine_id, name),
    FOREIGN KEY (machine_id, name) REFERENCES configurations(machine_id, name) ON DELETE CASCADE
);

CREATE TABLE admins (
    id TEXT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE api_keys (
    key_id TEXT NOT NULL PRIMARY KEY,
    owner_type TEXT NOT NULL CHECK (owner_type IN ('admin', 'machine')),
    owner_id TEXT NOT NULL,
    secret_hash BLOB NOT NULL CHECK (length(secret_hash) = 32),
    created_at TEXT NOT NULL,
    revoked_at TEXT,
    description TEXT
);
CREATE INDEX api_keys_owner ON api_keys(owner_type, owner_id);

-- The polymorphic owner cannot be represented by a single foreign key.
CREATE TRIGGER api_keys_owner_insert BEFORE INSERT ON api_keys
BEGIN
    SELECT RAISE(ABORT, 'API key owner does not exist')
    WHERE (NEW.owner_type = 'machine' AND NOT EXISTS (SELECT 1 FROM machines WHERE id = NEW.owner_id))
       OR (NEW.owner_type = 'admin' AND NOT EXISTS (SELECT 1 FROM admins WHERE id = NEW.owner_id));
END;

CREATE TRIGGER api_keys_owner_update BEFORE UPDATE OF owner_type, owner_id ON api_keys
BEGIN
    SELECT RAISE(ABORT, 'API key owner does not exist')
    WHERE (NEW.owner_type = 'machine' AND NOT EXISTS (SELECT 1 FROM machines WHERE id = NEW.owner_id))
       OR (NEW.owner_type = 'admin' AND NOT EXISTS (SELECT 1 FROM admins WHERE id = NEW.owner_id));
END;

CREATE TRIGGER machines_delete_keys AFTER DELETE ON machines
BEGIN
    DELETE FROM api_keys WHERE owner_type = 'machine' AND owner_id = OLD.id;
END;

CREATE TRIGGER admins_delete_keys AFTER DELETE ON admins
BEGIN
    DELETE FROM api_keys WHERE owner_type = 'admin' AND owner_id = OLD.id;
END;

CREATE TRIGGER machines_identity_immutable BEFORE UPDATE OF id ON machines
WHEN NEW.id != OLD.id
BEGIN
    SELECT RAISE(ABORT, 'machine identity is immutable');
END;

CREATE TRIGGER admins_identity_immutable BEFORE UPDATE OF id ON admins
WHEN NEW.id != OLD.id
BEGIN
    SELECT RAISE(ABORT, 'admin identity is immutable');
END;
