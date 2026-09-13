package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const maxVersions = 8
const maxInterviews = 6

var ErrConflict = errors.New("preparation changed or busy")

type RefinementQuestion struct {
	ID       string `json:"id"`
	Quote    string `json:"quote"`
	Question string `json:"question"`
	Why      string `json:"why"`
	Answer   string `json:"answer"`
}
type Evidence struct {
	Source string `json:"source"`
	Quote  string `json:"quote"`
}
type ResumeEdit struct {
	ID       string     `json:"id"`
	Before   string     `json:"before"`
	After    string     `json:"after"`
	Reason   string     `json:"reason"`
	Evidence []Evidence `json:"evidence"`
	Status   string     `json:"status"`
}
type ResumeVersion struct {
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	JD        string               `json:"jd"`
	Text      string               `json:"text"`
	Facts     []string             `json:"facts"`
	Questions []RefinementQuestion `json:"questions"`
	Edits     []ResumeEdit         `json:"edits"`
	Notes     []string             `json:"notes"`
	CreatedAt int64                `json:"created_at"`
	UpdatedAt int64                `json:"updated_at"`
}
type InterviewQuestion struct {
	ID          string `json:"id"`
	Question    string `json:"question"`
	Focus       string `json:"focus"`
	ResumeQuote string `json:"resume_quote"`
}
type AnswerFeedback struct {
	AnswerQuote string   `json:"answer_quote"`
	Assessment  string   `json:"assessment"`
	Improvement string   `json:"improvement"`
	Outline     []string `json:"outline"`
}
type InterviewTurn struct {
	Question InterviewQuestion `json:"question"`
	Answer   string            `json:"answer"`
	Feedback *AnswerFeedback   `json:"feedback,omitempty"`
}
type InterviewReview struct {
	Summary      string   `json:"summary"`
	Strengths    []string `json:"strengths"`
	Gaps         []string `json:"gaps"`
	PracticePlan []string `json:"practice_plan"`
}
type Interview struct {
	ID        string           `json:"id"`
	VersionID string           `json:"version_id"`
	Name      string           `json:"name"`
	Resume    string           `json:"resume"`
	JD        string           `json:"jd"`
	Facts     []string         `json:"facts"`
	Focus     string           `json:"focus"`
	Rounds    int              `json:"rounds"`
	Status    string           `json:"status"`
	Turns     []InterviewTurn  `json:"turns"`
	Review    *InterviewReview `json:"review,omitempty"`
	Practice  []string         `json:"practice"`
	CreatedAt int64            `json:"created_at"`
}
type Preparation struct {
	Revision   int64           `json:"revision"`
	Versions   []ResumeVersion `json:"versions"`
	Interviews []Interview     `json:"interviews"`
}
type PreparationInput struct {
	Kind      string        `json:"kind"`
	Version   ResumeVersion `json:"version"`
	Interview *Interview    `json:"interview,omitempty"`
}
type PreparationOutput struct {
	Questions []RefinementQuestion `json:"questions,omitempty"`
	Edits     []ResumeEdit         `json:"edits,omitempty"`
	Notes     []string             `json:"notes,omitempty"`
	Question  *InterviewQuestion   `json:"question,omitempty"`
	Feedback  *AnswerFeedback      `json:"feedback,omitempty"`
	Review    *InterviewReview     `json:"review,omitempty"`
}
type PreparationProvider interface {
	Prepare(context.Context, PreparationInput) (PreparationOutput, Usage, error)
}

func (p *Preparation) version(id string) *ResumeVersion {
	for i := range p.Versions {
		if p.Versions[i].ID == id {
			return &p.Versions[i]
		}
	}
	return nil
}
func (p *Preparation) interview(id string) *Interview {
	for i := range p.Interviews {
		if p.Interviews[i].ID == id {
			return &p.Interviews[i]
		}
	}
	return nil
}
func defaultPreparation(input Input, created int64) Preparation {
	return Preparation{Versions: []ResumeVersion{{ID: "base", Name: "基础简历", JD: input.JD, Text: input.Resume, Facts: []string{}, CreatedAt: created, UpdatedAt: created}}, Interviews: []Interview{}}
}

var numericClaim = regexp.MustCompile(`[0-9]+(?:[.,][0-9]+)*(?:\s*[%％])?`)

