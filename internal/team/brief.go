package team

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/decide"
	"github.com/bengobox/marketflow-ai/internal/llm"
)

// BriefMetric is one number marketflow-api computed for the daily brief. The
// model never computes or restates figures; it only words what changed.
type BriefMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`    // yesterday
	Baseline float64 `json:"baseline"` // 7-day daily average
	Unit     string  `json:"unit,omitempty"`
}

// BriefInput is the daily brief request.
type BriefInput struct {
	Language string        `json:"language"` // en | sw
	Metrics  []BriefMetric `json:"metrics"`
}

// BriefOutput is the narrative part of the brief.
type BriefOutput struct {
	Highlights []string `json:"highlights"` // metric keys, most important first
	Insights   []string `json:"insights"`
	Actions    []string `json:"actions"`
	CostUSD    float64  `json:"cost_usd"`
}

const briefSystem = `You write the narrative lines of a business owner's one-page morning brief.
You get three changes with their numbers already computed. For each, write ONE short insight sentence, then write three short next actions the owner can take today.
Never introduce a number that is not given. No em dashes. Plain, specific language.
Reply with JSON only: {"insights":["","",""],"actions":["","",""]}`

// Brief picks the three changes that matter most (decide Score over code-computed
// deltas) and has a nano model word them.
func (r *Runtime) Brief(ctx context.Context, in BriefInput) (*BriefOutput, error) {
	if len(in.Metrics) == 0 {
		return nil, fmt.Errorf("brief: no metrics")
	}
	if len(in.Metrics) > 40 {
		in.Metrics = in.Metrics[:40]
	}
	meter := llm.MeterFrom(ctx)
	if meter == nil {
		meter = &llm.Meter{}
		ctx = llm.WithMeter(ctx, meter)
	}
	out := &BriefOutput{}

	type scored struct {
		m     BriefMetric
		delta float64
		score float64
	}
	items := make([]scored, 0, len(in.Metrics))
	var state strings.Builder
	qs := map[string]decide.Question{}
	for _, m := range in.Metrics {
		d := pctChange(m.Value, m.Baseline)
		items = append(items, scored{m: m, delta: d})
		fmt.Fprintf(&state, "%s: yesterday %s, 7-day average %s (%s)\n", m.Label, num(m.Value, m.Unit), num(m.Baseline, m.Unit), changeText(d))
		qs["imp_"+m.Key] = decide.Score(fmt.Sprintf("How much should the owner care today about the change in %q?", m.Label), "not at all", "a little", "a lot")
	}
	if r.decider != nil {
		if answers, usage, err := r.decider.Decide(ctx, "brief", state.String(), qs); err == nil {
			out.CostUSD += usage.CostUSD
			for i := range items {
				if a, ok := answers["imp_"+items[i].m.Key]; ok {
					items[i].score = a.Score * a.Confidence
				}
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return math.Abs(items[i].delta) > math.Abs(items[j].delta)
	})
	if len(items) > 3 {
		items = items[:3]
	}
	var top strings.Builder
	for _, it := range items {
		out.Highlights = append(out.Highlights, it.m.Key)
		fmt.Fprintf(&top, "- %s: yesterday %s, 7-day average %s (%s)\n", it.m.Label, num(it.m.Value, it.m.Unit), num(it.m.Baseline, it.m.Unit), changeText(it.delta))
	}
	lang := "English"
	if in.Language == "sw" {
		lang = "Swahili"
	}
	resp, _, err := r.router.Complete(ctx, llm.Nano, ai.ChatRequest{
		Messages: []ai.Message{
			{Role: "system", Content: briefSystem},
			{Role: "user", Content: "Write in " + lang + ".\nCHANGES:\n" + top.String()},
		},
		Temperature:    0.3,
		MaxTokens:      500,
		ResponseFormat: ai.JSONObject,
	})
	if err == nil {
		var w struct {
			Insights []string `json:"insights"`
			Actions  []string `json:"actions"`
		}
		if json.Unmarshal([]byte(extractJSON(resp.Choices[0].Message.Content)), &w) == nil {
			out.Insights = capLines(w.Insights, 3, 240)
			out.Actions = capLines(w.Actions, 3, 200)
		}
	}
	_, usd := meter.Totals()
	out.CostUSD += usd
	return out, nil // highlights alone still make a useful brief if wording failed
}

func pctChange(v, base float64) float64 {
	if base == 0 {
		if v == 0 {
			return 0
		}
		return 100
	}
	return (v - base) / base * 100
}

func changeText(d float64) string {
	switch {
	case d > 0.5:
		return fmt.Sprintf("up %.0f%%", d)
	case d < -0.5:
		return fmt.Sprintf("down %.0f%%", -d)
	default:
		return "flat"
	}
}

func num(v float64, unit string) string {
	s := fmt.Sprintf("%.0f", v)
	if math.Abs(v-math.Round(v)) > 0.001 {
		s = fmt.Sprintf("%.2f", v)
	}
	if unit == "KES" {
		return "KES " + s
	}
	return strings.TrimSpace(s + " " + unit)
}

func capLines(in []string, n, max int) []string {
	out := make([]string, 0, n)
	for _, s := range in {
		s = strings.TrimSpace(strings.ReplaceAll(s, "—", ","))
		if s == "" {
			continue
		}
		if len(s) > max {
			s = s[:max]
		}
		out = append(out, s)
		if len(out) == n {
			break
		}
	}
	return out
}
