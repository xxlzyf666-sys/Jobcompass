package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func userAdmin(t *testing.T, a *App) *browser {
	t.Helper()
	a.config.BillingAdminKey = strings.Repeat("user-admin-test-", 4)
	b := newBrowser(t, a)
	loginBillingAdmin(t, b)
	return b
}

func TestAdminUsersAuthorizationSearchPaginationAndPrivacy(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	accountAuth(t, b, "register", "team_alpha", "user-admin-password", "", false)
	owner := accountOwner(t, b)
	enableTestBilling(t, a, map[string]int{"diagnosis": 2})
	fundAccount(t, b)
	other := newBrowser(t, a)
	accountAuth(t, other, "register", "team_alpha2", "other-admin-password", "", false)
	fundAccount(t, other)
	for i := 0; i < 31; i++ {
		_, err := a.store.db.Exec(`INSERT INTO billing_accounts(owner_id,username,password_salt,password_hash,recovery_hash,created_at)
 SELECT ?,?,password_salt,password_hash,?,? FROM billing_accounts WHERE owner_id=?`, fmt.Sprintf("%032x", i+1), fmt.Sprintf("seed_user_%02d", i), fmt.Sprintf("seed-recovery-%02d", i), time.Now().Add(-time.Duration(i+1)*time.Hour).Unix(), owner)
		if err != nil {
			t.Fatal(err)
		}
	}
	if b.request("GET", "/api/admin/users", nil).Code != 401 || b.request("GET", "/api/admin/users/"+owner, nil).Code != 401 || b.request("POST", "/api/admin/users/"+owner, AdminUserChange{Action: "note", Note: "unauthorized"}).Code != 401 {
		t.Fatal("ordinary account accessed user management")
	}
	admin := userAdmin(t, a)
	first := billingJSON[AdminUsers](t, admin.request("GET", "/api/admin/users", nil), 200)
	last := billingJSON[AdminUsers](t, admin.request("GET", "/api/admin/users?page=999", nil), 200)
	if first.Total != 33 || first.Stats.Total != 33 || len(first.Users) != 30 || last.Page != 1 || len(last.Users) != 3 {
		t.Fatal("pagination does not match counts", first.Total, len(first.Users), last.Page, len(last.Users))
	}
	seen := map[string]bool{}
	for _, u := range append(first.Users, last.Users...) {
		if seen[u.ID] {
			t.Fatal("duplicate user across pages")
		}
		seen[u.ID] = true
	}
	matched := billingJSON[AdminUsers](t, admin.request("GET", "/api/admin/users?q=TEAM_ALPHA", nil), 200)
	if len(matched.Users) != 2 {
		t.Fatal("case-insensitive search failed")
	}
	if literal := billingJSON[AdminUsers](t, admin.request("GET", "/api/admin/users?q=%25", nil), 200); literal.Total != 0 {
		t.Fatal("search interpreted literal percent as wildcard")
	}
	if admin.request("GET", "/api/admin/users?status=unknown", nil).Code != 400 {
		t.Fatal("invalid filter accepted")
	}
	w := admin.request("GET", "/api/admin/users/"+owner, nil)
	detail := billingJSON[AdminUserDetail](t, w, 200)
	if detail.User.Wallet["diagnosis"].Available != 3 || detail.User.Wallet["diagnosis"].TrialAvailable != 1 || detail.ActiveSessions != 1 || detail.PaidCents != 1990 {
		t.Fatal("user details differ from the account wallet", detail)
	}
	for _, field := range []string{"password", "recovery_hash", "recovery_code", "token_hash", "actor_hash", "data_cipher"} {
		if strings.Contains(w.Body.String(), field) {
			t.Fatal("sensitive field in management response", field)
		}
	}
	orders := billingJSON[struct {
		Orders []BillingOrder `json:"orders"`
	}](t, admin.request("GET", "/api/admin/orders?owner="+owner, nil), 200)
	if len(orders.Orders) != 1 || orders.Orders[0].AccountID != owner {
		t.Fatal("user order filter included another account")
	}
	bad := *admin
	bad.adminCSRF = "wrong-token"
	if bad.request("POST", "/api/admin/users/"+owner, AdminUserChange{Action: "restrict", Note: "测试权限保护", Confirmed: true}).Code != 403 {
		t.Fatal("missing administrator CSRF protection")
	}
	if admin.request("GET", "/api/admin/users/not-an-id", nil).Code != 404 {
		t.Fatal("invalid account ID accepted")
	}
}

