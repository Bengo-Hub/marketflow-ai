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

// Message represents a chat message in the OpenAI format.
// ToolCalls is populated on assistant messages when the model wants to invoke tools.
// ToolCallID is set on "tool" role messages to reference the originating call.
type Message struct {
	Role       string     `json:"role"`    // system, user, assistant, tool
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is an assistant-issued function invocation (OpenAI-compatible).
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // always "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded string
	} `json:"function"`
}

// ToolDef declares a callable tool for the model.
type ToolDef struct {
	Type     string `json:"type"` // always "function"
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"` // JSON schema
	} `json:"function"`
}

// ChatRequest is the request body for the OpenAI-compatible chat completions API.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	Tools       []ToolDef `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"` // "auto", "required", "none"
}

// ChatResponse is the non-streaming response from the chat completions API.
type ChatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Index   int     `json:"index"`
		Message Message `json:"message"`
		Finish  string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// StreamDelta represents a single chunk in a streaming response.
type StreamDelta struct {
	Type         string `json:"type,omitempty"` // "delta" (default) or "replace"
	Content      string `json:"content"`
	FinishReason string `json:"finish_reason"`
	Done         bool   `json:"done"`
}

// Client is an OpenAI-compatible HTTP client that works with Groq, Ollama, and any
// provider implementing the /v1/chat/completions endpoint.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	logger     *zap.Logger
}

// NewClient creates a new AI client.
func NewClient(baseURL, apiKey string, logger *zap.Logger) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		logger: logger.Named("ai.client"),
	}
}

// Chat sends a non-streaming chat completion request with automatic retry
// on 429 (rate limit). Groq's free tier has low TPM limits; a short backoff
// typically succeeds on the second try.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	req.Stream = false

	for attempt := 0; attempt < 3; attempt++ {
		body, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("send request: %w", err)
		}

		if resp.StatusCode == 429 {
			resp.Body.Close()
			wait := time.Duration(5*(attempt+1)) * time.Second
			c.logger.Warn("rate limited, retrying",
				zap.Int("attempt", attempt+1), zap.Duration("wait", wait))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
				continue
			}
		}

		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
		}

		var chatResp ChatResponse
		if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		return &chatResp, nil
	}
	return nil, fmt.Errorf("rate limit exceeded after 3 retries")
}

// ChatStream sends a streaming chat completion request and returns a channel of deltas.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamDelta, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamDelta, 100)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				ch <- StreamDelta{Done: true}
				return
			}

			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				c.logger.Warn("failed to parse stream chunk", zap.Error(err))
				continue
			}

			if len(chunk.Choices) > 0 {
				delta := StreamDelta{
					Content: chunk.Choices[0].Delta.Content,
				}
				if chunk.Choices[0].FinishReason != nil {
					delta.FinishReason = *chunk.Choices[0].FinishReason
				}
				ch <- delta
			}
		}
		ch <- StreamDelta{Done: true}
	}()

	return ch, nil
}

// Embed generates vector embeddings using the Ollama API.
func (c *Client) Embed(ctx context.Context, model string, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{
		"model":  model,
		"prompt": text,
	})

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create embed request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send embed request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embed API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}
	return result.Embedding, nil
}
