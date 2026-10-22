package promptopt

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		budget   float64
		days     int
		lang     string
		platform []string
	}{
		{"nataka tangazo la course yangu ya Python wiki hii, budget 3k", 3000, 7, "sw", nil},
		{"Ads for my salon on Facebook and IG this week, KES 5,000", 5000, 7, "en", []string{"facebook", "instagram"}},
		{"promote my shoes for 10 days with ksh 2500 on tiktok", 2500, 10, "en", []string{"tiktok"}},
		{"tangazo la wiki 2 bajeti elfu tatu", 3000, 14, "sw", nil},
		{"make a 30 second video ad, 2 posters", 0, 0, "en", nil},
		{"budget 1.5m for this month on youtube", 1500000, 30, "en", []string{"youtube"}},
		{"msee nisaidie na status ya whatsapp leo", 0, 1, "sw", []string{"whatsapp"}},
		{"Plan posts for next week, see www.example.co.ke", 0, 7, "en", nil},
	}
	for _, c := range cases {
		p := Parse(c.in)
		if p.BudgetKES != c.budget {
			t.Errorf("%q: budget = %v (%q), want %v", c.in, p.BudgetKES, p.BudgetText, c.budget)
		}
		if p.PeriodDays != c.days {
			t.Errorf("%q: days = %d (%q), want %d", c.in, p.PeriodDays, p.PeriodText, c.days)
		}
		if p.Language != c.lang {
			t.Errorf("%q: language = %s, want %s", c.in, p.Language, c.lang)
		}
		if !reflect.DeepEqual(p.Platforms, c.platform) {
			t.Errorf("%q: platforms = %v, want %v", c.in, p.Platforms, c.platform)
		}
	}
	if u := Parse("see https://shop.example.com/item?id=3 for 4000").URLs; len(u) != 1 {
		t.Fatalf("urls = %v", u)
	}
}
