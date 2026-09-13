package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type billingQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) billingSettings(ctx context.Context, q billingQuery) (BillingSettings, error) {
	var data []byte
	var revision int64
	err := q.QueryRowContext(ctx, "SELECT revision,data_cipher FROM billing_settings WHERE id=1").Scan(&revision, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultBillingSettings(), nil
	}
	if err != nil {
		return BillingSettings{}, err
	}
	plain, err := s.decrypt(data, "billing-settings")
	if err != nil {
		return BillingSettings{}, err
	}
	var settings BillingSettings
	if err = json.Unmarshal(plain, &settings); err != nil {
		return settings, err
	}
	settings.Revision = revision
	return settings, nil
}

func (s *Store) BillingSettings(ctx context.Context) (BillingSettings, error) {
	return s.billingSettings(ctx, s.db)
}

func (s *Store) SaveBillingSettings(ctx context.Context, settings BillingSettings) error {
	if err := validateBillingSettings(&settings); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	previous, err := s.billingSettings(ctx, tx)
	if err != nil {
		return err
	}
	if previous.Revision != settings.Revision {
		return ErrConflict
	}
	if settings.QRCodeID != "" {
		var exists int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM billing_qr_codes WHERE id=?", settings.QRCodeID).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return &billingValidationError{"收款码不存在，请重新上传。"}
		}
	}
	settings.Revision++
	plain, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	data, err := s.encrypt(plain, "billing-settings")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO billing_settings(id,revision,data_cipher) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,data_cipher=excluded.data_cipher`, settings.Revision, data)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveBillingQRCode(ctx context.Context, png []byte, now time.Time) (string, error) {
	id := digest(string(png))
	data, err := s.encrypt(png, "billing-qr:"+id)
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO billing_qr_codes(id,data_cipher,created_at) VALUES(?,?,?)", id, data, now.Unix())
	return id, err
}

func (s *Store) BillingQRCode(ctx context.Context, id string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, "SELECT data_cipher FROM billing_qr_codes WHERE id=?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.decrypt(data, "billing-qr:"+id)
}

func (s *Store) BillingAccount(ctx context.Context, owner string) (*BillingAccount, error) {
	var account BillingAccount
	err := s.db.QueryRowContext(ctx, "SELECT owner_id,username,created_at FROM billing_accounts WHERE owner_id=?", owner).Scan(&account.ID, &account.Username, &account.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &account, nil
}

func (s *Store) BillingWallet(ctx context.Context, owner string) (map[string]CreditBalance, []BillingEvent, error) {
	balances := map[string]CreditBalance{}
	for _, kind := range creditKinds {
		balances[kind] = CreditBalance{}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT g.kind,
 SUM(CASE WHEN o.status='paid' AND o.refund_requested_at=0 THEN g.units-COALESCE(u.reserved,0)-COALESCE(u.used,0) ELSE 0 END),
 SUM(COALESCE(u.reserved,0)), SUM(COALESCE(u.used,0)),
 SUM(CASE WHEN o.refund_requested_at>0 THEN g.units-COALESCE(u.reserved,0)-COALESCE(u.used,0) ELSE 0 END)
 FROM billing_grants g JOIN billing_orders o ON o.id=g.order_id
 LEFT JOIN (SELECT order_id,kind,SUM(state='reserved') reserved,SUM(state='consumed') used FROM billing_usage WHERE owner_id=? GROUP BY order_id,kind) u ON u.order_id=g.order_id AND u.kind=g.kind
 WHERE o.owner_id=? GROUP BY g.kind`, owner, owner)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var kind string
		var balance CreditBalance
		if err = rows.Scan(&kind, &balance.Available, &balance.Reserved, &balance.Used, &balance.OnHold); err != nil {
			rows.Close()
			return nil, nil, err
		}
		balances[kind] = balance
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT id,order_id,kind,event,units,created_at FROM billing_events WHERE owner_id=? ORDER BY id DESC LIMIT 60", owner)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	events := []BillingEvent{}
	for rows.Next() {
		var event BillingEvent
		if err = rows.Scan(&event.ID, &event.OrderID, &event.Kind, &event.Event, &event.Units, &event.CreatedAt); err != nil {
			return nil, nil, err
		}
		events = append(events, event)
	}
	return balances, events, rows.Err()
}

