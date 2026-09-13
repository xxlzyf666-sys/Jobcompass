package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWelcomeRegistrationOnceAcrossLoginRecoveryAndRestart(t *testing.T) {
	a := testApp(t, nil)
	enableTestBilling(t, a, map[string]int{"diagnosis": 1})
	b := newBrowser(t, a)
	recovery := accountAuth(t, b, "register", "welcome_user", "welcome-test-password", "", false)
	for _, kind := range creditKinds {
		wantBalance(t, b, kind, CreditBalance{Available: 1, TrialAvailable: 1})
	}
	owner := accountOwner(t, b)
	account, err := a.store.BillingAccount(context.Background(), owner)
	if err != nil || !account.WelcomeGranted {
		t.Fatal("registration gift not visible", err)
	}
	input, _ := fixture(t)
	submit(t, b, input)
	if !a.processOne(context.Background()) {
		t.Fatal("trial diagnosis did not run")
	}
	duplicate := newBrowser(t, a)
	if w := duplicate.request("POST", "/api/account/register", map[string]any{"username": "WELCOME_USER", "password": "another-password"}); w.Code != 409 {
		t.Fatal("duplicate registration was accepted", w.Code)
	}
	accountAuth(t, duplicate, "login", "welcome_user", "welcome-test-password", "", false)
	accountAuth(t, b, "reset", "welcome_user", "welcome-new-password", recovery, false)
	if duplicate.request("GET", "/api/billing", nil).Code != 401 {
		t.Fatal("recovery did not revoke the other session")
	}
	if err = a.store.Close(); err != nil {
		t.Fatal(err)
	}
	a.store, err = OpenStore(a.config.DataDir, a.config.Key)
	if err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Used: 1})
	for _, kind := range []string{"refine", "tailor", "interview"} {
		wantBalance(t, b, kind, CreditBalance{Available: 1, TrialAvailable: 1})
	}
	var grants, orders, grantEvents int
	if err = a.store.db.QueryRow("SELECT (SELECT COUNT(*) FROM billing_welcome_grants WHERE owner_id=?),(SELECT COUNT(*) FROM billing_orders WHERE owner_id=?),(SELECT COUNT(*) FROM billing_events WHERE owner_id=? AND event='welcome_granted')", owner, owner, owner).Scan(&grants, &orders, &grantEvents); err != nil || grants != 4 || orders != 0 || grantEvents != 4 {
		t.Fatal("registration did not grant exactly once without an order", grants, orders, grantEvents, err)
	}
}

func TestWelcomeConcurrentReservationFailureAndDeletion(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	accountAuth(t, b, "register", "welcome_parallel", "welcome-test-password", "", false)
	enableTestBilling(t, a, map[string]int{"diagnosis": 1})
	ctx, now, owner := context.Background(), time.Now(), accountOwner(t, b)
	input, report := fixture(t)
	full := a.config
	full.QueueLimit = 0
	if _, _, _, err := a.store.Create(ctx, owner, "test", input, full, now); !errors.Is(err, ErrQueueFull) {
		t.Fatal("queue rejection not preserved", err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1, TrialAvailable: 1})
	type result struct {
		id    string
		input Input
		err   error
	}
	results := make(chan result, 2)
	for n := 0; n < 2; n++ {
		go func(n int) {
			candidate := input
			candidate.JD += fmt.Sprintf("\n目标岗位 %d", n)
			id, _, _, err := a.store.Create(ctx, owner, "test", candidate, a.config, now)
			results <- result{id, candidate, err}
		}(n)
	}
	var won result
	rejected := 0
	for n := 0; n < 2; n++ {
		r := <-results
		if r.err == nil {
			won = r
		} else if errors.Is(r.err, ErrCreditRequired) {
			rejected++
		} else {
			t.Fatal(r.err)
		}
	}
	if won.id == "" || rejected != 1 {
		t.Fatal("one free diagnosis was reserved more than once")
	}
	if id, _, duplicate, err := a.store.Create(ctx, owner, "test", won.input, a.config, now); err != nil || !duplicate || id != won.id {
		t.Fatal("repeat submission did not reuse the free task", err)
	}
	job, err := a.store.Claim(ctx, now, time.Second)
	if err != nil || job == nil {
		t.Fatal("missing reserved job", err)
	}
	if err = a.store.Fail(ctx, job, "temporary failure", true, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Reserved: 1})
	now = now.Add(3 * time.Second)
	job, err = a.store.Claim(ctx, now, time.Second)
	if err != nil || job == nil {
		t.Fatal("missing retry", err)
	}
	if err = a.store.Fail(ctx, job, "terminal failure", false, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	if err = a.store.Complete(ctx, job, report, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1, TrialAvailable: 1})
	id, _, _, err := a.store.Create(ctx, owner, "test", input, a.config, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.Delete(ctx, owner, id, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1, TrialAvailable: 1})
	id, _, _, err = a.store.Create(ctx, owner, "test", input, a.config, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err = a.store.Claim(ctx, now, time.Second)
	if err != nil || job == nil {
		t.Fatal(err)
	}
	if err = a.store.Complete(ctx, job, report, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	if err = a.store.Delete(ctx, owner, id, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Used: 1})
	if _, _, _, err = a.store.Create(ctx, owner, "test", input, a.config, now); !errors.Is(err, ErrCreditRequired) {
		t.Fatal("deleting a delivered diagnosis replenished the gift", err)
	}
}

