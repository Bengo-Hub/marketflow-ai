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
	IntentSimpleFAQ      Intent = "simple_faq"
	IntentProductQA      Intent = "product_qa"
	IntentBusinessOpp    Intent = "business_opportunity"
	IntentRecruitment    Intent = "recruitment"
	IntentTaskExecute    Intent = "task_execute"
	IntentOffTopic       Intent = "off_topic"
	IntentEscalate       Intent = "escalate"
	IntentServiceInquiry Intent = "service_inquiry"  // questions about Codevertex platform services
	IntentCourseInquiry  Intent = "course_inquiry"   // questions about Digitika courses
	IntentSupportRequest Intent = "support_request"  // explicit support/help ticket request
	IntentDataQuery      Intent = "data_query"       // query live service data
)

// Router classifies user questions and routes them to the appropriate model.
// Uses a fast 8B model for classification, then routes to 70B for complex responses.
type Router struct {
	routerClient *Client // fast 8B model (Groq llama-3.1-8b-instant)
	routerModel  string
	logger       *zap.Logger
}

// NewRouter creates a new intent router.
func NewRouter(routerClient *Client, routerModel string, logger *zap.Logger) *Router {
	return &Router{
		routerClient: routerClient,
		routerModel:  routerModel,
		logger:       logger.Named("ai.router"),
	}
}

const classifierSystemPrompt = `You are an intent classifier for Vera, the Codevertex AI assistant. Codevertex provides enterprise software, AI tools, and tech education (Digitika). Classify the user's question.

Respond with ONLY a JSON object, nothing else:
{"intent": "<category>", "confidence": <0.0-1.0>, "needs_live_info": <true|false>}

"needs_live_info" is true when answering correctly requires fresh/real-world data (news, availability, current pricing, recent events, competitor facts, location-specific info); false when a general or internal-KB answer suffices.

Categories:
- "simple_faq": Simple factual questions about the company, hours, location, contact info
- "product_qa": Questions about specific products, features, pricing, or recommendations (FLP or other tenant products)
- "business_opportunity": Questions about the FBO business model, compensation plan, income, ranks, or how to earn (FLP context)
- "recruitment": Interest in joining, signing up, getting started, startup costs (FLP context)
- "task_execute": User wants to CREATE something: write ad copy, social media post, email campaign, funnel content, product description
- "service_inquiry": Questions about Codevertex platform services (MarketFlow, ordering, POS, logistics, ISP billing, ERP, auth, subscriptions)
- "course_inquiry": Questions about Digitika courses, Code-Starter bootcamp, enrollment, pricing, curriculum, schedule, certificates
- "support_request": User needs technical help, reports a bug, asks for support, wants to open a ticket
- "data_query": User asks for live data: "what are my orders", "show my invoices", "check inventory", "what is my subscription status"
- "off_topic": Questions completely unrelated to business, products, services, or education (weather, sports, politics, etc.)
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
	offTopicPatterns := []string{
		"weather", "football", "soccer", "basketball", "politics",
		"president", "election", "cryptocurrency", "bitcoin",
		"joke", "tell me a joke", "what day is it", "what time",
	}
	for _, p := range offTopicPatterns {
		if strings.Contains(q, p) {
			return Classification{Intent: IntentOffTopic, Confidence: 0.95}, true
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
		IntentOffTopic, IntentEscalate, IntentServiceInquiry, IntentCourseInquiry, IntentSupportRequest, IntentDataQuery:
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
