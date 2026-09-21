CREATE TABLE device_pairings (
    code_hash BLOB PRIMARY KEY CHECK (length(code_hash) = 32),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    redeemed_at TEXT,
    token_id TEXT
);
