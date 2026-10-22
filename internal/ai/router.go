package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Intent represents the classified intent of a user question.
type Intent string

const (
	IntentSimpleFAQ           Intent = "simple_faq"
	IntentProductQA           Intent = "product_qa"
	IntentBusinessOpp         Intent = "business_opportunity"
	IntentRecruitment         Intent = "recruitment"
	IntentTaskExecute         Intent = "task_execute"
	IntentOffTopic            Intent = "off_topic"
	IntentEscalate            Intent = "escalate"
	IntentServiceInquiry      Intent = "service_inquiry"      // questions about Codevertex platform services
	IntentCourseInquiry       Intent = "course_inquiry"       // questions about Digitika courses
	IntentSupportRequest      Intent = "support_request"      // explicit support/help ticket request
	IntentDataQuery           Intent = "data_query"           // query live service data
	IntentIntegrationInterest Intent = "integration_interest" // wants to connect/integrate a platform integration (eTIMS, TruLoad, cameras, PLCs, Arduino/IoT)
)

// Router classifies user questions and routes them to the appropriate model.
// Uses a fast 8B model for classification, then routes to 70B for complex responses.
type Router struct {
	routerClient ChatClient // fast 8B model (Groq llama-3.1-8b-instant)
	routerModel  string
	logger       *zap.Logger
}

// NewRouter creates a new intent router.
func NewRouter(routerClient ChatClient, routerModel string, logger *zap.Logger) *Router {
	return &Router{
		routerClient: routerClient,
		routerModel:  routerModel,
		logger:       logger.Named("ai.router"),
	}
}

const classifierSystemPrompt = `You are an intent classifier for Vera, a business AI assistant deployed on customer-facing websites and dashboards. Classify the user's question.

Respond with ONLY a JSON object, nothing else:
{"intent": "<category>", "confidence": <0.0-1.0>, "needs_live_info": <true|false>}

"needs_live_info" is true when answering correctly requires fresh/real-world data (news, availability, current pricing, recent events, competitor facts, location-specific info); false when a general or internal-KB answer suffices.

IMPORTANT: Vera is a BUSINESS assistant, not a general-purpose AI. Questions about general programming (showing code samples, coding tutorials, homework, how to write functions in Python/JavaScript/etc.) are OFF-TOPIC unless the user is asking about a specific Codevertex service or course. General coding help requests ARE off_topic.

Categories:
- "simple_faq": Simple factual questions about the company, hours, location, contact info
- "product_qa": Questions about specific products, features, pricing, or recommendations (FLP or other tenant products)
- "business_opportunity": Questions about the FBO business model, compensation plan, income, ranks, or how to earn (FLP context)
- "recruitment": Interest in joining, signing up, getting started, startup costs (FLP context)
- "task_execute": User wants to CREATE business content: write ad copy, social media post, email campaign, funnel content, product description — NOT general code samples
- "service_inquiry": Questions about Codevertex platform services (MarketFlow, ordering, POS, logistics, ISP billing, ERP, auth, subscriptions)
- "course_inquiry": Questions about Digitika courses, Code-Starter bootcamp, enrollment, pricing, curriculum, schedule, certificates
- "support_request": User needs technical help, reports a bug, asks for support, wants to open a ticket
- "data_query": User asks for live data: "what are my orders", "show my invoices", "check inventory", "what is my subscription status"
- "integration_interest": User wants to CONNECT or INTEGRATE something with the Codevertex platform: KRA eTIMS/tax fiscalization, TruLoad weighing hardware/software, camera systems, PLCs, Arduino, or general IoT/hardware automation. This is a Codevertex integration topic, NOT general programming — e.g. "how do I integrate eTIMS", "can you connect to our weighbridge", "we want to hook up our PLC/Arduino/cameras to your system" are integration_interest, not off_topic
- "off_topic": Questions unrelated to business products, services, or the company — includes general programming tutorials, code samples unrelated to a Codevertex service, weather, sports, politics, homework, cooking, general knowledge
- "escalate": User explicitly asks to speak to a human, requests a call, or expresses serious frustration`

// Classification is the full router output.
type Classification struct {
	Intent        Intent
	Confidence    float64
	NeedsLiveInfo bool
}

// ClassifyIntent uses the fast router model to classify a user question.
// Kept for back-compat (returns the subset most callers need).
func (r *Router) ClassifyIntent(ctx context.Context, question string) (Intent, float64, error) {
	c, err := r.Classify(ctx, question)
	return c.Intent, c.Confidence, err
}

