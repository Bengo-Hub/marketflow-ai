package rag

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/marketflow-ai/internal/agent"
	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/compliance"
	"github.com/bengobox/marketflow-ai/internal/providers"
	"github.com/bengobox/marketflow-ai/internal/sessions"
	"github.com/bengobox/marketflow-ai/internal/tasks"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// SearchResult represents a vector similarity search result from pgvector.
type SearchResult struct {
	EntryID    uuid.UUID
	ChunkText  string
	Similarity float64
	Category   string
	Title      string
}

// Pipeline orchestrates the full RAG flow: embed → search → prompt → LLM → filter.
type Pipeline struct {
	db              *sql.DB
	redis           *redis.Client
	groqClient      *ai.Client
	ollamaClient    *ai.Client
	router          *ai.Router
	filter          *compliance.Filter
	resolver        *providers.Resolver
	sessionService  *sessions.Service
	taskExecutor    *tasks.Executor
	agentLoop       *agent.Loop
	agentEnabled    bool
	embeddingModel  string
	mainModel       string
	fallbackModel   string
	embeddingDim    int
	redisNamespace  string
	cache           *cacheLayer
	logger          *zap.Logger
}

// Config holds pipeline configuration.
type Config struct {
	DB              *sql.DB
	Redis           *redis.Client
	GroqClient      *ai.Client
	OllamaClient    *ai.Client
	Router          *ai.Router
	Resolver        *providers.Resolver
	SessionService  *sessions.Service
	TaskExecutor    *tasks.Executor
	Agent           *agent.Loop
	AgentEnabled    bool
	EmbeddingModel  string
	MainModel       string
	FallbackModel   string
	EmbeddingDim    int
	RedisNamespace  string
	Logger          *zap.Logger
}

// NewPipeline creates a new RAG pipeline.
func NewPipeline(cfg Config) *Pipeline {
	p := &Pipeline{
		db:             cfg.DB,
		redis:          cfg.Redis,
		groqClient:     cfg.GroqClient,
		ollamaClient:   cfg.OllamaClient,
		router:         cfg.Router,
		filter:         compliance.NewFilter(),
		resolver:       cfg.Resolver,
		sessionService: cfg.SessionService,
		taskExecutor:   cfg.TaskExecutor,
		agentLoop:      cfg.Agent,
		agentEnabled:   cfg.AgentEnabled,
		embeddingModel: cfg.EmbeddingModel,
		mainModel:      cfg.MainModel,
		fallbackModel:  cfg.FallbackModel,
		embeddingDim:   cfg.EmbeddingDim,
		redisNamespace: cfg.RedisNamespace,
		logger:         cfg.Logger.Named("rag"),
	}
	p.cache = newCacheLayer(cfg.Redis, cfg.RedisNamespace, p.logger)
	return p
}

// ChatInput represents a user chat message.
type ChatInput struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	Question  string
	History   []ai.Message // previous messages in this session
}

// ChatResult contains the AI response and metadata.
type ChatResult struct {
	Response   string  `json:"response"`
	Intent     string  `json:"intent"`
	Confidence float64 `json:"confidence"`
	Sources    int     `json:"sources"`
	Cached     bool    `json:"cached"`
	Model      string  `json:"model"`
}