// Account balance, reservation and task insertion are guarded by one transaction.
// A continuation without a reservation belongs to an earlier free round.
func (s *Store) reserveBillingCredit(ctx context.Context, tx *sql.Tx, owner, operation, task, parent, kind string, first, finishRefinement bool, now time.Time) error {
	settings, err := s.billingSettings(ctx, tx)
	if err != nil {
		return err
	}
	var previousOwner, state string
	var completed int
	err = tx.QueryRowContext(ctx, "SELECT owner_id,state,completed FROM billing_usage WHERE id=?", operation).Scan(&previousOwner, &state, &completed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	exists := err == nil
	if !exists && (!first || !settings.Enabled) {
		return nil
	}
	if exists {
		if previousOwner != owner {
			return ErrBillingAccess
		}
		var orderStatus string
		var refundPending, refunded int64
		if err = tx.QueryRowContext(ctx, "SELECT o.status,o.refund_requested_at,o.refunded_cents FROM billing_orders o JOIN billing_usage u ON u.order_id=o.id WHERE u.id=?", operation).Scan(&orderStatus, &refundPending, &refunded); err != nil {
			return err
		}
		if state != "released" && (orderStatus == "refunded" || refundPending != 0 || refunded > 0) {
			return &billingValidationError{"本轮关联的订单正在处理退款或已经退款，请先查看订单。"}
		}
		if finishRefinement && completed != 0 {
			return ErrRoundCompleted
		}
		if state != "released" {
			_, err = tx.ExecContext(ctx, "UPDATE billing_usage SET active_task=?,updated_at=? WHERE id=?", task, now.Unix(), operation)
			return err
		}
	}
	if !settings.Enabled {
		return nil // A failed, already-refunded round may be retried for free.
	}
	var account int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM billing_accounts WHERE owner_id=?", owner).Scan(&account); err != nil {
		return err
	}
	if account == 0 {
		return ErrAccountRequired
	}
	var order string
	err = tx.QueryRowContext(ctx, `SELECT g.order_id FROM billing_grants g JOIN billing_orders o ON o.id=g.order_id
 WHERE o.owner_id=? AND o.status='paid' AND o.refund_requested_at=0 AND g.kind=?
 AND g.units>(SELECT COUNT(*) FROM billing_usage u WHERE u.order_id=g.order_id AND u.kind=g.kind AND u.state IN ('reserved','consumed'))
 ORDER BY o.paid_at,o.id LIMIT 1`, owner, kind).Scan(&order)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCreditRequired
	}
	if err != nil {
		return err
	}
	if exists {
		_, err = tx.ExecContext(ctx, "UPDATE billing_usage SET order_id=?,state='reserved',active_task=?,completed=0,updated_at=? WHERE id=?", order, task, now.Unix(), operation)
	} else {
		_, err = tx.ExecContext(ctx, "INSERT INTO billing_usage(id,owner_id,order_id,kind,state,parent_id,active_task,created_at,updated_at) VALUES(?,?,?,?,'reserved',?,?,?,?)", operation, owner, order, kind, parent, task, now.Unix(), now.Unix())
	}
	return err
}

func (s *Store) reservePreparationCredit(ctx context.Context, tx *sql.Tx, owner, parent, taskID string, input PreparationInput, now time.Time) error {
	key, task := "", "preparation:"+taskID
	switch input.Kind {
	case "questions", "rewrite":
		if input.Version.RefinementID == "" {
			return nil // Questions saved before billing was introduced.
		}
		key = "refine:" + parent + ":" + input.Version.RefinementID
		return s.reserveBillingCredit(ctx, tx, owner, key, task, parent, "refine", input.Kind == "questions", input.Kind == "rewrite", now)
	case "tailor":
		return s.reserveBillingCredit(ctx, tx, owner, "tailor:"+taskID, task, parent, "tailor", true, false, now)
	case "interview_start", "interview_answer", "interview_review":
		if input.Interview == nil {
			return fmt.Errorf("missing interview")
		}
		if input.Kind == "interview_start" && input.Interview.Rounds > 5 {
			settings, err := s.billingSettings(ctx, tx)
			if err != nil {
				return err
			}
			if settings.Enabled {
				return ErrPaidRounds
			}
		}
		key = "interview:" + parent + ":" + input.Interview.ID
		return s.reserveBillingCredit(ctx, tx, owner, key, task, parent, "interview", input.Kind == "interview_start", false, now)
	}
	return fmt.Errorf("unknown preparation billing action")
}
