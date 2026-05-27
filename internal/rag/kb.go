package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

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

// searchPgVector queries the knowledge_base_embeddings for similar chunks.
func (p *Pipeline) searchPgVector(ctx context.Context, tenantID uuid.UUID, queryEmbedding []float32, topK int) ([]SearchResult, error) {
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

// applyFilter runs the compliance filter for the given businessType and returns the
// filtered response. Blocked responses are replaced with a safe fallback message.
func (p *Pipeline) applyFilter(raw, businessType string) string {
	filtered, blocked, reason := p.filter.CheckForTenant(raw, businessType)
	if blocked {
		p.logger.Warn("response blocked by compliance filter", zap.String("reason", reason))
		switch businessType {
		case "flp":
			return "I appreciate your question! For the most accurate and up-to-date information, please visit foreverliving.com or speak directly with your Forever Business Owner."
		case "codevertex":
			return "I appreciate your question! For the most accurate information about Codevertex Africa Limited services, please visit codevertexitsolutions.com or contact our team at info@codevertexitsolutions.com."
		case "cafe":
			return "I appreciate your question! For the most accurate information, please contact the cafe directly or visit us in person."
		default:
			return "I appreciate your question! Please contact the business directly for the most accurate and up-to-date information."
		}
	}
	return filtered
}

// filterAndCache writes the RAW (pre-filter) response to L1 + L2 caches so that
// future cache hits receive the correct filter for their own businessType. Skips
// cache writes when the request has conversation history (session-specific responses
// must not be recycled). Returns the compliance-filtered response for this request.
func (p *Pipeline) filterAndCache(ctx context.Context, input ChatInput, raw string, emb []float32) string {
	if len(input.History) == 0 {
		p.cache.setExact(ctx, input.TenantID, input.Question, raw)
		p.cache.setSemantic(ctx, input.TenantID, input.Question, emb, raw)
	}
	return p.applyFilter(raw, input.BusinessType)
}

// cacheKey kept for back-compat with ChatStream; delegates to cache layer.
func (p *Pipeline) cacheKey(tenantID uuid.UUID, question string) string {
	return p.cache.exactKey(tenantID, question)
}
