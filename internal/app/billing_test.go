package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func billingJSON[T any](t *testing.T, w *httptest.ResponseRecorder, want int) T {
	t.Helper()
	if w.Code != want {
		t.Fatalf("want HTTP %d, got %d: %s", want, w.Code, w.Body.String())
	}
	var data T
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func accountAuth(t *testing.T, b *browser, mode, username, password, recovery string, claim bool) string {
	t.Helper()
	w := b.request("POST", "/api/account/"+mode, map[string]any{"username": username, "password": password, "recovery_code": recovery, "claim_reports": claim})
	data := billingJSON[struct {
		CSRF     string `json:"csrf"`
		Recovery string `json:"recovery_code"`
	}](t, w, 200)
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "jc_session" {
			b.cookie = cookie
		}
	}
	b.csrf = data.CSRF
	return data.Recovery
}

// Existing billing tests model accounts created before registration gifts.
// New-user tests use the real registration endpoint without this fixture step.
func registerExistingAccount(t *testing.T, b *browser, username, password string, claim bool) string {
	t.Helper()
	recovery := accountAuth(t, b, "register", username, password, "", claim)
	owner := accountOwner(t, b)
	if _, err := b.app.store.db.Exec("DELETE FROM billing_welcome_grants WHERE owner_id=?", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := b.app.store.db.Exec("DELETE FROM billing_events WHERE owner_id=? AND event='welcome_granted'", owner); err != nil {
		t.Fatal(err)
	}
	return recovery
}

func accountOwner(t *testing.T, b *browser) string {
	t.Helper()
	owner, err := b.app.store.Session(b.cookie.Value, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func billingPNG(t *testing.T, size int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			im.Set(x, y, color.RGBA{R: 220, G: 230, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func enableTestBilling(t *testing.T, a *App, credits map[string]int) BillingSettings {
	t.Helper()
	ctx := context.Background()
	settings, err := a.store.BillingSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	qr, err := a.store.SaveBillingQRCode(ctx, billingPNG(t, 120), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	settings.Enabled, settings.Payee, settings.QRCodeID = true, "测试收款人（不是真实付款码）", qr
	settings.Support, settings.Notice = "测试联系信息", "测试核款说明，仅用于自动测试。"
	settings.Plans = []BillingPlan{{ID: "test-kit", Name: "测试套餐", PriceCents: 1990, Credits: credits, Active: true}}
	if err = a.store.SaveBillingSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	settings, err = a.store.BillingSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func createTestOrder(t *testing.T, b *browser) BillingOrder {
	t.Helper()
	settings, err := b.app.store.BillingSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := randomHex(16)
	w := b.request("POST", "/api/billing/orders", map[string]any{"plan_id": settings.Plans[0].ID, "request_key": key, "revision": settings.Revision, "policy_version": billingPolicyVersion, "confirmed": true})
	data := billingJSON[struct {
		ID string `json:"id"`
	}](t, w, 201)
	return billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+data.ID, nil), 200)
}

func claimTestOrder(t *testing.T, b *browser, order BillingOrder, receipt string) BillingOrder {
	t.Helper()
	return billingJSON[BillingOrder](t, b.request("POST", "/api/billing/orders/"+order.ID, map[string]any{"action": "claim", "revision": order.Revision, "receipt": receipt, "note": "自动测试付款申报"}), 200)
}

func fundAccount(t *testing.T, b *browser) BillingOrder {
	t.Helper()
	raw, _ := randomHex(12)
	order := claimTestOrder(t, b, createTestOrder(t, b), raw)
	if err := b.app.store.ConfirmBillingOrder(context.Background(), order.ID, order.Revision, order.AmountCents, PaymentReview{Receipt: raw}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+order.ID, nil), 200)
}

func wallet(t *testing.T, b *browser) map[string]CreditBalance {
	t.Helper()
	balances, _, err := b.app.store.BillingWallet(context.Background(), accountOwner(t, b))
	if err != nil {
		t.Fatal(err)
	}
	return balances
}

func wantBalance(t *testing.T, b *browser, kind string, want CreditBalance) {
	t.Helper()
	if got := wallet(t, b)[kind]; got != want {
		t.Fatalf("%s balance: got %+v want %+v", kind, got, want)
	}
}

func TestBillingAccountsRotateSessionsAndKeepOwnershipSeparate(t *testing.T) {
	a, b, id, reportCode := preparationApp(t)
	old := *b
	recovery := registerExistingAccount(t, b, "Account_A", "a-long-test-password", true)
	if !strings.HasPrefix(recovery, "JCA-") || len(recovery) != 52 || b.cookie.Value == old.cookie.Value || b.csrf == old.csrf {
		t.Fatal("missing one-time recovery or session rotation")
	}
	if old.request("GET", "/api/diagnoses", nil).Code != 401 {
		t.Fatal("anonymous session survived registration")
	}
	if b.request("GET", "/api/diagnoses/"+id, nil).Code != 200 {
		t.Fatal("anonymous report was not attached")
	}
	owner := accountOwner(t, b)
	var salt, hash []byte
	var storedCode string
	if err := a.store.db.QueryRow("SELECT password_salt,password_hash,recovery_hash FROM billing_accounts WHERE owner_id=?", owner).Scan(&salt, &hash, &storedCode); err != nil {
		t.Fatal(err)
	}
	if len(salt) != 16 || len(hash) != 32 || storedCode == recovery || strings.Contains(string(hash), "password") {
		t.Fatal("credentials stored incorrectly")
	}
	other := newBrowser(t, a)
	registerExistingAccount(t, other, "account_b", "b-long-test-password", false)
	accountAuth(t, b, "login", "account_b", "b-long-test-password", "", true)
	if b.request("GET", "/api/diagnoses/"+id, nil).Code != 404 {
		t.Fatal("registered accounts merged their reports")
	}
	accountAuth(t, b, "login", "account_a", "a-long-test-password", "", false)
	enableTestBilling(t, a, map[string]int{"diagnosis": 1})
	fundAccount(t, b)
	if w := other.request("POST", "/api/recover", map[string]string{"code": reportCode}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	wantBalance(t, other, "diagnosis", CreditBalance{})
	if other.request("POST", "/api/account/reset", map[string]string{"username": "account_a", "password": "another-long-password", "recovery_code": reportCode}).Code != 401 {
		t.Fatal("report code recovered an account")
	}
	second := newBrowser(t, a)
	accountAuth(t, second, "login", "ACCOUNT_A", "a-long-test-password", "", false)
	prior := *b
	newCode := accountAuth(t, b, "reset", "account_a", "the-new-test-password", strings.ToLower(recovery), false)
	if newCode == recovery || prior.request("GET", "/api/billing", nil).Code != 401 || second.request("GET", "/api/billing", nil).Code != 401 {
		t.Fatal("reset failed to rotate recovery and revoke sessions")
	}
	if w := other.request("POST", "/api/account/reset", map[string]string{"username": "account_a", "password": "another-long-password", "recovery_code": recovery}); w.Code != 401 {
		t.Fatal("old recovery code was reusable", w.Code)
	}
	if other.request("POST", "/api/account/login", map[string]string{"username": "account_a", "password": "a-long-test-password"}).Code != 401 {
		t.Fatal("old password survived reset")
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1})
	if w := b.request("POST", "/api/account/logout", map[string]string{}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if b.request("GET", "/api/billing", nil).Code != 401 {
		t.Fatal("logout retained session")
	}
}

func TestBillingOrderClaimsPricingSnapshotsAndConcurrentConfirmation(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	registerExistingAccount(t, b, "buyer_one", "test-buyer-password", false)
	s := enableTestBilling(t, a, map[string]int{"diagnosis": 1, "refine": 2})
	key, _ := randomHex(16)
	body := map[string]any{"plan_id": "test-kit", "request_key": key, "revision": s.Revision, "policy_version": billingPolicyVersion, "confirmed": true}
	body["price_cents"] = 1
	if b.request("POST", "/api/billing/orders", body).Code != 400 {
		t.Fatal("client supplied price was accepted")
	}
	delete(body, "price_cents")
	body["confirmed"] = false
	if b.request("POST", "/api/billing/orders", body).Code != 400 {
		t.Fatal("missing policy acceptance accepted")
	}
	body["confirmed"] = true
	created := billingJSON[map[string]string](t, b.request("POST", "/api/billing/orders", body), 201)
	again := billingJSON[map[string]string](t, b.request("POST", "/api/billing/orders", body), 201)
	if created["id"] != again["id"] {
		t.Fatal("retry duplicated order")
	}
	order := billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+created["id"], nil), 200)
	if order.AmountCents != 1990 {
		t.Fatal("wrong authoritative amount")
	}
	if w := b.request("GET", "/api/billing/orders/"+order.ID+"/qr", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), billingPNG(t, 120)) {
		t.Fatal("order QR differs")
	}
	s.Payee = "更换后的收款名称"
	s.Plans[0].PriceCents = 2990
	s.Plans[0].Credits["diagnosis"] = 9
	s.QRCodeID, _ = a.store.SaveBillingQRCode(context.Background(), billingPNG(t, 130), time.Now())
	if err := a.store.SaveBillingSettings(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SaveBillingSettings(context.Background(), s); !errors.Is(err, ErrConflict) {
		t.Fatal("stale config overwritten", err)
	}
	order = billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+order.ID, nil), 200)
	if order.AmountCents != 1990 || order.Snapshot.Plan.Credits["diagnosis"] != 1 || order.Snapshot.Payee == s.Payee {
		t.Fatal("order snapshot mutated with config")
	}
	if w := b.request("GET", "/api/billing/orders/"+order.ID+"/qr", nil); !bytes.Equal(w.Body.Bytes(), billingPNG(t, 120)) {
		t.Fatal("order QR changed with settings")
	}
	order = claimTestOrder(t, b, order, "Trade_Number_0001")
	wantBalance(t, b, "diagnosis", CreditBalance{})
	if err := a.store.ConfirmBillingOrder(context.Background(), order.ID, order.Revision, 1, PaymentReview{Receipt: "Trade_Number_0001"}, time.Now()); err == nil {
		t.Fatal("wrong received amount accepted")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- a.store.ConfirmBillingOrder(context.Background(), order.ID, order.Revision, 1990, PaymentReview{Receipt: "trade_number_0001"}, time.Now())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("idempotent confirmation failed", err)
		}
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1})
	var confirmations int
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM billing_order_audit WHERE order_id=? AND action='confirmed'", order.ID).Scan(&confirmations); err != nil || confirmations != 1 {
		t.Fatal("confirmation not atomic", confirmations, err)
	}
	second := claimTestOrder(t, b, createTestOrder(t, b), "Trade_Number_0001")
	if err := a.store.ConfirmBillingOrder(context.Background(), second.ID, second.Revision, second.AmountCents, PaymentReview{Receipt: "TRADE_NUMBER_0001"}, time.Now()); !errors.Is(err, ErrReceiptUsed) {
		t.Fatal("receipt reuse accepted", err)
	}
	if err := a.store.RejectBillingOrder(context.Background(), second.ID, second.Revision, "交易单号重复，请补充正确记录。", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	second = billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+second.ID, nil), 200)
	if second.Status != "rejected" || second.Review.Note == "" {
		t.Fatal("missing review result")
	}
	claimTestOrder(t, b, second, "Different_Receipt_0002")
	stranger := newBrowser(t, a)
	registerExistingAccount(t, stranger, "other_buyer", "other-buyer-password", false)
	if stranger.request("GET", "/api/billing/orders/"+order.ID, nil).Code != 404 || stranger.request("GET", "/api/billing/orders/"+order.ID+"/qr", nil).Code != 404 {
		t.Fatal("order leaked across accounts")
	}
	var cipher []byte
	if err := a.store.db.QueryRow("SELECT claim_cipher FROM billing_orders WHERE id=?", order.ID).Scan(&cipher); err != nil || bytes.Contains(cipher, []byte("TRADE_NUMBER")) {
		t.Fatal("plaintext receipt", err)
	}
}

