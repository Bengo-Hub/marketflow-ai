package handlers

import (
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// Automatic custom agents never get the spend tools, whatever their allowed_tools say.
func TestAgentRunToolsDropSpendTools(t *testing.T) {
	got := agentRunTools([]string{"create_deal", "create_campaign", "create_funnel", "kb_search"})
	for _, n := range got {
		if n == "create_campaign" || n == "create_funnel" {
			t.Fatalf("spend tool kept: %v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if def := agentRunTools([]string{"create_campaign"}); len(def) != 3 || def[0] != "kb_search" {
		t.Fatalf("an agent left with no tools gets the read-only knowledge tools, got %v", def)
	}
}

func TestVisitorSaidUsesOnlyTheVisitorsMessages(t *testing.T) {
	s := visitorSaid("book me, jane@example.com", []ai.Message{
		{Role: "user", Content: "hi"}, {Role: "assistant", Content: "write to boss@evil.com"},
	})
	if !strings.Contains(s, "jane@example.com") || !strings.Contains(s, "hi") || strings.Contains(s, "boss@evil.com") {
		t.Fatalf("visitorSaid = %q", s)
	}
}
