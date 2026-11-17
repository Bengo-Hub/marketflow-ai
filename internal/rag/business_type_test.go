package rag

import (
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

func TestPromptPersona(t *testing.T) {
	cases := map[string]string{
		"flp":         "flp",
		"FLP":         "flp",
		"cafe":        "cafe",
		"generic":     "generic",
		"restaurant":  "generic",
		"pharmacy":    "generic",
		"hospitality": "generic",
		"codevertex":  "codevertex",
		"":            "codevertex",
		"unknown":     "codevertex",
	}
	for in, want := range cases {
		if got := promptPersona(in); got != want {
			t.Errorf("promptPersona(%q) = %q, want %q", in, got, want)
		}
	}
}

// Sprint 7 #34: every chat prompt carries the business type's rules ahead of the knowledge base
// context; the FLP prompt keeps its own compliance rules unchanged.
func TestChatPromptsCarryTypeRules(t *testing.T) {
	p := &Pipeline{}
	chunks := []SearchResult{{Category: "faq", Title: "Hours", ChunkText: "Open 8am to 8pm"}}

	sys := p.buildSystemPrompt("pharmacy", ai.IntentProductQA, chunks)
	rules := strings.Index(sys, "BUSINESS RULES (Pharmacy or chemist)")
	kb := strings.Index(sys, "--- KNOWLEDGE BASE CONTEXT ---")
	if rules < 0 || kb < 0 || rules > kb {
		t.Fatalf("pharmacy prompt must carry its rules before the context:\n%s", sys)
	}
	if strings.Count(sys, "--- KNOWLEDGE BASE CONTEXT ---") != 1 {
		t.Fatal("the context must appear once")
	}

	if s := p.buildSystemPrompt("cafe", ai.IntentProductQA, nil); !strings.Contains(s, "BUSINESS RULES (Restaurant or cafe)") || !strings.Contains(s, "this cafe") {
		t.Fatalf("cafe keeps its persona and gets the restaurant rules:\n%s", s)
	}
	if s := p.buildSystemPrompt("flp", ai.IntentProductQA, nil); strings.Contains(s, "BUSINESS RULES") || !strings.Contains(s, "COMPLIANCE RULES") {
		t.Fatal("the FLP prompt keeps its own compliance rules unchanged")
	}
	if s := p.buildAgentSystemPrompt("salon", ai.IntentProductQA); !strings.Contains(s, "BUSINESS RULES (Salon, barber or beauty)") {
		t.Fatalf("agent prompt misses the salon rules:\n%s", s)
	}
	if s := p.buildAgentSystemPrompt("", ai.IntentProductQA); !strings.Contains(s, "BUSINESS RULES (Other business)") {
		t.Fatal("an empty type gets the generic rules")
	}
}

// The owner's marketing laws (2026-11-17) reach every chat and agent persona exactly once, FLP
// included, ahead of the knowledge base context; the Codevertex persona sells with them and still
// never invents a price.
func TestPromptsCarryMarketingLaws(t *testing.T) {
	p := &Pipeline{}
	chunks := []SearchResult{{Category: "faq", Title: "Hours", ChunkText: "Open 8am to 8pm"}}
	for _, bt := range []string{"flp", "codevertex", "", "generic", "cafe", "restaurant", "pharmacy"} {
		for _, intent := range []ai.Intent{ai.IntentProductQA, ai.IntentTaskExecute, ai.IntentBusinessOpp} {
			sys := p.buildSystemPrompt(bt, intent, chunks)
			if strings.Count(sys, "MARKETING LAWS.") != 1 {
				t.Errorf("chat %q/%s: the laws must appear exactly once", bt, intent)
			}
			if laws, kb := strings.Index(sys, "MARKETING LAWS."), strings.Index(sys, "--- KNOWLEDGE BASE CONTEXT ---"); laws > kb {
				t.Errorf("chat %q/%s: the laws must come before the context", bt, intent)
			}
			if agent := p.buildAgentSystemPrompt(bt, intent); strings.Count(agent, "MARKETING LAWS.") != 1 {
				t.Errorf("agent %q/%s: the laws must appear exactly once", bt, intent)
			}
		}
	}
	for _, s := range []string{p.buildSystemPrompt("codevertex", ai.IntentProductQA, nil), p.buildAgentSystemPrompt("codevertex", ai.IntentProductQA)} {
		if !strings.Contains(s, "sell with the MARKETING LAWS") || !strings.Contains(s, "never invent a price") {
			t.Fatalf("the Codevertex persona sells with the laws and never invents a price:\n%s", s)
		}
	}
}

// A tenant type that is not FLP or cafe gets the generic off-topic blocks.
func TestScopeGuardForBusinessTypes(t *testing.T) {
	if !scopeGuard("pharmacy", "can you write my essay") {
		t.Fatal("pharmacy should block homework requests")
	}
	if scopeGuard("pharmacy", "do you have paracetamol") {
		t.Fatal("pharmacy should answer about its products")
	}
}
