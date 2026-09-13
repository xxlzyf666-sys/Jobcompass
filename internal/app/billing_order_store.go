package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const billingOrderColumns = `o.id,o.owner_id,a.username,o.plan_name,o.amount_cents,o.status,o.revision,o.created_at,o.updated_at,o.expires_at,o.paid_at,o.refund_requested_at,o.refund_reviewing,o.refunded_cents,o.snapshot_cipher,o.claim_cipher,o.review_cipher`
const billingActiveTasksQuery = `SELECT COUNT(*) FROM billing_usage u WHERE u.order_id=? AND
 (u.state='reserved' OR EXISTS(SELECT 1 FROM diagnoses d WHERE 'diagnosis:'||d.id=u.active_task AND d.status IN ('pending','running'))
 OR EXISTS(SELECT 1 FROM preparation_tasks t WHERE 'preparation:'||t.id=u.active_task AND t.status IN ('pending','running')))`

type billingScanner interface{ Scan(...any) error }

func (s *Store) scanBillingOrder(row billingScanner) (BillingOrder, error) {
	var order BillingOrder
	var snapshot, claim, review []byte
	err := row.Scan(&order.ID, &order.OwnerID, &order.Username, &order.PlanName, &order.AmountCents, &order.Status, &order.Revision, &order.CreatedAt, &order.UpdatedAt, &order.ExpiresAt, &order.PaidAt, &order.RefundRequestedAt, &order.RefundReviewing, &order.RefundedCents, &snapshot, &claim, &review)
	if errors.Is(err, sql.ErrNoRows) {
		return order, ErrNotFound
	}
	if err != nil {
		return order, err
	}
	for _, part := range []struct {
		data []byte
		aad  string
		out  any
	}{{snapshot, "order:", &order.Snapshot}, {claim, "claim:", &order.Claim}, {review, "review:", &order.Review}} {
		if len(part.data) == 0 {
			continue
		}
		plain, e := s.decrypt(part.data, part.aad+order.ID)
		if e != nil {
			return order, e
		}
		if e = json.Unmarshal(plain, part.out); e != nil {
			return order, e
		}
	}
	return order, nil
}

func (s *Store) billingOrder(ctx context.Context, q billingQuery, id, owner string) (BillingOrder, error) {
	return s.scanBillingOrder(q.QueryRowContext(ctx, "SELECT "+billingOrderColumns+" FROM billing_orders o JOIN billing_accounts a ON a.owner_id=o.owner_id WHERE o.id=? AND (?='' OR o.owner_id=?)", id, owner, owner))
}

func (s *Store) BillingOrder(ctx context.Context, id, owner string) (BillingOrder, error) {
	order, err := s.billingOrder(ctx, s.db, id, owner)
	if err != nil {
		return order, err
	}
	order.Remaining = map[string]int{}
	for _, kind := range creditKinds {
		var units, reserved, used int
		err = s.db.QueryRowContext(ctx, `SELECT g.units,
 (SELECT COUNT(*) FROM billing_usage WHERE order_id=g.order_id AND kind=g.kind AND state='reserved'),
 (SELECT COUNT(*) FROM billing_usage WHERE order_id=g.order_id AND kind=g.kind AND state='consumed')
 FROM billing_grants g WHERE g.order_id=? AND g.kind=?`, order.ID, kind).Scan(&units, &reserved, &used)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return order, err
		}
		order.Remaining[kind] = units - reserved - used
		order.Reserved += reserved
	}
	if err = s.db.QueryRowContext(ctx, billingActiveTasksQuery, order.ID).Scan(&order.ActiveTasks); err != nil {
		return order, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,amount_cents,created_at,data_cipher FROM billing_refunds WHERE order_id=? ORDER BY created_at,id", order.ID)
	if err != nil {
		return order, err
	}
	defer rows.Close()
	for rows.Next() {
		var refund BillingRefund
		var id string
		var data []byte
		if err = rows.Scan(&id, &refund.AmountCents, &refund.CreatedAt, &data); err != nil {
			return order, err
		}
		plain, e := s.decrypt(data, "refund:"+id)
		if e != nil {
			return order, e
		}
		if e = json.Unmarshal(plain, &refund.PaymentReview); e != nil {
			return order, e
		}
		order.Refunds = append(order.Refunds, refund)
	}
	return order, rows.Err()
}

