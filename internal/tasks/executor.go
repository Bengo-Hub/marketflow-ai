package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"go.uber.org/zap"
)

// TaskType represents the type of AI task to execute.
type TaskType string

const (
	TaskCreateAdCopy        TaskType = "create_ad_copy"
	TaskGenerateSocialPost  TaskType = "generate_social_post"
	TaskDraftEmail          TaskType = "draft_email"
	TaskCreateFunnelContent TaskType = "create_funnel_content"
	TaskGenerateProductDesc TaskType = "generate_product_description"
)

// TaskResult contains the structured output of a task execution.
type TaskResult struct {
	TaskType    TaskType       `json:"task_type"`
	Content     map[string]any `json:"content"`
	NextActions []string       `json:"next_actions"`
	Model       string         `json:"model"`
	Intent      string         `json:"intent"`
}

// Executor handles AI-powered content generation tasks.
type Executor struct {
	logger *zap.Logger
}

// NewExecutor creates a new task executor.
func NewExecutor(logger *zap.Logger) *Executor {
	return &Executor{logger: logger.Named("tasks")}
}

const taskClassifierPrompt = `Classify this user request into exactly ONE task type.
Respond with ONLY a JSON object: {"task_type": "<type>"}

Task types:
- "create_ad_copy": Create advertisement text/copy for Meta (Facebook/Instagram) ads
- "generate_social_post": Generate a social media post (Facebook, Instagram, etc.)
- "draft_email": Draft a marketing or nurture email
- "create_funnel_content": Create landing page or funnel content
- "generate_product_description": Generate or improve a product description`

var taskSystemPrompts = map[TaskType]string{
	TaskCreateAdCopy: `You are an expert digital marketer creating Meta (Facebook/Instagram) ad copy for Forever Living Products.

RULES:
- NEVER make health claims. Use "supports", "promotes" — NOT "cures", "treats"
- Include product name and item code when relevant
- Always suggest a target audience
- Compliance: "These statements have not been evaluated by the FDA."

Output ONLY valid JSON with this structure:
{
  "headline": "...",
  "primary_text": "...",
  "description": "...",
  "cta": "Learn More" or "Shop Now",
  "product_link": "Include the FLP shop link if product code mentioned",
  "target_audience": {"age_min": 25, "age_max": 55, "interests": ["wellness", "health"]},
  "ad_format": "single_image" or "carousel" or "video",
  "suggested_hashtags": ["#ForeverLiving", "#AloeVera"]
}`,

	TaskGenerateSocialPost: `You are a social media manager for Forever Living Products FBOs.

RULES:
- Keep it engaging, authentic, and personal
- Use emojis sparingly (2-3 max)
- Include relevant hashtags (5-8)
- NEVER make health claims
- Compliance: Add disclaimer for health/income mentions

Output ONLY valid JSON:
{
  "platform": "instagram" or "facebook" or "tiktok",
  "caption": "...",
  "hashtags": ["#ForeverLiving", ...],
  "suggested_image": "Description of ideal image",
  "best_posting_time": "e.g., Tuesday 10am EST",
  "content_type": "product_spotlight" or "testimonial" or "lifestyle" or "educational"
}`,

	TaskDraftEmail: `You are an email marketing specialist for Forever Living Products.

RULES:
- Write compelling subject lines (under 50 chars)
- Personal, warm tone
- Clear CTA
- NEVER make health claims
- Include product links when relevant
- Add unsubscribe footer note

Output ONLY valid JSON:
{
  "subject_line": "...",
  "preview_text": "...",
  "body_html": "HTML email body",
  "body_text": "Plain text version",
  "cta_text": "...",
  "cta_url": "product or funnel link",
  "email_type": "welcome" or "product" or "nurture" or "reengagement"
}`,

	TaskCreateFunnelContent: `You are a conversion optimization specialist creating landing page content for Forever Living Products.

RULES:
- Focus on benefits, not features
- Include social proof elements
- Clear value proposition
- Strong CTA
- NEVER make health claims

Output ONLY valid JSON:
{
  "headline": "...",
  "subheadline": "...",
  "hero_description": "...",
  "benefits": ["benefit 1", "benefit 2", "benefit 3"],
  "social_proof": "Testimonial or stat",
  "cta_text": "...",
  "cta_url": "...",
  "funnel_type": "product_inquiry" or "c9_challenge" or "recruitment"
}`,

	TaskGenerateProductDesc: `You are a product copywriter for Forever Living Products.

RULES:
- Highlight key ingredients and benefits
- Use "supports", "promotes" language — NEVER "cures" or "treats"
- Include product code and CC value when provided
- Compliance: "These statements have not been evaluated by the FDA."

Output ONLY valid JSON:
{
  "product_name": "...",
  "short_description": "1-2 sentences",
  "long_description": "Full product description",
  "key_benefits": ["benefit 1", "benefit 2", "benefit 3"],
  "usage_directions": "...",
  "tags": ["aloe vera", "wellness"]
}`,
}

