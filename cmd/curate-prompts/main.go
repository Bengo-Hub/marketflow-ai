// Command curate-prompts selects the marketing-relevant prompts from the
// prompts.chat library (prompts.csv, CC0 1.0) and writes them to
// internal/library/data/prompts.json, which the library embeds.
//
// It is run by hand when the library is refreshed, so the selection is a
// reviewable diff rather than a runtime download:
//
//	go run ./cmd/curate-prompts ../../my-prompt-library/prompts.csv
package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
)

// prompt is one curated entry; the field names match library.Prompt.
type prompt struct {
	ID      string   `json:"id"`
	Act     string   `json:"act"`
	Prompt  string   `json:"prompt"`
	Type    string   `json:"type"`
	Tags    []string `json:"tags"`
	Source  string   `json:"source"`
	License string   `json:"license"`
}

// actTopics map a tag to the words that mark a marketing role in the "act"
// column (the prompt's role name).
var actTopics = map[string][]string{
	"advertising": {"advertis", "ad copy", "ads ", "campaign", "tv commercial", "promot", "marketer", "marketing"},
	"copywriting": {"copywrit", "slogan", "headline", "tagline", "caption", "product description", "sales page", "landing page"},
	"social":      {"social media", "instagram", "tiktok", "youtube", "twitter", "linkedin", "facebook", "influencer", "content creator", "hashtag"},
	"content":     {"content", "blog", "newsletter", "storytell", "story teller", "storyteller", "scriptwriter", "script writer", "video script", "podcast"},
	"seo":         {"seo", "search engine"},
	"sales":       {"sales", "salesperson", "negotiat", "cold email", "pitch", "customer service", "customer support", "real estate agent"},
	"brand":       {"brand", "public relations", "naming", "logo"},
	"strategy":    {"startup", "business plan", "entrepreneur", "growth hacker", "product manager", "market research", "consultant"},
	"email":       {"email"},
	"design":      {"poster", "flyer", "graphic design", "ad design", "packaging", "product photo"},
}

// excludeAct drops software, coding and unrelated roles that share a marketing
// word ("Instagram API", "blog system development", "commercial aircraft").
var excludeAct = []string{
	" app", " api", "website", "web template", "development", "developer", " code", "coding", "program",
	"dashboard", "react", "portal", "renaming", "chrome", "procedural", "aircraft", "kitchen", "legal",
	"phishing", "pull request", "medical", "triage", "landing page.", "3d ", "platform", "platorm",
	// Reviewed by hand 2026-10-22: art, entertainment and job-seeker prompts that
	// share a keyword with marketing but do not help a business sell.
	"scraper", "downloader", "simpsons", "movie poster", "superhero", "interstellar", "ocean", "hollywood",
	"footballer", "football player", "solar system", "quantitative", "lawyer", "time management",
	"hiring manager", "recruiter", "experience optimization", "recommendation request", "selfie",
	"clay bust", "noir portrait", "yarn", "frog band", "sailboat", "seaplane", "floating city",
	"winter poster", "space-themed", "detector", "calculator content", "airfare", "canonical markdown",
	"glühwein", "cocktail web", "airfare", "street-art", "stencil vector", "travel poster", "torn paper",
	"profile search", "auteur", "for students", "self-learning poster",
}

// imageWords keep IMAGE prompts only when they are about products or ads.
var imageWords = []string{"product", "advert", "poster", "marketing", "brand", "commercial", "packaging", "billboard", "flyer", "banner"}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

const maxPromptChars = 1500

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: go run ./cmd/curate-prompts <path to prompts.csv>")
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		log.Fatalf("read csv: %v", err)
	}
	if len(rows) < 2 {
		log.Fatal("empty csv")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.TrimSpace(strings.ToLower(h))] = i
	}
	for _, need := range []string{"act", "prompt", "type"} {
		if _, ok := col[need]; !ok {
			log.Fatalf("csv has no %q column", need)
		}
	}

	seen := map[string]bool{}
	var out []prompt
	for _, row := range rows[1:] {
		get := func(name string) string {
			if i, ok := col[name]; ok && i < len(row) {
				return strings.TrimSpace(row[i])
			}
			return ""
		}
		act, text, typ := get("act"), get("prompt"), strings.ToUpper(get("type"))
		if act == "" || text == "" || strings.EqualFold(get("for_devs"), "TRUE") {
			continue
		}
		lowerAct := " " + strings.ToLower(act) + " "
		if containsAny(lowerAct, excludeAct) {
			continue
		}
		tags := matchTags(lowerAct)
		if typ == "IMAGE" || typ == "VIDEO" {
			if !containsAny(strings.ToLower(text), imageWords) {
				continue
			}
			tags = append(tags, "design")
		}
		if len(tags) == 0 {
			continue
		}
		id := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(act), "-"), "-")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if len([]rune(text)) > maxPromptChars {
			text = string([]rune(text)[:maxPromptChars])
		}
		if typ == "" {
			typ = "TEXT"
		}
		out = append(out, prompt{ID: id, Act: act, Prompt: text, Type: typ, Tags: uniq(tags), Source: "prompts.chat", License: "CC0-1.0"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	dest := "internal/library/data/prompts.json"
	if err := os.WriteFile(dest, append(data, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("curated %d of %d prompts into %s\n", len(out), len(rows)-1, dest)
}

func matchTags(act string) []string {
	var tags []string
	for tag, words := range actTopics {
		if containsAny(act, words) {
			tags = append(tags, tag)
		}
	}
	return tags
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func uniq(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