func TestAdminUserGiftIsAtomicOnceAndAudited(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	registerExistingAccount(t, b, "legacy_support", "legacy-support-password", false)
	enableTestBilling(t, a, map[string]int{"diagnosis": 2})
	order := fundAccount(t, b)
	owner, ctx := accountOwner(t, b), context.Background()
	change := AdminUserChange{Action: "grant_welcome", Note: "核实为上线前账号，补发体验。", Confirmed: true}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- a.store.ChangeAdminUser(ctx, owner, "test-operator", change, time.Now()) }()
	}
	ok, conflict := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			ok++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatal("simultaneous grants were not serialized", ok, conflict)
	}
	detail, err := a.store.AdminUser(ctx, owner, time.Now())
	if err != nil || detail.User.Revision != 1 || len(detail.Actions) != 1 || detail.Actions[0].Action != "grant_welcome" || detail.Actions[0].Note != change.Note {
		t.Fatal("missing gift audit", detail, err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 3, TrialAvailable: 1})
	for _, kind := range []string{"refine", "tailor", "interview"} {
		wantBalance(t, b, kind, CreditBalance{Available: 1, TrialAvailable: 1})
	}
	change.Revision = 1
	if err = a.store.ChangeAdminUser(ctx, owner, "test-operator", change, time.Now()); err == nil {
		t.Fatal("a second lifetime grant succeeded")
	}
	after, err := a.store.BillingOrder(ctx, order.ID, owner)
	if err != nil || after.AmountCents != order.AmountCents || after.Revision != order.Revision || after.Remaining["diagnosis"] != 2 {
		t.Fatal("gift changed purchased credits", err)
	}
	failing := newBrowser(t, a)
	registerExistingAccount(t, failing, "audit_failure", "audit-failure-password", false)
	if _, err = a.store.db.Exec("CREATE TRIGGER reject_admin_audit BEFORE INSERT ON billing_account_audit BEGIN SELECT RAISE(ABORT,'test audit failure'); END"); err != nil {
		t.Fatal(err)
	}
	change.Revision = 0
	if err = a.store.ChangeAdminUser(ctx, accountOwner(t, failing), "test-operator", change, time.Now()); err == nil {
		t.Fatal("unaudited gift was committed")
	}
	for _, kind := range creditKinds {
		wantBalance(t, failing, kind, CreditBalance{})
	}
}

func TestAdminRestrictionGuardsGenerationAndPreservesAccess(t *testing.T) {
	for _, paid := range []bool{false, true} {
		t.Run(fmt.Sprintf("paid_%v", paid), func(t *testing.T) {
			a, b, id, _ := preparationApp(t)
			accountAuth(t, b, "register", "restricted_user", "restricted-password", "", true)
			var order BillingOrder
			if paid {
				enableTestBilling(t, a, map[string]int{"diagnosis": 1, "refine": 1})
				order = fundAccount(t, b)
			}
			changePreparation(t, b, id, "questions", nil, 202)
			ctx, owner := context.Background(), accountOwner(t, b)
			change := AdminUserChange{Action: "restrict", Note: "异常请求，暂时限制生成。", Confirmed: true}
			if err := a.store.ChangeAdminUser(ctx, owner, "test-operator", change, time.Now()); err != nil {
				t.Fatal(err)
			}
			finishPreparation(t, a, b, id)
			before := wallet(t, b)
			input, _ := fixture(t)
			if w := b.request("POST", "/api/diagnoses", map[string]any{"resume": input.Resume, "jd": input.JD, "role": input.Role, "consent": true, "consent_version": consentVersion}); w.Code != 403 || !strings.Contains(w.Body.String(), "account_restricted") {
				t.Fatal("restricted diagnosis accepted", w.Code, w.Body.String())
			}
			changePreparation(t, b, id, "rewrite", refineAnswers(), 403)
			changePreparation(t, b, id, "tailor", nil, 403)
			changePreparation(t, b, id, "start_interview", map[string]any{"rounds": 3, "focus": "project"}, 403)
			if after := wallet(t, b); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected tasks consumed credits")
			}
			p := readPreparation(t, b, id).Preparation
			p.Versions[0].Name = "仍可手动编辑"
			if err := a.store.SavePreparation(ctx, owner, "test", id, p, nil, a.config, time.Now()); err != nil {
				t.Fatal("restriction blocked manual editing", err)
			}
			if b.request("GET", "/api/diagnoses/"+id, nil).Code != 200 || b.request("GET", "/api/diagnoses/"+id+"/versions/base/export", nil).Code != 200 {
				t.Fatal("restriction blocked material access or export")
			}
			accountAuth(t, b, "login", "restricted_user", "restricted-password", "", false)
			account, err := a.store.BillingAccount(ctx, owner)
			if err != nil || !account.AIRestricted {
				t.Fatal("logging in bypassed restriction", err)
			}
			if paid {
				billingJSON[BillingOrder](t, b.request("POST", "/api/billing/orders/"+order.ID, map[string]any{"action": "request_refund", "revision": order.Revision}), 200)
			}
			change.Action, change.Revision = "restore", 1
			if err = a.store.ChangeAdminUser(ctx, owner, "test-operator", change, time.Now()); err != nil {
				t.Fatal(err)
			}
			changePreparation(t, b, id, "tailor", nil, 202)
			finishPreparation(t, a, b, id)
		})
	}
}

