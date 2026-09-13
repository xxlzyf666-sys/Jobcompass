package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) (Input, Report) {
	t.Helper()
	data, err := embedded.ReadFile("example.json")
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		Input  Input  `json:"input"`
		Report Report `json:"report"`
	}
	if err = json.Unmarshal(data, &example); err != nil {
		t.Fatal(err)
	}
	return example.Input, example.Report
}

type fixtureProvider struct {
	ready bool
	run   func(context.Context, Input) (Report, Usage, error)
}

func (p fixtureProvider) Ready() bool { return p.ready }
func (p fixtureProvider) Diagnose(ctx context.Context, input Input) (Report, Usage, error) {
	return p.run(ctx, input)
}

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{Addr: "127.0.0.1:8080", Origin: "http://127.0.0.1:8080", DataDir: t.TempDir(), Key: bytes.Repeat([]byte{7}, 32), BaseURL: "https://api.anthropic.com", ProviderName: "本地测试服务", DataRegion: "本地测试", Workers: 2, QueueLimit: 20, IPQuota: 10, SessionQuota: 10, Retention: 7 * 24 * time.Hour, RequestTimeout: 10 * time.Second}
}
func testApp(t *testing.T, provider Provider) *App {
	t.Helper()
	if provider == nil {
		_, report := fixture(t)
		provider = fixtureProvider{ready: true, run: func(context.Context, Input) (Report, Usage, error) {
			return report, Usage{Input: 100, Output: 200}, nil
		}}
	}
	a, err := New(testConfig(t), provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

type browser struct {
	cookie      *http.Cookie
	csrf        string
	app         *App
	ip          string
	adminCookie *http.Cookie
	adminCSRF   string
}

func newBrowser(t *testing.T, a *App) *browser {
	t.Helper()
	b := &browser{app: a, ip: "192.0.2.1:4321"}
	w := b.request(http.MethodGet, "/api/bootstrap", nil)
	if w.Code != 200 {
		t.Fatalf("bootstrap: %d %s", w.Code, w.Body.String())
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "jc_session" {
			b.cookie = cookie
		}
	}
	var data struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	b.csrf = data.CSRF
	if b.cookie == nil || b.csrf == "" {
		t.Fatal("missing session or CSRF token")
	}
	return b
}
func (b *browser) request(method, target string, body any) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+target, reader)
	r.RemoteAddr = b.ip
	if b.cookie != nil {
		r.AddCookie(b.cookie)
	}
	if b.adminCookie != nil {
		r.AddCookie(b.adminCookie)
	}
	if method != "GET" {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", b.csrf)
		if strings.HasPrefix(target, "/api/admin/") && target != "/api/admin/login" {
			r.Header.Set("X-CSRF-Token", b.adminCSRF)
		}
		r.Header.Set("Origin", b.app.config.Origin)
	}
	w := httptest.NewRecorder()
	b.app.ServeHTTP(w, r)
	return w
}

type submitted struct {
	ID        string `json:"id"`
	Code      string `json:"recovery_code"`
	Duplicate bool   `json:"duplicate"`
}

