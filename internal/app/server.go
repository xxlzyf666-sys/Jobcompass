package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

//go:embed web example.json
var embedded embed.FS

type runningJob struct {
	cancel  context.CancelFunc
	expires int64
	parent  string
}
type App struct {
	config    Config
	store     *Store
	provider  Provider
	mux       *http.ServeMux
	wake      chan struct{}
	jobsMu    sync.Mutex
	jobs      map[string]runningJob
	index     []byte
	authSlots chan struct{}
}

func New(config Config, provider Provider) (*App, error) {
	store, err := OpenStore(config.DataDir, config.Key)
	if err != nil {
		return nil, err
	}
	if err = store.Cleanup(context.Background(), time.Now()); err != nil {
		store.Close()
		return nil, err
	}
	if err = store.RecoverLeases(context.Background(), time.Now()); err != nil {
		store.Close()
		return nil, err
	}
	if provider == nil {
		provider = NewClaudeProvider(config)
	}
	index, err := embedded.ReadFile("web/index.html")
	if err != nil {
		store.Close()
		return nil, err
	}
	// Content versions prevent a newly deployed page from using cached old UI code.
	for _, name := range []string{"app.css", "app.js", "preparation.css", "preparation.js", "billing.css", "billing.js"} {
		asset, readErr := embedded.ReadFile("web/" + name)
		if readErr != nil {
			store.Close()
			return nil, readErr
		}
		index = bytes.ReplaceAll(index, []byte("/assets/"+name), []byte("/assets/"+name+"?v="+digest(string(asset))[:12]))
	}
	a := &App{config: config, store: store, provider: provider, mux: http.NewServeMux(), wake: make(chan struct{}, 1), jobs: map[string]runningJob{}, index: index, authSlots: make(chan struct{}, 2)}
	a.routes()
	return a, nil
}

func (a *App) Close() error { return a.store.Close() }
func (a *App) Run(ctx context.Context) error {
	workerCtx, stop := context.WithCancel(ctx)
	defer stop()
	wg := a.startWorkers(workerCtx)
	server := &http.Server{Addr: a.config.Addr, Handler: a, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 * 1024}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	slog.Info("JobCompass ready", "address", a.config.Addr, "diagnosis_enabled", a.provider.Ready())
	var err error
	select {
	case <-ctx.Done():
	case err = <-result:
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; worker-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	a.mux.ServeHTTP(w, r)
}

func (a *App) routes() {
	a.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(a.index)
	})
	assets, _ := fs.Sub(embedded, "web")
	fileServer := http.StripPrefix("/assets/", http.FileServer(http.FS(assets)))
	a.mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		info, err := fs.Stat(assets, path.Clean(name))
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if strings.HasSuffix(name, ".mjs") || strings.HasSuffix(name, ".js") {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		fileServer.ServeHTTP(w, r)
	})
	a.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if a.store.Ping(r.Context()) != nil {
			jsonError(w, 503, "服务暂未就绪。")
			return
		}
		jsonResponse(w, 200, map[string]bool{"ok": true})
	})
	a.mux.HandleFunc("GET /api/bootstrap", a.bootstrap)
	a.mux.HandleFunc("GET /api/example", func(w http.ResponseWriter, r *http.Request) {
		data, _ := embedded.ReadFile("example.json")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(data)
	})
	a.mux.HandleFunc("GET /api/diagnoses", a.listDiagnoses)
	a.mux.HandleFunc("POST /api/diagnoses", a.createDiagnosis)
	a.mux.HandleFunc("GET /api/diagnoses/{id}", a.getDiagnosis)
	a.mux.HandleFunc("DELETE /api/diagnoses/{id}", a.deleteDiagnosis)
	a.mux.HandleFunc("GET /api/diagnoses/{id}/export", a.exportDiagnosis)
	a.mux.HandleFunc("POST /api/recover", a.recoverDiagnosis)
	a.mux.HandleFunc("GET /api/diagnoses/{id}/preparation", a.getPreparation)
	a.mux.HandleFunc("POST /api/diagnoses/{id}/preparation", a.changePreparation)
	a.mux.HandleFunc("GET /api/diagnoses/{id}/versions/{version}/export", a.exportResume)
	a.billingRoutes()
}

