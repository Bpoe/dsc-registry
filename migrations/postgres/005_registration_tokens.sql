CREATE TABLE registration_tokens (
    token_hash BYTEA NOT NULL PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    expires_at TEXT NOT NULL
);
