package rag

import (
	"context"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/llm"
	"go.uber.org/zap"
)

// ChatStream processes a user question and returns a streaming channel. It takes
// the same cheap paths as Chat before any model: guards, templates, the answer
// caches and a knowledge-base entry that already answers the question. The
// public widget streams from the light class (free models first). When every
// model fails, or one ends without text, the closest knowledge-base entry (or
// the contact options) answers, so the visitor never gets an empty reply.
func (p *Pipeline) ChatStream(ctx context.Context, input ChatInput) (<-chan ai.StreamDelta, *ChatResult, error) {
	// Jailbreak guard — same as Chat()
	if blocked, resp := ScreenInput(input.Question); blocked {
		p.logger.Info("jailbreak attempt blocked (stream)", zap.String("tenant", input.TenantSlug))
		return staticStream(resp), &ChatResult{Intent: "blocked", Confidence: 1.0, Model: "guard"}, nil
	}
	if input.BusinessType == "" {
		input.BusinessType = detectNiche(input.Question)
	}
	// Scope guard and template intents are answered by Chat without a model.
	if scopeGuard(input.BusinessType, input.Question) {
		result, err := p.Chat(ctx, input)
		if err != nil {
			return nil, nil, err
		}
		return staticStream(result.Response), result, nil
	}

	intent, confidence, _ := p.classify(ctx, input.Question)
	q := strings.ToLower(strings.TrimSpace(input.Question))
	if intent == ai.IntentOffTopic || intent == ai.IntentEscalate || (intent == ai.IntentSimpleFAQ && isGreeting(q)) {
		result, err := p.Chat(ctx, input)
		if err != nil {
			return nil, nil, err
		}
		return staticStream(result.Response), result, nil
	}

	skipCache := len(input.History) > 0
	if !skipCache {
		if resp, ok := p.cache.getExact(ctx, input.TenantID, input.Question); ok {
			return staticStream(p.applyFilter(resp, input.BusinessType)),
				&ChatResult{Intent: "cached", Cached: true, Model: "cache"}, nil
		}
	}

	embedding := p.cachedEmbed(ctx, input.Question)
	if !skipCache && embedding != nil {
		if resp, ok := p.cache.getSemantic(ctx, input.TenantID, embedding); ok {
			return staticStream(p.applyFilter(resp, input.BusinessType)),
				&ChatResult{Intent: string(intent), Confidence: confidence, Cached: true, Model: "semantic_cache"}, nil
		}
	}

	var contextChunks []SearchResult
	if embedding != nil {
		var err error
		if contextChunks, err = p.cachedKBSearch(ctx, input.TenantID, embedding, 5); err != nil {
			p.logger.Warn("KB search failed (stream)", zap.Error(err))
		}
	}
	if answer, ok := p.kbDirect(ctx, input, embedding, contextChunks); ok {
		return staticStream(p.applyFilter(answer, input.BusinessType)),
			&ChatResult{Intent: string(intent), Confidence: confidence, Sources: 1, Model: ModelKnowledgeBase}, nil
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

	// The router opens the stream on the first healthy target of the class. No
	// per-target timeout: the stream outlives this call.
	rawCh, call, err := p.llm.Stream(ctx, answerClass(input), ai.ChatRequest{
		Messages: messages, Temperature: 0.7, MaxTokens: 2048,
	})
	if err != nil {
		p.logger.Warn("all models failed (stream); answering from the knowledge base", zap.Error(err))
		return staticStream(p.applyFilter(kbFallback(input, contextChunks), input.BusinessType)),
			&ChatResult{Intent: string(intent), Confidence: confidence, Sources: len(contextChunks), Model: ModelKBFallback}, nil
	}

	meta := &ChatResult{
		Intent:     string(intent),
		Confidence: confidence,
		Sources:    len(contextChunks),
		Model:      call.Model,
	}

	// Detached: finalize runs when the stream ends and only writes the caches.
	cacheCtx := context.WithoutCancel(ctx)
	return guardStream(rawCh, func(text string) string {
		if strings.TrimSpace(text) == "" {
			p.logger.Warn("model stream ended without text; answering from the knowledge base", zap.String("model", call.Model))
			return p.applyFilter(kbFallback(input, contextChunks), input.BusinessType)
		}
		// Same output rules as the non-streaming path: jailbreak and schema-leak screen,
		// then the tenant's compliance filter (blocked claims, substitutions, disclaimers).
		return p.filterAndCache(cacheCtx, input, ScreenOutput(text), embedding)
	}), meta, nil
}

// answerClass is the model class for a chat answer: the light class for the
// public widget, standard for signed-in users.
func answerClass(input ChatInput) llm.Class {
	if input.PublicMode {
		return llm.Nano
	}
	return llm.Standard
}

// guardStream wraps an upstream delta channel so that finalize (output screen plus
// compliance filter) is applied to the fully-accumulated response. Tokens are forwarded in
// real-time; if the finalized text differs (a blocked claim, a substituted word, an added
// disclaimer, a jailbreak), a replace delta is injected before the Done sentinel so the
// widget overwrites the bubble with the compliant text.
func guardStream(upstream <-chan ai.StreamDelta, finalize func(string) string) <-chan ai.StreamDelta {
	out := make(chan ai.StreamDelta, 32)
	go func() {
		defer close(out)
		var buf strings.Builder
		for delta := range upstream {
			if delta.Done {
				if final := finalize(buf.String()); final != buf.String() {
					out <- ai.StreamDelta{Type: "replace", Content: final}
				}
				out <- ai.StreamDelta{Done: true}
				return
			}
			if delta.Content != "" {
				buf.WriteString(delta.Content)
				out <- delta
			}
		}
		// Upstream closed without Done: still finalize, so an abrupt provider close cannot
		// skip the compliance filter, then forward a Done so the handler terminates.
		if final := finalize(buf.String()); final != buf.String() {
			out <- ai.StreamDelta{Type: "replace", Content: final}
		}
		out <- ai.StreamDelta{Done: true}
	}()
	return out
}
