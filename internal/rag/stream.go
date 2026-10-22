package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/llm"
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

	// The router keeps the same provider order as Chat() and opens the stream on
	// the first healthy target. No per-target timeout: the stream outlives this call.
	rawCh, call, err := p.llm.Stream(ctx, llm.Standard, ai.ChatRequest{
		Messages: messages, Temperature: 0.7, MaxTokens: 2048,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("all LLM providers exhausted (stream): %w", err)
	}
	model := call.Model

	meta := &ChatResult{
		Intent:     string(intent),
		Confidence: confidence,
		Sources:    len(contextChunks),
		Model:      model,
	}

	businessType := input.BusinessType
	return guardStream(rawCh, func(text string) string {
		// Same output rules as the non-streaming path: jailbreak and schema-leak screen,
		// then the tenant's compliance filter (blocked claims, substitutions, disclaimers).
		return p.applyFilter(ScreenOutput(text), businessType)
	}), meta, nil
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
