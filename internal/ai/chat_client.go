package ai

import "context"

// ChatClient is the provider-neutral chat surface. *Client (Groq, Ollama, OpenAI) and
// *AnthropicClient implement it, and so does a class-bound model router client
// (internal/llm), which lets every caller get fallback and usage metering without
// knowing which provider answered.
type ChatClient interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamDelta, error)
}

// ResponseFormat asks OpenAI-compatible providers for a JSON object reply.
type ResponseFormat struct {
	Type string `json:"type"` // "json_object"
}

// JSONObject is the response format that forces a single JSON object reply.
var JSONObject = &ResponseFormat{Type: "json_object"}

// Usage is the token accounting of one completion. Cache fields are filled by
// providers with prompt caching (Anthropic); the rest by every provider.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_creation_input_tokens,omitempty"`
}

var (
	_ ChatClient = (*Client)(nil)
	_ ChatClient = (*AnthropicClient)(nil)
)
