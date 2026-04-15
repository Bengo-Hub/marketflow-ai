// Package tools implements the concrete agent.Tool set for MarketFlow RAG.
package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// KBSearcher runs a pgvector tenant-scoped similarity search over the knowledge base.
type KBSearcher struct {
	db             *sql.DB
	ollamaClient   *ai.Client
	embeddingModel string
	logger         *zap.Logger
}

// NewKBSearcher constructs a KBSearcher.
func NewKBSearcher(db *sql.DB, ollama *ai.Client, embeddingModel string, logger *zap.Logger) *KBSearcher {
	return &KBSearcher{db: db, ollamaClient: ollama, embeddingModel: embeddingModel, logger: logger.Named("tool.kb_search")}
}

func (*KBSearcher) Name() string { return "kb_search" }

func (*KBSearcher) Description() string {
	return "Searches the tenant's internal knowledge base (products, FAQ, business playbook, compliance rules) by semantic similarity. Use this FIRST before any external lookup. Returns up to topK relevant chunks with category and title."
}

func (*KBSearcher) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Natural-language search query"},
			"topK":  map[string]any{"type": "integer", "description": "Number of chunks to return (1-10)", "default": 5},
			"category": map[string]any{
				"type":        "string",
				"description": "Optional filter: product | faq | business | compliance",
			},
		},
		"required": []string{"query"},
	}
}

func (t *KBSearcher) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		Query    string `json:"query"`
		TopK     int    `json:"topK"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.TopK <= 0 || args.TopK > 10 {
		args.TopK = 5
	}
	if t.ollamaClient == nil {
		return "", fmt.Errorf("ollama client not configured")
	}

	embedding, err := t.ollamaClient.Embed(ctx, t.embeddingModel, args.Query)
	if err != nil {
		return "", fmt.Errorf("embed: %w", err)
	}
	parts := make([]string, len(embedding))
	for i, v := range embedding {
		parts[i] = fmt.Sprintf("%f", v)
	}
	vectorStr := "[" + strings.Join(parts, ",") + "]"

	query := `
		SELECT e.chunk_text, e.category, e.title,
		       1 - (e.embedding <=> $1::vector) AS similarity
		FROM knowledge_base_embeddings e
		JOIN knowledge_base_entries kb ON kb.id = e.entry_id
		WHERE kb.tenant_id = $2
		  AND ($3 = '' OR e.category = $3)
		ORDER BY e.embedding <=> $1::vector
		LIMIT $4`

	rows, err := t.db.QueryContext(ctx, query, vectorStr, tenantID, args.Category, args.TopK)
	if err != nil {
		return "", fmt.Errorf("pgvector search: %w", err)
	}
	defer rows.Close()

	type chunk struct {
		Category   string  `json:"category"`
		Title      string  `json:"title"`
		Text       string  `json:"text"`
		Similarity float64 `json:"similarity"`
	}
	results := make([]chunk, 0, args.TopK)
	for rows.Next() {
		var c chunk
		if err := rows.Scan(&c.Text, &c.Category, &c.Title, &c.Similarity); err != nil {
			continue
		}
		results = append(results, c)
	}
	out, _ := json.Marshal(map[string]any{"results": results, "count": len(results)})
	return string(out), nil
}
