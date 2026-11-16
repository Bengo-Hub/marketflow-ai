package agent

import "github.com/bengobox/marketflow-ai/internal/guardrail"

// Taint (Sprint 26, owner decision G4, 2026-11-16): once a turn holds third-party text (a web page,
// a lead's message, a CRM note, trigger data), the model may still read and answer, but it can no
// longer call a tool that writes or reaches someone outside the chat. An instruction hidden in
// that text therefore cannot cause a side effect. The person asks again in a fresh message to act.

// writingTools change records or contact people outside the conversation.
var writingTools = map[string]bool{
	"create_campaign":     true,
	"create_funnel":       true,
	"create_deal":         true,
	"move_pipeline_stage": true,
	"create_task":         true,
	"create_activity":     true,
	"create_ticket":       true,
	"request_integration": true,
	"book_meeting":        true,
}

// taintingTools return text written by people outside the business: search results, CRM notes
// and profiles, enrichment from the web.
var taintingTools = map[string]bool{
	"web_search":         true,
	"search_images":      true,
	"get_contact_360":    true,
	"enrich_contact":     true,
	"analyze_deal":       true,
	"meeting_prep":       true,
	"draft_email":        true,
	"query_service_data": true,
}

// WritingTool reports whether a tool writes or reaches someone outside the chat.
func WritingTool(name string) bool { return writingTools[name] }

// taintedToolResult is what the model gets when it calls a writing tool in a tainted turn.
const taintedToolResult = `{"error":"not available in this turn: the conversation now holds outside text (search results, customer messages or records), so actions wait for a fresh request. Tell the person what you found and ask them to confirm the action in a new message."}`

// taints reports whether a tool's output makes the turn tainted: a tainting tool, or any output
// that carries an instruction aimed at the assistant.
func taints(name, out string) bool {
	return taintingTools[name] || guardrail.LooksInjected(out)
}
