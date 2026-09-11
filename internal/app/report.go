package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const rubricVersion = "backend-evidence-v1.0"
const consentVersion = "2026-09-12"

type Input struct {
	Resume string `json:"resume"`
	JD     string `json:"jd"`
	Role   string `json:"role"`
}

type Requirement struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	JDQuote     string `json:"jd_quote"`
	Priority    string `json:"priority"`
	Status      string `json:"status"`
	ResumeQuote string `json:"resume_quote"`
	Explanation string `json:"explanation"`
}

type Action struct {
	RequirementID string `json:"requirement_id"`
	Title         string `json:"title"`
	Suggestion    string `json:"suggestion"`
	Question      string `json:"question"`
}

type Counts struct {
	Supported   int `json:"supported"`
	NeedsDetail int `json:"needs_detail"`
	NotFound    int `json:"not_found"`
}

type Report struct {
	Title         string        `json:"title"`
	Summary       string        `json:"summary"`
	Requirements  []Requirement `json:"requirements"`
	Actions       []Action      `json:"actions"`
	Counts        Counts        `json:"counts"`
	RubricVersion string        `json:"rubric_version"`
	Model         string        `json:"model"`
	Example       bool          `json:"example"`
}

func validateInput(input *Input) error {
	input.Resume = cleanInput(input.Resume)
	input.JD = cleanInput(input.JD)
	if input.Role != "backend" {
		return errors.New("首版支持后端开发岗位，请选择后端开发方向。")
	}
	if n := utf8.RuneCountInString(input.Resume); n < 100 || n > 20000 {
		return errors.New("简历文字需为 100—20,000 字符，请检查提取结果或粘贴完整经历。")
	}
	if n := utf8.RuneCountInString(input.JD); n < 80 || n > 12000 {
		return errors.New("岗位要求需为 80—12,000 字符，请粘贴完整 JD。")
	}
	return nil
}

func cleanInput(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text))
}

func normalized(text string) string { return strings.Join(strings.Fields(text), " ") }
func containsQuote(source, quote string) bool {
	q := normalized(quote)
	return utf8.RuneCountInString(q) >= 4 && strings.Contains(normalized(source), q)
}
func textLength(text string, min, max int) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(text))
	return n >= min && n <= max
}

func validateReport(report *Report, input Input) error {
	if !textLength(report.Title, 2, 100) || !textLength(report.Summary, 10, 700) {
		return errors.New("invalid report introduction")
	}
	if len(report.Requirements) < 3 || len(report.Requirements) > 12 {
		return errors.New("report must contain 3 to 12 requirements")
	}
	if len(report.Actions) < 1 || len(report.Actions) > 3 {
		return errors.New("report must contain 1 to 3 actions")
	}
	seen := map[string]bool{}
	quotes := map[string]bool{}
	report.Counts = Counts{}
	for i := range report.Requirements {
		r := &report.Requirements[i]
		if r.ID != fmt.Sprintf("r%d", i+1) || seen[r.ID] {
			return errors.New("invalid requirement identity")
		}
		seen[r.ID] = true
		if !textLength(r.Title, 2, 100) || !textLength(r.Explanation, 5, 600) || !textLength(r.JDQuote, 4, 1000) || !containsQuote(input.JD, r.JDQuote) {
			return errors.New("JD citation is not grounded in submitted text")
		}
		if quotes[normalized(r.JDQuote)] {
			return errors.New("duplicate requirement citation")
		}
		quotes[normalized(r.JDQuote)] = true
		if r.Priority != "required" && r.Priority != "preferred" {
			return errors.New("invalid requirement priority")
		}
		switch r.Status {
		case "supported", "needs_detail":
			if !textLength(r.ResumeQuote, 4, 1500) || !containsQuote(input.Resume, r.ResumeQuote) {
				return errors.New("resume citation is not grounded in submitted text")
			}
			if r.Status == "supported" {
				report.Counts.Supported++
			} else {
				report.Counts.NeedsDetail++
			}
		case "not_found":
			if strings.TrimSpace(r.ResumeQuote) != "" {
				return errors.New("missing evidence cannot contain a fabricated citation")
			}
			report.Counts.NotFound++
		default:
			return errors.New("invalid evidence status")
		}
	}
	for _, action := range report.Actions {
		if !seen[action.RequirementID] || !textLength(action.Title, 2, 100) || !textLength(action.Suggestion, 10, 600) || !textLength(action.Question, 5, 400) {
			return errors.New("invalid action or requirement reference")
		}
	}
	// Counts and metadata always come from the application, never from model claims.
	report.RubricVersion = rubricVersion
	report.Example = false
	return nil
}

func reportMarkdown(report Report, created string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	if report.Example {
		line("> 固定虚构示例，用于展示结构；不是对你提交材料的诊断。\n")
	}
	line("# %s\n", markdownText(report.Title))
	line("生成时间：%s  ·  规则版本：%s\n", created, report.RubricVersion)
	line("%s\n", markdownText(report.Summary))
	line("已有依据 %d 项 · 待补充 %d 项 · 未体现 %d 项\n", report.Counts.Supported, report.Counts.NeedsDetail, report.Counts.NotFound)
	labels := map[string]string{"supported": "已有依据", "needs_detail": "待补充", "not_found": "未体现"}
	for i, r := range report.Requirements {
		line("## %d. %s [%s]\n", i+1, markdownText(r.Title), labels[r.Status])
		line("**JD 原文**：%s\n", markdownText(r.JDQuote))
		if r.ResumeQuote == "" {
			line("**简历依据**：未找到明确表述，不代表本人没有这项能力。\n")
		} else {
			line("**简历原文**：%s\n", markdownText(r.ResumeQuote))
		}
		line("**判断说明**：%s\n", markdownText(r.Explanation))
	}
	line("## 优先修改建议\n")
	for i, action := range report.Actions {
		line("### %d. %s\n", i+1, markdownText(action.Title))
		line("%s\n", markdownText(action.Suggestion))
		line("**需要你确认**：%s\n", markdownText(action.Question))
	}
	line("---\n岗位罗盘 · AI 辅助分析。引文经过来源核对，语义判断仍需本人确认。请仅补充真实经历；证据状态不代表录用概率。")
	return b.String()
}

func markdownText(text string) string {
	r := strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "#", "\\#", "\r", "", "\n", " ")
	return r.Replace(text)
}

func reportSchema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object", "additionalProperties":false,
  "required":["title","summary","requirements","actions"],
  "properties":{
    "title":{"type":"string"}, "summary":{"type":"string"},
    "requirements":{"type":"array","minItems":3,"maxItems":12,"items":{
      "type":"object","additionalProperties":false,
      "required":["id","title","jd_quote","priority","status","resume_quote","explanation"],
      "properties":{
        "id":{"type":"string"},"title":{"type":"string"},"jd_quote":{"type":"string"},
        "priority":{"type":"string","enum":["required","preferred"]},
        "status":{"type":"string","enum":["supported","needs_detail","not_found"]},
        "resume_quote":{"type":"string"},"explanation":{"type":"string"}
      }}},
    "actions":{"type":"array","minItems":1,"maxItems":3,"items":{
      "type":"object","additionalProperties":false,
      "required":["requirement_id","title","suggestion","question"],
      "properties":{"requirement_id":{"type":"string"},"title":{"type":"string"},"suggestion":{"type":"string"},"question":{"type":"string"}}
    }}
  }
}`)
}
