CREATE TABLE billing_welcome_grants (
  owner_id TEXT NOT NULL REFERENCES billing_accounts(owner_id),
  kind TEXT NOT NULL CHECK(kind IN ('diagnosis','refine','tailor','interview')),
  units INTEGER NOT NULL DEFAULT 1 CHECK(units=1),
  created_at INTEGER NOT NULL,
  PRIMARY KEY(owner_id,kind)
);

-- A NULL order identifies a registration gift. Paid usage retains its order
-- reference, so refunds and receipt checks continue to apply only to purchases.
CREATE TABLE billing_usage_v4 (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL REFERENCES billing_accounts(owner_id),
  order_id TEXT REFERENCES billing_orders(id),
  kind TEXT NOT NULL CHECK(kind IN ('diagnosis','refine','tailor','interview')),
  state TEXT NOT NULL CHECK(state IN ('reserved','consumed','released')),
  parent_id TEXT NOT NULL,
  active_task TEXT NOT NULL UNIQUE,
  completed INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
INSERT INTO billing_usage_v4 SELECT * FROM billing_usage;
DROP TRIGGER billing_usage_reserved;
DROP TRIGGER billing_usage_changed;
DROP TRIGGER billing_diagnosis_done;
DROP TRIGGER billing_diagnosis_failed;
DROP TRIGGER billing_preparation_done;
DROP TRIGGER billing_preparation_failed;
DROP TRIGGER billing_parent_deleted;
DROP TABLE billing_usage;
ALTER TABLE billing_usage_v4 RENAME TO billing_usage;
CREATE INDEX billing_usage_grant ON billing_usage(order_id,kind,state);
CREATE INDEX billing_usage_parent ON billing_usage(parent_id,state);
CREATE INDEX billing_usage_owner_kind ON billing_usage(owner_id,kind,order_id,state);

CREATE TRIGGER billing_usage_reserved AFTER INSERT ON billing_usage
BEGIN
  INSERT INTO billing_events(owner_id,order_id,usage_id,kind,event,units,created_at)
  VALUES(NEW.owner_id,COALESCE(NEW.order_id,''),NEW.id,NEW.kind,'reserved',1,NEW.updated_at);
END;
CREATE TRIGGER billing_usage_changed AFTER UPDATE OF state ON billing_usage
WHEN OLD.state<>NEW.state
BEGIN
  INSERT INTO billing_events(owner_id,order_id,usage_id,kind,event,units,created_at)
  VALUES(NEW.owner_id,COALESCE(NEW.order_id,''),NEW.id,NEW.kind,NEW.state,1,NEW.updated_at);
END;
CREATE TRIGGER billing_diagnosis_done AFTER UPDATE OF status ON diagnoses
WHEN NEW.status='done' AND OLD.status<>'done'
BEGIN
  UPDATE billing_usage SET state='consumed',completed=1,updated_at=NEW.updated_at
  WHERE active_task='diagnosis:'||NEW.id AND state='reserved';
END;
CREATE TRIGGER billing_diagnosis_failed AFTER UPDATE OF status ON diagnoses
WHEN NEW.status='failed' AND OLD.status<>'failed'
BEGIN
  UPDATE billing_usage SET state='released',updated_at=NEW.updated_at
  WHERE active_task='diagnosis:'||NEW.id AND state IN ('reserved','consumed');
END;
CREATE TRIGGER billing_preparation_done AFTER UPDATE OF status ON preparation_tasks
WHEN NEW.status='done' AND OLD.status<>'done'
BEGIN
  UPDATE billing_usage SET state='consumed',
    completed=CASE WHEN NEW.kind IN ('rewrite','tailor','interview_review') THEN 1 ELSE completed END,
    updated_at=NEW.updated_at
  WHERE active_task='preparation:'||NEW.id AND state IN ('reserved','consumed');
END;
CREATE TRIGGER billing_preparation_failed AFTER UPDATE OF status ON preparation_tasks
WHEN NEW.status='failed' AND OLD.status<>'failed'
BEGIN
  UPDATE billing_usage SET state='released',updated_at=NEW.updated_at
  WHERE active_task='preparation:'||NEW.id AND state IN ('reserved','consumed');
END;
CREATE TRIGGER billing_parent_deleted BEFORE DELETE ON diagnoses
BEGIN
  UPDATE billing_usage SET state='released',updated_at=unixepoch()
  WHERE parent_id=OLD.id AND state='reserved';
END;
INSERT INTO schema_migrations(version) VALUES (4);