func (s *Store) BillingOrders(ctx context.Context, owner, status, search string, page int) ([]BillingOrder, error) {
	search = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(search, "\\", "\\\\"), "%", "\\%"), "_", "\\_")
	rows, err := s.db.QueryContext(ctx, "SELECT "+billingOrderColumns+` FROM billing_orders o JOIN billing_accounts a ON a.owner_id=o.owner_id
 WHERE (?='' OR o.owner_id=?) AND (?='' OR o.status=? OR (?='refund_requested' AND o.refund_requested_at>0))
 AND (?='' OR o.id LIKE ? ESCAPE '\' OR a.username LIKE ? ESCAPE '\')
 ORDER BY o.created_at DESC,o.id DESC LIMIT 30 OFFSET ?`, owner, owner, status, status, status, search, search+"%", search+"%", page*30)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []BillingOrder{}
	for rows.Next() {
		order, e := s.scanBillingOrder(rows)
		if e != nil {
			return nil, e
		}
		// Lists omit the payment instructions and receipt details; the private
		// order view is the authoritative place to inspect them.
		order.Snapshot, order.Claim, order.Review = nil, nil, nil
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (s *Store) orderCipher(value any, aad, id string) ([]byte, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return s.encrypt(plain, aad+id)
}

func (s *Store) auditOrder(ctx context.Context, tx *sql.Tx, id, action string, value any, now time.Time) error {
	data, err := s.orderCipher(value, "order-audit:"+action+":", id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO billing_order_audit(order_id,action,data_cipher,created_at) VALUES(?,?,?,?)", id, action, data, now.Unix())
	return err
}

func (s *Store) CreateBillingOrder(ctx context.Context, owner, ip, planID, requestKey string, revision int64, now time.Time) (string, error) {
	if !validID(requestKey) {
		return "", &billingValidationError{"下单标识无效，请刷新后重试。"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, "SELECT id FROM billing_orders WHERE owner_id=? AND request_key=?", owner, requestKey).Scan(&id)
	if err == nil {
		order, e := s.billingOrder(ctx, tx, id, owner)
		if e != nil {
			return "", e
		}
		if order.Snapshot.Plan.ID != planID {
			return "", ErrConflict
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	settings, err := s.billingSettings(ctx, tx)
	if err != nil {
		return "", err
	}
	if !settings.Enabled {
		return "", ErrBillingClosed
	}
	if settings.Revision != revision {
		return "", ErrConflict
	}
	var account int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM billing_accounts WHERE owner_id=?", owner).Scan(&account); err != nil {
		return "", err
	}
	if account == 0 {
		return "", ErrAccountRequired
	}
	var plan *BillingPlan
	for i := range settings.Plans {
		if settings.Plans[i].ID == planID && settings.Plans[i].Active {
			plan = &settings.Plans[i]
		}
	}
	if plan == nil {
		return "", ErrBillingClosed
	}
	var pending int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM billing_orders WHERE owner_id=? AND (status='submitted' OR (status='awaiting_payment' AND expires_at>?))", owner, now.Unix()).Scan(&pending); err != nil {
		return "", err
	}
	if pending >= 3 {
		return "", &billingValidationError{"已有 3 个订单待处理，请先查看或取消已有订单。"}
	}
	if err = takeQuota(ctx, tx, s.privateHash("order-ip:"+ip), now.Unix()/3600*3600, 12); err != nil {
		return "", err
	}
	if err = takeQuota(ctx, tx, s.privateHash("order-owner:"+owner), now.Unix()/86400*86400, 20); err != nil {
		return "", err
	}
	id, err = randomHex(16)
	if err != nil {
		return "", err
	}
	snapshot := OrderSnapshot{Plan: *plan, Payee: settings.Payee, Channel: settings.Channel, QRCodeID: settings.QRCodeID, Notice: settings.Notice, Support: settings.Support, PolicyVersion: billingPolicyVersion}
	data, err := s.orderCipher(snapshot, "order:", id)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO billing_orders(id,owner_id,request_key,plan_name,amount_cents,snapshot_cipher,status,created_at,updated_at,expires_at) VALUES(?,?,?,?,?,?,'awaiting_payment',?,?,?)`, id, owner, requestKey, plan.Name, plan.PriceCents, data, now.Unix(), now.Unix(), now.Add(2*time.Hour).Unix())
	if err != nil {
		return "", err
	}
	if err = s.auditOrder(ctx, tx, id, "created", map[string]string{"policy_version": billingPolicyVersion}, now); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *Store) ChangeBillingOrder(ctx context.Context, owner, id, action string, revision int64, claim PaymentClaim, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	order, err := s.billingOrder(ctx, tx, id, owner)
	if err != nil {
		return err
	}
	if order.Revision != revision {
		return ErrConflict
	}
	switch action {
	case "claim":
		if order.Status != "awaiting_payment" && order.Status != "expired" && order.Status != "rejected" && order.Status != "cancelled" {
			return ErrConflict
		}
		claim.Receipt, err = normalizeReceipt(claim.Receipt)
		if err != nil {
			return err
		}
		claim.Note = cleanInput(claim.Note)
		if !textLength(claim.Note, 0, 300) {
			return &billingValidationError{"付款说明请控制在 300 字符以内。"}
		}
		data, e := s.orderCipher(claim, "claim:", id)
		if e != nil {
			return e
		}
		_, err = tx.ExecContext(ctx, "UPDATE billing_orders SET status='submitted',claim_cipher=?,review_cipher=NULL,revision=revision+1,updated_at=? WHERE id=?", data, now.Unix(), id)
	case "cancel":
		if order.Status != "awaiting_payment" && order.Status != "rejected" && order.Status != "expired" {
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, "UPDATE billing_orders SET status='cancelled',revision=revision+1,updated_at=? WHERE id=?", now.Unix(), id)
	case "request_refund", "withdraw_refund":
		if order.Status != "paid" || order.RefundReviewing || (action == "request_refund" && order.RefundRequestedAt != 0) || (action == "withdraw_refund" && order.RefundRequestedAt == 0) {
			return ErrConflict
		}
		requested := int64(0)
		if action == "request_refund" {
			requested = now.Unix()
		}
		_, err = tx.ExecContext(ctx, "UPDATE billing_orders SET refund_requested_at=?,revision=revision+1,updated_at=? WHERE id=?", requested, now.Unix(), id)
	default:
		return &billingValidationError{"没有找到这个订单操作。"}
	}
	if err != nil {
		return err
	}
	if err = s.auditOrder(ctx, tx, id, action, claim, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ConfirmBillingOrder(ctx context.Context, id string, revision int64, amount int64, review PaymentReview, now time.Time) error {
	receipt, err := normalizeReceipt(review.Receipt)
	if err != nil {
		return err
	}
	review.Receipt, review.Note = receipt, cleanInput(review.Note)
	if !textLength(review.Note, 0, 600) {
		return &billingValidationError{"核款备注请控制在 600 字符以内。"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	order, err := s.billingOrder(ctx, tx, id, "")
	if err != nil {
		return err
	}
	if amount != order.AmountCents {
		return &billingValidationError{"实际收款金额与订单金额不一致，请核对后再确认。"}
	}
	receiptHash := s.privateHash("payment:" + order.Snapshot.Channel + ":" + receipt)
	var receivedBy string
	err = tx.QueryRowContext(ctx, "SELECT id FROM billing_orders WHERE receipt_hash=?", receiptHash).Scan(&receivedBy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if receivedBy != "" {
		if receivedBy == id && (order.Status == "paid" || order.Status == "refunded") {
			return nil // Retried confirmation after a lost response.
		}
		return ErrReceiptUsed
	}
	if order.Status != "submitted" || order.Revision != revision {
		return ErrConflict
	}
	data, err := s.orderCipher(review, "review:", id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE billing_orders SET status='paid',receipt_hash=?,review_cipher=?,paid_at=?,updated_at=?,revision=revision+1 WHERE id=?", receiptHash, data, now.Unix(), now.Unix(), id)
	if err != nil {
		return err
	}
	for _, kind := range creditKinds {
		units := order.Snapshot.Plan.Credits[kind]
		if units == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO billing_grants(order_id,kind,units) VALUES(?,?,?)", id, kind, units); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO billing_events(owner_id,order_id,kind,event,units,created_at) VALUES(?,?,?,'granted',?,?)", order.OwnerID, id, kind, units, now.Unix()); err != nil {
			return err
		}
	}
	if err = s.auditOrder(ctx, tx, id, "confirmed", review, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RejectBillingOrder(ctx context.Context, id string, revision int64, note string, refund bool, now time.Time) error {
	note = cleanInput(note)
	if !textLength(note, 4, 600) {
		return &billingValidationError{"请填写 4—600 字符的核对结果，方便用户补充信息。"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	order, err := s.billingOrder(ctx, tx, id, "")
	if err != nil {
		return err
	}
	if order.Revision != revision || (!refund && order.Status != "submitted") || (refund && (order.Status != "paid" || order.RefundRequestedAt == 0)) {
		return ErrConflict
	}
	review := PaymentReview{Note: note}
	if refund && order.Review != nil {
		review.Receipt = order.Review.Receipt
	}
	data, err := s.orderCipher(review, "review:", id)
	if err != nil {
		return err
	}
	status, action := "rejected", "rejected"
	if refund {
		status, action = "paid", "refund_declined"
	}
	_, err = tx.ExecContext(ctx, "UPDATE billing_orders SET status=?,review_cipher=?,refund_requested_at=0,refund_reviewing=0,revision=revision+1,updated_at=? WHERE id=?", status, data, now.Unix(), id)
	if err != nil {
		return err
	}
	if err = s.auditOrder(ctx, tx, id, action, review, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RefundBillingOrder(ctx context.Context, id string, revision int64, amount int64, review PaymentReview, now time.Time) error {
	receipt, err := normalizeReceipt(review.Receipt)
	if err != nil {
		return err
	}
	review.Receipt, review.Note = receipt, cleanInput(review.Note)
	if !textLength(review.Note, 4, 600) {
		return &billingValidationError{"请填写实际退款说明（4—600 字符）。"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	order, err := s.billingOrder(ctx, tx, id, "")
	if err != nil {
		return err
	}
	receiptHash := s.privateHash("refund:" + order.Snapshot.Channel + ":" + receipt)
	var previousOrder string
	var previousAmount int64
	err = tx.QueryRowContext(ctx, "SELECT order_id,amount_cents FROM billing_refunds WHERE receipt_hash=?", receiptHash).Scan(&previousOrder, &previousAmount)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previousOrder != "" {
		if previousOrder == id && previousAmount == amount {
			return nil
		}
		return ErrReceiptUsed
	}
	if order.Status != "paid" || !order.RefundReviewing || order.Revision != revision {
		return ErrConflict
	}
	if amount <= 0 || amount > order.AmountCents-order.RefundedCents {
		return &billingValidationError{"退款金额必须大于 0，且不能超过订单尚未退款的金额。"}
	}
	var active int
	err = tx.QueryRowContext(ctx, billingActiveTasksQuery, id).Scan(&active)
	if err != nil {
		return err
	}
	if active != 0 {
		return &billingValidationError{"此订单还有生成任务在执行，请等待完成或取消任务后再登记退款。"}
	}
	for _, kind := range creditKinds {
		var units, used int
		err = tx.QueryRowContext(ctx, "SELECT units,(SELECT COUNT(*) FROM billing_usage WHERE order_id=? AND kind=? AND state='consumed') FROM billing_grants WHERE order_id=? AND kind=?", id, kind, id, kind).Scan(&units, &used)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE billing_grants SET units=? WHERE order_id=? AND kind=?", used, id, kind); err != nil {
			return err
		}
		if units > used {
			if _, err = tx.ExecContext(ctx, "INSERT INTO billing_events(owner_id,order_id,kind,event,units,created_at) VALUES(?,?,?,'revoked',?,?)", order.OwnerID, id, kind, units-used, now.Unix()); err != nil {
				return err
			}
		}
	}
	refundID, err := randomHex(16)
	if err != nil {
		return err
	}
	data, err := s.orderCipher(review, "refund:", refundID)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO billing_refunds(id,order_id,amount_cents,receipt_hash,data_cipher,created_at) VALUES(?,?,?,?,?,?)", refundID, id, amount, receiptHash, data, now.Unix()); err != nil {
		return err
	}
	status := "paid"
	if order.RefundedCents+amount == order.AmountCents {
		status = "refunded"
	}
	_, err = tx.ExecContext(ctx, "UPDATE billing_orders SET status=?,refunded_cents=refunded_cents+?,refund_requested_at=0,refund_reviewing=0,revision=revision+1,updated_at=? WHERE id=?", status, amount, now.Unix(), id)
	if err != nil {
		return err
	}
	if err = s.auditOrder(ctx, tx, id, "refunded", map[string]any{"amount_cents": amount, "review": review}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginBillingRefund(ctx context.Context, id string, revision int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	order, err := s.billingOrder(ctx, tx, id, "")
	if err != nil {
		return err
	}
	if order.Status != "paid" || order.Revision != revision {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, "UPDATE billing_orders SET refund_requested_at=?,refund_reviewing=1,revision=revision+1,updated_at=? WHERE id=?", now.Unix(), now.Unix(), id)
	if err != nil {
		return err
	}
	if err = s.auditOrder(ctx, tx, id, "refund_reviewing", map[string]bool{"credits_held": true}, now); err != nil {
		return err
	}
	return tx.Commit()
}
