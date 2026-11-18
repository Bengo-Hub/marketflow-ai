package adintel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/llm"
)

type fakeCompleter struct {
	class llm.Class
	req   ai.ChatRequest
	reply string
}

func (f *fakeCompleter) Complete(_ context.Context, class llm.Class, req ai.ChatRequest) (*ai.ChatResponse, llm.Call, error) {
	f.class = class
	f.req = req
	resp := &ai.ChatResponse{}
	body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": f.reply}}}})
	if err := json.Unmarshal(body, resp); err != nil {
		return nil, llm.Call{}, err
	}
	return resp, llm.Call{}, nil
}

func TestWinnerShapeHasNoWordsOrAdvertiser(t *testing.T) {
	a := Ad{Advertiser: "Rival Bakers", Format: "VIDEO", DaysShown: 92, StillRunning: true,
		Tags: &Tags{Hook: "social_proof", Angle: "pain", Offer: "free_delivery", Relevance: "related"}}
	got := WinnerShape(a)
	want := "video ad, ran 92 days, still running; hook: social proof; angle: pain; offer: free delivery; relevance: related"
	if got != want {
		t.Fatalf("shape\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "Rival") {
		t.Fatal("the advertiser is never named")
	}
}

func TestSummarizeUsesTaggedShapesOnly(t *testing.T) {
	m := &fakeCompleter{reply: "Long-running ads open with a question about a pain and offer a discount."}
	ads := []Ad{
		{Advertiser: "Rival", Format: "video", DaysShown: 40, Tags: &Tags{Hook: "question", Angle: "pain", Offer: "discount"}},
		{Advertiser: "Other", Format: "image", DaysShown: 10},
	}
	got := Summarize(context.Background(), m, llm.Standard, ads)
	if got == "" || m.class != llm.Standard {
		t.Fatalf("the configured class writes the summary: %q class=%s", got, m.class)
	}
	user := m.req.Messages[len(m.req.Messages)-1].Content
	if strings.Contains(user, "Rival") || strings.Contains(user, "image ad") {
		t.Fatalf("only tagged winners, by shape, reach the model: %s", user)
	}
}

func TestSummarizeEmptyWithoutTagsOrModel(t *testing.T) {
	m := &fakeCompleter{reply: "x"}
	if Summarize(context.Background(), m, llm.Nano, []Ad{{Format: "video", DaysShown: 9}}) != "" {
		t.Fatal("no tagged winner, no summary")
	}
	if Summarize(context.Background(), nil, llm.Nano, []Ad{{Tags: &Tags{Hook: "question"}}}) != "" {
		t.Fatal("no model, no summary")
	}
}

func TestSummaryClassDefaultsToNano(t *testing.T) {
	if (Settings{}).summaryClass() != llm.Nano || (Settings{SummaryClass: "huge"}).summaryClass() != llm.Nano {
		t.Fatal("an unset or unknown class is nano")
	}
	if (Settings{SummaryClass: llm.Standard}).summaryClass() != llm.Standard {
		t.Fatal("a known class is used")
	}
}
