package app

import (
	"bytes"
	"crypto/hmac"
	"errors"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func billingError(w http.ResponseWriter, err error) bool {
	var validation *billingValidationError
	status, code, message := 0, "", ""
	switch {
	case errors.Is(err, ErrAccountRequired):
		status, code, message = 402, "account_required", "请先登录账号。新账号注册后可免费体验各项功能一次。"
	case errors.Is(err, ErrCreditRequired):
		status, code, message = 402, "credits_required", "这项功能的可用次数不足，请到账号页查看次数或购买套餐。"
	case errors.Is(err, ErrBillingClosed):
		status, message = 409, "购买暂未开放或套餐已下架，请刷新页面。"
	case errors.Is(err, ErrBillingAccess):
		status, message = 403, "本轮由另一个账号开启，请登录原账号继续，或开启自己的新一轮练习。"
	case errors.Is(err, ErrRoundCompleted):
		status, message = 409, "本轮精修已完成。需要再次精修时，请生成新一轮追问。"
	case errors.Is(err, ErrPaidRounds):
		status, message = 400, "每场付费面试包含最多 5 题，请选择 3 题或 5 题。"
	case errors.Is(err, ErrReceiptUsed):
		status, message = 409, "这笔交易已登记，请核对交易单号和对应订单；次数不会重复发放。"
	case errors.Is(err, ErrBadCredentials):
		status, message = 401, "账号、密码或账号恢复码不正确，请检查后重试。"
	case errors.Is(err, ErrUsernameTaken):
		status, message = 409, "这个账号名已被使用，请选择另一个名称。"
	case errors.As(err, &validation):
		status, message = 400, validation.Error()
	default:
		return false
	}
	jsonResponse(w, status, map[string]string{"error": message, "code": code})
	return true
}

func billingStoreError(w http.ResponseWriter, err error) {
	if billingError(w, err) {
		return
	}
	switch {
	case errors.Is(err, ErrConflict):
		jsonError(w, 409, "订单或套餐已更新，请刷新后核对再操作。")
	case errors.Is(err, ErrNotFound):
		jsonError(w, 404, "没有找到这个订单，或当前账号没有访问权限。")
	case errors.Is(err, ErrRateLimit):
		w.Header().Set("Retry-After", "900")
		jsonError(w, 429, "操作较频繁，请稍后再试。")
	default:
		jsonError(w, 500, "暂时无法处理账号或订单，请稍后重试。")
	}
}

func (a *App) billingRoutes() {
	a.mux.HandleFunc("POST /api/account/register", a.accountAuthentication)
	a.mux.HandleFunc("POST /api/account/login", a.accountAuthentication)
	a.mux.HandleFunc("POST /api/account/reset", a.accountAuthentication)
	a.mux.HandleFunc("POST /api/account/logout", a.accountLogout)
	a.mux.HandleFunc("GET /api/billing", a.billingOverview)
	a.mux.HandleFunc("GET /api/billing/orders", a.billingOrders)
	a.mux.HandleFunc("POST /api/billing/orders", a.createBillingOrder)
	a.mux.HandleFunc("GET /api/billing/orders/{order}", a.getBillingOrder)
	a.mux.HandleFunc("POST /api/billing/orders/{order}", a.changeBillingOrder)
	a.mux.HandleFunc("GET /api/billing/orders/{order}/qr", a.orderQRCode)
	a.mux.HandleFunc("GET /api/admin/session", a.adminSession)
	a.mux.HandleFunc("POST /api/admin/login", a.adminLogin)
	a.mux.HandleFunc("POST /api/admin/logout", a.adminLogout)
	a.mux.HandleFunc("GET /api/admin/billing", a.adminBillingSettings)
	a.mux.HandleFunc("POST /api/admin/billing", a.adminSaveBillingSettings)
	a.mux.HandleFunc("POST /api/admin/billing/qr", a.adminUploadQRCode)
	a.mux.HandleFunc("GET /api/admin/billing/qr/{qr}", a.adminQRCode)
	a.mux.HandleFunc("GET /api/admin/orders", a.adminOrders)
	a.mux.HandleFunc("GET /api/admin/orders/{order}", a.adminOrder)
	a.mux.HandleFunc("POST /api/admin/orders/{order}", a.adminReviewOrder)
}

func (a *App) setAccountCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: "jc_session", Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.config.Origin, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
}

