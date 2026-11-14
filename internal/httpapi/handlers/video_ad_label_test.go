package handlers

import (
	"encoding/json"
	"testing"
)

// Owner 2026-11-14: marketflow-api sends the owner's AI label choice on every ad call. Only an
// explicit false turns the on-screen note off; a call without it (an older API) keeps the note.
func TestAdRequestAILabel(t *testing.T) {
	cases := map[string]bool{
		`{}`:                 false,
		`{"ai_label":true}`:  false,
		`{"ai_label":false}`: true,
		`{"ai_label":null}`:  false,
	}
	for body, wantOff := range cases {
		var req adRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got := req.noAILabel(); got != wantOff {
			t.Errorf("%s: noAILabel = %v, want %v", body, got, wantOff)
		}
	}
}