// HeuristicClassify attempts to classify the question using cheap keyword
// rules BEFORE burning an LLM call. Returns (cls, true) when confident;
// (zero, false) when the heuristic doesn't match and the caller should
// proceed to the full LLM classifier. Saves ~200ms on 30-40% of traffic.
func HeuristicClassify(question string) (Classification, bool) {
	q := strings.ToLower(strings.TrimSpace(question))

	// Off-topic blocklist — common chatbot abuse / irrelevant queries.
	// These patterns are universally off-topic for any business assistant
	// regardless of niche (FLP, Codevertex, generic SME, etc.).
	offTopicPatterns := []string{
		"weather", "football", "soccer", "basketball", "politics",
		"president", "election", "cryptocurrency", "bitcoin",
		"joke", "tell me a joke", "what day is it", "what time",
		// General programming/code requests — Vera is a business assistant,
		// not a coding tutor. These are off-scope for all niches.
		"python code", "python script", "javascript code", "java code",
		"c++ code", "c# code", "php code", "ruby code", "go code",
		"rust code", "swift code", "kotlin code", "typescript code",
		"show me code", "show me a function", "show me a script",
		"write a program", "write me a program", "sample code",
		"code snippet", "can i see code", "give me code",
		"how to code ", "programming tutorial", "coding tutorial",
		"sorting algorithm", "fibonacci sequence", "binary search algorithm",
		"recursion example", "import os", "import sys",
		"console.log(", "print(f\"", "def main(", "public static void",
	}
	for _, p := range offTopicPatterns {
		if strings.Contains(q, p) {
			return Classification{Intent: IntentOffTopic, Confidence: 0.97}, true
		}
	}

	// Escalation signals.
	escalatePatterns := []string{
		"speak to a human", "talk to a person", "call me",
		"connect me to", "real person", "agent please",
		"i want to complain", "this is frustrating",
	}
	for _, p := range escalatePatterns {
		if strings.Contains(q, p) {
			return Classification{Intent: IntentEscalate, Confidence: 0.95}, true
		}
	}

	// Task-execute signals (content creation verbs).
	taskPrefixes := []string{
		"write ", "draft ", "create ", "generate ", "compose ",
		"make a post", "design a", "come up with",
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(q, p) || strings.Contains(q, p) {
			return Classification{Intent: IntentTaskExecute, Confidence: 0.85}, true
		}
	}

	// Support request signals.
	supportPatterns := []string{
		"help me with", "not working", "broken", "bug", "issue with",
		"open a ticket", "report a problem", "something is wrong",
		"create a ticket", "raise a ticket", "submit a ticket",
	}
	for _, p := range supportPatterns {
		if strings.Contains(q, p) {
			return Classification{Intent: IntentSupportRequest, Confidence: 0.9}, true
		}
	}

	// Course inquiry signals.
	coursePatterns := []string{
		"digitika", "code starter", "bootcamp", "course", "enroll",
		"curriculum", "certificate", "training", "class", "learn to code",
	}
	for _, p := range coursePatterns {
		if strings.Contains(q, p) {
			return Classification{Intent: IntentCourseInquiry, Confidence: 0.9}, true
		}
	}

	// Integration-interest signals — checked before the generic service-inquiry
	// patterns below since "integrate treasury" etc. should route here, not there.
	integrationPatterns := []string{
		"etims", "kra pin", "kra integration", "tax fiscalization", "fiscal device",
		"truload", "weighbridge", "weighing hardware", "weighing software",
		"integrate with", "integration with", "connect our", "connect my",
		"hook up our", "camera integration", "cctv integration", "plc",
		"arduino", "iot automation", "iot integration", "hardware automation",
		"developer portal", "api access", "api integration",
	}
	for _, p := range integrationPatterns {
		if strings.Contains(q, p) {
			return Classification{Intent: IntentIntegrationInterest, Confidence: 0.85}, true
		}
	}

	// Service inquiry signals.
	servicePatterns := []string{
		"marketflow", "ordering", "pos ", "point of sale", "logistics",
		"isp billing", "erp", "inventory", "subscriptions", "notifications",
		"auth", "sso", "truload", "finance service", "treasury",
	}
	for _, p := range servicePatterns {
		if strings.Contains(q, p) {
			return Classification{Intent: IntentServiceInquiry, Confidence: 0.85}, true
		}
	}

	// Simple greetings → simple_faq (avoid agent loop on "hi").
	greetings := []string{
		"hi", "hello", "hey", "good morning", "good afternoon",
		"good evening", "thanks", "thank you", "bye", "goodbye",
	}
	for _, g := range greetings {
		if q == g || q == g+"!" || q == g+"." {
			return Classification{Intent: IntentSimpleFAQ, Confidence: 0.9}, true
		}
	}

	return Classification{}, false
}

// Classify returns the full classification including the needs_live_info signal.
func (r *Router) Classify(ctx context.Context, question string) (Classification, error) {
	resp, err := r.routerClient.Chat(ctx, ChatRequest{
		Model: r.routerModel,
		Messages: []Message{
			{Role: "system", Content: classifierSystemPrompt},
			{Role: "user", Content: question},
		},
		Temperature: 0.1,
		MaxTokens:   80,
	})
	if err != nil {
		return Classification{Intent: IntentProductQA, Confidence: 0.5}, fmt.Errorf("classify: %w", err)
	}

	if len(resp.Choices) == 0 {
		return Classification{Intent: IntentProductQA, Confidence: 0.5}, nil
	}

	content := strings.TrimSpace(resp.Choices[0].Message.Content)

	var result struct {
		Intent        string  `json:"intent"`
		Confidence    float64 `json:"confidence"`
		NeedsLiveInfo bool    `json:"needs_live_info"`
	}
	if idx := strings.Index(content, "{"); idx >= 0 {
		if end := strings.LastIndex(content, "}"); end > idx {
			content = content[idx : end+1]
		}
	}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		r.logger.Warn("failed to parse classification", zap.String("raw", content), zap.Error(err))
		return Classification{Intent: IntentProductQA, Confidence: 0.5}, nil
	}

	intent := Intent(result.Intent)
	switch intent {
	case IntentSimpleFAQ, IntentProductQA, IntentBusinessOpp, IntentRecruitment, IntentTaskExecute,
		IntentOffTopic, IntentEscalate, IntentServiceInquiry, IntentCourseInquiry, IntentSupportRequest, IntentDataQuery,
		IntentIntegrationInterest:
		r.logger.Debug("classified intent",
			zap.String("intent", string(intent)),
			zap.Float64("confidence", result.Confidence),
			zap.Bool("needs_live_info", result.NeedsLiveInfo),
		)
		return Classification{Intent: intent, Confidence: result.Confidence, NeedsLiveInfo: result.NeedsLiveInfo}, nil
	default:
		return Classification{Intent: IntentProductQA, Confidence: 0.5}, nil
	}
}
