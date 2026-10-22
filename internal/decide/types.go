// Package decide is the fast decision layer of the Marketing OS. It asks typed
// questions about a piece of state and gets typed answers with a confidence,
// never free text:
//
//   - Choice picks one option from a set;
//   - Score places the state on an ordered scale of labels;
//   - Noul is a yes/no probability.
//
// The primary adapter is TypeSafe's Jev ("System One" decision model). When it is
// not configured, fails, or the monthly spend ceiling is reached, a small LLM
// answers the same questions through the model router, with its confidence
// capped so low-trust answers take the caller's safe branch.
//
// Rules for callers: never ask for arithmetic, counts or date comparisons (Jev's
// documented weak spots); compute those in code and ask about the computed facts.
// Money and caps are always enforced in code, never by a decision.
package decide

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// Kind is a question primitive.
type Kind string

const (
	KindChoice Kind = "choice"
	KindScore  Kind = "score"
	KindNoul   Kind = "noul"
)

// Question is one typed question about the state.
type Question struct {
	Type         Kind   `json:"type"`
	Instructions string `json:"instructions"`
	// Options are the choice options mapped to a short description (Choice only).
	Options map[string]string `json:"-"`
	// Scale is the ordered list of labels, lowest first (Score only).
	Scale []string `json:"-"`
}

// Choice builds a Choice question.
func Choice(instructions string, options map[string]string) Question {
	return Question{Type: KindChoice, Instructions: instructions, Options: options}
}

// Score builds a Score question over an ordered scale, lowest first.
func Score(instructions string, scale ...string) Question {
	return Question{Type: KindScore, Instructions: instructions, Scale: scale}
}

// Noul builds a yes/no question.
func Noul(instructions string) Question {
	return Question{Type: KindNoul, Instructions: instructions}
}

// Validate checks a question is well formed.
func (q Question) Validate() error {
	if q.Instructions == "" {
		return fmt.Errorf("decide: question without instructions")
	}
	switch q.Type {
	case KindChoice:
		if len(q.Options) < 2 || len(q.Options) > 255 {
			return fmt.Errorf("decide: a choice needs 2 to 255 options, got %d", len(q.Options))
		}
	case KindScore:
		if len(q.Scale) < 2 {
			return fmt.Errorf("decide: a score needs at least 2 scale labels")
		}
	case KindNoul:
	default:
		return fmt.Errorf("decide: unknown question type %q", q.Type)
	}
	return nil
}

// Answer is the typed answer to one question.
type Answer struct {
	Type Kind `json:"type"`
	// Choice is the picked option (Choice).
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Score is the position on the scale, 0 for the first label (Score).
	Score float64 `json:"score,omitempty"`
	// Label is the scale label nearest to Score (Score).
	Label string `json:"label,omitempty"`
	// Prob is the probability of "yes" (Noul).
	Prob float64 `json:"noul,omitempty"`
	// Confidence is the calibrated confidence in the answer, 0 to 1. For a Noul
	// it is derived from how far Prob is from 0.5.
	Confidence float64 `json:"confidence"`
}

// Confident reports whether the answer clears threshold.
func (a Answer) Confident(threshold float64) bool { return a.Confidence >= threshold }

// Yes reports a "yes" at or above threshold. Fallback answers are shrunk toward
// 0.5, so a high threshold is only met by a confident primary answer.
func (a Answer) Yes(threshold float64) bool { return a.Prob >= threshold }

// Answers maps question ids to answers.
type Answers map[string]Answer

// Usage reports what a decision cost.
type Usage struct {
	Adapter     string  `json:"adapter"`
	InputTokens int     `json:"input_tokens"`
	CostUSD     float64 `json:"cost_usd"`
	Fallback    bool    `json:"fallback"`
}

// Adapter answers questions. Implementations must return an answer for every
// question or an error.
type Adapter interface {
	Name() string
	Decide(ctx context.Context, state string, questions map[string]Question) (Answers, Usage, error)
}

// noulConfidence derives a confidence from a yes/no probability.
func noulConfidence(p float64) float64 { return math.Min(1, math.Abs(p-0.5)*2) }

// nearestLabel maps a scale position to its nearest label.
func nearestLabel(scale []string, score float64) string {
	if len(scale) == 0 {
		return ""
	}
	i := int(math.Round(score))
	if i < 0 {
		i = 0
	}
	if i >= len(scale) {
		i = len(scale) - 1
	}
	return scale[i]
}

// sortedKeys returns map keys in stable order, so prompts and requests are
// byte-identical across calls (cache friendly and testable).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// complete checks every question got a valid answer.
func complete(questions map[string]Question, answers Answers) error {
	for id, q := range questions {
		a, ok := answers[id]
		if !ok {
			return fmt.Errorf("decide: no answer for %q", id)
		}
		switch q.Type {
		case KindChoice:
			if _, ok := q.Options[a.Choice]; !ok {
				return fmt.Errorf("decide: %q answered %q, not an option", id, a.Choice)
			}
		case KindScore:
			if a.Score < 0 || a.Score > float64(len(q.Scale)-1) {
				return fmt.Errorf("decide: %q score %v outside the scale", id, a.Score)
			}
		case KindNoul:
			if a.Prob < 0 || a.Prob > 1 {
				return fmt.Errorf("decide: %q probability %v outside 0..1", id, a.Prob)
			}
		}
	}
	return nil
}