func (a *App) bootstrap(w http.ResponseWriter, r *http.Request) {
	var token, owner string
	var err error
	if cookie, e := r.Cookie("jc_session"); e == nil {
		token = cookie.Value
		owner, err = a.store.Session(token, time.Now())
	}
	if owner == "" || err != nil {
		token, owner, err = a.store.NewSession(time.Now())
		if err != nil {
			jsonError(w, 500, "暂时无法建立会话，请刷新重试。")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "jc_session", Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.config.Origin, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
	}
	settings, err := a.store.BillingSettings(r.Context())
	if err != nil {
		jsonError(w, 503, "暂时无法读取服务状态，请稍后重试。")
		return
	}
	account, err := a.store.BillingAccount(r.Context(), owner)
	if err != nil {
		jsonError(w, 503, "暂时无法读取账号状态，请稍后重试。")
		return
	}
	jsonResponse(w, 200, map[string]any{"csrf": a.store.privateHash("csrf:" + token), "ready": a.provider.Ready(), "provider": a.config.ProviderName, "region": a.config.DataRegion, "retention_hours": int(a.config.Retention.Hours()), "consent_version": consentVersion, "billing_enabled": settings.Enabled, "account": account})
}

func (a *App) authorize(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	cookie, err := r.Cookie("jc_session")
	if err != nil {
		jsonError(w, 401, "会话已失效，请刷新页面或使用恢复码找回报告。")
		return "", false
	}
	owner, err := a.store.Session(cookie.Value, time.Now())
	if err != nil {
		jsonError(w, 401, "会话已失效，请刷新页面或使用恢复码找回报告。")
		return "", false
	}
	if write {
		origin := r.Header.Get("Origin")
		if origin != "" && origin != a.config.Origin {
			jsonError(w, 403, "请求来源不匹配，请在本站重新操作。")
			return "", false
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" || !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(a.store.privateHash("csrf:"+cookie.Value))) {
			jsonError(w, 403, "页面验证已失效，请刷新后重试。")
			return "", false
		}
	}
	return owner, true
}

func decodeRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		jsonError(w, 415, "请以 JSON 文字格式提交材料。")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 160*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		jsonError(w, 400, "提交内容无效或过长，请检查材料后重试。")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		jsonError(w, 400, "提交内容格式不正确。")
		return false
	}
	return true
}

