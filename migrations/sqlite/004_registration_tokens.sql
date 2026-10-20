CREATE TABLE registration_tokens (
    token_hash BLOB NOT NULL PRIMARY KEY CHECK (length(token_hash) = 32),
    expires_at TEXT NOT NULL
);
