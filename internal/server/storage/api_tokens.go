package storage

const apiTokensSchema = `
CREATE TABLE api_tokens (
 id TEXT PRIMARY KEY, name TEXT NOT NULL CHECK(length(CAST(name AS BLOB)) BETWEEN 1 AND 100),
 token_hash BLOB NOT NULL UNIQUE CHECK(length(token_hash)=32),
 prefix TEXT NOT NULL CHECK(length(prefix)=8),
 created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 created_at TEXT NOT NULL, expires_at TEXT, last_used_at TEXT, revoked_at TEXT
);
CREATE INDEX api_tokens_creator ON api_tokens(created_by);
CREATE INDEX api_tokens_revoked ON api_tokens(revoked_at,id) WHERE revoked_at IS NOT NULL;
CREATE INDEX api_tokens_expired ON api_tokens(expires_at,id) WHERE expires_at IS NOT NULL;
`
