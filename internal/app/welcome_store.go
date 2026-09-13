package app

import (
	"context"
	"database/sql"
	_ "embed"
	"time"
)

//go:embed welcome-migration.sql
var welcomeMigration string

func (s *Store) migrateWelcomeCredits(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var installed int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version=4").Scan(&installed); err != nil {
		return err
	}
	if installed == 0 {
		if _, err = tx.ExecContext(ctx, welcomeMigration); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Registration and all four gifts commit together. The primary key keeps the
// lifetime grant idempotent; signing in or recovering an account never grants it.
func (s *Store) grantWelcomeCredits(ctx context.Context, tx *sql.Tx, owner string, now time.Time) error {
	for _, kind := range creditKinds {
		result, err := tx.ExecContext(ctx, "INSERT INTO billing_welcome_grants(owner_id,kind,created_at) VALUES(?,?,?) ON CONFLICT(owner_id,kind) DO NOTHING", owner, kind, now.Unix())
		if err != nil {
			return err
		}
		added, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if added == 1 {
			if _, err = tx.ExecContext(ctx, "INSERT INTO billing_events(owner_id,order_id,kind,event,units,created_at) VALUES(?,'',?,'welcome_granted',1,?)", owner, kind, now.Unix()); err != nil {
				return err
			}
		}
	}
	return nil
}