func TestWelcomeAllPreparationFunctionsAndRoundOwnership(t *testing.T) {
	a, b, id, code := preparationApp(t)
	accountAuth(t, b, "register", "welcome_prepare", "welcome-test-password", "", true)
	enableTestBilling(t, a, map[string]int{"refine": 1})
	changePreparation(t, b, id, "questions", nil, 202)
	finishPreparation(t, a, b, id)
	wantBalance(t, b, "refine", CreditBalance{Used: 1})
	changePreparation(t, b, id, "rewrite", refineAnswers(), 202)
	job, err := a.store.ClaimPreparation(context.Background(), time.Now(), time.Second)
	if err != nil || job == nil {
		t.Fatal(err)
	}
	if err = a.store.FailPreparation(context.Background(), job, "terminal rewrite failure", false, Usage{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "refine", CreditBalance{Available: 1, TrialAvailable: 1})
	changePreparation(t, b, id, "retry", map[string]any{"task_id": job.ID}, 202)
	finishPreparation(t, a, b, id)
	wantBalance(t, b, "refine", CreditBalance{Used: 1})
	changePreparation(t, b, id, "rewrite", refineAnswers(), 409)
	changePreparation(t, b, id, "questions", nil, 402)
	changePreparation(t, b, id, "tailor", nil, 202)
	finishPreparation(t, a, b, id)
	wantBalance(t, b, "tailor", CreditBalance{Used: 1})
	changePreparation(t, b, id, "tailor", nil, 402)
	changePreparation(t, b, id, "start_interview", map[string]any{"rounds": 8, "focus": "project"}, 400)
	p := changePreparation(t, b, id, "start_interview", map[string]any{"rounds": 5, "focus": "project"}, 202)
	interviewID := p.Preparation.Interviews[0].ID
	finishPreparation(t, a, b, id)
	other := newBrowser(t, a)
	accountAuth(t, other, "register", "welcome_other", "welcome-test-password", "", false)
	if w := other.request("POST", "/api/recover", map[string]string{"code": code}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	changePreparation(t, other, id, "answer", map[string]any{"interview_id": interviewID, "question_id": "q1", "answer": "我不能用报告恢复码使用另一个账号的体验轮次。"}, 403)
	for n := 1; n <= 5; n++ {
		changePreparation(t, b, id, "answer", map[string]any{"interview_id": interviewID, "question_id": fmt.Sprintf("q%d", n), "answer": "我负责缓存读取，并通过日志核对异常情况。"}, 202)
		finishPreparation(t, a, b, id)
		wantBalance(t, b, "interview", CreditBalance{Used: 1})
	}
	changePreparation(t, b, id, "start_interview", map[string]any{"rounds": 3, "focus": "project"}, 402)
	if err = a.store.Delete(context.Background(), accountOwner(t, b), id, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"refine", "tailor", "interview"} {
		wantBalance(t, b, kind, CreditBalance{Used: 1})
	}
	wantBalance(t, other, "interview", CreditBalance{Available: 1, TrialAvailable: 1})
}

func TestWelcomePriorityAndPurchaseRefundIsolation(t *testing.T) {
	a, b, id, _ := preparationApp(t)
	accountAuth(t, b, "register", "welcome_refund", "welcome-test-password", "", true)
	enableTestBilling(t, a, map[string]int{"refine": 1, "diagnosis": 1})
	order := fundAccount(t, b)
	wantBalance(t, b, "refine", CreditBalance{Available: 2, TrialAvailable: 1})
	changePreparation(t, b, id, "questions", nil, 202)
	wantBalance(t, b, "refine", CreditBalance{Available: 1, Reserved: 1})
	order = billingJSON[BillingOrder](t, b.request("POST", "/api/billing/orders/"+order.ID, map[string]any{"action": "request_refund", "revision": order.Revision}), 200)
	if order.ActiveTasks != 0 || order.Remaining["refine"] != 1 {
		t.Fatal("gift charged or attached to purchase", order)
	}
	wantBalance(t, b, "refine", CreditBalance{Reserved: 1, OnHold: 1})
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1, TrialAvailable: 1, OnHold: 1})
	ctx, now := context.Background(), time.Now()
	if err := a.store.BeginBillingRefund(ctx, order.ID, order.Revision, now); err != nil {
		t.Fatal(err)
	}
	order = billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+order.ID, nil), 200)
	if err := a.store.RefundBillingOrder(ctx, order.ID, order.Revision, order.AmountCents, PaymentReview{Receipt: "WELCOME_REFUND_TEST", Note: "退还未使用的购买次数。"}, now); err != nil {
		t.Fatal(err)
	}
	finishPreparation(t, a, b, id)
	changePreparation(t, b, id, "rewrite", refineAnswers(), 202)
	finishPreparation(t, a, b, id)
	wantBalance(t, b, "refine", CreditBalance{Used: 1})
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1, TrialAvailable: 1})
	changePreparation(t, b, id, "questions", nil, 402)
	fundAccount(t, b)
	changePreparation(t, b, id, "questions", nil, 202)
	finishPreparation(t, a, b, id)
	wantBalance(t, b, "refine", CreditBalance{Used: 2})
}

