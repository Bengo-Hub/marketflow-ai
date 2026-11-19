package handlers

import (
	"encoding/json"
	"strconv"
	"strings"
)

// posterPalette is the brand palette of a poster request. marketflow-api sends hex strings
// ("#1a73e8"); RGB triples are accepted too. Before 2026-11-19 only triples decoded, so every
// poster of a tenant with brand colours failed with "invalid request body".
type posterPalette [][3]int

func (p *posterPalette) UnmarshalJSON(b []byte) error {
	var triples [][3]int
	if err := json.Unmarshal(b, &triples); err == nil {
		*p = triples
		return nil
	}
	var hexes []string
	if err := json.Unmarshal(b, &hexes); err != nil {
		return err
	}
	out := make([][3]int, 0, len(hexes))
	for _, h := range hexes {
		if rgb, ok := parseHexColour(h); ok {
			out = append(out, rgb)
		}
	}
	*p = out
	return nil
}

// parseHexColour reads "#rgb" or "#rrggbb" (the # optional); ok is false for anything else.
func parseHexColour(s string) ([3]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return [3]int{}, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return [3]int{}, false
	}
	return [3]int{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}, true
}
