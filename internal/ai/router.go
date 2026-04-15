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
	IntentSimpleFAQ    Intent = "simple_faq"
	IntentProductQA    Intent = "product_qa"
	IntentBusinessOpp  Intent = "business_opportunity"
	IntentRecruitment  Intent = "recruitment"
	IntentTaskExecute  Intent = "task_execute"
	IntentOffTopic     Intent = "off_topic"
	IntentEscalate     Intent = "escalate"
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

const classifierSystemPrompt = `You are an intent classifier for a Forever Living Products marketing chatbot. Classify the user's question.

Respond with ONLY a JSON object, nothing else:
{"intent": "<category>", "confidence": <0.0-1.0>, "needs_live_info": <true|false>}

"needs_live_info" is true when answering correctly requires fresh/real-world data (news, availability, current pricing, recent events, competitor facts, location-specific info); false when a general or internal-KB answer suffices.

Categories:
- "simple_faq": Simple factual questions about the company (what is FLP, how old, etc.)
- "product_qa": Questions about specific products, ingredients, benefits, prices, or product recommendations
- "business_opportunity": Questions about the FBO business model, compensation plan, income, ranks, or how to earn
- "recruitment": Interest in joining, signing up, becoming an FBO, getting started, or questions about startup costs
- "task_execute": User wants to CREATE something: write ad copy, generate social media post, draft email campaign, create funnel content, generate product description
- "off_topic": Questions unrelated to FLP products or business (weather, sports, politics, etc.)
- "escalate": User explicitly asks to speak to a human, requests a call, or expresses frustration`

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
	case IntentSimpleFAQ, IntentProductQA, IntentBusinessOpp, IntentRecruitment, IntentTaskExecute, IntentOffTopic, IntentEscalate:
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
