package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func (s *Store) migrateAccountAdmin(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var installed int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version=5").Scan(&installed); err != nil {
		return err
	}
	if installed == 0 {
		if _, err = tx.ExecContext(ctx, accountAdminMigration); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) allowAccountAI(ctx context.Context, q billingQuery, owner string) error {
	var restricted int
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM billing_account_controls WHERE owner_id=? AND ai_restricted=1)", owner).Scan(&restricted); err != nil {
		return err
	}
	if restricted != 0 {
		return ErrAIRestricted
	}
	return nil
}

const adminUserColumns = `a.owner_id,a.username,a.created_at,
 EXISTS(SELECT 1 FROM billing_welcome_grants g WHERE g.owner_id=a.owner_id),
 COALESCE(c.ai_restricted,0),a.last_login_at,COALESCE(c.revision,0),
 (SELECT COUNT(*) FROM billing_orders o WHERE o.owner_id=a.owner_id)`

func scanAdminUser(row billingScanner) (AdminUser, error) {
	var u AdminUser
	err := row.Scan(&u.ID, &u.Username, &u.CreatedAt, &u.WelcomeGranted, &u.AIRestricted, &u.LastLoginAt, &u.Revision, &u.OrderCount)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return u, err
}

func (s *Store) AdminUsers(ctx context.Context, search, status string, page int, now time.Time) (AdminUsers, error) {
	result := AdminUsers{Users: []AdminUser{}, PageSize: adminUserPageSize}
	search = strings.ToLower(strings.TrimSpace(search))
	if len(search) > 64 || (status != "" && status != "active" && status != "restricted") {
		return result, &billingValidationError{"请检查搜索内容和账号状态筛选。"}
	}
	if page < 0 {
		page = 0
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(created_at>=?),0),
 (SELECT COUNT(*) FROM billing_account_controls WHERE ai_restricted=1) FROM billing_accounts`, now.Add(-7*24*time.Hour).Unix()).Scan(&result.Stats.Total, &result.Stats.NewWeek, &result.Stats.Restricted); err != nil {
		return result, err
	}
	pattern := "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(search) + "%"
	where := ` FROM billing_accounts a LEFT JOIN billing_account_controls c ON c.owner_id=a.owner_id
 WHERE (a.username LIKE ? ESCAPE '\' OR a.owner_id LIKE ? ESCAPE '\')
 AND (?='' OR (?='restricted' AND COALESCE(c.ai_restricted,0)=1) OR (?='active' AND COALESCE(c.ai_restricted,0)=0))`
	args := []any{pattern, pattern, status, status, status}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*)"+where, args...).Scan(&result.Total); err != nil {
		return result, err
	}
	lastPage := 0
	if result.Total > 0 {
		lastPage = (result.Total - 1) / adminUserPageSize
	}
	if page > lastPage {
		page = lastPage
	}
	result.Page = page
	rows, err := s.db.QueryContext(ctx, "SELECT "+adminUserColumns+where+" ORDER BY a.created_at DESC,a.owner_id DESC LIMIT ? OFFSET ?", append(args, adminUserPageSize, page*adminUserPageSize)...)
	if err != nil {
		return result, err
	}
	owners := []string{}
	for rows.Next() {
		u, e := scanAdminUser(rows)
		if e != nil {
			rows.Close()
			return result, e
		}
		result.Users = append(result.Users, u)
		owners = append(owners, u.ID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	wallets, err := s.billingWallets(ctx, owners)
	if err != nil {
		return result, err
	}
	for i := range result.Users {
		result.Users[i].Wallet = wallets[result.Users[i].ID]
	}
	return result, nil
}

func (s *Store) AdminUser(ctx context.Context, owner string, now time.Time) (AdminUserDetail, error) {
	detail := AdminUserDetail{Actions: []AdminUserAction{}}
	if !validID(owner) {
		return detail, ErrNotFound
	}
	user, err := scanAdminUser(s.db.QueryRowContext(ctx, "SELECT "+adminUserColumns+" FROM billing_accounts a LEFT JOIN billing_account_controls c ON c.owner_id=a.owner_id WHERE a.owner_id=?", owner))
	if err != nil {
		return detail, err
	}
	detail.User = user
	if detail.User.Wallet, detail.Events, err = s.BillingWallet(ctx, owner); err != nil {
		return detail, err
	}
	if detail.Orders, err = s.BillingOrders(ctx, owner, "", "", 0); err != nil {
		return detail, err
	}
	for i := range detail.Orders {
		detail.Orders[i].AccountID = owner
	}
	if err = s.db.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM sessions WHERE owner_id=? AND expires_at>?),
 (SELECT COUNT(*) FROM diagnoses d JOIN diagnosis_access a ON a.diagnosis_id=d.id WHERE a.owner_id=? AND d.expires_at>?),
 (SELECT COALESCE(SUM(amount_cents-refunded_cents),0) FROM billing_orders WHERE owner_id=? AND paid_at>0)`, owner, now.Unix(), owner, now.Unix(), owner).Scan(&detail.ActiveSessions, &detail.ReportCount, &detail.PaidCents); err != nil {
		return detail, err
	}
	var note []byte
	err = s.db.QueryRowContext(ctx, "SELECT note_cipher FROM billing_account_controls WHERE owner_id=?", owner).Scan(&note)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return detail, err
	}
	if len(note) > 0 {
		plain, e := s.decrypt(note, "account-note:"+owner)
		if e != nil {
			return detail, e
		}
		detail.Note = string(plain)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,action,created_at,data_cipher FROM billing_account_audit WHERE owner_id=? ORDER BY id DESC LIMIT 30", owner)
	if err != nil {
		return detail, err
	}
	defer rows.Close()
	for rows.Next() {
		var action AdminUserAction
		var data []byte
		if err = rows.Scan(&action.ID, &action.Action, &action.CreatedAt, &data); err != nil {
			return detail, err
		}
		plain, e := s.decrypt(data, "account-audit:"+action.Action+":"+owner)
		if e != nil {
			return detail, e
		}
		if e = json.Unmarshal(plain, &action); e != nil {
			return detail, e
		}
		detail.Actions = append(detail.Actions, action)
	}
	return detail, rows.Err()
}

