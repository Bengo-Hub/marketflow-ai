package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/marketflow-ai/internal/agent"
	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/llm"
	"go.uber.org/zap"
)

// Chat processes a user question through the optimized multi-layer RAG pipeline.
//
// Source priority (fastest → richest):
//  1. L1 exact cache (5ms) — SHA256(tenant:question) → response
//  2. Heuristic classifier (0ms) — keyword rules skip Groq for obvious intents
//  3. L3 classification cache (5ms) — saved LLM classifications reused for 7d
//  4. L4 embedding cache (5ms) — reuse cached question vectors
//  5. L2 semantic cache (10ms) — cosine(cached_emb, new_emb) > 0.92 → reuse answer
//  6. L5 KB result cache (5ms) — cached pgvector search results
//  7. KB-only single-shot LLM (800ms) — when KB top-sim > 0.55, skip agent
//  8. Agentic loop (2-4s) — only when KB is insufficient AND needs_live_info
//  9. Web search — last resort, inside the agent loop
func (p *Pipeline) Chat(ctx context.Context, input ChatInput) (*ChatResult, error) {
	started := time.Now()

	// ── Jailbreak / prompt-injection guard ───────────────────────────────
	if blocked, resp := ScreenInput(input.Question); blocked {
		p.logger.Info("jailbreak attempt blocked", zap.String("tenant", input.TenantSlug))
		return &ChatResult{Response: resp, Intent: "blocked", Confidence: 1.0, Model: "guard"}, nil
	}

	// ── Niche auto-detection ──────────────────────────────────────────────
	// If the client did not send business_type (public widget on old pages,
	// direct API calls, or the codevertex-website ChatBot), detect it from
	// the question keywords so FLP queries are never routed to the Codevertex
	// persona and vice versa.
	if input.BusinessType == "" {
		input.BusinessType = detectNiche(input.Question)
	}

	// ── Scope guard — pre-classification niche enforcement ───────────────
	// Runs BEFORE any cache lookup or LLM call. Blocks questions that are
	// clearly outside the widget's business scope (e.g. Python code on an
	// FLP health site) without wasting tokens on classification or generation.
	if scopeGuard(input.BusinessType, input.Question) {
		msg := offTopicMessage(input.BusinessType)
		return &ChatResult{
			Response: msg, Intent: string(ai.IntentOffTopic),
			Confidence: 0.99, Model: "scope_guard",
		}, nil
	}

	// ── L1: exact cache ──────────────────────────────────────────────────
	// Skip caches when conversation history exists — the response must be
	// generated in the context of the ongoing conversation, not recycled from
	// a previous standalone session.
	skipCache := len(input.History) > 0
	if !skipCache {
		if resp, ok := p.cache.getExact(ctx, input.TenantID, input.Question); ok {
			p.logger.Debug("L1 exact cache hit", zap.Duration("latency", time.Since(started)))
			filtered := p.applyFilter(resp, input.BusinessType)
			return &ChatResult{Response: filtered, RawResponse: resp, Cached: true, Intent: "cached"}, nil
		}
	}

	// ── Classify: heuristic first, then cached, then the classifier model ─
	intent, confidence, needsLiveInfo := p.classify(ctx, input.Question)

	// ── Template intents (0ms, no LLM) ───────────────────────────────────
	switch intent {
	case ai.IntentOffTopic:
		var offTopicMsg string
		switch input.BusinessType {
		case "flp":
			offTopicMsg = "I'm here to help with Forever Living Products and business opportunities. Could you ask me about our products, wellness tips, or the FBO business?"
		case "codevertex":
			offTopicMsg = "I'm Vera, the Codevertex Africa Limited AI assistant. I can help with our IT services, Digitika Academy courses, or the Power Suite platform. What would you like to know?"
		case "cafe":
			offTopicMsg = "I'm Vera, your cafe assistant. I can help with our menu, upcoming events, table reservations, or anything about your dining experience. What would you like to know?"
		default:
			offTopicMsg = "I'm Vera, your AI assistant. I can help with products, services, and business questions. What would you like to know?"
		}
		return &ChatResult{Response: offTopicMsg, Intent: string(intent), Confidence: confidence, Model: "template"}, nil
	case ai.IntentEscalate:
		return &ChatResult{Response: escalateMessage(input.BusinessType), Intent: string(intent), Confidence: confidence, Model: "template"}, nil
	case ai.IntentSimpleFAQ:
		// Greetings and very simple FAQs get a warm template without burning
		// an LLM call + embedding. The agent is overkill for "hi".
		q := strings.ToLower(strings.TrimSpace(input.Question))
		if isGreeting(q) {
			var resp string
			switch input.BusinessType {
			case "flp":
				resp = "Hello! Welcome to Forever Living Products. I can help you with product info, the business opportunity, or anything else about Forever. What would you like to know?"
			case "codevertex":
				resp = "Hello! I'm Vera, your Codevertex Africa Limited AI assistant. I can help with our IT services, Digitika Academy courses, or the Power Suite platform. What would you like to know?"
			case "cafe":
				resp = "Hello! Welcome! I'm Vera, your cafe assistant. I can help with our menu, upcoming events, table reservations, or anything about your visit. What can I help you with today?"
			default:
				resp = "Hello! I'm Vera, your AI assistant. Feel free to ask me about products, services, or anything else I can help with."
			}
			return &ChatResult{Response: resp, Intent: string(intent), Confidence: confidence, Model: "template"}, nil
		}
	case ai.IntentTaskExecute:
		if p.taskExecutor != nil {
			var ragCtx string
			emb := p.cachedEmbed(ctx, input.Question)
			if emb != nil {
				if chunks, err := p.cachedKBSearch(ctx, input.TenantID, emb, 3); err == nil {
					for _, c := range chunks {
						ragCtx += fmt.Sprintf("[%s] %s: %s\n", c.Category, c.Title, c.ChunkText)
					}
				}
			}
			result, err := p.taskExecutor.Execute(ctx, p.llm.Client(llm.Standard), string(llm.Standard), input.Question, ragCtx)
			if err != nil {
				return nil, fmt.Errorf("task execution failed: %w", err)
			}
			respJSON, _ := json.Marshal(result)
			return &ChatResult{Response: string(respJSON), Intent: "task_execute", Confidence: confidence, Model: result.Model}, nil
		}
	}

	// ── Embed question (cached or live) ──────────────────────────────────
	embedding := p.cachedEmbed(ctx, input.Question)

	// ── L2: semantic cache (check before KB search) ──────────────────────
	if !skipCache && embedding != nil {
		if resp, ok := p.cache.getSemantic(ctx, input.TenantID, embedding); ok {
			// Semantic hit — apply per-request filter (raw responses are cached).
			p.logger.Debug("L2 semantic cache hit", zap.Duration("latency", time.Since(started)))
			filtered := p.applyFilter(resp, input.BusinessType)
			return &ChatResult{Response: filtered, RawResponse: resp, Cached: true, Intent: string(intent), Confidence: confidence, Model: "semantic_cache"}, nil
		}
	}

	// ── KB search (cached or live) ───────────────────────────────────────
	var contextChunks []SearchResult
	var topSim float64
	if embedding != nil {
		var err error
		contextChunks, err = p.cachedKBSearch(ctx, input.TenantID, embedding, 5)
		if err != nil {
			p.logger.Warn("KB search failed", zap.Error(err))
		}
		if len(contextChunks) > 0 {
			topSim = contextChunks[0].Similarity
		}
	}

	// ── Knowledge base already answers it: no model ─────────────────────
	// Action and live-data intents still go to the agent: a stored text cannot
	// book a meeting or read today's orders.
	if intent != ai.IntentTaskExecute && intent != ai.IntentDataQuery && intent != ai.IntentRecruitment {
		if answer, ok := p.kbDirect(ctx, input, embedding, contextChunks); ok {
			return &ChatResult{
				Response: p.applyFilter(answer, input.BusinessType), RawResponse: answer,
				Intent: string(intent), Confidence: confidence, Sources: 1, Model: ModelKnowledgeBase,
			}, nil
		}
	}

	// ── Source routing decision ──────────────────────────────────────────
	//
	// intentNeedsTools: intents that MUST go through the agent loop because
	// they require tool calls to produce a correct answer. The agent is also
	// forced to call at least one tool on the first iteration (ForceToolUse)
	// so the model cannot skip straight to generating ad copy / plain text.
	//
	// intentNeedsLiveData: intents where the answer requires live service
	// data that is NOT in the knowledge base (course catalog, enrollments,
	// orders, subscriptions). These always trigger the agent so
	// query_service_data can fetch real records.
	//
	// Public mode (widget on /p/{slug}): throttle the agent for general Q&A
	// to keep response times fast. Action intents (task_execute, recruitment)
	// always use the agent regardless of public mode.
	intentNeedsTools := intent == ai.IntentRecruitment ||
		intent == ai.IntentTaskExecute ||
		intent == ai.IntentBusinessOpp
	intentNeedsLiveData := intent == ai.IntentDataQuery ||
		intent == ai.IntentServiceInquiry ||
		intent == ai.IntentCourseInquiry

	useAgent := p.agentEnabled && p.agentLoop != nil &&
		(intent == ai.IntentProductQA || intentNeedsTools || intentNeedsLiveData) &&
		(needsLiveInfo || topSim < 0.55 || intentNeedsTools || intentNeedsLiveData)

	if input.PublicMode {
		if !intentNeedsTools && !intentNeedsLiveData {
			qLower := strings.ToLower(input.Question)
			needsToolsKW := strings.Contains(qLower, "join") ||
				strings.Contains(qLower, "sign up") ||
				strings.Contains(qLower, "link") ||
				strings.Contains(qLower, "book") ||
				strings.Contains(qLower, "meeting") ||
				strings.Contains(qLower, "schedule") ||
				strings.Contains(qLower, "call") ||
				strings.Contains(qLower, "shop") ||
				strings.Contains(qLower, "buy") ||
				strings.Contains(qLower, "price") ||
				strings.Contains(qLower, "whatsapp") ||
				strings.Contains(qLower, "recruit") ||
				strings.Contains(qLower, "demo") ||
				strings.Contains(qLower, "started")
			if !needsToolsKW {
				useAgent = useAgent && topSim < 0.3
			}
		}
	}

	if useAgent {
		sys := p.buildAgentSystemPrompt(input.BusinessType, intent)
		agentInput := agent.Input{
			TenantID:     input.TenantID,
			SystemPrompt: sys,
			History:      input.History,
			Question:     input.Question,
			// Force the model to call a tool on iter 1 so it cannot skip
			// straight to generating ad copy / plain text answers.
			ForceToolUse: intentNeedsTools || intentNeedsLiveData,
			PublicMode:   input.PublicMode,
		}
		// task_execute needs more iterations for full tool chains
		// (check_integrations → kb_search → search_images → calculate_ad_cost → create_campaign).
		if intent == ai.IntentTaskExecute {
			agentInput.MaxIters = 8
		}
		ares, aerr := p.agentLoop.Run(ctx, agentInput)
		if aerr == nil && ares != nil && ares.Answer != "" {
			ares.Answer = ScreenOutput(ares.Answer)
			filtered := p.filterAndCache(ctx, input, ares.Answer, embedding)
			p.logger.Info("agentic chat completed",
				zap.Int("iterations", ares.Iterations),
				zap.Int("tool_calls", ares.ToolCallsN),
				zap.Int("tokens", ares.TotalTokens),
				zap.Duration("latency", time.Since(started)),
			)
			return &ChatResult{
				Response: filtered, RawResponse: ares.Answer,
				Intent: string(intent), Confidence: confidence,
				Sources: len(contextChunks) + ares.ToolCallsN, Model: ares.Model,
			}, nil
		}
		p.logger.Warn("agent loop failed, falling back to single-shot", zap.Error(aerr))
	}

	// ── Single-shot LLM with KB context ─────────────────────────────────
	systemPrompt := p.buildSystemPrompt(input.BusinessType, intent, contextChunks)
	messages := make([]ai.Message, 0, len(input.History)+2)
	messages = append(messages, ai.Message{Role: "system", Content: systemPrompt})
	historyStart := 0
	if len(input.History) > 6 {
		historyStart = len(input.History) - 6
	}
	messages = append(messages, input.History[historyStart:]...)
	messages = append(messages, ai.Message{Role: "user", Content: input.Question})

	// Provider fallback, breakers and metering live in the model router. When
	// every model fails (or answers with nothing) the knowledge base answers.
	resp, call, err := p.llm.Complete(ctx, answerClass(input), ai.ChatRequest{
		Messages: messages, Temperature: 0.7, MaxTokens: 2048,
	})
	if err == nil && (len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "") {
		err = fmt.Errorf("%s answered without text", call.Model)
	}
	if err != nil {
		p.logger.Warn("all models failed; answering from the knowledge base", zap.Error(err))
		fb := kbFallback(input, contextChunks)
		return &ChatResult{
			Response: p.applyFilter(fb, input.BusinessType), RawResponse: fb,
			Intent: string(intent), Confidence: confidence, Sources: len(contextChunks), Model: ModelKBFallback,
		}, nil
	}
	model := call.Model

	raw := ScreenOutput(resp.Choices[0].Message.Content)
	filtered := p.filterAndCache(ctx, input, raw, embedding)
	p.logger.Debug("single-shot completed", zap.Duration("latency", time.Since(started)))
	return &ChatResult{
		Response: filtered, RawResponse: raw,
		Intent: string(intent), Confidence: confidence,
		Sources: len(contextChunks), Model: model,
	}, nil
}