func TestBillingLastCreditReservationIsAtomicAndQueueRollback(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	registerExistingAccount(t, b, "concurrent_user", "test-concurrent-password", false)
	enableTestBilling(t, a, map[string]int{"diagnosis": 1})
	fundAccount(t, b)
	ctx, now, owner := context.Background(), time.Now(), accountOwner(t, b)
	input, _ := fixture(t)
	full := a.config
	full.QueueLimit = 0
	if _, _, _, err := a.store.Create(ctx, owner, "test", input, full, now); !errors.Is(err, ErrQueueFull) {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1})
	type result struct {
		id    string
		input Input
		err   error
	}
	results := make(chan result, 2)
	for n := 0; n < 2; n++ {
		go func(n int) {
			candidate := input
			candidate.JD += fmt.Sprintf("\n候选岗位 %d", n)
			id, _, _, err := a.store.Create(ctx, owner, "test", candidate, a.config, now)
			results <- result{id, candidate, err}
		}(n)
	}
	var won result
	failures := 0
	for n := 0; n < 2; n++ {
		r := <-results
		if r.err == nil {
			won = r
		} else if errors.Is(r.err, ErrCreditRequired) {
			failures++
		} else {
			t.Fatal(r.err)
		}
	}
	if won.id == "" || failures != 1 {
		t.Fatal("last credit was double spent")
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Reserved: 1})
	id, _, duplicate, err := a.store.Create(ctx, owner, "test", won.input, a.config, now)
	if err != nil || !duplicate || id != won.id {
		t.Fatal("dedup reserved twice", err)
	}
	if !a.processOne(ctx) {
		t.Fatal("missing job")
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Used: 1})
	if err := a.store.Delete(ctx, owner, id, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Used: 1})
}

