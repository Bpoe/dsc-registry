CREATE TABLE machines (
    id TEXT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL,
    configuration_revision BIGINT NOT NULL DEFAULT 0 CHECK (configuration_revision >= 0),
    created_at TEXT NOT NULL
);

CREATE TABLE configurations (
    machine_id TEXT NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    revision TEXT NOT NULL,
    template_json TEXT NOT NULL CHECK (jsonb_typeof(template_json::jsonb) = 'object'),
    parameters_json TEXT CHECK (parameters_json IS NULL OR jsonb_typeof(parameters_json::jsonb) = 'object'),
    content_sha256 BYTEA NOT NULL CHECK (octet_length(content_sha256) = 32),
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
    secret_hash BYTEA NOT NULL CHECK (octet_length(secret_hash) = 32),
    created_at TEXT NOT NULL,
    revoked_at TEXT,
    description TEXT
);
CREATE INDEX api_keys_owner ON api_keys(owner_type, owner_id);

-- Lock the owner against concurrent deletion while adding a key.
CREATE FUNCTION check_api_key_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_type = 'machine' THEN
        PERFORM id FROM machines WHERE id = NEW.owner_id FOR KEY SHARE;
    ELSIF NEW.owner_type = 'admin' THEN
        PERFORM id FROM admins WHERE id = NEW.owner_id FOR KEY SHARE;
    ELSE
        RAISE EXCEPTION 'invalid API key owner type';
    END IF;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'API key owner does not exist';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER api_keys_owner_insert BEFORE INSERT ON api_keys
    FOR EACH ROW EXECUTE FUNCTION check_api_key_owner();
CREATE TRIGGER api_keys_owner_update BEFORE UPDATE OF owner_type, owner_id ON api_keys
    FOR EACH ROW EXECUTE FUNCTION check_api_key_owner();

CREATE FUNCTION delete_owner_keys() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM api_keys WHERE owner_type = TG_ARGV[0] AND owner_id = OLD.id;
    RETURN OLD;
END;
$$;
CREATE TRIGGER machines_delete_keys AFTER DELETE ON machines
    FOR EACH ROW EXECUTE FUNCTION delete_owner_keys('machine');
CREATE TRIGGER admins_delete_keys AFTER DELETE ON admins
    FOR EACH ROW EXECUTE FUNCTION delete_owner_keys('admin');

CREATE FUNCTION immutable_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id <> OLD.id THEN
        RAISE EXCEPTION 'owner identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER machines_identity_immutable BEFORE UPDATE OF id ON machines
    FOR EACH ROW EXECUTE FUNCTION immutable_identity();
CREATE TRIGGER admins_identity_immutable BEFORE UPDATE OF id ON admins
    FOR EACH ROW EXECUTE FUNCTION immutable_identity();
