package team

import "testing"

func TestCopiedFrom(t *testing.T) {
	refs := []string{
		"Is your child glued to a screen all holiday? Let them build their own game this December.",
		"Watoto wako wako kwa screen siku nzima hii holiday? Wapeleke wajifunze coding Kisumu.",
	}
	cases := []struct {
		name  string
		draft string
		want  bool
	}{
		{"verbatim English run", "This season, let them build their own game this December with us.", true},
		{"case and punctuation do not hide it", "LET THEM, build their OWN game... this December!", true},
		{"verbatim Swahili run", "Je, watoto wako wako kwa screen siku nzima? Tuko Pioneer House.", true},
		{"same idea, own words", "Turn holiday screen time into a game your child designs, at Digitika Kisumu.", false},
		{"too short to judge", "Build a game", false},
	}
	for _, c := range cases {
		if got, _ := copiedFrom(c.draft, refs); got != c.want {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if got, _ := copiedFrom("let them build their own game this December", nil); got {
		t.Fatal("no references, nothing to copy")
	}
}

func TestOverlapCatchesLightRewording(t *testing.T) {
	ref := words("fresh bread every morning delivered warm to your door before seven")
	draft := words("fresh bread every morning, delivered warm to your gate before seven")
	if overlap(draft, ref) < shingleLimit {
		t.Fatalf("a light rewording overlaps above the limit: %.2f", overlap(draft, ref))
	}
}