func TestBillingFailedDeletedAndExhaustedJobsReturnCreditOnce(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	registerExistingAccount(t, b, "failure_user", "test-failure-password", false)
	enableTestBilling(t, a, map[string]int{"diagnosis": 1})
	order := fundAccount(t, b)
	ctx, now, owner := context.Background(), time.Now(), accountOwner(t, b)
	input, report := fixture(t)
	_, _, _, err := a.store.Create(ctx, owner, "test", input, a.config, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err := a.store.Claim(ctx, now, time.Second)
	if err != nil || job == nil {
		t.Fatal(err)
	}
	if err = a.store.Fail(ctx, job, "transient", true, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Reserved: 1})
	now = now.Add(3 * time.Second)
	job, err = a.store.Claim(ctx, now, time.Second)
	if err != nil || job == nil {
		t.Fatal(err)
	}
	if err = a.store.Fail(ctx, job, "terminal", false, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	if err = a.store.Complete(ctx, job, report, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1})
	input.JD += "\n第二次任务。"
	id, _, _, err := a.store.Create(ctx, owner, "test", input, a.config, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err = a.store.Claim(ctx, now, time.Second)
	if err != nil || job == nil {
		t.Fatal(err)
	}
	if err = a.store.Delete(ctx, owner, id, now); err != nil {
		t.Fatal(err)
	}
	if err = a.store.Complete(ctx, job, report, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1})
	input.JD += "\n租约中断测试。"
	_, _, _, err = a.store.Create(ctx, owner, "test", input, a.config, now)
	if err != nil {
		t.Fatal(err)
	}
	var first *Job
	for n := 0; n < 3; n++ {
		job, err = a.store.Claim(ctx, now, time.Second)
		if err != nil || job == nil {
			t.Fatal(err)
		}
		if n == 0 {
			first = job
		}
		now = now.Add(2 * time.Second)
		if err = a.store.RecoverLeases(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.store.Complete(ctx, first, report, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1})
	input.JD += "\n保留期结束。"
	_, _, _, err = a.store.Create(ctx, owner, "test", input, a.config, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.Cleanup(ctx, now.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1})
	if _, err = a.store.BillingOrder(ctx, order.ID, owner); err != nil {
		t.Fatal("financial record expired with resume", err)
	}
}

func refineAnswers() map[string]any {
	return map[string]any{"confirmed": true, "answers": []any{map[string]string{"id": "q1", "answer": "我负责缓存读取和错误日志分析。"}, map[string]string{"id": "q2", "answer": "通过日志检查实际处理结果，没有统计性能改善指标。"}}}
}

func TestBillingRefinementBundlesAndFailureRetry(t *testing.T) {
	a, b, id, _ := preparationApp(t)
	registerExistingAccount(t, b, "refine_user", "refine-test-password", true)
	enableTestBilling(t, a, map[string]int{"refine": 1, "tailor": 1})
	fundAccount(t, b)
	changePreparation(t, b, id, "questions", nil, 202)
	changePreparation(t, b, id, "questions", nil, 409)
	wantBalance(t, b, "refine", CreditBalance{Reserved: 1})
	finishPreparation(t, a, b, id)
	wantBalance(t, b, "refine", CreditBalance{Used: 1})
	changePreparation(t, b, id, "rewrite", refineAnswers(), 202)
	ctx, now := context.Background(), time.Now()
	job, err := a.store.ClaimPreparation(ctx, now, time.Second)
	if err != nil || job == nil {
		t.Fatal(err)
	}
	if err = a.store.FailPreparation(ctx, job, "test terminal failure", false, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "refine", CreditBalance{Available: 1})
	changePreparation(t, b, id, "retry", map[string]any{"task_id": job.ID}, 202)
	wantBalance(t, b, "refine", CreditBalance{Reserved: 1})
	finishPreparation(t, a, b, id)
	wantBalance(t, b, "refine", CreditBalance{Used: 1})
	changePreparation(t, b, id, "rewrite", refineAnswers(), 409)
	changePreparation(t, b, id, "questions", nil, 402)
	p := readPreparation(t, b, id)
	if !p.Preparation.Versions[0].RefinementComplete {
		t.Fatal("completed round not marked")
	}
	changePreparation(t, b, id, "accept_edit", map[string]any{"edit_id": "e1", "confirmed": true}, 200)
	changePreparation(t, b, id, "tailor", nil, 202)
	finishPreparation(t, a, b, id)
	wantBalance(t, b, "tailor", CreditBalance{Used: 1})
	changePreparation(t, b, id, "tailor", nil, 402)
	if err = a.store.Delete(ctx, accountOwner(t, b), id, now); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "refine", CreditBalance{Used: 1})
}

