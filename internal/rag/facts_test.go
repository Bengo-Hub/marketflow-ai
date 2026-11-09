package rag

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWithFactsAlwaysAddsTheRule(t *testing.T) {
	p := &Pipeline{}
	got := p.withFacts(context.Background(), uuid.New(), "PERSONA")
	if !strings.HasPrefix(got, "PERSONA") || !strings.Contains(got, "Never guess") || strings.Contains(got, "BUSINESS FACTS (true") {
		t.Fatalf("without facts: %q", got)
	}

	p.facts = func(context.Context, uuid.UUID) string { return "- Address: Busia\n" }
	got = p.withFacts(context.Background(), uuid.New(), "PERSONA")
	if !strings.Contains(got, "Never guess") || !strings.Contains(got, "- Address: Busia") {
		t.Fatalf("with facts: %q", got)
	}
	// No tenant, no facts lookup.
	if got := p.withFacts(context.Background(), uuid.Nil, "P"); strings.Contains(got, "Busia") {
		t.Fatal("facts were read without a tenant")
	}
}
