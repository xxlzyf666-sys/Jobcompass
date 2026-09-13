package app

import (
	"errors"
	"html/template"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"
)

type preparationRequest struct {
	Action         string    `json:"action"`
	Revision       int64     `json:"revision"`
	VersionID      string    `json:"version_id"`
	Name           string    `json:"name"`
	JD             string    `json:"jd"`
	Text           string    `json:"text"`
	Facts          *[]string `json:"facts"`
	EditID         string    `json:"edit_id"`
	Confirmed      bool      `json:"confirmed"`
	InterviewID    string    `json:"interview_id"`
	QuestionID     string    `json:"question_id"`
	Answer         string    `json:"answer"`
	Rounds         int       `json:"rounds"`
	Focus          string    `json:"focus"`
	TaskID         string    `json:"task_id"`
	Consent        bool      `json:"consent"`
	ConsentVersion string    `json:"consent_version"`
	Answers        []struct {
		ID     string `json:"id"`
		Answer string `json:"answer"`
	} `json:"answers"`
}

func preparationError(w http.ResponseWriter, err error) {
	if billingError(w, err) {
		return
	}
	switch {
	case errors.Is(err, ErrNotFound):
		jsonError(w, 404, "求职准备资料不存在、已到期，或当前浏览器没有访问权限。")
	case errors.Is(err, ErrConflict):
		jsonError(w, 409, "内容已更新或仍在生成，请刷新工作台后继续。诊断完成后才能开始准备。")
	case errors.Is(err, ErrQueueFull):
		w.Header().Set("Retry-After", "30")
		jsonError(w, 503, "目前生成任务较多，请稍后再试。")
	case errors.Is(err, ErrRateLimit):
		w.Header().Set("Retry-After", "3600")
		jsonError(w, 429, "已达到本时段的生成频率上限，请稍后继续；本次未扣除购买次数，已保存内容仍可查看和导出。")
	default:
		jsonError(w, 500, "求职准备资料暂时无法读写，请稍后重试。")
	}
}

func (a *App) preparationResponse(w http.ResponseWriter, r *http.Request, owner string, status int) {
	p, task, expires, err := a.store.GetPreparation(r.Context(), owner, r.PathValue("id"), time.Now())
	if err != nil {
		preparationError(w, err)
		return
	}
	_, supported := a.provider.(PreparationProvider)
	settings, err := a.store.BillingSettings(r.Context())
	if err != nil {
		preparationError(w, err)
		return
	}
	account, err := a.store.BillingAccount(r.Context(), owner)
	if err != nil {
		preparationError(w, err)
		return
	}
	restricted := account != nil && account.AIRestricted
	jsonResponse(w, status, map[string]any{"preparation": p, "task": task, "expires_at": expires, "ready": a.provider.Ready() && supported, "billing_enabled": settings.Enabled, "ai_restricted": restricted})
}
func (a *App) getPreparation(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.authorize(w, r, false)
	if !ok {
		return
	}
	a.preparationResponse(w, r, owner, 200)
}