func (a *App) accountAuthentication(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.authorize(w, r, true)
	if !ok {
		return
	}
	var request struct {
		Username     string `json:"username"`
		Password     string `json:"password"`
		RecoveryCode string `json:"recovery_code"`
		ClaimReports bool   `json:"claim_reports"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	if len(request.Username) > 64 || len(request.Password) > 512 || len(request.RecoveryCode) > 80 {
		jsonError(w, 400, "账号资料过长，请检查后重试。")
		return
	}
	now := time.Now()
	if err := a.store.TakeLimit(r.Context(), a.store.privateHash("auth-ip:"+a.clientIP(r)), now, 15*time.Minute, 15); err != nil {
		billingStoreError(w, err)
		return
	}
	if err := a.store.TakeLimit(r.Context(), a.store.privateHash("auth-name:"+strings.ToLower(strings.TrimSpace(request.Username))), now, 15*time.Minute, 10); err != nil {
		billingStoreError(w, err)
		return
	}
	select {
	case a.authSlots <- struct{}{}:
		defer func() { <-a.authSlots }()
	default:
		jsonError(w, 429, "正在处理登录，请稍后再试。")
		return
	}
	cookie, _ := r.Cookie("jc_session")
	var token, recovery string
	var err error
	switch r.URL.Path {
	case "/api/account/register":
		account, e := a.store.BillingAccount(r.Context(), owner)
		if e != nil {
			billingStoreError(w, e)
			return
		}
		if account != nil {
			jsonError(w, 409, "请先退出当前账号，再创建新账号。")
			return
		}
		token, recovery, err = a.store.RegisterAccount(r.Context(), cookie.Value, request.Username, request.Password, request.ClaimReports, now)
	case "/api/account/login":
		token, err = a.store.LoginAccount(r.Context(), cookie.Value, request.Username, request.Password, request.ClaimReports, now)
	case "/api/account/reset":
		token, recovery, err = a.store.ResetAccount(r.Context(), cookie.Value, request.Username, request.Password, request.RecoveryCode, now)
	}
	if err != nil {
		billingStoreError(w, err)
		return
	}
	a.setAccountCookie(w, token)
	jsonResponse(w, 200, map[string]any{"authenticated": true, "recovery_code": recovery, "csrf": a.store.privateHash("csrf:" + token)})
}

func (a *App) accountLogout(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, true); !ok {
		return
	}
	cookie, _ := r.Cookie("jc_session")
	if _, err := a.store.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash=?", digest(cookie.Value)); err != nil {
		billingStoreError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "jc_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(a.config.Origin, "https://"), SameSite: http.SameSiteLaxMode})
	jsonResponse(w, 200, map[string]bool{"logged_out": true})
}

func (a *App) billingOverview(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.authorize(w, r, false)
	if !ok {
		return
	}
	settings, err := a.store.BillingSettings(r.Context())
	if err != nil {
		billingStoreError(w, err)
		return
	}
	account, err := a.store.BillingAccount(r.Context(), owner)
	if err != nil {
		billingStoreError(w, err)
		return
	}
	plans := []BillingPlan{}
	if settings.Enabled {
		for _, plan := range settings.Plans {
			if plan.Active {
				plans = append(plans, plan)
			}
		}
	}
	wallet, events, err := a.store.BillingWallet(r.Context(), owner)
	if err != nil {
		billingStoreError(w, err)
		return
	}
	jsonResponse(w, 200, map[string]any{"enabled": settings.Enabled, "purchasing_available": settings.Enabled && a.provider.Ready(), "revision": settings.Revision, "plans": plans, "account": account, "wallet": wallet, "events": events, "support": settings.Support, "policy_version": billingPolicyVersion})
}

func (a *App) requireBillingAccount(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	owner, ok := a.authorize(w, r, write)
	if !ok {
		return "", false
	}
	account, err := a.store.BillingAccount(r.Context(), owner)
	if err != nil {
		billingStoreError(w, err)
		return "", false
	}
	if account == nil {
		billingError(w, ErrAccountRequired)
		return "", false
	}
	return owner, true
}

func billingPage(r *http.Request) int {
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 0 || page > 1000 {
		return 0
	}
	return page
}

func (a *App) billingOrders(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.requireBillingAccount(w, r, false)
	if !ok {
		return
	}
	orders, err := a.store.BillingOrders(r.Context(), owner, "", "", billingPage(r))
	if err != nil {
		billingStoreError(w, err)
		return
	}
	jsonResponse(w, 200, map[string]any{"orders": orders})
}

func (a *App) createBillingOrder(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.requireBillingAccount(w, r, true)
	if !ok {
		return
	}
	if !a.provider.Ready() {
		jsonError(w, 503, "生成服务暂不可用，请稍后再购买。")
		return
	}
	var request struct {
		PlanID        string `json:"plan_id"`
		RequestKey    string `json:"request_key"`
		Revision      int64  `json:"revision"`
		PolicyVersion string `json:"policy_version"`
		Confirmed     bool   `json:"confirmed"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	if !request.Confirmed || request.PolicyVersion != billingPolicyVersion {
		jsonError(w, 400, "请阅读并确认人工核款和次数使用规则后下单。")
		return
	}
	id, err := a.store.CreateBillingOrder(r.Context(), owner, a.clientIP(r), request.PlanID, request.RequestKey, request.Revision, time.Now())
	if err != nil {
		billingStoreError(w, err)
		return
	}
	jsonResponse(w, 201, map[string]string{"id": id})
}

func (a *App) getBillingOrder(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.requireBillingAccount(w, r, false)
	if !ok {
		return
	}
	order, err := a.store.BillingOrder(r.Context(), r.PathValue("order"), owner)
	if err != nil {
		billingStoreError(w, err)
		return
	}
	jsonResponse(w, 200, order)
}

func (a *App) changeBillingOrder(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.requireBillingAccount(w, r, true)
	if !ok {
		return
	}
	var request struct {
		Action   string `json:"action"`
		Revision int64  `json:"revision"`
		Receipt  string `json:"receipt"`
		Note     string `json:"note"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	if err := a.store.TakeLimit(r.Context(), a.store.privateHash("claim:"+owner), time.Now(), time.Hour, 30); err != nil {
		billingStoreError(w, err)
		return
	}
	err := a.store.ChangeBillingOrder(r.Context(), owner, r.PathValue("order"), request.Action, request.Revision, PaymentClaim{Receipt: request.Receipt, Note: request.Note}, time.Now())
	if err != nil {
		billingStoreError(w, err)
		return
	}
	order, err := a.store.BillingOrder(r.Context(), r.PathValue("order"), owner)
	if err != nil {
		billingStoreError(w, err)
		return
	}
	jsonResponse(w, 200, order)
}

func (a *App) sendBillingQRCode(w http.ResponseWriter, r *http.Request, id string) {
	data, err := a.store.BillingQRCode(r.Context(), id)
	if err != nil {
		billingStoreError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (a *App) orderQRCode(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.requireBillingAccount(w, r, false)
	if !ok {
		return
	}
	order, err := a.store.BillingOrder(r.Context(), r.PathValue("order"), owner)
	if err != nil {
		billingStoreError(w, err)
		return
	}
	if order.Status != "awaiting_payment" || order.ExpiresAt <= time.Now().Unix() {
		jsonError(w, 409, "此订单已不再接受付款；已付款的订单仍可提交核对。")
		return
	}
	a.sendBillingQRCode(w, r, order.Snapshot.QRCodeID)
}

func (a *App) adminAuthenticated(r *http.Request) (string, bool) {
	if a.config.BillingAdminKey == "" {
		return "", false
	}
	cookie, err := r.Cookie("jc_admin")
	if err != nil || len(cookie.Value) != 64 {
		return "", false
	}
	var exists int
	err = a.store.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM billing_admin_sessions WHERE token_hash=? AND key_hash=? AND expires_at>?", digest(cookie.Value), a.store.privateHash("admin-key:"+a.config.BillingAdminKey), time.Now().Unix()).Scan(&exists)
	return cookie.Value, err == nil && exists == 1
}

func (a *App) requireBillingAdmin(w http.ResponseWriter, r *http.Request, write bool) bool {
	token, ok := a.adminAuthenticated(r)
	if !ok {
		jsonError(w, 401, "请先登录收款管理页。")
		return false
	}
	if write && ((r.Header.Get("Origin") != "" && r.Header.Get("Origin") != a.config.Origin) || r.Header.Get("Sec-Fetch-Site") == "cross-site" || !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(a.store.privateHash("admin-csrf:"+token)))) {
		jsonError(w, 403, "管理页验证已失效，请刷新后重试。")
		return false
	}
	return true
}

func (a *App) adminSession(w http.ResponseWriter, r *http.Request) {
	token, authenticated := a.adminAuthenticated(r)
	csrf := ""
	if authenticated {
		csrf = a.store.privateHash("admin-csrf:" + token)
	}
	jsonResponse(w, 200, map[string]any{"configured": a.config.BillingAdminKey != "", "authenticated": authenticated, "csrf": csrf})
}

func (a *App) adminLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, true); !ok {
		return
	}
	if a.config.BillingAdminKey == "" {
		jsonError(w, 503, "收款管理尚未配置，请先在服务器设置管理密钥。")
		return
	}
	if err := a.store.TakeLimit(r.Context(), a.store.privateHash("admin-login:"+a.clientIP(r)), time.Now(), 15*time.Minute, 8); err != nil {
		billingStoreError(w, err)
		return
	}
	var request struct {
		Key string `json:"key"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	if !hmac.Equal([]byte(digest(request.Key)), []byte(digest(a.config.BillingAdminKey))) {
		jsonError(w, 401, "管理密钥不正确，请检查后重试。")
		return
	}
	token, err := randomHex(32)
	if err != nil {
		billingStoreError(w, err)
		return
	}
	_, err = a.store.db.ExecContext(r.Context(), "INSERT INTO billing_admin_sessions(token_hash,key_hash,expires_at) VALUES(?,?,?)", digest(token), a.store.privateHash("admin-key:"+a.config.BillingAdminKey), time.Now().Add(8*time.Hour).Unix())
	if err != nil {
		billingStoreError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "jc_admin", Value: token, Path: "/api/admin", HttpOnly: true, Secure: strings.HasPrefix(a.config.Origin, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: 8 * 3600})
	jsonResponse(w, 200, map[string]any{"authenticated": true, "csrf": a.store.privateHash("admin-csrf:" + token)})
}

func (a *App) adminLogout(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, true) {
		return
	}
	token, _ := a.adminAuthenticated(r)
	if _, err := a.store.db.ExecContext(r.Context(), "DELETE FROM billing_admin_sessions WHERE token_hash=?", digest(token)); err != nil {
		billingStoreError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "jc_admin", Path: "/api/admin", HttpOnly: true, Secure: strings.HasPrefix(a.config.Origin, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	jsonResponse(w, 200, map[string]bool{"logged_out": true})
}

func (a *App) adminBillingSettings(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, false) {
		return
	}
	settings, err := a.store.BillingSettings(r.Context())
	if err != nil {
		billingStoreError(w, err)
		return
	}
	jsonResponse(w, 200, settings)
}

func (a *App) adminSaveBillingSettings(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, true) {
		return
	}
	var settings BillingSettings
	if !decodeRequest(w, r, &settings) {
		return
	}
	if err := a.store.SaveBillingSettings(r.Context(), settings); err != nil {
		billingStoreError(w, err)
		return
	}
	a.adminBillingSettings(w, r)
}

func (a *App) adminUploadQRCode(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, true) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err != nil {
		jsonError(w, 400, "收款码图片不能超过 1 MB。")
		return
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 96 || config.Height < 96 || config.Width > 1600 || config.Height > 1600 {
		jsonError(w, 400, "请上传 PNG 或 JPEG 收款码，宽高需在 96—1,600 像素之间。")
		return
	}
	decoded, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		jsonError(w, 400, "图片无法读取，请重新导出收款码。")
		return
	}
	var normalized bytes.Buffer
	if err = png.Encode(&normalized, decoded); err != nil || normalized.Len() > 1024*1024 {
		jsonError(w, 400, "图片较大，请裁剪至收款二维码附近后重新上传。")
		return
	}
	id, err := a.store.SaveBillingQRCode(r.Context(), normalized.Bytes(), time.Now())
	if err != nil {
		billingStoreError(w, err)
		return
	}
	jsonResponse(w, 201, map[string]string{"id": id})
}

