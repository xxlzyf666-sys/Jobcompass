package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type preparationFixtureProvider struct {
	fixtureProvider
	prepare func(context.Context, PreparationInput) (PreparationOutput, Usage, error)
}

func (p preparationFixtureProvider) Prepare(ctx context.Context, in PreparationInput) (PreparationOutput, Usage, error) {
	return p.prepare(ctx, in)
}

func sourceLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "Redis") && len(line) > 8 {
			return line
		}
	}
	return strings.Split(text, "\n")[0]
}
func preparationFixture(in PreparationInput) PreparationOutput {
	if in.Kind == "questions" {
		return PreparationOutput{Questions: []RefinementQuestion{{Quote: sourceLine(in.Version.Text), Question: "这段经历中你实际负责什么工作？", Why: "补充个人职责边界。"}, {Quote: sourceLine(in.Version.Text), Question: "你如何验证方案，有哪些事实可核实？", Why: "补充可验证的结果。"}}}
	}
	if in.Kind == "rewrite" || in.Kind == "tailor" {
		before := sourceLine(in.Version.Text)
		after := "相关实践：" + before
		evidence := []Evidence{{Source: "resume", Quote: before}}
		if len(in.Version.Facts) > 0 {
			after = before + "\n" + in.Version.Facts[0]
			evidence = append(evidence, Evidence{Source: "fact", Quote: in.Version.Facts[0]})
		}
		return PreparationOutput{Edits: []ResumeEdit{{Before: before, After: after, Reason: "结合已确认事实说明个人职责。", Evidence: evidence}}, Notes: []string{"没有依据的指标暂不写入。"}}
	}
	i := in.Interview
	q := &InterviewQuestion{Question: fmt.Sprintf("第 %d 题：请说明你的项目职责和方案取舍。", len(i.Turns)+1), Focus: "项目职责与取舍", ResumeQuote: sourceLine(i.Resume)}
	if in.Kind == "interview_start" {
		return PreparationOutput{Question: q}
	}
	r := &InterviewReview{Summary: "回答说明了实际职责，下一轮需要补充异常处理的设计取舍。", Strengths: []string{"明确说明了自己的职责。"}, Gaps: []string{"异常处理还需要展开。"}, PracticePlan: []string{"梳理失败场景，再说明可行的处理方案。"}}
	if in.Kind == "interview_review" {
		return PreparationOutput{Review: r}
	}
	answer := i.Turns[len(i.Turns)-1].Answer
	f := &AnswerFeedback{AnswerQuote: answer, Assessment: "回答已经说明过程，但验证方法还不充分。", Improvement: "补充实际观察和你负责的处理步骤。", Outline: []string{"说明职责和约束。", "说明方案与验证方法。"}}
	if len(i.Turns) < i.Rounds {
		q.Question += " 继续解释：“" + answer + "”。"
		return PreparationOutput{Question: q, Feedback: f}
	}
	return PreparationOutput{Feedback: f, Review: r}
}
func preparationApp(t *testing.T) (*App, *browser, string, string) {
	_, report := fixture(t)
	p := preparationFixtureProvider{fixtureProvider: fixtureProvider{ready: true, run: func(context.Context, Input) (Report, Usage, error) { return report, Usage{}, nil }}, prepare: func(_ context.Context, in PreparationInput) (PreparationOutput, Usage, error) {
		return preparationFixture(in), Usage{Input: 10, Output: 20}, nil
	}}
	a := testApp(t, p)
	b := newBrowser(t, a)
	input, _ := fixture(t)
	w := b.request("POST", "/api/diagnoses", map[string]any{"resume": input.Resume, "jd": input.JD, "role": "backend", "consent": true, "consent_version": consentVersion})
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		ID   string `json:"id"`
		Code string `json:"recovery_code"`
	}
	json.Unmarshal(w.Body.Bytes(), &result)
	if !a.processOne(context.Background()) {
		t.Fatal("diagnosis did not run")
	}
	return a, b, result.ID, result.Code
}

type preparationResponse struct {
	Preparation Preparation      `json:"preparation"`
	Task        *PreparationTask `json:"task"`
	Expires     int64            `json:"expires_at"`
}

