package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/compliance"
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
	TaskWhatsAppBroadcast   TaskType = "whatsapp_broadcast"
	TaskRecruitmentMessage  TaskType = "recruitment_message"
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
- "generate_product_description": Generate or improve a product description
- "whatsapp_broadcast": Craft a WhatsApp broadcast message (short, conversational, CTA link)
- "recruitment_message": Draft a 1:1 business opportunity or recruitment outreach (invitation to learn about the business)`

// The task prompts hold only what is specific to each format. The honesty rules, the business
// type's rules and the marketing laws come from compliance.PromptRules, added in Execute, so every
// business gets its own rules (until 2026-11-17 these prompts were written for Forever Living only
// and every tenant got FLP ad copy with an FDA line and FLP hashtags).
var taskSystemPrompts = map[TaskType]string{
	TaskCreateAdCopy: `You write one Meta (Facebook/Instagram) ad for this business.

FORMAT RULES:
- The first line of primary_text is the hook: the buyer's specific problem, a surprising true number from the facts, or the result.
- The headline states the outcome the buyer gets, in under 40 characters.
- One call to action only, naming the action and the channel (for example "Order on WhatsApp"); never "Learn More" or "Get started".
- Use the exact product name and only prices and offers from the knowledge base or the request.

Output ONLY valid JSON with this structure:
{
  "hook": "...",
  "headline": "...",
  "primary_text": "...",
  "description": "...",
  "cta": "...",
  "product_link": "a link from the knowledge base or the request, else empty",
  "target_audience": {"age_min": 18, "age_max": 65, "interests": ["..."]},
  "ad_format": "single_image" or "carousel" or "video"
}`,

	TaskGenerateSocialPost: `You write one social media post for this business.

FORMAT RULES:
- The first line is the hook and must work on its own before "see more".
- One idea per post; end with one call to action (one clear next step) or a real question, never "Agree?" or engagement bait.
- Emojis sparingly (at most 3); 3 to 5 relevant hashtags.
- Suggest the post's visual. Posting times are East Africa Time.

Output ONLY valid JSON:
{
  "platform": "instagram" or "facebook" or "tiktok",
  "caption": "...",
  "hashtags": ["#..."],
  "suggested_image": "Description of ideal image",
  "best_posting_time": "e.g. Tuesday 7pm EAT",
  "content_type": "product_spotlight" or "customer_story" or "lifestyle" or "educational"
}`,

	TaskDraftEmail: `You write one marketing email for this business.

FORMAT RULES:
- Subject line under 50 characters, specific and honest; the preview text adds to it.
- The first sentence is the hook; value before the ask.
- One call to action and one link.
- End with an unsubscribe note.

Output ONLY valid JSON:
{
  "subject_line": "...",
  "preview_text": "...",
  "body_html": "HTML email body",
  "body_text": "Plain text version",
  "cta_text": "...",
  "cta_url": "a link from the knowledge base or the request, else empty",
  "email_type": "welcome" or "product" or "nurture" or "reengagement"
}`,

	TaskCreateFunnelContent: `You write landing page copy for this business.

FORMAT RULES:
- The headline states the outcome the visitor gets; the subheadline says how, for whom.
- Benefits before features, each tied to a fact from the knowledge base.
- social_proof is a real review, count or rating from the knowledge base, quoted as given; if there is none, write "[NEED: a real customer review]".
- One call to action, the same on the whole page (one WhatsApp message, one M-Pesa payment or one short form).

Output ONLY valid JSON:
{
  "headline": "...",
  "subheadline": "...",
  "hero_description": "...",
  "benefits": ["benefit 1", "benefit 2", "benefit 3"],
  "social_proof": "a real review or stat from the knowledge base, or [NEED: ...]",
  "cta_text": "...",
  "cta_url": "...",
  "funnel_type": "product_inquiry" or "event" or "recruitment"
}`,

	TaskWhatsAppBroadcast: `You write one WhatsApp broadcast for this business's opted-in customers.

