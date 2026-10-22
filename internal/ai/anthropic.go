package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// AnthropicClient calls the Anthropic Messages API.
// It translates the internal OpenAI-style Message/ToolDef format into
// Anthropic's native format and back, so callers need no Anthropic-specific code.
type AnthropicClient struct {
	baseURL    string
	apiKey     string
	version    string
	httpClient *http.Client
	logger     *zap.Logger
}

// NewAnthropicClient creates an Anthropic Messages API client.
func NewAnthropicClient(baseURL, apiKey, version string, logger *zap.Logger) *AnthropicClient {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/v1"
	}
	if version == "" {
		version = "2023-06-01"
	}
	return &AnthropicClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		version: version,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		logger: logger.Named("ai.anthropic"),
	}
}

// anthropicMessage is the Anthropic Messages API message format.
// Content is either a plain string or a list of content blocks (tool use/result).
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | []anthropicContentBlock
}

type anthropicContentBlock struct {
	Type      string `json:"type"`                  // text | tool_use | tool_result
	Text      string `json:"text,omitempty"`        // for text blocks
	ID        string `json:"id,omitempty"`          // for tool_use
	Name      string `json:"name,omitempty"`        // for tool_use
	Input     any    `json:"input,omitempty"`       // for tool_use (parsed args)
	ToolUseID string `json:"tool_use_id,omitempty"` // for tool_result
	Content   string `json:"content,omitempty"`     // for tool_result
}

type anthropicToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicRequest never carries temperature: current Claude models reject
// non-default sampling parameters, so the provider default is always used.
type anthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	System       any                    `json:"system,omitempty"` // string | []anthropicSystemBlock
	Messages     []anthropicMessage     `json:"messages"`
	Tools        []anthropicToolDef     `json:"tools,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

type anthropicSystemBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"` // "ephemeral" (5 minute TTL)
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// systemField renders the system prompt. With caching requested it becomes one
// text block carrying a cache breakpoint, so tools plus system are cached together
// (prefixes under the model minimum are simply not cached, without error).
func systemField(system string, cache bool) any {
	if system == "" {
		return nil
	}
	if !cache {
		return system
	}
	return []anthropicSystemBlock{{
		Type: "text", Text: system,
		CacheControl: &anthropicCacheControl{Type: "ephemeral"},
	}}
}

type anthropicResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type  string `json:"type"`
		Text  string `json:"text,omitempty"`
		ID    string `json:"id,omitempty"`
		Name  string `json:"name,omitempty"`
		Input any    `json:"input,omitempty"`
	} `json:"content"`
	Model        string `json:"model"`
	StopReason   string `json:"stop_reason"`
	StopSequence string `json:"stop_sequence"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// Chat sends a chat completion request using the Anthropic Messages API.
// The internal OpenAI-format request is translated automatically.
func (c *AnthropicClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	antReq, systemPrompt := c.translateRequest(req)
	antReq.System = systemField(systemPrompt, req.CacheSystem)
	antReq.Stream = false

	body, err := json.Marshal(antReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: create request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 529 || resp.StatusCode == 429 {
		return nil, fmt.Errorf("anthropic: rate limited (%d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic: API error %d: %s", resp.StatusCode, string(respBody))
	}

	var antResp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&antResp); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	return c.translateResponse(&antResp, req.Model), nil
}

// ChatStream streams a chat completion response from the Anthropic Messages API.
func (c *AnthropicClient) ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamDelta, error) {
	antReq, systemPrompt := c.translateRequest(req)
	antReq.System = systemField(systemPrompt, req.CacheSystem)
	antReq.Stream = true

	body, err := json.Marshal(antReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: create request: %w", err)
	}
	c.setHeaders(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: send request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic: API error %d: %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamDelta, 128)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		var eventType string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")

			switch eventType {
			case "content_block_delta":
				var chunk struct {
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Delta.Text != "" {
					ch <- StreamDelta{Content: chunk.Delta.Text}
				}
			case "message_delta":
				var stop struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(data), &stop) == nil && stop.Delta.StopReason != "" {
					ch <- StreamDelta{FinishReason: stop.Delta.StopReason}
				}
			case "message_stop":
				ch <- StreamDelta{Done: true}
				return
			}
		}
		ch <- StreamDelta{Done: true}
	}()

	return ch, nil
}

// translateRequest converts an OpenAI-format ChatRequest into an Anthropic request.
// Returns the request and the extracted system prompt (Anthropic separates it).
func (c *AnthropicClient) translateRequest(req ChatRequest) (anthropicRequest, string) {
	var system string
	var messages []anthropicMessage

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			system = m.Content
		case "user":
			messages = append(messages, anthropicMessage{Role: "user", Content: m.Content})
		case "assistant":
			if len(m.ToolCalls) > 0 {
				var blocks []anthropicContentBlock
				if m.Content != "" {
					blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
				}
				for _, tc := range m.ToolCalls {
					var inputMap any
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &inputMap)
					blocks = append(blocks, anthropicContentBlock{
						Type:  "tool_use",
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: inputMap,
					})
				}
				messages = append(messages, anthropicMessage{Role: "assistant", Content: blocks})
			} else {
				messages = append(messages, anthropicMessage{Role: "assistant", Content: m.Content})
			}
		case "tool":
			// Tool result — Anthropic expects this as a user message with tool_result block
			block := anthropicContentBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			}
			// Merge into previous user message if it exists and is already a tool_result,
			// otherwise create a new user message
			if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
				if blocks, ok := messages[len(messages)-1].Content.([]anthropicContentBlock); ok {
					messages[len(messages)-1].Content = append(blocks, block)
					continue
				}
			}
			messages = append(messages, anthropicMessage{Role: "user", Content: []anthropicContentBlock{block}})
		}
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 2048
	}

	antReq := anthropicRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		Messages:  messages,
	}
	if req.Effort != "" {
		antReq.OutputConfig = &anthropicOutputConfig{Effort: req.Effort}
	}

	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			antReq.Tools = append(antReq.Tools, anthropicToolDef{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}

	return antReq, system
}

// translateResponse converts an Anthropic response into the internal OpenAI-compat format.
func (c *AnthropicClient) translateResponse(antResp *anthropicResponse, model string) *ChatResponse {
	var msg Message
	msg.Role = "assistant"

	for _, block := range antResp.Content {
		switch block.Type {
		case "text":
			msg.Content += block.Text
		case "tool_use":
			argsJSON, _ := json.Marshal(block.Input)
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      block.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}

	finishReason := antResp.StopReason
	return &ChatResponse{
		ID: antResp.ID,
		Choices: []struct {
			Index   int     `json:"index"`
			Message Message `json:"message"`
			Finish  string  `json:"finish_reason"`
		}{
			{Index: 0, Message: msg, Finish: finishReason},
		},
		// input_tokens is only the uncached remainder, so the prompt total adds the
		// cached reads and writes back in.
		Usage: Usage{
			PromptTokens:     antResp.Usage.InputTokens + antResp.Usage.CacheReadInputTokens + antResp.Usage.CacheCreationInputTokens,
			CompletionTokens: antResp.Usage.OutputTokens,
			TotalTokens: antResp.Usage.InputTokens + antResp.Usage.CacheReadInputTokens +
				antResp.Usage.CacheCreationInputTokens + antResp.Usage.OutputTokens,
			CacheReadTokens:  antResp.Usage.CacheReadInputTokens,
			CacheWriteTokens: antResp.Usage.CacheCreationInputTokens,
		},
	}
}

func (c *AnthropicClient) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", c.version)
}