func TestAdminUserNotesSessionRevocationAndStaleChanges(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	accountAuth(t, b, "register", "multi_device", "multi-device-password", "", false)
	second := newBrowser(t, a)
	accountAuth(t, second, "login", "multi_device", "multi-device-password", "", false)
	other := newBrowser(t, a)
	admin, owner := userAdmin(t, a), accountOwner(t, b)
	note := "已核对用户反馈，等待下次跟进。"
	detail := billingJSON[AdminUserDetail](t, admin.request("POST", "/api/admin/users/"+owner, AdminUserChange{Action: "note", Note: note}), 200)
	if detail.Note != note || detail.ActiveSessions != 2 || detail.User.Revision != 1 {
		t.Fatal("note or session count incorrect", detail)
	}
	var noteCipher, auditCipher []byte
	if err := a.store.db.QueryRow("SELECT c.note_cipher,a.data_cipher FROM billing_account_controls c JOIN billing_account_audit a ON a.owner_id=c.owner_id WHERE c.owner_id=?", owner).Scan(&noteCipher, &auditCipher); err != nil || bytes.Contains(noteCipher, []byte(note)) || bytes.Contains(auditCipher, []byte(note)) {
		t.Fatal("management notes stored in plaintext", err)
	}
	if admin.request("POST", "/api/admin/users/"+owner, AdminUserChange{Action: "restrict", Note: "过期页面的请求", Confirmed: true}).Code != 409 {
		t.Fatal("stale control overwrote a newer change")
	}
	detail = billingJSON[AdminUserDetail](t, admin.request("POST", "/api/admin/users/"+owner, AdminUserChange{Action: "revoke_sessions", Revision: 1, Note: "用户反馈设备遗失。", Confirmed: true}), 200)
	if detail.ActiveSessions != 0 || detail.User.Revision != 2 || len(detail.Actions) != 2 || detail.Actions[0].RevokedSessions != 2 {
		t.Fatal("session revocation was not audited", detail)
	}
	if b.request("GET", "/api/billing", nil).Code != 401 || second.request("GET", "/api/billing", nil).Code != 401 || other.request("GET", "/api/billing", nil).Code != 200 {
		t.Fatal("session revocation did not target exactly one user")
	}
	accountAuth(t, other, "login", "multi_device", "multi-device-password", "", false)
	for _, kind := range creditKinds {
		wantBalance(t, other, kind, CreditBalance{Available: 1, TrialAvailable: 1})
	}
}

func TestAccountManagementMigrationPreservesV4WalletAndSessions(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	accountAuth(t, b, "register", "before_user_admin", "migration-test-password", "", false)
	enableTestBilling(t, a, map[string]int{"diagnosis": 2})
	fundAccount(t, b)
	input, _ := fixture(t)
	submit(t, b, input)
	if !a.processOne(context.Background()) {
		t.Fatal("missing diagnosis")
	}
	before := wallet(t, b)
	for _, statement := range []string{"DROP TABLE billing_account_audit", "DROP TABLE billing_account_controls", "DROP INDEX billing_accounts_created", "ALTER TABLE billing_accounts DROP COLUMN last_login_at", "DELETE FROM schema_migrations WHERE version=5"} {
		if _, err := a.store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := a.store.Close(); err != nil {
			t.Fatal(err)
		}
		var err error
		a.store, err = OpenStore(a.config.DataDir, a.config.Key)
		if err != nil {
			t.Fatal(err)
		}
		if after := wallet(t, b); !reflect.DeepEqual(before, after) {
			t.Fatal("v4 migration changed the wallet or session", before, after)
		}
	}
	detail, err := a.store.AdminUser(context.Background(), accountOwner(t, b), time.Now())
	if err != nil || detail.User.AIRestricted || detail.User.Revision != 0 || detail.User.LastLoginAt == 0 || len(detail.Actions) != 0 {
		t.Fatal("unexpected migrated account state", detail, err)
	}
	var version, violations int
	if err = a.store.db.QueryRow("SELECT MAX(version),(SELECT COUNT(*) FROM pragma_foreign_key_check) FROM schema_migrations").Scan(&version, &violations); err != nil || version != 5 || violations != 0 {
		t.Fatal("invalid v5 database", version, violations, err)
	}
}
