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
CREATE TABLE IF NOT EXISTS preparations (
  diagnosis_id TEXT PRIMARY KEY REFERENCES diagnoses(id) ON DELETE CASCADE,
  revision INTEGER NOT NULL,
  data_cipher BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS preparation_tasks (
  id TEXT PRIMARY KEY,
  diagnosis_id TEXT NOT NULL REFERENCES diagnoses(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  revision INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','running','done','failed')),
  input_cipher BLOB,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  lease_until INTEGER NOT NULL DEFAULT 0,
  worker_token TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  tokens_in INTEGER NOT NULL DEFAULT 0,
  tokens_out INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS preparation_queue ON preparation_tasks(status,next_attempt_at);
CREATE INDEX IF NOT EXISTS preparation_parent ON preparation_tasks(diagnosis_id);
INSERT OR IGNORE INTO schema_migrations(version) VALUES (2);

CREATE TABLE IF NOT EXISTS billing_accounts (
  owner_id TEXT PRIMARY KEY,
  username TEXT UNIQUE NOT NULL,
  password_salt BLOB NOT NULL,
  password_hash BLOB NOT NULL,
  recovery_hash TEXT UNIQUE NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS billing_settings (
  id INTEGER PRIMARY KEY CHECK(id=1),
  revision INTEGER NOT NULL,
  data_cipher BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS billing_qr_codes (
  id TEXT PRIMARY KEY,
  data_cipher BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS billing_orders (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL REFERENCES billing_accounts(owner_id),
  request_key TEXT NOT NULL,
  plan_name TEXT NOT NULL,
  amount_cents INTEGER NOT NULL CHECK(amount_cents>0),
  snapshot_cipher BLOB NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('awaiting_payment','submitted','paid','rejected','cancelled','expired','refunded')),
  claim_cipher BLOB,
  receipt_hash TEXT UNIQUE,
  review_cipher BLOB,
  revision INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  paid_at INTEGER NOT NULL DEFAULT 0,
  refund_requested_at INTEGER NOT NULL DEFAULT 0,
  refund_reviewing INTEGER NOT NULL DEFAULT 0,
  refunded_cents INTEGER NOT NULL DEFAULT 0 CHECK(refunded_cents>=0 AND refunded_cents<=amount_cents),
  UNIQUE(owner_id,request_key)
);
CREATE INDEX IF NOT EXISTS billing_orders_owner ON billing_orders(owner_id,created_at);
CREATE INDEX IF NOT EXISTS billing_orders_queue ON billing_orders(status,created_at);
CREATE TABLE IF NOT EXISTS billing_grants (
  order_id TEXT NOT NULL REFERENCES billing_orders(id),
  kind TEXT NOT NULL CHECK(kind IN ('diagnosis','refine','tailor','interview')),
  units INTEGER NOT NULL CHECK(units>=0),
  PRIMARY KEY(order_id,kind)
);
CREATE TABLE IF NOT EXISTS billing_usage (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL REFERENCES billing_accounts(owner_id),
  order_id TEXT NOT NULL REFERENCES billing_orders(id),
  kind TEXT NOT NULL CHECK(kind IN ('diagnosis','refine','tailor','interview')),
  state TEXT NOT NULL CHECK(state IN ('reserved','consumed','released')),
  parent_id TEXT NOT NULL,
  active_task TEXT NOT NULL UNIQUE,
  completed INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS billing_usage_grant ON billing_usage(order_id,kind,state);
CREATE INDEX IF NOT EXISTS billing_usage_parent ON billing_usage(parent_id,state);
CREATE TABLE IF NOT EXISTS billing_events (
  id INTEGER PRIMARY KEY,
  owner_id TEXT NOT NULL,
  order_id TEXT NOT NULL,
  usage_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  event TEXT NOT NULL,
  units INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS billing_events_owner ON billing_events(owner_id,id);
CREATE TABLE IF NOT EXISTS billing_order_audit (
  id INTEGER PRIMARY KEY,
  order_id TEXT NOT NULL REFERENCES billing_orders(id),
  action TEXT NOT NULL,
  data_cipher BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS billing_refunds (
  id TEXT PRIMARY KEY,
  order_id TEXT NOT NULL REFERENCES billing_orders(id),
  amount_cents INTEGER NOT NULL CHECK(amount_cents>0),
  receipt_hash TEXT UNIQUE NOT NULL,
  data_cipher BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS billing_admin_sessions (
  token_hash TEXT PRIMARY KEY,
  key_hash TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);

-- Credit movement and task completion share the same SQLite transaction,
-- including lease exhaustion, cascading deletion and process restarts.
CREATE TRIGGER IF NOT EXISTS billing_usage_reserved AFTER INSERT ON billing_usage
BEGIN
  INSERT INTO billing_events(owner_id,order_id,usage_id,kind,event,units,created_at)
  VALUES(NEW.owner_id,NEW.order_id,NEW.id,NEW.kind,'reserved',1,NEW.updated_at);
END;
CREATE TRIGGER IF NOT EXISTS billing_usage_changed AFTER UPDATE OF state ON billing_usage
WHEN OLD.state<>NEW.state
BEGIN
  INSERT INTO billing_events(owner_id,order_id,usage_id,kind,event,units,created_at)
  VALUES(NEW.owner_id,NEW.order_id,NEW.id,NEW.kind,NEW.state,1,NEW.updated_at);
END;
CREATE TRIGGER IF NOT EXISTS billing_diagnosis_done AFTER UPDATE OF status ON diagnoses
WHEN NEW.status='done' AND OLD.status<>'done'
BEGIN
  UPDATE billing_usage SET state='consumed',completed=1,updated_at=NEW.updated_at
  WHERE active_task='diagnosis:'||NEW.id AND state='reserved';
END;
CREATE TRIGGER IF NOT EXISTS billing_diagnosis_failed AFTER UPDATE OF status ON diagnoses
WHEN NEW.status='failed' AND OLD.status<>'failed'
BEGIN
  UPDATE billing_usage SET state='released',updated_at=NEW.updated_at
  WHERE active_task='diagnosis:'||NEW.id AND state IN ('reserved','consumed');
END;
CREATE TRIGGER IF NOT EXISTS billing_preparation_done AFTER UPDATE OF status ON preparation_tasks
WHEN NEW.status='done' AND OLD.status<>'done'
BEGIN
  UPDATE billing_usage SET state='consumed',
    completed=CASE WHEN NEW.kind IN ('rewrite','tailor','interview_review') THEN 1 ELSE completed END,
    updated_at=NEW.updated_at
  WHERE active_task='preparation:'||NEW.id AND state IN ('reserved','consumed');
END;
CREATE TRIGGER IF NOT EXISTS billing_preparation_failed AFTER UPDATE OF status ON preparation_tasks
WHEN NEW.status='failed' AND OLD.status<>'failed'
BEGIN
  UPDATE billing_usage SET state='released',updated_at=NEW.updated_at
  WHERE active_task='preparation:'||NEW.id AND state IN ('reserved','consumed');
END;
CREATE TRIGGER IF NOT EXISTS billing_parent_deleted BEFORE DELETE ON diagnoses
BEGIN
  UPDATE billing_usage SET state='released',updated_at=unixepoch()
  WHERE parent_id=OLD.id AND state='reserved';
END;
INSERT OR IGNORE INTO schema_migrations(version) VALUES (3);