// resolveClient returns the platform-level Groq client and model.
// AI models are shared resources — NOT per-tenant. Configured once by platform admin.
func (p *Pipeline) resolveClient(ctx context.Context) (*ai.Client, string) {
	if p.resolver != nil {
		client, mainModel, _, err := p.resolver.ResolveGroqClient(ctx)
		if err == nil && client != nil {
			return client, mainModel
		}
		p.logger.Debug("resolver failed, using static client", zap.Error(err))
	}
	return p.groqClient, p.mainModel
}

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

	// ── L1: exact cache ──────────────────────────────────────────────────
	if resp, ok := p.cache.getExact(ctx, input.TenantID, input.Question); ok {
		p.logger.Debug("L1 exact cache hit", zap.Duration("latency", time.Since(started)))
		return &ChatResult{Response: resp, Cached: true, Intent: "cached"}, nil
	}

	// ── Classify: heuristic first, then cached, then LLM ─────────────────
	var intent ai.Intent
	var confidence float64
	var needsLiveInfo bool

	if hcls, ok := ai.HeuristicClassify(input.Question); ok {
		intent, confidence, needsLiveInfo = hcls.Intent, hcls.Confidence, hcls.NeedsLiveInfo
		p.logger.Debug("heuristic classified", zap.String("intent", string(intent)))
	} else if cc, ok := p.cache.getClassification(ctx, input.Question); ok {
		intent = ai.Intent(cc.Intent)
		confidence = cc.Confidence
		needsLiveInfo = cc.NeedsLiveInfo
		p.logger.Debug("L3 classification cache hit", zap.String("intent", string(intent)))
	} else {
		cls, err := p.router.Classify(ctx, input.Question)
		if err != nil {
			p.logger.Warn("classification failed, defaulting to product_qa", zap.Error(err))
			intent, confidence = ai.IntentProductQA, 0.5
		} else {
			intent, confidence, needsLiveInfo = cls.Intent, cls.Confidence, cls.NeedsLiveInfo
		}
		p.cache.setClassification(ctx, input.Question, clsCache{
			Intent: string(intent), Confidence: confidence, NeedsLiveInfo: needsLiveInfo,
		})
	}

	// ── Template intents (0ms, no LLM) ───────────────────────────────────
	switch intent {
	case ai.IntentOffTopic:
		return &ChatResult{Response: "I'm here to help with Forever Living Products and business opportunities. Could you ask me about our products, wellness tips, or the FBO business?",
			Intent: string(intent), Confidence: confidence, Model: "template"}, nil
	case ai.IntentEscalate:
		return &ChatResult{Response: "I'd be happy to connect you with your FBO directly. You can reach them via WhatsApp for a personal conversation, or book a virtual meeting.",
			Intent: string(intent), Confidence: confidence, Model: "template"}, nil
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
			client, model := p.resolveClient(ctx)
			result, err := p.taskExecutor.Execute(ctx, client, model, input.Question, ragCtx)
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
	if embedding != nil {
		if resp, ok := p.cache.getSemantic(ctx, input.TenantID, embedding); ok {
			// Semantic hit — still run compliance filter in case rules changed.
			filtered, blocked, _ := p.filter.Check(resp)
			if blocked {
				filtered = "I appreciate your question! For the most accurate and up-to-date information, please visit foreverliving.com or speak directly with your Forever Business Owner."
			}
			p.logger.Debug("L2 semantic cache hit", zap.Duration("latency", time.Since(started)))
			return &ChatResult{Response: filtered, Cached: true, Intent: string(intent), Confidence: confidence, Model: "semantic_cache"}, nil
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

	// ── Source routing decision ──────────────────────────────────────────
	//
	// KB-first: when the top chunk similarity ≥ 0.55, the KB has strong
	// coverage → single-shot LLM with RAG context is enough (~800ms).
	// Only engage the expensive agentic loop when KB is weak or the
	// classifier flagged needs_live_info.
	useAgent := p.agentEnabled && p.agentLoop != nil &&
		(intent == ai.IntentProductQA || intent == ai.IntentBusinessOpp || intent == ai.IntentRecruitment) &&
		(needsLiveInfo || topSim < 0.55)

	if useAgent {
		sys := p.buildAgentSystemPrompt(intent)
		ares, aerr := p.agentLoop.Run(ctx, agent.Input{
			TenantID: input.TenantID, SystemPrompt: sys,
			History: input.History, Question: input.Question,
		})
		if aerr == nil && ares != nil && ares.Answer != "" {
			filtered := p.filterAndCache(ctx, input, ares.Answer, embedding)
			p.logger.Info("agentic chat completed",
				zap.Int("iterations", ares.Iterations),
				zap.Int("tool_calls", ares.ToolCallsN),
				zap.Int("tokens", ares.TotalTokens),
				zap.Duration("latency", time.Since(started)),
			)
			return &ChatResult{
				Response: filtered, Intent: string(intent), Confidence: confidence,
				Sources: len(contextChunks) + ares.ToolCallsN, Model: ares.Model,
			}, nil
		}
		p.logger.Warn("agent loop failed, falling back to single-shot", zap.Error(aerr))
	}

	// ── Single-shot LLM with KB context ─────────────────────────────────
	systemPrompt := p.buildSystemPrompt(intent, contextChunks)
	messages := make([]ai.Message, 0, len(input.History)+2)
	messages = append(messages, ai.Message{Role: "system", Content: systemPrompt})
	historyStart := 0
	if len(input.History) > 6 {
		historyStart = len(input.History) - 6
	}
	messages = append(messages, input.History[historyStart:]...)
	messages = append(messages, ai.Message{Role: "user", Content: input.Question})

	client, model := p.resolveClient(ctx)
	resp, err := client.Chat(ctx, ai.ChatRequest{
		Model: model, Messages: messages, Temperature: 0.7, MaxTokens: 2048,
	})
	if err != nil {
		p.logger.Warn("main model failed, trying fallback", zap.Error(err))
		model = p.fallbackModel
		resp, err = p.groqClient.Chat(ctx, ai.ChatRequest{
			Model: model, Messages: messages, Temperature: 0.7, MaxTokens: 1024,
		})
		if err != nil {
			return nil, fmt.Errorf("all LLM models failed: %w", err)
		}
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("empty response from LLM")
	}

	filtered := p.filterAndCache(ctx, input, resp.Choices[0].Message.Content, embedding)
	p.logger.Debug("single-shot completed", zap.Duration("latency", time.Since(started)))
	return &ChatResult{
		Response: filtered, Intent: string(intent), Confidence: confidence,
		Sources: len(contextChunks), Model: model,
	}, nil
}

// cachedEmbed returns the question embedding, preferring the L4 cache
// and falling back to a live Ollama call + cache write.
func (p *Pipeline) cachedEmbed(ctx context.Context, question string) []float32 {
	if emb, ok := p.cache.getEmbedding(ctx, question); ok {
		return emb
	}
	if p.ollamaClient == nil {
		return nil
	}
	emb, err := p.ollamaClient.Embed(ctx, p.embeddingModel, question)
	if err != nil {
		p.logger.Warn("embedding failed", zap.Error(err))
		return nil
	}
	p.cache.setEmbedding(ctx, question, emb)
	return emb
}

// cachedKBSearch returns pgvector results, preferring the L5 cache.
func (p *Pipeline) cachedKBSearch(ctx context.Context, tenantID uuid.UUID, emb []float32, topK int) ([]SearchResult, error) {
	if results, ok := p.cache.getKBResults(ctx, tenantID, emb); ok {
		return results, nil
	}
	results, err := p.searchPgVector(ctx, tenantID, emb, topK)
	if err != nil {
		return nil, err
	}
	p.cache.setKBResults(ctx, tenantID, emb, results)
	return results, nil
}

// filterAndCache applies the compliance filter, writes L1 + L2 caches.
func (p *Pipeline) filterAndCache(ctx context.Context, input ChatInput, raw string, emb []float32) string {
	filtered, blocked, reason := p.filter.Check(raw)
	if blocked {
		p.logger.Warn("response blocked by compliance filter",
			zap.String("reason", reason), zap.String("question", input.Question))
		filtered = "I appreciate your question! For the most accurate and up-to-date information, please visit foreverliving.com or speak directly with your Forever Business Owner."
	}
	p.cache.setExact(ctx, input.TenantID, input.Question, filtered)
	p.cache.setSemantic(ctx, input.TenantID, input.Question, emb, filtered)
	return filtered
}

// ChatStream processes a user question and returns a streaming channel.
func (p *Pipeline) ChatStream(ctx context.Context, input ChatInput) (<-chan ai.StreamDelta, *ChatResult, error) {
	// Classify intent
	intent, confidence, _ := p.router.ClassifyIntent(ctx, input.Question)

	// Handle non-LLM intents
	switch intent {
	case ai.IntentOffTopic, ai.IntentEscalate:
		result, _ := p.Chat(ctx, input)
		ch := make(chan ai.StreamDelta, 1)
		go func() {
			ch <- ai.StreamDelta{Content: result.Response, Done: true}
			close(ch)
		}()
		return ch, result, nil
	}

	// Embed and search
	var contextChunks []SearchResult
	if p.ollamaClient != nil {
		if embedding, err := p.ollamaClient.Embed(ctx, p.embeddingModel, input.Question); err == nil {
			contextChunks, _ = p.searchPgVector(ctx, input.TenantID, embedding, 5)
		}
	}

	systemPrompt := p.buildSystemPrompt(intent, contextChunks)
	messages := make([]ai.Message, 0, len(input.History)+2)
	messages = append(messages, ai.Message{Role: "system", Content: systemPrompt})
	historyStart := 0
	if len(input.History) > 6 {
		historyStart = len(input.History) - 6
	}
	messages = append(messages, input.History[historyStart:]...)
	messages = append(messages, ai.Message{Role: "user", Content: input.Question})

	ch, err := p.groqClient.ChatStream(ctx, ai.ChatRequest{
		Model:       p.mainModel,
		Messages:    messages,
		Temperature: 0.7,
		MaxTokens:   2048,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("stream request failed: %w", err)
	}

	meta := &ChatResult{
		Intent:     string(intent),
		Confidence: confidence,
		Sources:    len(contextChunks),
		Model:      p.mainModel,
	}

	return ch, meta, nil
}

// searchPgVector queries the knowledge_base_embeddings for similar chunks.
func (p *Pipeline) searchPgVector(ctx context.Context, tenantID uuid.UUID, queryEmbedding []float32, topK int) ([]SearchResult, error) {
	// Build the vector literal for pgvector
	parts := make([]string, len(queryEmbedding))
	for i, v := range queryEmbedding {
		parts[i] = fmt.Sprintf("%f", v)
	}
	vectorStr := "[" + strings.Join(parts, ",") + "]"

	// Include tenant-private KB rows AND platform-shared KB rows (scope='platform').
	// Platform rows power the base FLP catalog / compliance corpus that every
	// tenant's agent should be able to cite. Tenant rows remain isolated.
	query := `
		SELECT e.id, e.chunk_text, e.category, e.title,
		       1 - (e.embedding <=> $1::vector) AS similarity
		FROM knowledge_base_embeddings e
		JOIN knowledge_base_entries kb ON kb.id = e.entry_id
		WHERE kb.scope = 'platform' OR kb.tenant_id = $2
		ORDER BY e.embedding <=> $1::vector
		LIMIT $3`

	rows, err := p.db.QueryContext(ctx, query, vectorStr, tenantID, topK)
	if err != nil {
		return nil, fmt.Errorf("pgvector search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.EntryID, &r.ChunkText, &r.Category, &r.Title, &r.Similarity); err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// buildSystemPrompt constructs the system prompt with RAG context and FLP compliance rules.
func (p *Pipeline) buildSystemPrompt(intent ai.Intent, chunks []SearchResult) string {
	var b strings.Builder

	b.WriteString(`You are a helpful AI assistant for Forever Living Products (FLP). You help customers learn about aloe vera products and the FBO business opportunity.

IMPORTANT RULES:
- NEVER make medical claims. Use "supports", "promotes", "helps maintain" — NOT "cures", "treats", "heals", "prevents disease"
- NEVER guarantee specific income amounts. Always say "results vary"
- Always recommend consulting a healthcare professional for medical questions
- Be warm, helpful, and factual. Cite product names and codes when relevant.
- FLP is the world's largest grower of aloe vera, operating in 160+ countries for over 46 years.
- Products are IASC certified. DR aloe farms are USDA and EU Organic certified.
`)

	switch intent {
	case ai.IntentBusinessOpp, ai.IntentRecruitment:
		b.WriteString(`
Focus on the business opportunity. Key points:
- 3 income streams: Retail profit (30-48%), Team building bonuses, Leadership bonuses
- Rank ladder: Assistant Supervisor → Supervisor → Manager → Senior Manager → Eagle Manager
- Forever2Drive incentive: $400-$800/month for 3 years
- Businesses are inheritable — a true legacy asset
- NEVER guarantee specific earnings. Always include income disclaimer.
`)
	case ai.IntentProductQA:
		b.WriteString(`
Focus on product information. When recommending products:
- Mention the product code (e.g., #015 for Aloe Vera Gel)
- Include the CC (case credit) value when relevant
- Suggest the C9 Clean 9 program as a great starting point for new customers
- Mention that products contain up to 99.7% pure stabilized aloe vera gel
`)
	}

	if len(chunks) > 0 {
		b.WriteString("\n--- KNOWLEDGE BASE CONTEXT ---\n")
		for _, c := range chunks {
			b.WriteString(fmt.Sprintf("[%s] %s: %s\n\n", c.Category, c.Title, c.ChunkText))
		}
		b.WriteString("--- END CONTEXT ---\n")
		b.WriteString("Use the above context to answer. If the context doesn't cover the question, say so honestly.\n")
	}

	return b.String()
}

// buildAgentSystemPrompt returns the system prompt used by the agent loop.
// Tells the model which tools exist and when to use each.
func (p *Pipeline) buildAgentSystemPrompt(intent ai.Intent) string {
	var b strings.Builder
	b.WriteString(`You are an agentic assistant for a Forever Living Products (FLP) Forever Business Owner.

You have access to tools:
- kb_search: semantic search over the tenant's internal knowledge base (products, FAQ, business playbook, compliance). Prefer this first.
- get_product: look up a specific product by code or name.
- get_faq: fetch FAQ entries by keyword.
- get_compliance_rules: returns the FDA/FLP compliance ruleset -- call this before drafting medical or income-claim content.
- web_search: live web search via Tavily when internal KB is insufficient OR the user asks about current/time-sensitive facts. Quota-limited per tenant.

Rules:
- Call tools iteratively; reason step by step.
- Stop calling tools once you have enough to answer; then respond in plain prose.
- NEVER make medical claims (cures, treats, heals, prevents disease). Use supports/promotes/helps maintain.
- NEVER guarantee income. Results vary. Include income disclaimer when relevant.
- Cite product codes and URLs from tool results where helpful.
`)
	switch intent {
	case ai.IntentBusinessOpp, ai.IntentRecruitment:
		b.WriteString("\nFocus on the business opportunity. Mention the 3 income streams, rank ladder, and Forever2Drive. Always include an income disclaimer.\n")
	case ai.IntentProductQA:
		b.WriteString("\nFocus on product information, certifications (IASC, USDA/EU Organic), and relevant starter offers like Clean 9.\n")
	}
	return b.String()
}

// cacheKey kept for back-compat with ChatStream; delegates to cache layer.
func (p *Pipeline) cacheKey(tenantID uuid.UUID, question string) string {
	return p.cache.exactKey(tenantID, question)
}
