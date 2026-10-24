package rag

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// Knowledge-base answers: a question the knowledge base already answers is
// answered from it, with no model call, and when every model fails the closest
// entry still answers instead of an empty reply.
const (
	// maxDirectAnswerLen is the longest entry that can be returned as it is
	// (FAQ-sized). Longer entries are documents and only feed the prompt.
	maxDirectAnswerLen = 1500
	// directFAQTitleSim: the question and an FAQ entry's title (itself a question)
	// say the same thing.
	directFAQTitleSim = 0.86
	// directFAQChunkSim: the question lands squarely on an FAQ answer.
	directFAQChunkSim = 0.80
	// directEntrySim: any other short entry needs a closer match.
	directEntrySim = 0.86
	// fallbackSim is the least similarity worth showing when the models are down.
	fallbackSim = 0.50
	// fallbackLen bounds a fallback excerpt of a long entry.
	fallbackLen = 700
	// classifyTimeout bounds the intent classifier.
	classifyTimeout = 8 * time.Second
)

// Answer sources that use no model; the chat handler does not charge for them.
const (
	ModelKnowledgeBase = "knowledge_base"
	ModelKBFallback    = "kb_fallback"
)

// FreeAnswer reports an answer that no model produced (guards, templates,
// caches, the knowledge base).
func FreeAnswer(r *ChatResult) bool {
	if r == nil {
		return false
	}
	if r.Cached {
		return true
	}
	switch r.Model {
	case "guard", "template", "scope_guard", "semantic_cache", ModelKnowledgeBase, ModelKBFallback:
		return true
	}
	return false
}

// kbDirect returns a short entry's own text when it clearly answers the
// question. It is used only for a question without conversation history: a
// follow-up ("how much is it?") needs the conversation, not a lookup.
func (p *Pipeline) kbDirect(ctx context.Context, input ChatInput, emb []float32, chunks []SearchResult) (string, bool) {
	if len(input.History) > 0 || len(emb) == 0 {
		return "", false
	}
	q := plainWords(input.Question)
	for i, c := range chunks {
		if i >= 3 {
			break
		}
		if c.Content == "" || c.ChunkIndex != 0 {
			continue
		}
		if c.Category != "faq" {
			if c.Similarity >= directEntrySim {
				return strings.TrimSpace(c.Content), true
			}
			continue
		}
		if c.Similarity >= directFAQChunkSim || plainWords(c.Title) == q {
			return strings.TrimSpace(c.Content), true
		}
		if c.Similarity < fallbackSim {
			continue // the title check costs an embedding; skip hopeless entries
		}
		if t := p.cachedEmbed(ctx, c.Title); len(t) > 0 && cosineSim(emb, t) >= directFAQTitleSim {
			return strings.TrimSpace(c.Content), true
		}
	}
	return "", false
}

// kbFallback is the answer when no model could: the closest knowledge-base
// entry when it is relevant enough, else the business's contact options.
// It never returns an empty string.
func kbFallback(input ChatInput, chunks []SearchResult) string {
	for _, c := range chunks {
		if c.Similarity < fallbackSim {
			break // results are ordered by similarity
		}
		text := strings.TrimSpace(c.Content)
		if text == "" {
			text = excerpt(strings.TrimSpace(c.ChunkText), fallbackLen)
		}
		if text == "" {
			continue
		}
		return "Here is what I can share on that:\n\n" + text + "\n\n" + contactLine(input.BusinessType)
	}
	return "I can't answer that right now. " + contactLine(input.BusinessType)
}

// escalateMessage hands the visitor to a person.
func escalateMessage(businessType string) string {
	switch businessType {
	case "flp":
		return "I'd be happy to connect you with your FBO directly. You can reach them via WhatsApp for a personal conversation, or book a virtual meeting."
	case "codevertex":
		return "I'd be happy to connect you with the Codevertex Africa Limited team. You can reach us at info@codevertexafrica.com, call +254 743 793 901, or visit our office at Pioneer House, Oginga-Odinga Road, Kisumu."
	case "cafe":
		return "I'd be happy to help you get in touch with our team. Please use the contact details on our website, give us a call, or visit us in person. We'd love to see you!"
	default:
		return "I'd be happy to connect you with the team. Please use the contact options below or send them a message directly."
	}
}

// contactLine points the visitor to a person for anything the answer missed.
func contactLine(businessType string) string {
	switch businessType {
	case "flp":
		return "For more, message your Forever Business Owner on WhatsApp using the button above."
	case "codevertex":
		return "For more, email info@codevertexafrica.com or call +254 743 793 901."
	case "cafe":
		return "For more, call us or visit us in person."
	default:
		return "For more, use the WhatsApp or call buttons to reach the team."
	}
}

// plainWords lowercases text and keeps only letters, digits and single spaces,
// so "How does it work?" and "how does it work" compare equal.
func plainWords(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
		default:
			space = true
		}
	}
	return b.String()
}

// excerpt cuts text to about n characters at a sentence or word boundary.
func excerpt(text string, n int) string {
	if len(text) <= n {
		return text
	}
	cut := text[:n]
	if i := strings.LastIndexAny(cut, ".!?\n"); i > n/2 {
		return strings.TrimSpace(cut[:i+1])
	}
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}

// staticStream sends one finished answer down a stream.
func staticStream(text string) <-chan ai.StreamDelta {
	ch := make(chan ai.StreamDelta, 2)
	ch <- ai.StreamDelta{Content: text}
	ch <- ai.StreamDelta{Done: true}
	close(ch)
	return ch
}

// classify picks the intent: keyword rules, then the classification cache, then
// the classifier model (cached for next time). A classifier failure is not an
// error: the question is treated as product Q&A.
func (p *Pipeline) classify(ctx context.Context, question string) (ai.Intent, float64, bool) {
	if hcls, ok := ai.HeuristicClassify(question); ok {
		return hcls.Intent, hcls.Confidence, hcls.NeedsLiveInfo
	}
	if cc, ok := p.cache.getClassification(ctx, question); ok {
		return ai.Intent(cc.Intent), cc.Confidence, cc.NeedsLiveInfo
	}
	// A visitor is waiting: past a few seconds the default intent is better than
	// a slow guess.
	cctx, cancel := context.WithTimeout(ctx, classifyTimeout)
	defer cancel()
	cls, err := p.router.Classify(cctx, question)
	if err != nil {
		p.logger.Warn("classification failed, defaulting to product_qa")
		return ai.IntentProductQA, 0.5, false
	}
	p.cache.setClassification(ctx, question, clsCache{
		Intent: string(cls.Intent), Confidence: cls.Confidence, NeedsLiveInfo: cls.NeedsLiveInfo,
	})
	return cls.Intent, cls.Confidence, cls.NeedsLiveInfo
}
