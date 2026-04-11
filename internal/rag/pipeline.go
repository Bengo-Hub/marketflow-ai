package rag

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	embeddingModel  string
	mainModel       string
	fallbackModel   string
	embeddingDim    int
	redisNamespace  string
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
	EmbeddingModel  string
	MainModel       string
	FallbackModel   string
	EmbeddingDim    int
	RedisNamespace  string
	Logger          *zap.Logger
}

// NewPipeline creates a new RAG pipeline.
func NewPipeline(cfg Config) *Pipeline {
	return &Pipeline{
		db:             cfg.DB,
		redis:          cfg.Redis,
		groqClient:     cfg.GroqClient,
		ollamaClient:   cfg.OllamaClient,
		router:         cfg.Router,
		filter:         compliance.NewFilter(),
		resolver:       cfg.Resolver,
		sessionService: cfg.SessionService,
		taskExecutor:   cfg.TaskExecutor,
		embeddingModel: cfg.EmbeddingModel,
		mainModel:      cfg.MainModel,
		fallbackModel:  cfg.FallbackModel,
		embeddingDim:   cfg.EmbeddingDim,
		redisNamespace: cfg.RedisNamespace,
		logger:         cfg.Logger.Named("rag"),
	}
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

// Chat processes a user question through the full RAG pipeline.
func (p *Pipeline) Chat(ctx context.Context, input ChatInput) (*ChatResult, error) {
	// Step 1: Check exact-match cache
	cacheKey := p.cacheKey(input.TenantID, input.Question)
	if cached, err := p.redis.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
		p.logger.Debug("cache hit", zap.String("key", cacheKey))
		return &ChatResult{Response: cached, Cached: true, Intent: "cached"}, nil
	}

	// Step 2: Classify intent via fast router model
	intent, confidence, err := p.router.ClassifyIntent(ctx, input.Question)
	if err != nil {
		p.logger.Warn("classification failed, defaulting to product_qa", zap.Error(err))
		intent = ai.IntentProductQA
		confidence = 0.5
	}

	// Step 3: Handle intents that don't need standard RAG
	switch intent {
	case ai.IntentOffTopic:
		resp := "I'm here to help with Forever Living Products and business opportunities. Could you ask me about our products, wellness tips, or the FBO business?"
		return &ChatResult{Response: resp, Intent: string(intent), Confidence: confidence, Model: "template"}, nil
	case ai.IntentEscalate:
		resp := "I'd be happy to connect you with your FBO directly. You can reach them via WhatsApp for a personal conversation, or book a virtual meeting."
		return &ChatResult{Response: resp, Intent: string(intent), Confidence: confidence, Model: "template"}, nil
	case ai.IntentTaskExecute:
		// Route to task executor for content generation
		if p.taskExecutor != nil {
			// Get RAG context for product-aware task generation
			var ragCtx string
			if p.ollamaClient != nil {
				if emb, err := p.ollamaClient.Embed(ctx, p.embeddingModel, input.Question); err == nil {
					if chunks, err := p.searchPgVector(ctx, input.TenantID, emb, 3); err == nil {
						for _, c := range chunks {
							ragCtx += fmt.Sprintf("[%s] %s: %s\n", c.Category, c.Title, c.ChunkText)
						}
					}
				}
			}

			// Resolve Groq client (per-tenant or fallback)
			client, model := p.resolveClient(ctx)
			result, err := p.taskExecutor.Execute(ctx, client, model, input.Question, ragCtx)
			if err != nil {
				return nil, fmt.Errorf("task execution failed: %w", err)
			}
			// Convert task result to chat result
			respJSON, _ := json.Marshal(result)
			return &ChatResult{
				Response:   string(respJSON),
				Intent:     "task_execute",
				Confidence: confidence,
				Model:      result.Model,
			}, nil
		}
	}

	// Step 4: Embed question and search pgvector for relevant KB chunks
	var context_chunks []SearchResult
	if p.ollamaClient != nil {
		embedding, err := p.ollamaClient.Embed(ctx, p.embeddingModel, input.Question)
		if err != nil {
			p.logger.Warn("embedding failed, continuing without RAG context", zap.Error(err))
		} else {
			context_chunks, err = p.searchPgVector(ctx, input.TenantID, embedding, 5)
			if err != nil {
				p.logger.Warn("pgvector search failed", zap.Error(err))
			}
		}
	}

	// Step 5: Build prompt with RAG context
	systemPrompt := p.buildSystemPrompt(intent, context_chunks)
	messages := make([]ai.Message, 0, len(input.History)+2)
	messages = append(messages, ai.Message{Role: "system", Content: systemPrompt})
	// Include last 6 messages of history for context
	historyStart := 0
	if len(input.History) > 6 {
		historyStart = len(input.History) - 6
	}
	messages = append(messages, input.History[historyStart:]...)
	messages = append(messages, ai.Message{Role: "user", Content: input.Question})

	// Step 6: Call Groq LLM
	model := p.mainModel
	resp, err := p.groqClient.Chat(ctx, ai.ChatRequest{
		Model:       model,
		Messages:    messages,
		Temperature: 0.7,
		MaxTokens:   2048,
	})
	if err != nil {
		// Fallback to smaller model
		p.logger.Warn("main model failed, trying fallback", zap.Error(err))
		model = p.fallbackModel
		resp, err = p.groqClient.Chat(ctx, ai.ChatRequest{
			Model:       model,
			Messages:    messages,
			Temperature: 0.7,
			MaxTokens:   1024,
		})
		if err != nil {
			return nil, fmt.Errorf("all LLM models failed: %w", err)
		}
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("empty response from LLM")
	}

	aiResponse := resp.Choices[0].Message.Content

	// Step 7: Compliance filter
	filtered, blocked, reason := p.filter.Check(aiResponse)
	if blocked {
		p.logger.Warn("response blocked by compliance filter",
			zap.String("reason", reason),
			zap.String("question", input.Question),
		)
		filtered = "I appreciate your question! For the most accurate and up-to-date information, I recommend visiting foreverliving.com or speaking directly with your Forever Business Owner."
	}

	// Step 8: Cache the response (24h TTL)
	_ = p.redis.Set(ctx, cacheKey, filtered, 24*time.Hour).Err()

	return &ChatResult{
		Response:   filtered,
		Intent:     string(intent),
		Confidence: confidence,
		Sources:    len(context_chunks),
		Cached:     false,
		Model:      model,
	}, nil
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

	query := `
		SELECT e.id, e.chunk_text, e.category, e.title,
		       1 - (e.embedding <=> $1::vector) AS similarity
		FROM knowledge_base_embeddings e
		JOIN knowledge_base_entries kb ON kb.id = e.entry_id
		WHERE kb.tenant_id = $2
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

func (p *Pipeline) cacheKey(tenantID uuid.UUID, question string) string {
	h := sha256.Sum256([]byte(tenantID.String() + ":" + strings.ToLower(strings.TrimSpace(question))))
	return p.redisNamespace + ":chat:exact:" + hex.EncodeToString(h[:16])
}
