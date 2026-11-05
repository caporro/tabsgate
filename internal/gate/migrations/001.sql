CREATE TABLE metadata (name TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE connections (
 id TEXT PRIMARY KEY,
 provider TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 secret BLOB NOT NULL,
 word_salt BLOB NOT NULL,
 word_hash BLOB NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL,
 UNIQUE(provider, fingerprint)
);
CREATE TABLE virtual_keys (
 id TEXT PRIMARY KEY,
 connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
 token_hash TEXT NOT NULL UNIQUE,
 prefix TEXT NOT NULL,
 name TEXT NOT NULL,
 max_requests INTEGER NOT NULL CHECK(max_requests > 0),
 token_budget INTEGER NOT NULL CHECK(token_budget > 0),
 expires_at INTEGER NOT NULL,
 allowed_models TEXT NOT NULL,
 max_output_tokens INTEGER NOT NULL CHECK(max_output_tokens > 0),
 requests_used INTEGER NOT NULL DEFAULT 0,
 tokens_used INTEGER NOT NULL DEFAULT 0,
 estimated_requests INTEGER NOT NULL DEFAULT 0,
 revoked INTEGER NOT NULL DEFAULT 0,
 in_flight TEXT,
 revision INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL
);
CREATE INDEX keys_connection ON virtual_keys(connection_id,created_at);
CREATE TABLE requests (
 id TEXT PRIMARY KEY,
 key_id TEXT NOT NULL REFERENCES virtual_keys(id) ON DELETE CASCADE,
 model TEXT NOT NULL,
 state TEXT NOT NULL,
 fallback_tokens INTEGER NOT NULL,
 tokens INTEGER NOT NULL DEFAULT 0,
 estimated INTEGER NOT NULL DEFAULT 0,
 started_at INTEGER NOT NULL,
 finished_at INTEGER
);
CREATE INDEX requests_pending ON requests(state);
