package adintel

import (
	"context"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/guardrail"
	"github.com/bengobox/marketflow-ai/internal/llm"
)

// Completer completes one model call; *llm.Router satisfies it.
type Completer interface {
	Complete(ctx context.Context, class llm.Class, req ai.ChatRequest) (*ai.ChatResponse, llm.Call, error)
}

// maxSummaryRunes caps the summary the Ads Specialist reads.
const maxSummaryRunes = 400

const summarySystem = "You brief an ads copywriter on what the competitor ads that ran longest in this market have in common. " +
	"You get one line per ad: its format, how long it ran and fixed labels for its hook, angle and offer. " +
	"Write two or three short plain sentences on the shared pattern and the gap the copywriter could take. " +
	"Use only the labels given: no business names, no quotes, no numbers other than those given, and never suggest copying an ad."

// WinnerShape describes a winner by shape and tags only, never its words or advertiser, for
// example "video ad, ran 92 days, still running; hook: question; angle: pain; offer: discount".
func WinnerShape(a Ad) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s ad", strings.ToLower(firstNonEmpty(a.Format, "unknown")))
	if a.DaysShown > 0 {
		fmt.Fprintf(&b, ", ran %d days", a.DaysShown)
	}
	if a.StillRunning {
		b.WriteString(", still running")
	}
	if t := a.Tags; t != nil {
		for _, p := range []struct{ label, v string }{{"hook", t.Hook}, {"angle", t.Angle}, {"offer", t.Offer}, {"relevance", t.Relevance}} {
			if p.v != "" {
				fmt.Fprintf(&b, "; %s: %s", p.label, strings.ReplaceAll(p.v, "_", " "))
			}
		}
	}
	return b.String()
}

// Summarize writes the short brief the Ads Specialist reads from the tagged winners, on the model
// class the provider row sets (nano by default). The input is tags and shapes only, so no
// competitor text reaches this call. Empty when nothing is tagged or no model answers.
func Summarize(ctx context.Context, m Completer, class llm.Class, ads []Ad) string {
	if m == nil {
		return ""
	}
	var lines []string
	for _, a := range ads {
		if a.Tags != nil {
			lines = append(lines, "- "+WinnerShape(a))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	resp, _, err := m.Complete(ctx, class, ai.ChatRequest{
		Messages: []ai.Message{
			{Role: "system", Content: summarySystem},
			{Role: "user", Content: "Winning ads:\n" + strings.Join(lines, "\n")},
		},
		Temperature: 0.2,
		MaxTokens:   200,
	})
	if err != nil || resp == nil || len(resp.Choices) == 0 {
		return ""
	}
	out := clip(resp.Choices[0].Message.Content, maxSummaryRunes)
	if guardrail.ScreenOutput(out) != out {
		return ""
	}
	return out
}