func TestWelcomeMigrationPreservesV3PaidUsage(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	registerExistingAccount(t, b, "legacy_credit", "legacy-test-password", false)
	enableTestBilling(t, a, map[string]int{"diagnosis": 2})
	fundAccount(t, b)
	input, _ := fixture(t)
	submit(t, b, input)
	if !a.processOne(context.Background()) {
		t.Fatal("missing paid diagnosis")
	}
	input.JD += "\n另一个岗位。"
	submit(t, b, input)
	before := wallet(t, b)
	// Restore the exact v3 usage table and triggers to model a deployed database.
	start := strings.Index(schema, "CREATE TABLE IF NOT EXISTS billing_usage (")
	end := strings.Index(schema[start:], ");") + start + 2
	queries := []string{"CREATE TEMP TABLE previous_usage AS SELECT * FROM billing_usage"}
	for _, name := range []string{"billing_usage_reserved", "billing_usage_changed", "billing_diagnosis_done", "billing_diagnosis_failed", "billing_preparation_done", "billing_preparation_failed", "billing_parent_deleted"} {
		queries = append(queries, "DROP TRIGGER "+name)
	}
	queries = append(queries, "DROP TABLE billing_usage", "DROP TABLE billing_welcome_grants", schema[start:end], "INSERT INTO billing_usage SELECT * FROM previous_usage", "DROP TABLE previous_usage", "DELETE FROM schema_migrations WHERE version=4", schema)
	for _, query := range queries {
		if _, err := a.store.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	a.store, err = OpenStore(a.config.DataDir, a.config.Key)
	if err != nil {
		t.Fatal(err)
	}
	if after := wallet(t, b); !reflect.DeepEqual(before, after) {
		t.Fatal("paid usage changed on upgrade", before, after)
	}
	accountAuth(t, b, "login", "legacy_credit", "legacy-test-password", "", false)
	account, err := a.store.BillingAccount(context.Background(), accountOwner(t, b))
	if err != nil || account.WelcomeGranted {
		t.Fatal("existing account retroactively granted credits", err)
	}
	if !a.processOne(context.Background()) {
		t.Fatal("migrated task did not run")
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Used: 2})
	n := newBrowser(t, a)
	accountAuth(t, n, "register", "after_upgrade", "welcome-test-password", "", false)
	for _, kind := range creditKinds {
		wantBalance(t, n, kind, CreditBalance{Available: 1, TrialAvailable: 1})
	}
	var version, violations int
	if err = a.store.db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 4 {
		t.Fatal("v4 missing", err)
	}
	if err = a.store.db.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil || violations != 0 {
		t.Fatal("migration broke references", err)
	}
}
