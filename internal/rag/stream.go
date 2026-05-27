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

	rawCh, err := p.groqClient.ChatStream(ctx, ai.ChatRequest{
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
