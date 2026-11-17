package rag

import (
	"testing"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// A signed-in person sees the marketing-law notes on an answer; a public visitor never does, and an
// answer whose urgency has a fact behind it gets no note.
func TestReviewNotesOnlyForSignedIn(t *testing.T) {
	sys := []ai.Message{{Role: "system", Content: "Business facts: Aloe Gel KES 3,500."}}
	pushy := "Last chance, only 2 left!"

	if got := reviewNotes(ChatInput{PublicMode: false}, sys, pushy); len(got) == 0 {
		t.Fatalf("signed-in answer with unproven scarcity got no notes")
	}
	if got := reviewNotes(ChatInput{PublicMode: true}, sys, pushy); got != nil {
		t.Fatalf("public visitor got review notes: %v", got)
	}
	if got := reviewNotes(ChatInput{}, sys, "Fresh mandazi every morning in Kisumu."); len(got) != 0 {
		t.Fatalf("plain answer got notes: %v", got)
	}
}