func (a *App) createDiagnosis(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.authorize(w, r, true)
	if !ok {
		return
	}
	if !a.provider.Ready() {
		jsonError(w, 503, "真实诊断暂未开放，可先查看完整示例报告。")
		return
	}
	var request struct {
		Resume         string `json:"resume"`
		JD             string `json:"jd"`
		Role           string `json:"role"`
		Consent        bool   `json:"consent"`
		ConsentVersion string `json:"consent_version"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	if !request.Consent || request.ConsentVersion != consentVersion {
		jsonError(w, 400, "请阅读并确认材料处理说明后再提交。")
		return
	}
	input := Input{Resume: request.Resume, JD: request.JD, Role: request.Role}
	if err := validateInput(&input); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	id, code, duplicate, err := a.store.Create(r.Context(), owner, a.clientIP(r), input, a.config, time.Now())
	if err != nil {
		if billingError(w, err) {
			return
		}
		switch {
		case errors.Is(err, ErrRateLimit):
			w.Header().Set("Retry-After", "3600")
			jsonError(w, 429, "已达到当前诊断频率上限，请稍后再来；本次未扣除购买次数。")
		case errors.Is(err, ErrQueueFull):
			w.Header().Set("Retry-After", "30")
			jsonError(w, 503, "目前排队人数较多，请稍后再试。")
		case errors.Is(err, ErrActiveLimit):
			jsonError(w, 409, "你已有两份材料正在分析，请等待完成后再提交。")
		default:
			jsonError(w, 500, "暂时无法保存任务，请稍后再试。")
		}
		return
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
	jsonResponse(w, 202, map[string]any{"id": id, "recovery_code": code, "duplicate": duplicate, "expires_at": time.Now().Add(a.config.Retention).Unix()})
}

func (a *App) listDiagnoses(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.authorize(w, r, false)
	if !ok {
		return
	}
	list, err := a.store.List(r.Context(), owner, time.Now())
	if err != nil {
		jsonError(w, 500, "报告列表暂时无法读取，请刷新重试。")
		return
	}
	jsonResponse(w, 200, map[string]any{"diagnoses": list})
}

func (a *App) getOwned(w http.ResponseWriter, r *http.Request, write bool) (Diagnosis, bool) {
	owner, ok := a.authorize(w, r, write)
	if !ok {
		return Diagnosis{}, false
	}
	id := r.PathValue("id")
	if !validID(id) {
		jsonError(w, 404, "报告不存在、已过期，或当前浏览器没有访问权限。")
		return Diagnosis{}, false
	}
	d, err := a.store.Get(r.Context(), owner, id, time.Now())
	if errors.Is(err, ErrNotFound) {
		jsonError(w, 404, "报告不存在、已过期，或当前浏览器没有访问权限。")
		return d, false
	}
	if err != nil {
		jsonError(w, 500, "暂时无法读取报告，请稍后重试。")
		return d, false
	}
	return d, true
}
func (a *App) getDiagnosis(w http.ResponseWriter, r *http.Request) {
	if d, ok := a.getOwned(w, r, false); ok {
		jsonResponse(w, 200, d)
	}
}

func (a *App) exportDiagnosis(w http.ResponseWriter, r *http.Request) {
	d, ok := a.getOwned(w, r, false)
	if !ok {
		return
	}
	if d.Status != "done" || d.Report == nil {
		jsonError(w, 409, "报告完成后即可导出。")
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="jobcompass-`+d.ID[:8]+`.md"`)
	_, _ = io.WriteString(w, reportMarkdown(*d.Report, createdLabel(d.CreatedAt)))
}

func (a *App) deleteDiagnosis(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.authorize(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !validID(id) {
		jsonError(w, 404, "报告不存在或已删除。")
		return
	}
	err := a.store.Delete(r.Context(), owner, id, time.Now())
	if errors.Is(err, ErrNotFound) {
		jsonError(w, 404, "报告不存在、已过期，或当前浏览器没有访问权限。")
		return
	}
	if err != nil {
		jsonError(w, 500, "删除未完成，请稍后重试。")
		return
	}
	a.cancelJob(id)
	jsonResponse(w, 200, map[string]bool{"deleted": true})
}

func (a *App) recoverDiagnosis(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.authorize(w, r, true)
	if !ok {
		return
	}
	if err := a.store.TakeLimit(r.Context(), a.store.privateHash("recover:"+a.clientIP(r)), time.Now(), time.Hour, 10); err != nil {
		if errors.Is(err, ErrRateLimit) {
			w.Header().Set("Retry-After", "3600")
			jsonError(w, 429, "尝试恢复的次数较多，请稍后再试。")
		} else {
			jsonError(w, 500, "恢复服务暂不可用。")
		}
		return
	}
	var request struct {
		Code string `json:"code"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	code, err := normalizeRecovery(request.Code)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	id, err := a.store.Recover(r.Context(), owner, code, time.Now())
	if errors.Is(err, ErrNotFound) {
		jsonError(w, 404, "未找到对应报告，请检查恢复码；报告也可能已过期或被删除。")
		return
	}
	if err != nil {
		jsonError(w, 500, "暂时无法恢复，请稍后重试。")
		return
	}
	jsonResponse(w, 200, map[string]string{"id": id})
}

func (a *App) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if a.config.Production && ip != nil && ip.IsLoopback() {
		// The supplied Caddy config overwrites X-Forwarded-For with the direct remote host.
		forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
		if parsed := net.ParseIP(forwarded); parsed != nil {
			return parsed.String()
		}
	}
	return host
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func jsonError(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]string{"error": message})
}
