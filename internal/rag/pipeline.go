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
	TenantID     uuid.UUID
	TenantSlug   string       // original slug from X-Tenant-ID (may differ from UUID)
	SessionID    uuid.UUID
	Question     string
	History      []ai.Message // previous messages in this session
	BusinessType string       // "flp" or "generic" — drives compliance filter routing
	PublicMode   bool         // true for /public/chat — prefer cache/KB, avoid expensive agent loops
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
	isFLP := input.BusinessType == "flp"
	switch intent {
	case ai.IntentOffTopic:
		var offTopicMsg string
		if isFLP {
			offTopicMsg = "I'm here to help with Forever Living Products and business opportunities. Could you ask me about our products, wellness tips, or the FBO business?"
		} else {
			offTopicMsg = "I'm Vera, the Codevertex AI assistant. I can help with our IT services, Digitika Academy courses, or the Power Suite platform. What would you like to know?"
		}
		return &ChatResult{Response: offTopicMsg, Intent: string(intent), Confidence: confidence, Model: "template"}, nil
	case ai.IntentEscalate:
		var escalateMsg string
		if isFLP {
			escalateMsg = "I'd be happy to connect you with your FBO directly. You can reach them via WhatsApp for a personal conversation, or book a virtual meeting."
		} else {
			escalateMsg = "I'd be happy to connect you with the Codevertex team. You can reach us at info@codevertexitsolutions.com, call +254 743 793 901, or visit our office at Pioneer House, 2nd Floor, Oginga Street, Kisumu."
		}
		return &ChatResult{Response: escalateMsg, Intent: string(intent), Confidence: confidence, Model: "template"}, nil
	case ai.IntentSimpleFAQ:
		// Greetings and very simple FAQs get a warm template without burning
		// an LLM call + embedding. The agent is overkill for "hi".
		q := strings.ToLower(strings.TrimSpace(input.Question))
		if isGreeting(q) {
			var resp string
			if isFLP {
				resp = "Hello! Welcome to Forever Living Products. I can help you with product info, the business opportunity, or anything else about Forever. What would you like to know?"
			} else {
				resp = "Hello! I'm Vera, your Codevertex AI assistant. I can help with our IT services, Digitika Academy courses, or the Power Suite platform. What would you like to know?"
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
	// Only engage the expensive agentic loop when:
	//   - KB coverage is weak (topSim < 0.55) AND/OR live info needed
	//   - OR the intent is task_execute (content generation benefits from
	//     tool-calling: get_fbo_profile for links, kb_search for product
	//     details, check_integrations for campaign setup guidance)
	//
	// Public mode (widget on /p/{slug}): prefer cache → KB → single-shot LLM.
	// Skip agent loop entirely unless KB has zero coverage — keeps responses
	// fast and minimises Groq API cost for unauthenticated traffic.
	// Recruitment + task_execute ALWAYS use the agent — they need tools
	// (generate_links for join URL, get_fbo_profile for personalisation,
	// check_integrations for campaign setup). Product/business Q&A only
	// use the agent when KB coverage is weak or live info is needed.
	intentNeedsTools := intent == ai.IntentRecruitment || intent == ai.IntentTaskExecute || intent == ai.IntentBusinessOpp
	useAgent := p.agentEnabled && p.agentLoop != nil &&
		(intent == ai.IntentProductQA || intentNeedsTools) &&
		(needsLiveInfo || topSim < 0.55 || intentNeedsTools)
	if input.PublicMode {
		// Public mode: skip the agent for general knowledge Q&A (expensive),
		// but ALWAYS use it for action-oriented queries and recruitment.
		if !intentNeedsTools {
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
		// intentNeedsTools (recruitment, task_execute, business_opp) always
		// keeps useAgent=true so the agent can generate personalised links.
	}

	if useAgent {
		sys := p.buildAgentSystemPrompt(input.BusinessType, intent)
		agentInput := agent.Input{
			TenantID: input.TenantID, SystemPrompt: sys,
			History: input.History, Question: input.Question,
		}
		// task_execute needs more iterations for full tool chains
		// (check_integrations → kb_search → search_images → calculate_ad_cost → create_campaign).
		if intent == ai.IntentTaskExecute {
			agentInput.MaxIters = 8
		}
		ares, aerr := p.agentLoop.Run(ctx, agentInput)
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
	systemPrompt := p.buildSystemPrompt(input.BusinessType, intent, contextChunks)
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
	filtered, blocked, reason := p.filter.CheckForTenant(raw, input.BusinessType)
	if blocked {
		p.logger.Warn("response blocked by compliance filter",
			zap.String("reason", reason), zap.String("question", input.Question))
		if input.BusinessType == "flp" {
			filtered = "I appreciate your question! For the most accurate and up-to-date information, please visit foreverliving.com or speak directly with your Forever Business Owner."
		} else {
			filtered = "I appreciate your question! For the most accurate information about Codevertex services, please visit codevertexitsolutions.com or contact our team at info@codevertexitsolutions.com."
		}
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

	systemPrompt := p.buildSystemPrompt(input.BusinessType, intent, contextChunks)
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

// buildSystemPrompt constructs the system prompt with RAG context.
// For FLP tenants (businessType="flp") it includes FLP compliance rules.
// For all other tenants it uses the Codevertex/generic Vera persona.
func (p *Pipeline) buildSystemPrompt(businessType string, intent ai.Intent, chunks []SearchResult) string {
	if businessType != "flp" {
		return p.buildGenericSystemPrompt(intent, chunks)
	}

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
- Rank ladder (EXACT, do NOT invent ranks): Assistant Supervisor → Supervisor → Assistant Manager → Manager → Senior Manager → Soaring Manager → Sapphire Manager → Diamond Sapphire Manager → Diamond Manager → Eagle Manager
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

// buildGenericSystemPrompt returns the Codevertex/Vera system prompt for non-FLP tenants.
func (p *Pipeline) buildGenericSystemPrompt(intent ai.Intent, chunks []SearchResult) string {
	var b strings.Builder

	b.WriteString(`You are Vera, the AI support assistant for Codevertex IT Solutions — a premier technology firm headquartered in Kisumu, Kenya, established in 2020.

COMPANY:
- Full name: Codevertex IT Solutions
- HQ: Pioneer House, 2nd Floor, Oginga Street, Kisumu, Kenya
- Phone: +254 743 793 901 | +254 792 548 766
- Email: info@codevertexitsolutions.com
- Website: https://codevertexitsolutions.com
- Client Portal (SSO): https://accounts.codevertexitsolutions.com

SERVICES:
1. Enterprise Software — full-stack platforms, mobile apps, ERP, CRM, workflow automation
2. AI & Analytics — predictive modelling, NLP chatbots, BI dashboards (Apache Superset)
3. Cloud Infrastructure — managed cloud, AES-256 encryption, domain management
4. Network & Hardware — TruLoad (IoT axle-load compliance), ISP billing, network automation
5. Digitika Academy — ICDL, CCNA v7, AI/ML courses, Software Engineering, Data Analytics

POWER SUITE PRODUCTS:
- ERP: https://erp.codevertexitsolutions.com
- POS: https://pos.codevertexitsolutions.com
- Books (Treasury): https://books.codevertexitsolutions.com
- Analytics: https://superset.codevertexitsolutions.com
- MarketFlow CRM: https://marketflow.codevertexitsolutions.com
- ISP Billing: https://ispbilling.codevertexitsolutions.com
- TruLoad: https://truload.codevertexitsolutions.com
- Notifications: https://notifications.codevertexitsolutions.com
- Ordering: https://ordersapp.codevertexitsolutions.com
- Logistics: https://logistics.codevertexitsolutions.com

DIGITIKA COURSES (KES):
Code-Starter Bootcamp: 30,000 (10 weeks, hybrid Kisumu + Zoom)
Full-Stack Web Dev: 45,000 | Mobile Dev: 38,000 | DevOps & Cloud: 42,000 | Cybersecurity: 28,000
ICDL Core: 12,000 | Advanced: 18,000 | Professional: 24,000 | Digital Citizen: 6,500
CCNA v7 Part 1–3: 22,000 each | Exam Prep: 15,000
AI Fundamentals: 14,000 | ML with Python: 32,000 | GenAI/LLM: 38,000 | AI for Business: 9,000
Data Python: 28,000 | Power BI: 16,000 | SQL: 12,000 | Advanced Analytics: 22,000

PAYMENTS: M-Pesa and card via Treasury Gateway at https://books.codevertexitsolutions.com
PRICING PLANS (KES/month): Starter 4,999 | Growth 14,999 | Enterprise: Custom

PERSONA RULES:
- Warm and professional, like a knowledgeable Kenyan tech consultant
- Keep responses concise (2–4 sentences unless listing items)
- Always end with a helpful next step or a call to action
- Never make up information; offer to connect with the team if unsure
`)

	switch intent {
	case ai.IntentProductQA:
		b.WriteString(`
When answering about products or services, mention the specific platform URL, pricing, and how to get started. If the user asks about a course, include the KES price and Digitika Academy contact.
`)
	case ai.IntentBusinessOpp, ai.IntentRecruitment:
		b.WriteString(`
Focus on Codevertex service capabilities, integration options, and how the Power Suite can solve the user's business problem. Mention relevant case studies or the Digitika Academy if training is relevant.
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
// For non-FLP tenants (businessType != "flp") it uses the Codevertex/Vera persona.
func (p *Pipeline) buildAgentSystemPrompt(businessType string, intent ai.Intent) string {
	if businessType != "flp" {
		return p.buildGenericAgentSystemPrompt(intent)
	}

	var b strings.Builder
	b.WriteString(`You are an agentic marketing assistant for a business owner on the MarketFlow platform. You help with Forever Living Products (FLP) and any other business use case on the platform.

TOOLS AVAILABLE:
Knowledge & Search:
- kb_search: semantic search over the tenant's knowledge base (products, FAQ, business). ALWAYS try this first.
- get_product: look up a specific product by code or name (e.g. "015", "C9").
- get_faq: fetch FAQ entries by keyword.
- get_compliance_rules: FDA/FLP compliance rules. Call before drafting medical/income content.
- web_search: live web search (Google/Tavily). Use only when KB doesn't cover the topic.

Profile & Links:
- get_fbo_profile: fetch the business owner's profile (name, distribID, links, WhatsApp). Use to personalise recommendations.
- generate_links: create distribID-embedded product links, shop links, recruit links, WhatsApp links, profile URLs.

Marketing Workflows:
- check_integrations: verify which ad platforms (Meta, Google Ads, TikTok, LinkedIn) and scheduling tools (Calendar, Cal.com) the tenant has connected.
- book_meeting: schedule a meeting on the tenant's Google Calendar.
- create_campaign: create a campaign on marketflow-api (name, platform, budget, audience, duration).
- create_funnel: create a landing page funnel at /f/{slug} with form fields, WhatsApp block, product block.
- calculate_ad_cost: compute the total cost (ads budget + service fee) before the user pays.

RULES:
- Call tools iteratively; reason step by step.
- Stop calling tools once you have enough information; respond in clear, well-formatted markdown.
- When the user asks to SET UP a campaign:
  1. check_integrations — verify the target platform is connected
  2. get_fbo_profile — get distribID for product links in the funnel
  3. Ask for: target audience, daily budget, duration, campaign focus
  4. calculate_ad_cost — show the total cost (ads + service fee, single wallet payment)
  5. Confirm with the user before proceeding
  6. create_funnel (if needed) → create_campaign
- The tenant pays ONCE from their wallet — MarketFlow handles paying the ad platform. Never tell the user they need to pay Meta/Google separately.
- When generating product recommendations, ALWAYS include distribID-embedded product links.
- NEVER make medical claims (cures, treats, heals, prevents disease). Use supports/promotes/helps maintain.
- NEVER guarantee income. Results vary. Include income disclaimer when relevant.
- Format responses with markdown: use **bold** for key terms, bullet points for lists, and headers for sections.
- When discussing products, use search_images to find a relevant product photo and include it in your response as a markdown image: ![description](url). This makes responses visually appealing.
- Include product links using generate_links when recommending specific products.
- Keep responses comprehensive but concise — aim for 150-300 words with images and links embedded.
`)
	switch intent {
	case ai.IntentBusinessOpp, ai.IntentRecruitment:
		b.WriteString("\nFocus on the business opportunity. Mention the 3 income streams, rank ladder, and Forever2Drive. Always include an income disclaimer.\n")
	case ai.IntentProductQA:
		b.WriteString("\nFocus on product information, certifications (IASC, USDA/EU Organic), and relevant starter offers like Clean 9.\n")
	case ai.IntentTaskExecute:
		b.WriteString(`
TOOL CHAINING FOR AD/CAMPAIGN/CONTENT CREATION:
When the user asks to create an ad, campaign, or marketing content, follow this tool chain:
1. FIRST call check_integrations to verify which ad platforms are connected.
2. Call kb_search to get product details (name, benefits, product code) for the product mentioned.
3. Call search_images to find relevant product images to include in the ad.
4. Call calculate_ad_cost to show the budget breakdown (ads spend + service fee = single wallet payment).
5. Present the ad copy, images, and cost breakdown to the user in a clear summary.
6. Ask if they want to proceed with creating the campaign via create_campaign.

If an ad platform is not connected, guide the user to /settings/integrations to connect it before proceeding.
If wallet balance is insufficient, guide the user to /settings/billing to top up their wallet.
Do NOT skip straight to text generation — always gather context and verify integrations first.
`)
	}
	return b.String()
}

// buildGenericAgentSystemPrompt returns the agent system prompt for non-FLP (Codevertex/generic) tenants.
func (p *Pipeline) buildGenericAgentSystemPrompt(intent ai.Intent) string {
	var b strings.Builder
	b.WriteString(`You are Vera, an agentic AI assistant for Codevertex IT Solutions. You help website visitors and clients with IT services, the Power Suite platform, Digitika Academy courses, and technical enquiries.

TOOLS AVAILABLE:
Knowledge & Search:
- kb_search: semantic search over the Codevertex knowledge base (services, courses, products, FAQ). ALWAYS try this first.
- web_search: live web search. Use only when KB doesn't cover the topic.
- query_service_data: fetch live data from Codevertex microservices (enrollments, courses, invoices, orders). Use when user asks about specific records.

Profile & Links:
- get_fbo_profile: fetch the tenant profile (company name, contact, links). Use to personalise responses.
- generate_links: create deep links to Power Suite products, Digitika enrollment pages, or support resources.

Marketing Workflows:
- check_integrations: verify which ad platforms and scheduling tools the tenant has connected.
- book_meeting: schedule a meeting on the tenant's Google Calendar.
- create_campaign: create a lead-generation campaign on marketflow-api.
- create_funnel: create a landing page funnel for service enquiries or course signups.
- calculate_ad_cost: compute ad budget + service fee before the client commits.

RULES:
- Call tools iteratively; reason step by step.
- Stop calling tools once you have enough information; respond in clear, well-formatted markdown.
- When recommending a Digitika course, always mention the KES price and payment method (M-Pesa / card via books.codevertexitsolutions.com).
- When discussing Power Suite products, include the relevant URL (e.g. pos.codevertexitsolutions.com).
- For technical or custom project enquiries, suggest contacting info@codevertexitsolutions.com or +254 743 793 901.
- Format responses with markdown: **bold** for key terms, bullet points for lists.
- Keep responses comprehensive but concise — aim for 150–300 words.
`)
	switch intent {
	case ai.IntentProductQA:
		b.WriteString("\nFocus on the specific Codevertex product or service. Include URL, pricing, and a clear next step.\n")
	case ai.IntentBusinessOpp, ai.IntentRecruitment:
		b.WriteString("\nFocus on Codevertex service capabilities and how the Power Suite or Digitika Academy can solve the prospect's business problem.\n")
	case ai.IntentTaskExecute:
		b.WriteString(`
TOOL CHAINING FOR CAMPAIGN/CONTENT CREATION:
1. FIRST call check_integrations to verify which ad platforms are connected.
2. Call kb_search to get relevant service or course details.
3. Call calculate_ad_cost to show the budget breakdown.
4. Present the campaign plan and cost to the user; confirm before proceeding.
5. create_funnel (if needed) → create_campaign.
`)
	}
	return b.String()
}

// cacheKey kept for back-compat with ChatStream; delegates to cache layer.
func (p *Pipeline) cacheKey(tenantID uuid.UUID, question string) string {
	return p.cache.exactKey(tenantID, question)
}

func isGreeting(q string) bool {
	greetings := []string{
		"hi", "hello", "hey", "good morning", "good afternoon",
		"good evening", "thanks", "thank you", "bye", "goodbye",
		"hi!", "hello!", "hey!", "thanks!", "thank you!",
		"hi.", "hello.", "hey.", "sup", "yo", "howdy",
	}
	for _, g := range greetings {
		if q == g {
			return true
		}
	}
	return false
}