FORMAT RULES:
- Under 300 characters. Name the business in the first line, then the hook.
- Conversational and friendly.
- One call to action with one short link or a reply word.
- End with how to stop these messages (for example "Reply STOP to opt out").

Output ONLY valid JSON:
{
  "message": "...",
  "cta_link": "...",
  "audience_segment": "warm_leads | dormant | new_buyers"
}`,

	TaskRecruitmentMessage: `You write a 1:1 outreach message inviting someone to learn about this business's opportunity.

FORMAT RULES:
- Warm, personal, non-pushy opening; a curiosity question that invites a conversation.
- Offer one next step: a short info document or a 15-minute call.
- Never hint at earnings without the income disclaimer the rules require.

Output ONLY valid JSON:
{
  "opener": "...",
  "value_prop": "...",
  "soft_cta": "...",
  "follow_up_if_no_reply": "...",
  "best_channel": "whatsapp | dm | email"
}`,

	TaskGenerateProductDesc: `You write a product description for this business.

FORMAT RULES:
- The short description leads with the outcome the buyer gets, then what it is.
- Key details (ingredients, size, item code, price) only as given in the knowledge base.

Output ONLY valid JSON:
{
  "product_name": "...",
  "short_description": "1-2 sentences",
  "long_description": "Full product description",
  "key_benefits": ["benefit 1", "benefit 2", "benefit 3"],
  "usage_directions": "...",
  "tags": ["..."]
}`,
}

// systemPromptFor is the task's format prompt plus the business type's rules and the marketing
// laws (compliance.PromptRules), then the knowledge base context.
func systemPromptFor(taskType TaskType, businessType, ragContext string) string {
	p, ok := taskSystemPrompts[taskType]
	if !ok {
		p = taskSystemPrompts[TaskCreateAdCopy] // default
	}
	p += "\n" + compliance.PromptRules(businessType)
	if ragContext != "" {
		p += "\n--- PRODUCT KNOWLEDGE ---\n" + ragContext + "\n--- END KNOWLEDGE ---\n"
	}
	return p
}

// Execute runs an AI task based on the user's request.
func (e *Executor) Execute(ctx context.Context, client ai.ChatClient, model string, question string, ragContext string, businessType string) (*TaskResult, error) {
	// Step 1: Classify the task type
	taskType, err := e.classifyTask(ctx, client, model, question)
	if err != nil {
		return nil, fmt.Errorf("classify task: %w", err)
	}

	// Step 2: the task's format prompt with the business's rules and the knowledge base.
	systemPrompt := systemPromptFor(taskType, businessType, ragContext)

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

	// Step 6: For ad copy tasks, add follow-up guidance to prompt tool chaining.
	if taskType == TaskCreateAdCopy {
		structured["_follow_up"] = "I've created the ad copy. Would you like me to check your connected ad platforms and calculate the campaign cost? I can also search for product images to use in the ad."
	}

	return &TaskResult{
		TaskType:    taskType,
		Content:     structured,
		NextActions: nextActions,
		Model:       model,
		Intent:      "task_execute",
	}, nil
}

// classifyTask determines what type of task the user is requesting.
func (e *Executor) classifyTask(ctx context.Context, client ai.ChatClient, model string, question string) (TaskType, error) {
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
	case TaskCreateAdCopy, TaskGenerateSocialPost, TaskDraftEmail,
		TaskCreateFunnelContent, TaskGenerateProductDesc,
		TaskWhatsAppBroadcast, TaskRecruitmentMessage:
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
	case TaskWhatsAppBroadcast:
		return []string{
			"Review segment size before sending",
			"Schedule broadcast via notifications-service",
			"Clone message for A/B variant",
		}
	case TaskRecruitmentMessage:
		return []string{
			"Send via WhatsApp or DM to warm lead",
			"Attach the business opportunity document",
			"Add follow-up reminder to nurture sequence",
		}
	default:
		return []string{"Review and refine the output"}
	}
}