func validatePreparationOutput(out *PreparationOutput, in PreparationInput) error {
	bad := func() error {
		return &ProviderError{"ungrounded_preparation", "结果中的引用或结构未通过核对，请重试；原有简历和回答已保留。", false}
	}
	validList := func(items []string, min, max int) bool {
		if len(items) < min || len(items) > max {
			return false
		}
		for _, text := range items {
			if !textLength(text, 2, 800) {
				return false
			}
		}
		return true
	}
	switch in.Kind {
	case "questions":
		if len(out.Questions) < 2 || len(out.Questions) > 5 {
			return bad()
		}
		seen := map[string]bool{}
		for i := range out.Questions {
			q := &out.Questions[i]
			if !textLength(q.Quote, 4, 1200) || !containsQuote(in.Version.Text, q.Quote) || !textLength(q.Question, 5, 500) || !textLength(q.Why, 4, 500) || seen[normalized(q.Question)] {
				return bad()
			}
			seen[normalized(q.Question)] = true
			q.ID, q.Answer = fmt.Sprintf("q%d", i+1), ""
		}
	case "rewrite", "tailor":
		if len(out.Edits) > 8 || !validList(out.Notes, 0, 5) || (len(out.Edits) == 0 && len(out.Notes) == 0) {
			return bad()
		}
		numbers := map[string]bool{}
		for _, n := range numericClaim.FindAllString(in.Version.Text+"\n"+strings.Join(in.Version.Facts, "\n"), -1) {
			numbers[strings.ReplaceAll(n, " ", "")] = true
		}
		for i := range out.Edits {
			e := &out.Edits[i]
			if !textLength(e.Before, 4, 4000) || strings.Count(in.Version.Text, e.Before) != 1 || !textLength(e.After, 4, 5000) || e.After == e.Before || !textLength(e.Reason, 4, 600) || len(e.Evidence) < 1 || len(e.Evidence) > 6 {
				return bad()
			}
			start := strings.Index(in.Version.Text, e.Before)
			for _, prev := range out.Edits[:i] {
				other := strings.Index(in.Version.Text, prev.Before)
				if start < other+len(prev.Before) && other < start+len(e.Before) {
					return bad()
				}
			}
			for _, evidence := range e.Evidence {
				if !textLength(evidence.Quote, 4, 4000) {
					return bad()
				}
				found := evidence.Source == "resume" && containsQuote(in.Version.Text, evidence.Quote)
				if evidence.Source == "fact" {
					for _, fact := range in.Version.Facts {
						found = found || containsQuote(fact, evidence.Quote)
					}
				}
				if !found {
					return bad()
				}
			}
			for _, n := range numericClaim.FindAllString(e.After, -1) {
				if !numbers[strings.ReplaceAll(n, " ", "")] {
					return bad()
				}
			}
			e.ID, e.Status = fmt.Sprintf("e%d", i+1), "pending"
		}
	case "interview_start", "interview_answer", "interview_review":
		interview := in.Interview
		if interview == nil {
			return bad()
		}
		needQuestion := in.Kind == "interview_start" || (in.Kind == "interview_answer" && len(interview.Turns) < interview.Rounds)
		if needQuestion {
			q := out.Question
			if q == nil || !textLength(q.Question, 5, 700) || !textLength(q.Focus, 2, 150) || !textLength(q.ResumeQuote, 4, 1200) || !containsQuote(interview.Resume+"\n"+strings.Join(interview.Facts, "\n"), q.ResumeQuote) {
				return bad()
			}
			for _, turn := range interview.Turns {
				if normalized(turn.Question.Question) == normalized(q.Question) {
					return bad()
				}
			}
			q.ID = fmt.Sprintf("q%d", len(interview.Turns)+1)
		} else if out.Question != nil {
			return bad()
		}
		if in.Kind == "interview_answer" {
			if len(interview.Turns) == 0 {
				return bad()
			}
			answer := interview.Turns[len(interview.Turns)-1].Answer
			f := out.Feedback
			if f == nil || !textLength(f.AnswerQuote, 2, 1500) || !strings.Contains(normalized(answer), normalized(f.AnswerQuote)) || !textLength(f.Assessment, 5, 800) || !textLength(f.Improvement, 5, 800) || !validList(f.Outline, 2, 5) {
				return bad()
			}
		}
		needReview := in.Kind == "interview_review" || (in.Kind == "interview_answer" && len(interview.Turns) == interview.Rounds)
		if needReview {
			r := out.Review
			if r == nil || !textLength(r.Summary, 5, 1000) || !validList(r.Strengths, 0, 4) || !validList(r.Gaps, 1, 5) || !validList(r.PracticePlan, 1, 5) {
				return bad()
			}
		} else if out.Review != nil {
			return bad()
		}
	default:
		return bad()
	}
	return nil
}

func applyPreparationOutput(p *Preparation, in PreparationInput, out PreparationOutput, now int64) error {
	if in.Kind == "questions" || in.Kind == "rewrite" || in.Kind == "tailor" {
		v := p.version(in.Version.ID)
		if v == nil || v.Text != in.Version.Text {
			return ErrConflict
		}
		if in.Kind == "questions" {
			v.Questions, v.Edits, v.Notes = out.Questions, nil, nil
		} else {
			v.Edits, v.Notes = out.Edits, out.Notes
		}
		v.UpdatedAt = now
		return nil
	}
	if in.Interview == nil {
		return ErrConflict
	}
	i := p.interview(in.Interview.ID)
	if i == nil {
		return ErrConflict
	}
	if in.Kind == "interview_answer" {
		if len(i.Turns) == 0 || len(i.Turns) != len(in.Interview.Turns) {
			return ErrConflict
		}
		i.Turns[len(i.Turns)-1].Feedback = out.Feedback
	}
	if out.Question != nil {
		i.Turns = append(i.Turns, InterviewTurn{Question: *out.Question})
	}
	i.Status = "active"
	if out.Review != nil {
		i.Review, i.Status = out.Review, "done"
	}
	return nil
}
