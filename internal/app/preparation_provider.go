package app

import (
	"context"
	_ "embed"
)

//go:embed preparation-prompt.txt
var preparationPrompt string

func (p *ClaudeProvider) Prepare(ctx context.Context, input PreparationInput) (PreparationOutput, Usage, error) {
	var output PreparationOutput
	providerInput := input
	providerInput.Version.RefinementID, providerInput.Version.RefinementComplete = "", false
	_, usage, err := p.callTool(ctx, providerInput, preparationPrompt, "deliver_preparation", preparationSchema(input), 8000, &output)
	if err == nil {
		err = validatePreparationOutput(&output, input)
	}
	return output, usage, err
}

func preparationSchema(input PreparationInput) map[string]any {
	text := func(min, max int) map[string]any {
		return map[string]any{"type": "string", "minLength": min, "maxLength": max}
	}
	object := func(fields map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": fields, "required": required, "additionalProperties": false}
	}
	array := func(item any, min, max int) map[string]any {
		return map[string]any{"type": "array", "items": item, "minItems": min, "maxItems": max}
	}
	question := object(map[string]any{"question": text(5, 700), "focus": text(2, 150), "resume_quote": text(4, 1200)}, "question", "focus", "resume_quote")
	feedback := object(map[string]any{"answer_quote": text(2, 1500), "assessment": text(5, 800), "improvement": text(5, 800), "outline": array(text(2, 800), 2, 5)}, "answer_quote", "assessment", "improvement", "outline")
	review := object(map[string]any{"summary": text(5, 1000), "strengths": array(text(2, 800), 0, 4), "gaps": array(text(2, 800), 1, 5), "practice_plan": array(text(2, 800), 1, 5)}, "summary", "strengths", "gaps", "practice_plan")
	switch input.Kind {
	case "questions":
		item := object(map[string]any{"quote": text(4, 1200), "question": text(5, 500), "why": text(4, 500)}, "quote", "question", "why")
		return object(map[string]any{"questions": array(item, 2, 5)}, "questions")
	case "rewrite", "tailor":
		evidence := object(map[string]any{"source": map[string]any{"type": "string", "enum": []string{"resume", "fact"}}, "quote": text(4, 4000)}, "source", "quote")
		edit := object(map[string]any{"before": text(4, 4000), "after": text(4, 5000), "reason": text(4, 600), "evidence": array(evidence, 1, 6)}, "before", "after", "reason", "evidence")
		return object(map[string]any{"edits": array(edit, 0, 8), "notes": array(text(2, 800), 0, 5)}, "edits", "notes")
	case "interview_start":
		return object(map[string]any{"question": question}, "question")
	case "interview_answer":
		if input.Interview != nil && len(input.Interview.Turns) < input.Interview.Rounds {
			return object(map[string]any{"feedback": feedback, "question": question}, "feedback", "question")
		}
		return object(map[string]any{"feedback": feedback, "review": review}, "feedback", "review")
	default:
		return object(map[string]any{"review": review}, "review")
	}
}