func (a *App) adminQRCode(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, false) {
		return
	}
	a.sendBillingQRCode(w, r, r.PathValue("qr"))
}

func (a *App) adminOrders(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, false) {
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(search) > 64 {
		jsonError(w, 400, "搜索内容过长。")
		return
	}
	orders, err := a.store.BillingOrders(r.Context(), "", r.URL.Query().Get("status"), search, billingPage(r))
	if err != nil {
		billingStoreError(w, err)
		return
	}
	jsonResponse(w, 200, map[string]any{"orders": orders})
}

func (a *App) adminOrder(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, false) {
		return
	}
	order, err := a.store.BillingOrder(r.Context(), r.PathValue("order"), "")
	if err != nil {
		billingStoreError(w, err)
		return
	}
	jsonResponse(w, 200, order)
}

func (a *App) adminReviewOrder(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, true) {
		return
	}
	var request struct {
		Action      string `json:"action"`
		Revision    int64  `json:"revision"`
		AmountCents int64  `json:"amount_cents"`
		Receipt     string `json:"receipt"`
		Note        string `json:"note"`
		Confirmed   bool   `json:"confirmed"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	if !request.Confirmed {
		jsonError(w, 400, "请先核实真实收款或退款记录，再确认登记。")
		return
	}
	id, now := r.PathValue("order"), time.Now()
	var err error
	review := PaymentReview{Receipt: request.Receipt, Note: request.Note}
	switch request.Action {
	case "confirm":
		err = a.store.ConfirmBillingOrder(r.Context(), id, request.Revision, request.AmountCents, review, now)
	case "reject", "reject_refund":
		err = a.store.RejectBillingOrder(r.Context(), id, request.Revision, request.Note, request.Action == "reject_refund", now)
	case "refund":
		err = a.store.RefundBillingOrder(r.Context(), id, request.Revision, request.AmountCents, review, now)
	case "hold_refund":
		err = a.store.BeginBillingRefund(r.Context(), id, request.Revision, now)
	default:
		err = &billingValidationError{"没有找到这个核款操作。"}
	}
	if err != nil {
		billingStoreError(w, err)
		return
	}
	a.adminOrder(w, r)
}