func readPreparation(t *testing.T, b *browser, id string) preparationResponse {
	t.Helper()
	w := b.request("GET", "/api/diagnoses/"+id+"/preparation", nil)
	if w.Code != 200 {
		t.Fatalf("read preparation: %d %s", w.Code, w.Body.String())
	}
	var result preparationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func changePreparation(t *testing.T, b *browser, id, action string, fields map[string]any, want int) preparationResponse {
	t.Helper()
	p := readPreparation(t, b, id)
	body := map[string]any{"action": action, "revision": p.Preparation.Revision, "version_id": "base", "consent": true, "consent_version": consentVersion}
	for k, v := range fields {
		body[k] = v
	}
	w := b.request("POST", "/api/diagnoses/"+id+"/preparation", body)
	if w.Code != want {
		t.Fatalf("%s: want %d got %d: %s", action, want, w.Code, w.Body.String())
	}
	if want != 200 && want != 202 {
		return p
	}
	var result preparationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func finishPreparation(t *testing.T, a *App, b *browser, id string) preparationResponse {
	t.Helper()
	if !a.processPreparation(context.Background()) {
		t.Fatal("preparation job not claimed")
	}
	p := readPreparation(t, b, id)
	if p.Task == nil || p.Task.Status != "done" {
		t.Fatalf("task not completed: %+v", p.Task)
	}
	return p
}

func TestPreparationRefinementVersionsAndPrivateExports(t *testing.T) {
	a, b, id, _ := preparationApp(t)
	original := readPreparation(t, b, id).Preparation.Versions[0]
	changePreparation(t, b, id, "questions", nil, 202)
	p := finishPreparation(t, a, b, id)
	if len(p.Preparation.Versions[0].Questions) != 2 {
		t.Fatal("missing questions")
	}
	fact := "我负责缓存读取逻辑，与同事一起评审删除缓存的处理方案。"
	changePreparation(t, b, id, "rewrite", map[string]any{"confirmed": true, "answers": []any{map[string]string{"id": "q1", "answer": fact}, map[string]string{"id": "q2", "answer": "通过日志检查接口错误，没有统计延迟改善比例。"}}}, 202)
	p = finishPreparation(t, a, b, id)
	if p.Preparation.Versions[0].Text != original.Text {
		t.Fatal("suggestion silently rewrote resume")
	}
	if len(p.Preparation.Versions[0].Edits) != 1 {
		t.Fatal("missing source diff")
	}
	stale := p.Preparation.Revision
	p = changePreparation(t, b, id, "accept_edit", map[string]any{"edit_id": "e1", "confirmed": true}, 200)
	if !strings.Contains(p.Preparation.Versions[0].Text, fact) {
		t.Fatal("accepted edit not saved")
	}
	if b.request("POST", "/api/diagnoses/"+id+"/preparation", map[string]any{"action": "accept_edit", "revision": stale, "version_id": "base", "edit_id": "e1", "confirmed": true}).Code != 409 {
		t.Fatal("stale write accepted")
	}
	targetJD := original.JD + "\n重点考察项目异常处理，岗位专属秘密标记。"
	p = changePreparation(t, b, id, "new_version", map[string]any{"name": "测试企业 · Go 后端", "jd": targetJD}, 200)
	v := p.Preparation.Versions[1]
	if v.Text != p.Preparation.Versions[0].Text || len(v.Facts) != 2 || v.JD != targetJD {
		t.Fatal("version did not copy confirmed material")
	}
	text := v.Text + "\n<script>alert('user-text')</script>\n版本独有的手动修改。"
	p = changePreparation(t, b, id, "save_version", map[string]any{"version_id": v.ID, "name": "岗位 <版本>", "text": text, "jd": targetJD, "confirmed": true}, 200)
	if strings.Contains(p.Preparation.Versions[0].Text, "版本独有") {
		t.Fatal("version leaked into base")
	}
	export := "/api/diagnoses/" + id + "/versions/" + v.ID + "/export"
	w := b.request("GET", export+"?format=html", nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "&lt;script&gt;") || strings.Contains(w.Body.String(), "<script>alert") || strings.Contains(w.Body.String(), "岗位专属秘密标记") {
		t.Fatal("unsafe or incorrect resume-only export", w.Code)
	}
	if got := b.request("GET", export, nil); got.Body.String() != text {
		t.Fatal("text export differs from saved version")
	}
	other := newBrowser(t, a)
	if other.request("GET", export, nil).Code != 404 || other.request("GET", "/api/diagnoses/"+id+"/preparation", nil).Code != 404 {
		t.Fatal("cross-session access allowed")
	}
	if other.request("POST", "/api/diagnoses/"+id+"/preparation", map[string]any{"action": "delete_version", "version_id": v.ID, "revision": p.Preparation.Revision}).Code != 404 {
		t.Fatal("cross-owner write allowed")
	}
	var encrypted []byte
	if err := a.store.db.QueryRow("SELECT data_cipher FROM preparations WHERE diagnosis_id=?", id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), fact) || strings.Contains(string(encrypted), text) {
		t.Fatal("preparation stored as plaintext")
	}
	changePreparation(t, b, id, "delete_version", map[string]any{"version_id": v.ID}, 200)
	if b.request("GET", export, nil).Code != 404 {
		t.Fatal("deleted version still exportable")
	}
}

func TestInterviewConversationRecoveryAndSnapshot(t *testing.T) {
	a, b, id, code := preparationApp(t)
	p := changePreparation(t, b, id, "start_interview", map[string]any{"rounds": 3, "focus": "project"}, 202)
	iid := p.Preparation.Interviews[0].ID
	p = finishPreparation(t, a, b, id)
	original := p.Preparation.Versions[0]
	changePreparation(t, b, id, "save_version", map[string]any{"name": original.Name, "jd": original.JD, "text": original.Text + "\n之后添加的经历，不能改变本轮快照。", "confirmed": true}, 200)
	other := newBrowser(t, a)
	if w := other.request("POST", "/api/recover", map[string]string{"code": code}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for round := 1; round <= 3; round++ {
		p = readPreparation(t, other, id)
		i := p.Preparation.Interviews[0]
		if strings.Contains(i.Resume, "之后添加") {
			t.Fatal("interview source changed after start")
		}
		if len(i.Turns) != round {
			t.Fatal("wrong question count")
		}
		answer := fmt.Sprintf("第%d次回答：我负责读取缓存和检查日志，异常处理方案由团队共同评审。", round)
		fields := map[string]any{"interview_id": iid, "question_id": i.Turns[round-1].Question.ID, "answer": answer}
		changePreparation(t, other, id, "answer", fields, 202)
		changePreparation(t, other, id, "answer", fields, 409)
		p = finishPreparation(t, a, other, id)
		if p.Preparation.Interviews[0].Turns[round-1].Feedback.AnswerQuote != answer {
			t.Fatal("feedback not anchored to actual answer")
		}
		if round < 3 && !strings.Contains(p.Preparation.Interviews[0].Turns[round].Question.Question, answer) {
			t.Fatal("follow-up did not receive latest answer")
		}
	}
	i := p.Preparation.Interviews[0]
	if i.Status != "done" || i.Review == nil || len(i.Turns) != 3 {
		t.Fatal("interview failed to stop and review")
	}
	p = changePreparation(t, other, id, "start_interview", map[string]any{"rounds": 3, "focus": "project", "interview_id": iid}, 202)
	if !reflect.DeepEqual(p.Preparation.Interviews[1].Practice, i.Review.Gaps) {
		t.Fatal("retraining lost gaps")
	}
	finishPreparation(t, a, other, id)
	second := p.Preparation.Interviews[1].ID
	changePreparation(t, other, id, "answer", map[string]any{"interview_id": second, "question_id": "q1", "answer": "我会先说明约束，再解释方案取舍和验证方法。"}, 202)
	finishPreparation(t, a, other, id)
	changePreparation(t, other, id, "finish_interview", map[string]any{"interview_id": second}, 202)
	p = finishPreparation(t, a, other, id)
	if len(p.Preparation.Interviews[1].Turns) != 1 || p.Preparation.Interviews[1].Review == nil {
		t.Fatal("early finish reviewed unanswered questions")
	}
}

func TestPreparationGroundingRejectsFabrication(t *testing.T) {
	input, _ := fixture(t)
	in := PreparationInput{Kind: "rewrite", Version: ResumeVersion{Text: input.Resume, JD: input.JD, Facts: []string{"我负责缓存读取与错误检查。"}}}
	for _, name := range []string{"invented_quote", "jd_as_fact", "new_metric", "overlap"} {
		t.Run(name, func(t *testing.T) {
			out := preparationFixture(in)
			switch name {
			case "invented_quote":
				out.Edits[0].Before = "这句话没有出现在简历原文中。"
			case "jd_as_fact":
				out.Edits[0].Evidence = []Evidence{{Source: "jd", Quote: input.JD}}
			case "new_metric":
				out.Edits[0].After += "性能提升 97.234%。"
			case "overlap":
				out.Edits = append(out.Edits, out.Edits[0])
			}
			if err := validatePreparationOutput(&out, in); err == nil {
				t.Fatal("unsafe output accepted")
			}
		})
	}
	i := &Interview{Resume: input.Resume, Rounds: 3, Turns: []InterviewTurn{{Question: InterviewQuestion{Question: "说明你负责的工作。"}, Answer: "我不清楚。"}}}
	interviewIn := PreparationInput{Kind: "interview_answer", Interview: i}
	out := preparationFixture(interviewIn)
	out.Feedback.AnswerQuote = "用户没有说过的话。"
	if validatePreparationOutput(&out, interviewIn) == nil {
		t.Fatal("invented interview quote accepted")
	}
}

func TestPreparationQueueConflictLimitsAndConsent(t *testing.T) {
	a, b, id, _ := preparationApp(t)
	body := map[string]any{"action": "questions", "revision": 0, "version_id": "base", "consent": true, "consent_version": consentVersion}
	w := b.request("POST", "/api/diagnoses/"+id+"/preparation", map[string]any{"action": "questions", "revision": 0, "version_id": "base"})
	if w.Code != 400 || readPreparation(t, b, id).Preparation.Revision != 0 {
		t.Fatal("missing consent persisted data")
	}
	old := b.csrf
	b.csrf = "invalid"
	w = b.request("POST", "/api/diagnoses/"+id+"/preparation", body)
	b.csrf = old
	if w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	a.config.PreparationIPQuota = 1
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for j := 0; j < 2; j++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- b.request("POST", "/api/diagnoses/"+id+"/preparation", body).Code }()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[202] != 1 || counts[409] != 1 {
		t.Fatal("duplicate task queued", counts)
	}
	finishPreparation(t, a, b, id)
	changePreparation(t, b, id, "questions", nil, 429)
	a.config.PreparationIPQuota = 60
	a.config.QueueLimit = 1
	input, _ := fixture(t)
	input.Resume += "\n新的诊断请求。"
	if w := b.request("POST", "/api/diagnoses", map[string]any{"resume": input.Resume, "jd": input.JD, "role": "backend", "consent": true, "consent_version": consentVersion}); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	changePreparation(t, b, id, "questions", nil, 503)
}

func TestPreparationDeleteDuringGenerationAndExpiry(t *testing.T) {
	a, b, id, code := preparationApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	base := a.provider.(preparationFixtureProvider)
	base.prepare = func(_ context.Context, in PreparationInput) (PreparationOutput, Usage, error) {
		close(started)
		<-release
		return preparationFixture(in), Usage{}, nil
	}
	a.provider = base
	changePreparation(t, b, id, "questions", nil, 202)
	done := make(chan struct{})
	go func() { a.processPreparation(context.Background()); close(done) }()
	<-started
	if b.request("DELETE", "/api/diagnoses/"+id, nil).Code != 200 {
		t.Fatal("delete failed")
	}
	close(release)
	<-done
	if b.request("GET", "/api/diagnoses/"+id+"/preparation", nil).Code != 404 {
		t.Fatal("deleted preparation resurrected")
	}
	if b.request("POST", "/api/recover", map[string]string{"code": code}).Code != 404 {
		t.Fatal("deleted recovery still works")
	}
	for _, table := range []string{"preparations", "preparation_tasks"} {
		var n int
		a.store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n)
		if n != 0 {
			t.Fatal("dependent material not removed", table)
		}
	}
	a2, b2, id2, _ := preparationApp(t)
	changePreparation(t, b2, id2, "questions", nil, 202)
	task, err := a2.store.ClaimPreparation(context.Background(), time.Now(), time.Minute)
	if err != nil || task == nil {
		t.Fatal(err)
	}
	if _, err = a2.store.db.Exec("UPDATE diagnoses SET expires_at=? WHERE id=?", time.Now().Add(-time.Second).Unix(), id2); err != nil {
		t.Fatal(err)
	}
	out := preparationFixture(task.Input)
	validatePreparationOutput(&out, task.Input)
	if err = a2.store.CompletePreparation(context.Background(), task, out, Usage{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if b2.request("GET", "/api/diagnoses/"+id2+"/preparation", nil).Code != 404 {
		t.Fatal("expired material exposed")
	}
	if err = a2.store.Cleanup(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationLeaseRecoveryAndExplicitRetry(t *testing.T) {
	a, b, id, _ := preparationApp(t)
	changePreparation(t, b, id, "questions", nil, 202)
	now := time.Now()
	first, err := a.store.ClaimPreparation(context.Background(), now, time.Second)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	if err = a.store.RecoverLeases(context.Background(), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := a.store.ClaimPreparation(context.Background(), now.Add(2*time.Second), time.Second)
	if err != nil || second == nil || second.Attempts != 2 || second.Token == first.Token {
		t.Fatal("lease did not recover", err)
	}
	out := preparationFixture(first.Input)
	validatePreparationOutput(&out, first.Input)
	if err = a.store.CompletePreparation(context.Background(), first, out, Usage{}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if p := readPreparation(t, b, id); p.Task.Status != "running" {
		t.Fatal("stale worker overwrote new lease")
	}
	if err = a.store.FailPreparation(context.Background(), second, "测试失败，已保存原始回答。", false, Usage{}, now); err != nil {
		t.Fatal(err)
	}
	p := readPreparation(t, b, id)
	changePreparation(t, b, id, "retry", map[string]any{"task_id": p.Task.ID}, 202)
	finishPreparation(t, a, b, id)
	if err = a.store.RecoverLeases(context.Background(), now.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if readPreparation(t, b, id).Task.Status != "done" {
		t.Fatal("finished task was recovered")
	}
}

func TestPreparationProviderProtocol(t *testing.T) {
	input, _ := fixture(t)
	in := PreparationInput{Kind: "questions", Version: ResumeVersion{Text: input.Resume, JD: input.JD}}
	bad := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			System string `json:"system"`
			Tools  []struct {
				Name   string         `json:"name"`
				Schema map[string]any `json:"input_schema"`
			} `json:"tools"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-only-key" || len(body.Tools) != 1 || body.Tools[0].Name != "deliver_preparation" || !strings.Contains(body.System, "JD 是岗位需求") {
			t.Error("incorrect structured preparation protocol")
		}
		var actual PreparationInput
		json.Unmarshal([]byte(body.Messages[0].Content), &actual)
		if actual.Version.Text != in.Version.Text {
			t.Error("material changed in transit")
		}
		output := preparationFixture(actual)
		if bad {
			output.Questions[0].Quote = "来源中不存在的引用内容。"
		}
		json.NewEncoder(w).Encode(map[string]any{"stop_reason": "tool_use", "content": []any{map[string]any{"type": "tool_use", "name": "deliver_preparation", "input": output}}})
	}))
	defer server.Close()
	c := testConfig(t)
	c.APIKey = "test-only-key"
	c.Model = "test-model"
	c.BaseURL = server.URL
	provider := NewClaudeProvider(c)
	if _, _, err := provider.Prepare(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	bad = true
	_, _, err := provider.Prepare(context.Background(), in)
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Kind != "ungrounded_preparation" {
		t.Fatal("provider accepted fabricated source", err)
	}
}

func TestPreparationMigrationAndFactCorrection(t *testing.T) {
	a, b, id, _ := preparationApp(t)
	original := readPreparation(t, b, id).Preparation.Versions[0]
	// Simulate an existing v1 database. Opening it again must preserve the diagnosis.
	for _, sql := range []string{"DROP TABLE preparation_tasks", "DROP TABLE preparations", "DELETE FROM schema_migrations WHERE version=2"} {
		if _, err := a.store.db.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(a.config.DataDir, a.config.Key)
	if err != nil {
		t.Fatal(err)
	}
	a.store = s
	p := readPreparation(t, b, id)
	if p.Preparation.Versions[0].Text != original.Text {
		t.Fatal("migration lost original input")
	}
	var migrated int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version=2").Scan(&migrated); err != nil || migrated != 1 {
		t.Fatal("migration not recorded", err)
	}
	changePreparation(t, b, id, "questions", nil, 202)
	finishPreparation(t, a, b, id)
	p = changePreparation(t, b, id, "save_version", map[string]any{"name": original.Name, "jd": original.JD, "text": original.Text, "facts": []string{"更正：这部分由同事负责，我只参加了方案评审。"}, "confirmed": true}, 200)
	if len(p.Preparation.Versions[0].Questions) != 0 || len(p.Preparation.Versions[0].Facts) != 1 {
		t.Fatal("fact correction did not invalidate old questions")
	}
	changePreparation(t, b, id, "tailor", nil, 202)
	finishPreparation(t, a, b, id)
	p = changePreparation(t, b, id, "save_version", map[string]any{"name": original.Name, "jd": original.JD, "text": original.Text, "facts": []string{}, "confirmed": true}, 200)
	if len(p.Preparation.Versions[0].Facts) != 0 || len(p.Preparation.Versions[0].Edits) != 0 {
		t.Fatal("deleted fact or obsolete rewrite retained")
	}
	provider := a.provider.(preparationFixtureProvider)
	provider.ready = false
	a.provider = provider
	changePreparation(t, b, id, "questions", nil, 503)
	if readPreparation(t, b, id).Preparation.Revision != p.Preparation.Revision {
		t.Fatal("unconfigured AI persisted a task")
	}
}