func (s *Store) ChangeAdminUser(ctx context.Context, owner, actor string, change AdminUserChange, now time.Time) error {
	if !validID(owner) {
		return ErrNotFound
	}
	change.Note = cleanInput(change.Note)
	if !textLength(change.Note, 0, 600) || actor == "" {
		return &billingValidationError{"管理备注或操作原因不能超过 600 个字符。"}
	}
	if change.Action != "note" && (!change.Confirmed || !textLength(change.Note, 4, 600)) {
		return &billingValidationError{"请填写至少 4 个字符的操作原因，并确认本次操作。"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision int64
	var restricted bool
	var note []byte
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(c.revision,0),COALESCE(c.ai_restricted,0),c.note_cipher
 FROM billing_accounts a LEFT JOIN billing_account_controls c ON c.owner_id=a.owner_id WHERE a.owner_id=?`, owner).Scan(&revision, &restricted, &note)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if revision != change.Revision {
		return ErrConflict
	}
	entry := AdminUserAction{Note: change.Note}
	switch change.Action {
	case "note":
		if note, err = s.encrypt([]byte(change.Note), "account-note:"+owner); err != nil {
			return err
		}
	case "grant_welcome":
		var granted int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM billing_welcome_grants WHERE owner_id=?", owner).Scan(&granted); err != nil {
			return err
		}
		if granted != 0 {
			return &billingValidationError{"该账号已领取过新用户体验，不能重复赠送。"}
		}
		if err = s.grantWelcomeCredits(ctx, tx, owner, now); err != nil {
			return err
		}
	case "restrict", "restore":
		restricted = change.Action == "restrict"
	case "revoke_sessions":
		result, e := tx.ExecContext(ctx, "DELETE FROM sessions WHERE owner_id=?", owner)
		if e != nil {
			return e
		}
		if entry.RevokedSessions, err = result.RowsAffected(); err != nil {
			return err
		}
	default:
		return &billingValidationError{"没有找到这个用户管理操作。"}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO billing_account_controls(owner_id,revision,ai_restricted,note_cipher,updated_at) VALUES(?,?,?,?,?)
 ON CONFLICT(owner_id) DO UPDATE SET revision=excluded.revision,ai_restricted=excluded.ai_restricted,note_cipher=excluded.note_cipher,updated_at=excluded.updated_at`, owner, revision+1, restricted, note, now.Unix()); err != nil {
		return err
	}
	// Only action details are encrypted here; row identity and time come from
	// the audit table, so ciphertext cannot replace the record's metadata.
	data, err := s.orderCipher(map[string]any{"note": entry.Note, "revoked_sessions": entry.RevokedSessions}, "account-audit:"+change.Action+":", owner)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO billing_account_audit(owner_id,actor_hash,action,data_cipher,created_at) VALUES(?,?,?,?,?)", owner, actor, change.Action, data, now.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
