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
	// The entry's own text comes back only when it is short enough to be a direct
	// answer (see kbanswer.go), so long documents never ride along.
	query := `
		SELECT e.entry_id, e.chunk_text, e.category, e.title, e.chunk_index,
		       CASE WHEN length(kb.content) <= $4 THEN kb.content ELSE '' END,
		       1 - (e.embedding <=> $1::vector) AS similarity
		FROM knowledge_base_embeddings e
		JOIN knowledge_base_entries kb ON kb.id = e.entry_id
		WHERE kb.scope = 'platform' OR kb.tenant_id = $2
		ORDER BY e.embedding <=> $1::vector
		LIMIT $3`

	rows, err := p.db.QueryContext(ctx, query, vectorStr, tenantID, topK, maxDirectAnswerLen)
	if err != nil {
		return nil, fmt.Errorf("pgvector search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.EntryID, &r.ChunkText, &r.Category, &r.Title, &r.ChunkIndex, &r.Content, &r.Similarity); err != nil {
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
		return FallbackReply(businessType)
	}
	return filtered
}

// HistoryText is the assistant text to store in a conversation: the checked answer before the
// compliance filter (so later turns do not repeat disclaimers), unless the answer was replaced by
// the fallback, in which case the fallback is stored and the blocked draft never returns as context.
func HistoryText(result *ChatResult) string {
	if result == nil {
		return ""
	}
	if result.RawResponse == "" || IsFallback(result.Response) {
		return result.Response
	}
	return result.RawResponse
}

// IsFallback reports whether a reply is one of the fallback replies (the answer was blocked or
// needed facts the assistant did not have).
func IsFallback(reply string) bool {
	for _, bt := range []string{"flp", "codevertex", "cafe", ""} {
		if reply == FallbackReply(bt) {
			return true
		}
	}
	return false
}

// FallbackReply is the safe answer when a reply cannot be sent: it points to the business itself.
func FallbackReply(businessType string) string {
	switch businessType {
	case "flp":
		return "I appreciate your question! For the most accurate and up-to-date information, please visit foreverliving.com or speak directly with your Forever Business Owner."
	case "codevertex":
		return "I appreciate your question! For the most accurate information about Codevertex Africa Limited services, please visit codevertexafrica.com or contact our team at info@codevertexafrica.com."
	case "cafe":
		return "I appreciate your question! For the most accurate information, please contact the cafe directly or visit us in person."
	default:
		return "I appreciate your question! Please contact the business directly for the most accurate and up-to-date information."
	}
}

// filterAndCache writes the checked (pre-compliance-filter) response to the L1 and L2 caches of
// the request's audience, so future hits get the filter for their own businessType, and returns
// the compliance-filtered response. Nothing is cached when the request has history or is a form
// turn (session-specific), or when cacheable is false: an agent answer that read tools or outside
// text is never reused for someone else.
func (p *Pipeline) filterAndCache(ctx context.Context, input ChatInput, raw string, emb []float32, cacheable bool) string {
	if cacheable && len(input.History) == 0 && !input.formTurn {
		aud := audienceOf(input)
		p.cache.setExact(ctx, input.TenantID, aud, input.Question, raw)
		p.cache.setSemantic(ctx, input.TenantID, aud, input.Question, emb, raw)
	}
	return p.applyFilter(raw, input.BusinessType)
}
