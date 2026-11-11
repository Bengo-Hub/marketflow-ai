package handlers

import "testing"

// Sprint 23 L26: a lead who asks for a person, or names a budget at or above the business's
// hand-off value, goes to the owner and the bot stops.
func TestDetectHandoff(t *testing.T) {
	cases := []struct {
		text      string
		threshold float64
		want      bool
	}{
		{"Can I speak to a human please", 0, true},
		{"I want to talk to a real person", 0, true},
		{"please call me", 0, true},
		{"Is the owner around?", 0, true},
		{"How much is the Python course?", 0, false},
		{"our budget is 50k", 100000, false},
		{"our budget is 150k", 100000, true},
		{"We can spend KES 120,000", 100000, true},
		{"about 1.2m for the whole team", 100000, true},
		{"I have 2 kids", 100000, false},
		{"budget 500000", 0, false}, // no threshold set: amounts never trigger
	}
	for _, c := range cases {
		if got, _ := detectHandoff(c.text, c.threshold); got != c.want {
			t.Errorf("%q (threshold %v): got %v want %v", c.text, c.threshold, got, c.want)
		}
	}
}

func TestLeadSessionIDIsStable(t *testing.T) {
	a := leadSessionID("t1", "l1")
	if a != leadSessionID("t1", "l1") || a == leadSessionID("t1", "l2") || a == leadSessionID("t2", "l1") {
		t.Fatal("one session per tenant and lead")
	}
}
