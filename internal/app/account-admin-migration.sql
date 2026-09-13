ALTER TABLE billing_accounts ADD COLUMN last_login_at INTEGER NOT NULL DEFAULT 0;
UPDATE billing_accounts SET last_login_at=COALESCE(
  (SELECT MAX(expires_at)-2592000 FROM sessions WHERE owner_id=billing_accounts.owner_id),0);
CREATE INDEX billing_accounts_created ON billing_accounts(created_at,owner_id);

CREATE TABLE billing_account_controls (
  owner_id TEXT PRIMARY KEY REFERENCES billing_accounts(owner_id),
  revision INTEGER NOT NULL CHECK(revision>0),
  ai_restricted INTEGER NOT NULL DEFAULT 0 CHECK(ai_restricted IN (0,1)),
  note_cipher BLOB,
  updated_at INTEGER NOT NULL
);
CREATE TABLE billing_account_audit (
  id INTEGER PRIMARY KEY,
  owner_id TEXT NOT NULL REFERENCES billing_accounts(owner_id),
  actor_hash TEXT NOT NULL,
  action TEXT NOT NULL CHECK(action IN ('note','grant_welcome','restrict','restore','revoke_sessions')),
  data_cipher BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX billing_account_audit_owner ON billing_account_audit(owner_id,id);
INSERT INTO schema_migrations(version) VALUES (5);
