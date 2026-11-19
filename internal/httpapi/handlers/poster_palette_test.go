package handlers

import (
	"encoding/json"
	"testing"
)

// marketflow-api's hex palette and the older triples both decode; bad entries are dropped.
func TestPosterPaletteDecodes(t *testing.T) {
	var req posterRequest
	if err := json.Unmarshal([]byte(`{"palette":["#1A73E8","#fff","nope"]}`), &req); err != nil {
		t.Fatalf("hex palette must decode: %v", err)
	}
	if len(req.Palette) != 2 || req.Palette[0] != [3]int{26, 115, 232} || req.Palette[1] != [3]int{255, 255, 255} {
		t.Fatalf("hex: %v", req.Palette)
	}
	if err := json.Unmarshal([]byte(`{"palette":[[1,2,3]]}`), &req); err != nil || req.Palette[0] != [3]int{1, 2, 3} {
		t.Fatalf("triples: %v %v", req.Palette, err)
	}
}