func TestBillingExistingPaidRoundsSettleWhenPurchasingIsDisabled(t *testing.T) {
	a, b, id, _ := preparationApp(t)
	registerExistingAccount(t, b, "billing_toggle", "toggle-test-password", true)
	enableTestBilling(t, a, map[string]int{"refine": 1})
	fundAccount(t, b)
	changePreparation(t, b, id, "questions", nil, 202)
	finishPreparation(t, a, b, id)
	ctx := context.Background()
	s, err := a.store.BillingSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled = false
	if err = a.store.SaveBillingSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	changePreparation(t, b, id, "rewrite", refineAnswers(), 202)
	job, err := a.store.ClaimPreparation(ctx, time.Now(), time.Second)
	if err != nil || job == nil {
		t.Fatal(err)
	}
	if err = a.store.FailPreparation(ctx, job, "terminal failure during free mode", false, Usage{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "refine", CreditBalance{Available: 1})
	// A new free round must remain free when paid mode is enabled later.
	changePreparation(t, b, id, "questions", nil, 202)
	finishPreparation(t, a, b, id)
	s, err = a.store.BillingSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled = true
	if err = a.store.SaveBillingSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	changePreparation(t, b, id, "rewrite", refineAnswers(), 202)
	finishPreparation(t, a, b, id)
	wantBalance(t, b, "refine", CreditBalance{Available: 1})
}

func TestBillingInterviewSessionOwnershipAndLegacyFreeContinuation(t *testing.T) {
	a, b, id, code := preparationApp(t)
	legacy := changePreparation(t, b, id, "start_interview", map[string]any{"rounds": 3, "focus": "project"}, 202).Preparation.Interviews[0].ID
	finishPreparation(t, a, b, id)
	registerExistingAccount(t, b, "interview_user", "interview-password", true)
	enableTestBilling(t, a, map[string]int{"interview": 1})
	fundAccount(t, b)
	changePreparation(t, b, id, "answer", map[string]any{"interview_id": legacy, "question_id": "q1", "answer": "旧免费练习仍然可以继续，不扣除新购买的次数。"}, 202)
	finishPreparation(t, a, b, id)
	wantBalance(t, b, "interview", CreditBalance{Available: 1})
	changePreparation(t, b, id, "start_interview", map[string]any{"rounds": 8, "focus": "project"}, 400)
	p := changePreparation(t, b, id, "start_interview", map[string]any{"rounds": 5, "focus": "project"}, 202)
	paid := p.Preparation.Interviews[len(p.Preparation.Interviews)-1].ID
	wantBalance(t, b, "interview", CreditBalance{Reserved: 1})
	finishPreparation(t, a, b, id)
	other := newBrowser(t, a)
	registerExistingAccount(t, other, "interview_other", "another-password", false)
	if w := other.request("POST", "/api/recover", map[string]string{"code": code}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	changePreparation(t, other, id, "answer", map[string]any{"interview_id": paid, "question_id": "q1", "answer": "这个账号只能查看报告，不能使用原账号购买的场次。"}, 403)
	for n := 1; n <= 5; n++ {
		changePreparation(t, b, id, "answer", map[string]any{"interview_id": paid, "question_id": fmt.Sprintf("q%d", n), "answer": fmt.Sprintf("回答第 %d 题：我负责缓存读取，验证以日志记录为准。", n)}, 202)
		finishPreparation(t, a, b, id)
		wantBalance(t, b, "interview", CreditBalance{Used: 1})
	}
	changePreparation(t, b, id, "start_interview", map[string]any{"rounds": 3, "focus": "project"}, 402)
	changePreparation(t, b, id, "delete_interview", map[string]any{"interview_id": paid}, 200)
	wantBalance(t, b, "interview", CreditBalance{Used: 1})
}

func TestBillingRefundHoldActiveTasksAndPartialRefund(t *testing.T) {
	a, b, id, _ := preparationApp(t)
	registerExistingAccount(t, b, "refund_user", "refund-test-password", true)
	enableTestBilling(t, a, map[string]int{"refine": 2, "diagnosis": 2})
	order := fundAccount(t, b)
	changePreparation(t, b, id, "questions", nil, 202)
	change := func(action string) {
		t.Helper()
		order = billingJSON[BillingOrder](t, b.request("POST", "/api/billing/orders/"+order.ID, map[string]any{"action": action, "revision": order.Revision}), 200)
	}
	change("request_refund")
	wantBalance(t, b, "refine", CreditBalance{Reserved: 1, OnHold: 1})
	change("withdraw_refund")
	wantBalance(t, b, "refine", CreditBalance{Reserved: 1, Available: 1})
	change("request_refund")
	ctx, now := context.Background(), time.Now()
	if err := a.store.BeginBillingRefund(ctx, order.ID, order.Revision, now); err != nil {
		t.Fatal(err)
	}
	order = billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+order.ID, nil), 200)
	if b.request("POST", "/api/billing/orders/"+order.ID, map[string]any{"action": "withdraw_refund", "revision": order.Revision}).Code != 409 {
		t.Fatal("user undid admin refund hold")
	}
	if err := a.store.RefundBillingOrder(ctx, order.ID, order.Revision, 990, PaymentReview{Receipt: "REFUND_TEST_001", Note: "双方确认部分退款。"}, now); err == nil {
		t.Fatal("refund accepted while generation active")
	}
	finishPreparation(t, a, b, id)
	changePreparation(t, b, id, "rewrite", refineAnswers(), 400)
	if err := a.store.RefundBillingOrder(ctx, order.ID, order.Revision, 2000, PaymentReview{Receipt: "REFUND_TEST_001", Note: "金额超出原订单。"}, now); err == nil {
		t.Fatal("excess refund accepted")
	}
	for n := 0; n < 2; n++ {
		if err := a.store.RefundBillingOrder(ctx, order.ID, order.Revision, 990, PaymentReview{Receipt: "REFUND_TEST_001", Note: "双方确认部分退款。"}, now); err != nil {
			t.Fatal(err)
		}
	}
	wantBalance(t, b, "refine", CreditBalance{Used: 1})
	wantBalance(t, b, "diagnosis", CreditBalance{})
	changePreparation(t, b, id, "rewrite", refineAnswers(), 400)
	order = billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+order.ID, nil), 200)
	if order.Status != "paid" || order.RefundedCents != 990 || len(order.Refunds) != 1 || order.Refunds[0].Receipt != "REFUND_TEST_001" {
		t.Fatal("refund not recorded once", order)
	}
	if err := a.store.RefundBillingOrder(ctx, order.ID, order.Revision, 991, PaymentReview{Receipt: "REFUND_TEST_001", Note: "不同金额不能重放。"}, now); !errors.Is(err, ErrReceiptUsed) {
		t.Fatal(err)
	}
	if err := a.store.BeginBillingRefund(ctx, order.ID, order.Revision, now); err != nil {
		t.Fatal(err)
	}
	order = billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+order.ID, nil), 200)
	if err := a.store.RefundBillingOrder(ctx, order.ID, order.Revision, 1000, PaymentReview{Receipt: "REFUND_TEST_002", Note: "补退剩余款项。"}, now); err != nil {
		t.Fatal(err)
	}
	order = billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+order.ID, nil), 200)
	if order.Status != "refunded" || order.RefundedCents != 1990 || len(order.Refunds) != 2 {
		t.Fatal("full refund status incorrect")
	}
	wantBalance(t, b, "refine", CreditBalance{Used: 1})
}

