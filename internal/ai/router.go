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

const classifierSystemPrompt = `You are an intent classifier for a Forever Living Products marketing chatbot. Classify the user's question into exactly one category.

Respond with ONLY a JSON object, nothing else:
{"intent": "<category>", "confidence": <0.0-1.0>}

Categories:
- "simple_faq": Simple factual questions about the company (what is FLP, how old, etc.)
- "product_qa": Questions about specific products, ingredients, benefits, prices, or product recommendations
- "business_opportunity": Questions about the FBO business model, compensation plan, income, ranks, or how to earn
- "recruitment": Interest in joining, signing up, becoming an FBO, getting started, or questions about startup costs
- "off_topic": Questions unrelated to FLP products or business (weather, sports, politics, etc.)
- "escalate": User explicitly asks to speak to a human, requests a call, or expresses frustration`

// ClassifyIntent uses the fast router model to classify a user question.
func (r *Router) ClassifyIntent(ctx context.Context, question string) (Intent, float64, error) {
	resp, err := r.routerClient.Chat(ctx, ChatRequest{
		Model: r.routerModel,
		Messages: []Message{
			{Role: "system", Content: classifierSystemPrompt},
			{Role: "user", Content: question},
		},
		Temperature: 0.1,
		MaxTokens:   50,
	})
	if err != nil {
		return IntentProductQA, 0.5, fmt.Errorf("classify: %w", err)
	}

	if len(resp.Choices) == 0 {
		return IntentProductQA, 0.5, nil
	}

	content := strings.TrimSpace(resp.Choices[0].Message.Content)

	// Parse JSON response
	var result struct {
		Intent     string  `json:"intent"`
		Confidence float64 `json:"confidence"`
	}
	// Try to extract JSON from response (model might wrap it in markdown)
	if idx := strings.Index(content, "{"); idx >= 0 {
		if end := strings.LastIndex(content, "}"); end > idx {
			content = content[idx : end+1]
		}
	}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		r.logger.Warn("failed to parse classification", zap.String("raw", content), zap.Error(err))
		return IntentProductQA, 0.5, nil // default to product_qa on parse failure
	}

	intent := Intent(result.Intent)
	switch intent {
	case IntentSimpleFAQ, IntentProductQA, IntentBusinessOpp, IntentRecruitment, IntentOffTopic, IntentEscalate:
		r.logger.Debug("classified intent",
			zap.String("intent", string(intent)),
			zap.Float64("confidence", result.Confidence),
			zap.String("question", question),
		)
		return intent, result.Confidence, nil
	default:
		return IntentProductQA, 0.5, nil
	}
}
