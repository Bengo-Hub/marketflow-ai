package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"go.uber.org/zap"
)

// ChatStream processes a user question and returns a streaming channel.
func (p *Pipeline) ChatStream(ctx context.Context, input ChatInput) (<-chan ai.StreamDelta, *ChatResult, error) {
	// Jailbreak guard — same as Chat()
	if blocked, resp := ScreenInput(input.Question); blocked {
		p.logger.Info("jailbreak attempt blocked (stream)", zap.String("tenant", input.TenantSlug))
		ch := make(chan ai.StreamDelta, 2)
		go func() {
			ch <- ai.StreamDelta{Content: resp}
			ch <- ai.StreamDelta{Done: true}
			close(ch)
		}()
		return ch, &ChatResult{Intent: "blocked", Confidence: 1.0, Model: "guard"}, nil
	}

	// Classify intent
	intent, confidence, _ := p.router.ClassifyIntent(ctx, input.Question)

	// Handle non-LLM intents
	switch intent {
	case ai.IntentOffTopic, ai.IntentEscalate:
		result, _ := p.Chat(ctx, input)
		ch := make(chan ai.StreamDelta, 2)
		go func() {
			ch <- ai.StreamDelta{Content: result.Response}
			ch <- ai.StreamDelta{Done: true}
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

	// ── LLM fallback ladder — mirrors Chat()'s tiers so streaming gets the
	// same DB-resolved client and the same resilience against a stale/rate
	// limited provider. Unlike Chat()'s per-tier timeout contexts, these calls
	// use ctx directly: ChatStream's response body is read by a goroutine that
	// outlives this function call, so a deferred cancel here would tear down
	// an in-flight stream instead of just bounding a synchronous request.
	client, model := p.resolveClient(ctx)
	rawCh, err := client.ChatStream(ctx, ai.ChatRequest{
		Model: model, Messages: messages, Temperature: 0.7, MaxTokens: 2048,
	})

	// Tier 2: Groq fallback model
	if err != nil {
		p.logger.Warn("Groq main stream failed, trying Groq fallback model",
			zap.String("model", model), zap.Error(err))
		model = p.fallbackModel
		rawCh, err = p.groqClient.ChatStream(ctx, ai.ChatRequest{
			Model: model, Messages: messages, Temperature: 0.7, MaxTokens: 1024,
		})
	}

	// Tier 3: Ollama local LLM — cluster-internal, no external quota
	if err != nil && p.ollamaLLMClient != nil {
		p.logger.Warn("Groq fallback stream failed, trying Ollama local LLM",
			zap.String("model", p.ollamaLLMModel), zap.Error(err))
		model = p.ollamaLLMModel
		rawCh, err = p.ollamaLLMClient.ChatStream(ctx, ai.ChatRequest{
			Model: model, Messages: messages, Temperature: 0.7, MaxTokens: 1024,
		})
		if err != nil {
			p.logger.Warn("Ollama local LLM stream failed", zap.Error(err))
		}
	}

	// Tier 4: OpenAI — skip fast if quota/auth error
	if err != nil && p.openaiClient != nil {
		p.logger.Warn("Ollama stream failed, trying OpenAI fallback",
			zap.String("model", p.openaiModel), zap.Error(err))
		model = p.openaiModel
		rawCh, err = p.openaiClient.ChatStream(ctx, ai.ChatRequest{
			Model: model, Messages: messages, Temperature: 0.7, MaxTokens: 1024,
		})
		if err != nil {
			p.logger.Warn("OpenAI fallback stream failed (quota/credits?)", zap.Error(err))
		}
	}

	// Tier 5: Anthropic Claude — last resort
	if err != nil && p.claudeClient != nil {
		claudeModel := p.claudeModel
		if claudeModel == "" {
			claudeModel = "claude-haiku-4-5-20251001"
		}
		p.logger.Warn("OpenAI stream failed, trying Claude last resort",
			zap.String("model", claudeModel), zap.Error(err))
		rawCh, err = p.claudeClient.ChatStream(ctx, ai.ChatRequest{
			Model: claudeModel, Messages: messages, Temperature: 0.7, MaxTokens: 1024,
		})
		if err == nil {
			model = claudeModel
		} else {
			p.logger.Warn("Claude stream last resort failed (credits?)", zap.Error(err))
		}
	}

	if err != nil {
		return nil, nil, fmt.Errorf("all LLM providers exhausted (stream): %w", err)
	}

	meta := &ChatResult{
		Intent:     string(intent),
		Confidence: confidence,
		Sources:    len(contextChunks),
		Model:      model,
	}

	return guardStream(rawCh), meta, nil
}

// guardStream wraps an upstream delta channel so that ScreenOutput is applied
// to the fully-accumulated response. Tokens are forwarded in real-time; if the
// completed text is detected as a jailbreak or schema leak, a replace delta is
// injected before the Done sentinel so the widget can overwrite the bubble.
func guardStream(upstream <-chan ai.StreamDelta) <-chan ai.StreamDelta {
	out := make(chan ai.StreamDelta, 32)
	go func() {
		defer close(out)
		var buf strings.Builder
		for delta := range upstream {
			if delta.Done {
				screened := ScreenOutput(buf.String())
				if screened != buf.String() {
					out <- ai.StreamDelta{Type: "replace", Content: screened}
				}
				out <- ai.StreamDelta{Done: true}
				return
			}
			if delta.Content != "" {
				buf.WriteString(delta.Content)
				out <- delta
			}
		}
		// upstream closed without Done — forward a Done so the handler terminates
		out <- ai.StreamDelta{Done: true}
	}()
	return out
}