func (a *App) changePreparation(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.authorize(w, r, true)
	if !ok {
		return
	}
	var req preparationRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	id, now := r.PathValue("id"), time.Now()
	p, task, _, err := a.store.GetPreparation(r.Context(), owner, id, now)
	if err != nil {
		preparationError(w, err)
		return
	}
	if p.Revision != req.Revision || (task != nil && (task.Status == "pending" || task.Status == "running")) {
		preparationError(w, ErrConflict)
		return
	}
	bad := func(message string) { jsonError(w, 400, message) }
	v := p.version(req.VersionID)
	var input *PreparationInput
	switch req.Action {
	case "new_version":
		if v == nil {
			preparationError(w, ErrNotFound)
			return
		}
		if len(p.Versions) >= maxVersions {
			bad("每份报告最多保存 8 个简历版本，请先导出并删除不再需要的版本。")
			return
		}
		name, jd := cleanInput(req.Name), cleanInput(req.JD)
		if !textLength(name, 2, 80) || !textLength(jd, 80, 12000) {
			bad("请填写 2—80 字符的版本名和 80—12,000 字符的岗位描述。")
			return
		}
		newID, e := randomHex(16)
		if e != nil {
			preparationError(w, e)
			return
		}
		p.Versions = append(p.Versions, ResumeVersion{ID: newID, Name: name, JD: jd, Text: v.Text, Facts: slices.Clone(v.Facts), CreatedAt: now.Unix(), UpdatedAt: now.Unix()})
	case "save_version":
		if v == nil {
			preparationError(w, ErrNotFound)
			return
		}
		name, text, jd := cleanInput(req.Name), cleanInput(req.Text), cleanInput(req.JD)
		if !req.Confirmed || !textLength(name, 2, 80) || !textLength(text, 100, 20000) || !textLength(jd, 80, 12000) {
			bad("请核对经历后保存：版本名 2—80 字符、简历 100—20,000 字符、岗位描述 80—12,000 字符。")
			return
		}
		factsChanged := false
		if req.Facts != nil {
			facts := []string{}
			if len(*req.Facts) > 40 {
				bad("补充事实最多保留 40 条，请合并或精简。")
				return
			}
			for _, fact := range *req.Facts {
				fact = cleanInput(fact)
				if !textLength(fact, 2, 1600) {
					bad("每条补充事实需为 2—1,600 字符。")
					return
				}
				if !slices.Contains(facts, fact) {
					facts = append(facts, fact)
				}
			}
			if !textLength(strings.Join(facts, "\n"), 0, 12000) {
				bad("补充事实总计不能超过 12,000 字符。")
				return
			}
			factsChanged = !slices.Equal(v.Facts, facts)
			v.Facts = facts
		}
		if text != v.Text || jd != v.JD || factsChanged {
			v.Questions, v.Edits, v.Notes = nil, nil, nil
			v.RefinementID, v.RefinementComplete = "", false
		}
		v.Name, v.Text, v.JD, v.UpdatedAt = name, text, jd, now.Unix()
	case "delete_version":
		if v == nil {
			preparationError(w, ErrNotFound)
			return
		}
		if v.ID == "base" {
			bad("基础简历需要保留，可以删除其他岗位版本。")
			return
		}
		p.Versions = slices.DeleteFunc(p.Versions, func(item ResumeVersion) bool { return item.ID == req.VersionID })
	case "questions", "rewrite", "tailor":
		if v == nil {
			preparationError(w, ErrNotFound)
			return
		}
		if req.Action == "questions" {
			cycle, e := randomHex(16)
			if e != nil {
				preparationError(w, e)
				return
			}
			v.RefinementID, v.RefinementComplete = cycle, false
			v.Questions, v.Edits, v.Notes = nil, nil, nil
		}
		if req.Action == "rewrite" {
			if !req.Confirmed || len(v.Questions) == 0 || len(req.Answers) != len(v.Questions) {
				bad("请逐项核对补充回答，确认真实后再生成改写。")
				return
			}
			seen := map[string]bool{}
			for _, answer := range req.Answers {
				if seen[answer.ID] {
					bad("回答重复，请刷新后重试。")
					return
				}
				seen[answer.ID] = true
				matched := false
				for j := range v.Questions {
					q := &v.Questions[j]
					if q.ID != answer.ID {
						continue
					}
					matched = true
					text := cleanInput(answer.Answer)
					if !textLength(text, 2, 1600) {
						bad("每条回答需为 2—1,600 字符；没有相关经历时请如实说明。")
						return
					}
					if q.Answer != "" {
						old := q.Answer
						v.Facts = slices.DeleteFunc(v.Facts, func(f string) bool { return f == old })
					}
					q.Answer = text
					if !slices.Contains(v.Facts, text) {
						v.Facts = append(v.Facts, text)
					}
				}
				if !matched {
					bad("补充问题已更新，请刷新工作台。")
					return
				}
			}
			if !textLength(strings.Join(v.Facts, "\n"), 2, 12000) {
				bad("累计补充事实超过 12,000 字符，请精简回答。")
				return
			}
		}
		v.UpdatedAt = now.Unix()
		input = &PreparationInput{Kind: req.Action, Version: *v}
	case "accept_edit", "reject_edit":
		if v == nil {
			preparationError(w, ErrNotFound)
			return
		}
		var edit *ResumeEdit
		for j := range v.Edits {
			if v.Edits[j].ID == req.EditID {
				edit = &v.Edits[j]
			}
		}
		if edit == nil || edit.Status != "pending" {
			preparationError(w, ErrConflict)
			return
		}
		if req.Action == "accept_edit" {
			if !req.Confirmed {
				bad("请确认建议符合你的真实经历后再采纳。")
				return
			}
			if strings.Count(v.Text, edit.Before) != 1 {
				preparationError(w, ErrConflict)
				return
			}
			text := strings.Replace(v.Text, edit.Before, edit.After, 1)
			if !textLength(text, 100, 20000) {
				bad("采纳后简历长度超出范围，请在正文编辑中调整。")
				return
			}
			v.Text, edit.Status = text, "accepted"
		} else {
			edit.Status = "rejected"
		}
		v.UpdatedAt = now.Unix()
	case "start_interview":
		if v == nil {
			preparationError(w, ErrNotFound)
			return
		}
		if len(p.Interviews) >= maxInterviews {
			bad("每份报告最多保存 6 场面试，请先导出并删除不再需要的记录。")
			return
		}
		if req.Rounds != 3 && req.Rounds != 5 && req.Rounds != 8 {
			bad("请选择 3、5 或 8 题。")
			return
		}
		if !slices.Contains([]string{"project", "backend", "behavioral"}, req.Focus) {
			bad("请选择项目深挖、后端技术或协作表达。")
			return
		}
		newID, e := randomHex(16)
		if e != nil {
			preparationError(w, e)
			return
		}
		i := Interview{ID: newID, VersionID: v.ID, Name: v.Name, Resume: v.Text, JD: v.JD, Facts: slices.Clone(v.Facts), Focus: req.Focus, Rounds: req.Rounds, Status: "starting", Turns: []InterviewTurn{}, CreatedAt: now.Unix()}
		if req.InterviewID != "" {
			previous := p.interview(req.InterviewID)
			if previous == nil || previous.Review == nil {
				bad("请先完成上一轮复盘。")
				return
			}
			i.Practice = slices.Clone(previous.Review.Gaps)
		}
		p.Interviews = append(p.Interviews, i)
		input = &PreparationInput{Kind: "interview_start", Interview: &i}
	case "answer", "finish_interview":
		i := p.interview(req.InterviewID)
		if i == nil {
			preparationError(w, ErrNotFound)
			return
		}
		if i.Status != "active" || len(i.Turns) == 0 {
			preparationError(w, ErrConflict)
			return
		}
		last := &i.Turns[len(i.Turns)-1]
		if req.Action == "answer" {
			answer := cleanInput(req.Answer)
			if last.Question.ID != req.QuestionID || last.Answer != "" {
				preparationError(w, ErrConflict)
				return
			}
			if !textLength(answer, 2, 3000) {
				bad("回答需为 2—3,000 字符，不清楚的部分可以如实说明。")
				return
			}
			last.Answer, i.Status = answer, "thinking"
			input = &PreparationInput{Kind: "interview_answer", Interview: i}
		} else {
			if last.Answer == "" {
				i.Turns = i.Turns[:len(i.Turns)-1]
			}
			if len(i.Turns) == 0 {
				bad("至少回答一题后才能生成复盘。")
				return
			}
			i.Status = "reviewing"
			input = &PreparationInput{Kind: "interview_review", Interview: i}
		}
	case "delete_interview":
		if p.interview(req.InterviewID) == nil {
			preparationError(w, ErrNotFound)
			return
		}
		p.Interviews = slices.DeleteFunc(p.Interviews, func(i Interview) bool { return i.ID == req.InterviewID })
	case "retry":
		if task == nil || task.ID != req.TaskID || task.Status != "failed" {
			preparationError(w, ErrConflict)
			return
		}
		input, err = a.store.RetryPreparation(r.Context(), id, task.ID, task.Revision)
		if err != nil {
			preparationError(w, err)
			return
		}
		if input.Interview != nil {
			if !reflect.DeepEqual(p.interview(input.Interview.ID), input.Interview) {
				preparationError(w, ErrConflict)
				return
			}
		} else if current := p.version(input.Version.ID); current == nil || !reflect.DeepEqual(*current, input.Version) {
			preparationError(w, ErrConflict)
			return
		}
	default:
		bad("没有找到这个操作。")
		return
	}
	if input != nil {
		if _, supported := a.provider.(PreparationProvider); !supported || !a.provider.Ready() {
			jsonError(w, 503, "AI 准备服务暂未开放，已有简历仍可编辑和导出。")
			return
		}
		if !req.Consent || req.ConsentVersion != consentVersion {
			bad("请确认补充材料和回答的处理说明后再生成。")
			return
		}
	}
	if err = a.store.SavePreparation(r.Context(), owner, a.clientIP(r), id, p, input, a.config, now); err != nil {
		preparationError(w, err)
		return
	}
	status := 200
	if input != nil {
		status = 202
		select {
		case a.wake <- struct{}{}:
		default:
		}
	}
	a.preparationResponse(w, r, owner, status)
}

var resumePage = template.Must(template.New("resume").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Name}} · 简历</title><link rel="stylesheet" href="/assets/resume-print.css"><script src="/assets/resume-print.js" defer></script></head><body><header class="print-controls"><div><strong>{{.Name}}</strong><p>导出当前已保存的简历正文。打印时选择“另存为 PDF”，关闭页眉和页脚。</p></div><button id="print-resume" type="button">打印 / 另存为 PDF</button></header><main class="resume-paper"><div class="resume-text">{{.Text}}</div></main></body></html>`))

func (a *App) exportResume(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.authorize(w, r, false)
	if !ok {
		return
	}
	p, _, _, err := a.store.GetPreparation(r.Context(), owner, r.PathValue("id"), time.Now())
	if err != nil {
		preparationError(w, err)
		return
	}
	v := p.version(r.PathValue("version"))
	if v == nil {
		preparationError(w, ErrNotFound)
		return
	}
	if r.URL.Query().Get("format") == "html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = resumePage.Execute(w, v)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="resume-`+v.ID+`.txt"`)
	_, _ = io.WriteString(w, v.Text)
}
