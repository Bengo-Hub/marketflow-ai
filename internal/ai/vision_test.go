package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageWithImagesIsSentAsParts(t *testing.T) {
	b, err := json.Marshal(Message{Role: "user", Content: "look", Images: []string{"data:image/jpeg;base64,QUJD"}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"type":"image_url"`) || !strings.Contains(s, `{"text":"look","type":"text"}`) || strings.Contains(s, "Images") {
		t.Fatalf("parts not sent: %s", s)
	}
	plain, _ := json.Marshal(Message{Role: "user", Content: "hi"})
	if string(plain) != `{"role":"user","content":"hi"}` {
		t.Fatalf("a plain message changed: %s", plain)
	}
}

func TestAnthropicImageBlocks(t *testing.T) {
	c := &AnthropicClient{}
	req, _ := c.translateRequest(ChatRequest{Messages: []Message{{Role: "user", Content: "look",
		Images: []string{"data:image/png;base64,QUJD", "https://example.com/a.jpg", "ftp://bad"}}}})
	blocks, ok := req.Messages[0].Content.([]anthropicContentBlock)
	if !ok || len(blocks) != 3 {
		t.Fatalf("blocks = %#v", req.Messages[0].Content)
	}
	if blocks[0].Source.Type != "base64" || blocks[0].Source.MediaType != "image/png" || blocks[0].Source.Data != "QUJD" {
		t.Fatalf("data block %+v", blocks[0].Source)
	}
	if blocks[1].Source.Type != "url" || blocks[2].Type != "text" {
		t.Fatalf("url or text block wrong: %+v %+v", blocks[1], blocks[2])
	}
}
