CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY);
CREATE TABLE IF NOT EXISTS metadata (name TEXT PRIMARY KEY, value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS diagnoses (
  id TEXT PRIMARY KEY,
  dedup_hash TEXT NOT NULL,
  recovery_hash TEXT UNIQUE NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','running','done','failed')),
  input_cipher BLOB NOT NULL,
  report_cipher BLOB,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  lease_until INTEGER NOT NULL DEFAULT 0,
  worker_token TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  tokens_in INTEGER NOT NULL DEFAULT 0,
  tokens_out INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS diagnoses_queue ON diagnoses(status,next_attempt_at);
CREATE INDEX IF NOT EXISTS diagnoses_expiry ON diagnoses(expires_at);
CREATE INDEX IF NOT EXISTS diagnoses_dedup ON diagnoses(dedup_hash);
CREATE TABLE IF NOT EXISTS diagnosis_access (
  diagnosis_id TEXT NOT NULL REFERENCES diagnoses(id) ON DELETE CASCADE,
  owner_id TEXT NOT NULL,
  PRIMARY KEY(diagnosis_id,owner_id)
);
CREATE INDEX IF NOT EXISTS access_owner ON diagnosis_access(owner_id);
CREATE TABLE IF NOT EXISTS rate_limits (
  key TEXT NOT NULL,
  window_start INTEGER NOT NULL,
  count INTEGER NOT NULL,
  PRIMARY KEY(key,window_start)
);
INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);