func submit(t *testing.T, b *browser, input Input) submitted {
	t.Helper()
	w := b.request("POST", "/api/diagnoses", map[string]any{"resume": input.Resume, "jd": input.JD, "role": input.Role, "consent": true, "consent_version": consentVersion})
	if w.Code != 202 {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	var result submitted
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestReportRequiresActualSourceEvidence(t *testing.T) {
	input, report := fixture(t)
	if err := validateInput(&input); err != nil {
		t.Fatal(err)
	}
	if err := validateReport(&report, input); err != nil {
		t.Fatal(err)
	}
	if report.Counts != (Counts{3, 2, 1}) || report.Example {
		t.Fatal("application did not derive counts and real-report metadata")
	}
	cases := map[string]func(*Report){
		"invented resume quote": func(r *Report) {
			r.Requirements[0].ResumeQuote = "把日活从 1.2 万提升到 3.5 万，留存提升 18%"
		},
		"invented JD quote":         func(r *Report) { r.Requirements[0].JDQuote = "必须具备航空发动机设计经验" },
		"missing status with quote": func(r *Report) { r.Requirements[0].Status = "not_found" },
		"unknown action target":     func(r *Report) { r.Actions[0].RequirementID = "r999" },
		"duplicate requirement":     func(r *Report) { r.Requirements[1].JDQuote = r.Requirements[0].JDQuote },
		"invented status":           func(r *Report) { r.Requirements[0].Status = "hireable" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input, r := fixture(t)
			mutate(&r)
			if validateReport(&r, input) == nil {
				t.Fatal("invalid report was accepted")
			}
		})
	}
	input, report = fixture(t)
	report.Requirements[0].JDQuote = "熟悉  Go  语言，具备后端服务开发经验。"
	if err := validateReport(&report, input); err != nil {
		t.Fatalf("whitespace-normalized quote should match: %v", err)
	}
}

func TestPrivateReportLifecycleAndRecovery(t *testing.T) {
	a := testApp(t, nil)
	first := newBrowser(t, a)
	second := newBrowser(t, a)
	input, _ := fixture(t)
	job := submit(t, first, input)
	if !validID(job.ID) || !strings.HasPrefix(job.Code, "JC-") {
		t.Fatal("invalid task credentials")
	}
	duplicate := submit(t, first, input)
	if duplicate.ID != job.ID || !duplicate.Duplicate || duplicate.Code != "" {
		t.Fatal("duplicate did not reuse task or leaked the recovery code")
	}
	for _, method := range []string{"GET", "DELETE"} {
		if w := second.request(method, "/api/diagnoses/"+job.ID, nil); w.Code != 404 {
			t.Fatalf("unauthorized %s got %d", method, w.Code)
		}
	}
	if w := second.request("GET", "/api/diagnoses/"+job.ID+"/export", nil); w.Code != 404 {
		t.Fatal("unauthorized export allowed")
	}
	if !a.processOne(context.Background()) {
		t.Fatal("worker did not claim task")
	}
	w := first.request("GET", "/api/diagnoses/"+job.ID, nil)
	var diagnosis Diagnosis
	if err := json.Unmarshal(w.Body.Bytes(), &diagnosis); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || diagnosis.Status != "done" || diagnosis.Report == nil {
		t.Fatalf("task not complete: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), job.Code) || strings.Contains(w.Body.String(), "resume\":") {
		t.Fatal("private credential or full input leaked into report response")
	}
	export := first.request("GET", "/api/diagnoses/"+job.ID+"/export", nil)
	if export.Code != 200 || !strings.Contains(export.Body.String(), "MySQL") || !strings.Contains(export.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("export failed")
	}
	recovered := second.request("POST", "/api/recover", map[string]string{"code": strings.ToLower(job.Code)})
	if recovered.Code != 200 {
		t.Fatalf("recovery failed: %s", recovered.Body.String())
	}
	if w := second.request("GET", "/api/diagnoses/"+job.ID, nil); w.Code != 200 {
		t.Fatal("recovered browser cannot read report")
	}
	if w := second.request("DELETE", "/api/diagnoses/"+job.ID, nil); w.Code != 200 {
		t.Fatal("authorized deletion failed")
	}
	if w := first.request("GET", "/api/diagnoses/"+job.ID, nil); w.Code != 404 {
		t.Fatal("deleted report remains available")
	}
	if w := first.request("POST", "/api/recover", map[string]string{"code": job.Code}); w.Code != 404 {
		t.Fatal("deleted report recovered")
	}
}

func TestCSRFConsentInputAndUnconfiguredService(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	input, _ := fixture(t)
	body := map[string]any{"resume": input.Resume, "jd": input.JD, "role": "backend", "consent": true, "consent_version": consentVersion}
	csrf := b.csrf
	b.csrf = "forged"
	if w := b.request("POST", "/api/diagnoses", body); w.Code != 403 {
		t.Fatal("forged CSRF token accepted")
	}
	b.csrf = csrf
	body["consent"] = false
	if w := b.request("POST", "/api/diagnoses", body); w.Code != 400 {
		t.Fatal("missing consent accepted")
	}
	body["consent"] = true
	body["resume"] = strings.Repeat("简", 20001)
	if w := b.request("POST", "/api/diagnoses", body); w.Code != 400 {
		t.Fatal("oversized text accepted")
	}
	body["resume"] = input.Resume
	data, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/diagnoses", bytes.NewReader(data))
	r.AddCookie(b.cookie)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", b.csrf)
	r.Header.Set("Origin", "https://unrelated.example")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	preview := testApp(t, fixtureProvider{ready: false})
	p := newBrowser(t, preview)
	if w := p.request("POST", "/api/diagnoses", body); w.Code != 503 {
		t.Fatal("unconfigured service accepted real material")
	}
	var count int
	if err := preview.store.db.QueryRow("SELECT COUNT(*) FROM diagnoses").Scan(&count); err != nil || count != 0 {
		t.Fatal("preview mode stored material")
	}
	if w := p.request("GET", "/api/example", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"example":true`) {
		t.Fatal("clearly marked example unavailable")
	}
}

func TestEncryptionAndExpiry(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	input, _ := fixture(t)
	input.Resume += "\nPRIVATE-CONTACT-TEST-NEVER-PLAINTEXT"
	job := submit(t, b, input)
	a.processOne(context.Background())
	for _, name := range []string{"app.db", "app.db-wal"} {
		raw, err := os.ReadFile(filepath.Join(a.config.DataDir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("PRIVATE-CONTACT-TEST-NEVER-PLAINTEXT")) {
			t.Fatal("source text stored unencrypted")
		}
	}
	wrong, err := OpenStore(a.config.DataDir, bytes.Repeat([]byte{8}, 32))
	if err == nil {
		wrong.Close()
		t.Fatal("wrong encryption key accepted")
	}
	owner, err := a.store.Session(b.cookie.Value, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(8 * 24 * time.Hour)
	if _, err = a.store.Get(context.Background(), owner, job.ID, future); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired report readable before cleanup")
	}
	if err = a.store.Cleanup(context.Background(), future); err != nil {
		t.Fatal(err)
	}
	var diagnoses, access int
	a.store.db.QueryRow("SELECT COUNT(*) FROM diagnoses").Scan(&diagnoses)
	a.store.db.QueryRow("SELECT COUNT(*) FROM diagnosis_access").Scan(&access)
	if diagnoses != 0 || access != 0 {
		t.Fatal("expiry did not remove data and grants")
	}
}

func TestDeletionCannotBeUndoneByLateProviderResult(t *testing.T) {
	input, report := fixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	a := testApp(t, fixtureProvider{ready: true, run: func(context.Context, Input) (Report, Usage, error) {
		close(started)
		<-release
		return report, Usage{}, nil
	}})
	b := newBrowser(t, a)
	job := submit(t, b, input)
	finished := make(chan struct{})
	go func() { a.processOne(context.Background()); close(finished) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	if w := b.request("DELETE", "/api/diagnoses/"+job.ID, nil); w.Code != 200 {
		t.Fatal("in-flight deletion failed")
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}
	var count int
	a.store.db.QueryRow("SELECT COUNT(*) FROM diagnoses").Scan(&count)
	if count != 0 {
		t.Fatal("late provider result resurrected deleted data")
	}
}

func TestPersistentLeaseRecoveryAndBoundedRetries(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	input, _ := fixture(t)
	job := submit(t, b, input)
	now := time.Now()
	ctx := context.Background()
	first, err := a.store.Claim(ctx, now, time.Second)
	if err != nil || first == nil {
		t.Fatal("claim failed", err)
	}
	if err = a.store.Fail(ctx, first, "temporary", true, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	if early, _ := a.store.Claim(ctx, now.Add(time.Second), time.Second); early != nil {
		t.Fatal("retry delay ignored")
	}
	second, err := a.store.Claim(ctx, now.Add(3*time.Second), time.Second)
	if err != nil || second == nil || second.Attempts != 2 {
		t.Fatal("retry did not increment attempt", err)
	}
	if err = a.store.RecoverLeases(ctx, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	third, err := a.store.Claim(ctx, now.Add(6*time.Second), time.Second)
	if err != nil || third == nil || third.Attempts != 3 {
		t.Fatal("expired lease did not recover", err)
	}
	if err = a.store.Fail(ctx, third, "temporary", true, Usage{}, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	var status string
	a.store.db.QueryRow("SELECT status FROM diagnoses WHERE id=?", job.ID).Scan(&status)
	if status != "failed" {
		t.Fatal("retries were not bounded")
	}
	if final, _ := a.store.Claim(ctx, now.Add(time.Minute), time.Second); final != nil {
		t.Fatal("failed task claimed again")
	}
}

func TestQueueQuotaAndConcurrentDeduplication(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	input, _ := fixture(t)
	owner, _ := a.store.Session(b.cookie.Value, time.Now())
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan string, 8)
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, _, err := a.store.Create(ctx, owner, "192.0.2.1", input, a.config, time.Now())
			if err != nil {
				failures <- err
			} else {
				results <- id
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	unique := map[string]bool{}
	for id := range results {
		unique[id] = true
	}
	if len(unique) != 1 {
		t.Fatal("concurrent duplicate tasks created")
	}
	a.config.QueueLimit = 1
	input.Resume += "\n另一份材料。"
	if _, _, _, err := a.store.Create(ctx, owner, "192.0.2.1", input, a.config, time.Now()); !errors.Is(err, ErrQueueFull) {
		t.Fatal("queue limit ignored", err)
	}
	a.processOne(ctx)
	a.config.QueueLimit = 20
	a.config.IPQuota = 1
	if _, _, _, err := a.store.Create(ctx, owner, "192.0.2.1", input, a.config, time.Now()); !errors.Is(err, ErrRateLimit) {
		t.Fatal("IP quota ignored", err)
	}
	for i := 0; i < 10; i++ {
		w := b.request("POST", "/api/recover", map[string]string{"code": "invalid"})
		if w.Code != 400 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w := b.request("POST", "/api/recover", map[string]string{"code": "invalid"}); w.Code != 429 {
		t.Fatal("invalid recovery attempts were not rate limited")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClaudeProtocolAndFailureHandling(t *testing.T) {
	input, report := fixture(t)
	report.Example = false
	config := testConfig(t)
	config.APIKey = "test-only-key"
	config.Model = "test-fixture-model"
	provider := NewClaudeProvider(config)
	provider.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-only-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Error("invalid Anthropic request")
		}
		var request map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&request)
		if !bytes.Contains(request["tools"], []byte("deliver_diagnosis")) || !bytes.Contains(request["system"], []byte("不可信")) {
			t.Error("missing schema or data-boundary prompt")
		}
		// Only the four model-owned fields are emitted by the tool.
		output := map[string]any{"title": report.Title, "summary": report.Summary, "requirements": report.Requirements, "actions": report.Actions}
		data, _ := json.Marshal(map[string]any{"model": "test-fixture-model", "stop_reason": "tool_use", "usage": map[string]int{"input_tokens": 100, "output_tokens": 200}, "content": []any{map[string]any{"type": "tool_use", "name": "deliver_diagnosis", "input": output}}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
	})
	r, usage, err := provider.Diagnose(context.Background(), input)
	if err != nil || r.Counts.Supported != 3 || usage.Output != 200 || r.Model != "test-fixture-model" {
		t.Fatal("valid tool response failed", err)
	}
	for _, status := range []int{401, 429, 500} {
		provider.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("secret resume and key")), Header: make(http.Header)}, nil
		})
		_, _, err := provider.Diagnose(context.Background(), input)
		message, _, retry := providerFailure(err)
		if err == nil || strings.Contains(message, "secret") || retry != (status == 429 || status >= 500) {
			t.Fatalf("unsafe/wrong error handling: %d", status)
		}
	}
}

func TestStaticAssetsAndSecurityHeaders(t *testing.T) {
	a := testApp(t, nil)
	b := newBrowser(t, a)
	page := b.request("GET", "/", nil)
	if !strings.Contains(page.Body.String(), "/assets/app.js?v=") || !strings.Contains(page.Body.String(), "/assets/app.css?v=") {
		t.Fatal("new deployments can load stale cached UI assets")
	}
	for _, path := range []string{"/", "/assets/app.js", "/assets/vendor/pdf.mjs", "/assets/vendor/pdf.worker.mjs", "/healthz"} {
		w := b.request("GET", path, nil)
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatal("missing browser security headers")
		}
	}
	for _, path := range []string{"/assets/", "/assets/../schema.sql", "/data/app.db", "/.env"} {
		if w := b.request("GET", path, nil); w.Code == 200 {
			t.Fatalf("non-public file exposed: %s", path)
		}
	}
}