func loginBillingAdmin(t *testing.T, b *browser) {
	t.Helper()
	w := b.request("POST", "/api/admin/login", map[string]string{"key": b.app.config.BillingAdminKey})
	data := billingJSON[struct {
		CSRF string `json:"csrf"`
	}](t, w, 200)
	b.adminCSRF = data.CSRF
	for _, c := range w.Result().Cookies() {
		if c.Name == "jc_admin" {
			b.adminCookie = c
		}
	}
	if b.adminCookie == nil || !b.adminCookie.HttpOnly || b.adminCookie.Path != "/api/admin" || b.adminCookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("weak admin cookie")
	}
}

func TestBillingAdminAuthorizationQRAndExplicitReceiptConfirmation(t *testing.T) {
	a := testApp(t, nil)
	a.config.BillingAdminKey = strings.Repeat("test-key", 8)
	admin := newBrowser(t, a)
	customer := newBrowser(t, a)
	if customer.request("GET", "/api/admin/billing", nil).Code != 401 {
		t.Fatal("admin settings public")
	}
	wrong := *admin
	wrong.csrf = "bad"
	if wrong.request("POST", "/api/admin/login", map[string]string{"key": a.config.BillingAdminKey}).Code != 403 {
		t.Fatal("admin login without CSRF")
	}
	loginBillingAdmin(t, admin)
	settings := billingJSON[BillingSettings](t, admin.request("GET", "/api/admin/billing", nil), 200)
	if settings.Enabled || settings.Plans[0].Active || settings.Plans[0].PriceCents != 0 {
		t.Fatal("billing active by default")
	}
	settings.Enabled = true
	if admin.request("POST", "/api/admin/billing", settings).Code != 400 {
		t.Fatal("enabled without payment details")
	}
	bad := *admin
	bad.adminCSRF = admin.csrf
	if bad.request("POST", "/api/admin/billing", settings).Code != 403 {
		t.Fatal("normal CSRF accepted for admin write")
	}
	upload := func(data []byte, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/admin/billing/qr", bytes.NewReader(data))
		r.AddCookie(admin.adminCookie)
		r.Header.Set("X-CSRF-Token", admin.adminCSRF)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	for _, data := range [][]byte{[]byte(`<svg onload="alert(1)"></svg>`), billingPNG(t, 32), bytes.Repeat([]byte{'x'}, 1024*1024+1)} {
		if upload(data, a.config.Origin).Code != 400 {
			t.Fatal("unsafe/oversized QR accepted")
		}
	}
	if upload(billingPNG(t, 120), "https://other.example").Code != 403 {
		t.Fatal("cross-origin admin mutation")
	}
	qr := billingJSON[map[string]string](t, upload(billingPNG(t, 120), a.config.Origin), 201)
	if admin.request("GET", "/api/admin/billing/qr/"+qr["id"], nil).Code != 200 || customer.request("GET", "/api/admin/billing/qr/"+qr["id"], nil).Code != 401 {
		t.Fatal("QR access not private")
	}
	registerExistingAccount(t, customer, "admin_test_buyer", "test-buyer-password", false)
	enableTestBilling(t, a, map[string]int{"diagnosis": 1})
	order := claimTestOrder(t, customer, createTestOrder(t, customer), "ADMIN_TEST_TRADE")
	request := map[string]any{"action": "confirm", "revision": order.Revision, "amount_cents": 1990, "receipt": "ADMIN_TEST_TRADE", "confirmed": false}
	if customer.request("POST", "/api/admin/orders/"+order.ID, request).Code != 401 || admin.request("POST", "/api/admin/orders/"+order.ID, request).Code != 400 {
		t.Fatal("unapproved payment granted")
	}
	request["confirmed"] = true
	billingJSON[BillingOrder](t, admin.request("POST", "/api/admin/orders/"+order.ID, request), 200)
	wantBalance(t, customer, "diagnosis", CreditBalance{Available: 1})
	a.config.BillingAdminKey = strings.Repeat("rotated-key", 6)
	if admin.request("GET", "/api/admin/orders", nil).Code != 401 {
		t.Fatal("admin key rotation did not revoke sessions")
	}
}

func TestBillingMigrationAndLatePaymentClaims(t *testing.T) {
	a, b, id, _ := preparationApp(t)
	changePreparation(t, b, id, "questions", nil, 202)
	finishPreparation(t, a, b, id)
	before := readPreparation(t, b, id)
	rows, err := a.store.db.Query("SELECT type,name FROM sqlite_master WHERE name LIKE 'billing_%' AND type IN ('trigger','table') ORDER BY type DESC")
	if err != nil {
		t.Fatal(err)
	}
	var drops []string
	for rows.Next() {
		var kind, name string
		if err = rows.Scan(&kind, &name); err != nil {
			t.Fatal(err)
		}
		drops = append(drops, "DROP "+kind+" "+name)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, query := range drops {
		if _, err = a.store.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = a.store.db.Exec("DELETE FROM schema_migrations WHERE version>=3"); err != nil {
		t.Fatal(err)
	}
	if err = a.store.Close(); err != nil {
		t.Fatal(err)
	}
	a.store, err = OpenStore(a.config.DataDir, a.config.Key)
	if err != nil {
		t.Fatal(err)
	}
	after := readPreparation(t, b, id)
	if after.Preparation.Revision != before.Preparation.Revision || after.Preparation.Versions[0].Text != before.Preparation.Versions[0].Text || len(after.Preparation.Versions[0].Questions) != 2 {
		t.Fatal("v2 data changed during migration")
	}
	var version int
	if err = a.store.db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 5 {
		t.Fatal("v5 not installed", err)
	}
	registerExistingAccount(t, b, "late_buyer", "late-buyer-password", true)
	s := enableTestBilling(t, a, map[string]int{"diagnosis": 1})
	key, _ := randomHex(16)
	oldID, err := a.store.CreateBillingOrder(context.Background(), accountOwner(t, b), "test", s.Plans[0].ID, key, s.Revision, time.Now().Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.Cleanup(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	order := billingJSON[BillingOrder](t, b.request("GET", "/api/billing/orders/"+oldID, nil), 200)
	if order.Status != "expired" || b.request("GET", "/api/billing/orders/"+oldID+"/qr", nil).Code != 409 {
		t.Fatal("expired order still accepting payment")
	}
	order = claimTestOrder(t, b, order, "LATE_TRADE_TEST_001")
	if err = a.store.ConfirmBillingOrder(context.Background(), order.ID, order.Revision, 1990, PaymentReview{Receipt: "LATE_TRADE_TEST_001"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantBalance(t, b, "diagnosis", CreditBalance{Available: 1})
}