// Execute runs an AI task based on the user's request.
func (e *Executor) Execute(ctx context.Context, client *ai.Client, model string, question string, ragContext string) (*TaskResult, error) {
	// Step 1: Classify the task type
	taskType, err := e.classifyTask(ctx, client, model, question)
	if err != nil {
		return nil, fmt.Errorf("classify task: %w", err)
	}

	// Step 2: Get task-specific system prompt
	systemPrompt, ok := taskSystemPrompts[taskType]
	if !ok {
		systemPrompt = taskSystemPrompts[TaskCreateAdCopy] // default
	}

	// Add RAG context if available
	if ragContext != "" {
		systemPrompt += "\n\n--- PRODUCT KNOWLEDGE ---\n" + ragContext + "\n--- END KNOWLEDGE ---\n"
	}

	// Step 3: Execute the task with structured output
	resp, err := client.Chat(ctx, ai.ChatRequest{
		Model: model,
		Messages: []ai.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: question},
		},
		Temperature: 0.8,
		MaxTokens:   2048,
	})
	if err != nil {
		return nil, fmt.Errorf("execute task: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("empty response from LLM")
	}

	content := strings.TrimSpace(resp.Choices[0].Message.Content)

	// Step 4: Parse JSON from response (handle markdown code blocks)
	if idx := strings.Index(content, "{"); idx >= 0 {
		if end := strings.LastIndex(content, "}"); end > idx {
			content = content[idx : end+1]
		}
	}

	var structured map[string]any
	if err := json.Unmarshal([]byte(content), &structured); err != nil {
		// If JSON parse fails, wrap raw text as content
		structured = map[string]any{
			"raw_content": resp.Choices[0].Message.Content,
			"parse_error": err.Error(),
		}
	}

	// Step 5: Build next actions based on task type
	nextActions := e.suggestNextActions(taskType)

	return &TaskResult{
		TaskType:    taskType,
		Content:     structured,
		NextActions: nextActions,
		Model:       model,
		Intent:      "task_execute",
	}, nil
}

// classifyTask determines what type of task the user is requesting.
func (e *Executor) classifyTask(ctx context.Context, client *ai.Client, model string, question string) (TaskType, error) {
	resp, err := client.Chat(ctx, ai.ChatRequest{
		Model: model,
		Messages: []ai.Message{
			{Role: "system", Content: taskClassifierPrompt},
			{Role: "user", Content: question},
		},
		Temperature: 0.1,
		MaxTokens:   50,
	})
	if err != nil {
		return TaskCreateAdCopy, err
	}

	if len(resp.Choices) == 0 {
		return TaskCreateAdCopy, nil
	}

	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	if idx := strings.Index(content, "{"); idx >= 0 {
		if end := strings.LastIndex(content, "}"); end > idx {
			content = content[idx : end+1]
		}
	}

	var result struct {
		TaskType string `json:"task_type"`
	}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return TaskCreateAdCopy, nil
	}

	switch TaskType(result.TaskType) {
	case TaskCreateAdCopy, TaskGenerateSocialPost, TaskDraftEmail, TaskCreateFunnelContent, TaskGenerateProductDesc:
		return TaskType(result.TaskType), nil
	default:
		return TaskCreateAdCopy, nil
	}
}

func (e *Executor) suggestNextActions(taskType TaskType) []string {
	switch taskType {
	case TaskCreateAdCopy:
		return []string{
			"Create campaign in Meta Ads Manager with this copy",
			"Generate matching image using AI",
			"Schedule ad for optimal posting time",
			"Create A/B test variant",
		}
	case TaskGenerateSocialPost:
		return []string{
			"Post to selected platform",
			"Schedule for suggested posting time",
			"Create additional variants for other platforms",
		}
	case TaskDraftEmail:
		return []string{
			"Send test email to yourself",
			"Add to nurture sequence",
			"Schedule broadcast to contact list",
		}
	case TaskCreateFunnelContent:
		return []string{
			"Create funnel page with this content",
			"Connect to Meta ad campaign",
			"Add lead capture form",
		}
	case TaskGenerateProductDesc:
		return []string{
			"Update knowledge base entry",
			"Use in product landing page",
			"Create social post from this description",
		}
	default:
		return []string{"Review and refine the output"}
	}
}
