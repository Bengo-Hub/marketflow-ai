package ai

import (
	"encoding/json"
	"strings"
)

// Vision: a user message may carry pictures (Message.Images) for a model that reads images, such
// as the cheap reviewer that checks finished posters and video frames (media/review.go). On
// OpenAI-compatible providers (Groq, OpenAI, Ollama) the message's content becomes a list of a
// text part and image_url parts; on Anthropic it becomes text and image blocks.

// MarshalJSON sends a message with images as content parts and every other message unchanged.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if len(m.Images) == 0 {
		return json.Marshal(plain(m))
	}
	parts := []map[string]any{{"type": "text", "text": m.Content}}
	for _, u := range m.Images {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
	}
	return json.Marshal(struct {
		plain
		Content any `json:"content"`
	}{plain(m), parts})
}

// anthropicImageSource is an Anthropic image block's source: base64 data or a URL.
type anthropicImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// anthropicImageBlocks is a user message with pictures as Anthropic content blocks: the images
// first, then the text, as Anthropic recommends.
func anthropicImageBlocks(m Message) []anthropicContentBlock {
	var blocks []anthropicContentBlock
	for _, u := range m.Images {
		if src, ok := imageSource(u); ok {
			blocks = append(blocks, anthropicContentBlock{Type: "image", Source: &src})
		}
	}
	return append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
}

// imageSource reads a data URL or an http(s) link.
func imageSource(u string) (anthropicImageSource, bool) {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		if !found || !strings.HasSuffix(meta, ";base64") {
			return anthropicImageSource{}, false
		}
		return anthropicImageSource{Type: "base64", MediaType: strings.TrimSuffix(meta, ";base64"), Data: data}, true
	}
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return anthropicImageSource{Type: "url", URL: u}, true
	}
	return anthropicImageSource{}, false
}
