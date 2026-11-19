package team

import (
	"strings"
	"testing"
)

func TestGuardOffersRemovesInventedOffers(t *testing.T) {
	v := VideoScript{CTA: "Book your free demo session today",
		Hooks: []string{"Get 20% off this week", "Close the day in five minutes"},
		Scenes: []Scene{
			{Shot: "presenter", Voiceover: "Duka POS counts every sale. Book a free demo session now!", Caption: "Free demo"},
			{Shot: "end_card", Voiceover: "Chat with us on WhatsApp.", Caption: "Duka POS"},
		}}
	found := GuardOffers(&v, "Duka POS\nPrice: KES 2,500 a month\nAbout it: point of sale for shops")
	if len(found) < 4 {
		t.Fatalf("findings: %+v", found)
	}
	for _, f := range found {
		if f.Severity != "review" {
			t.Fatalf("severity: %+v", f)
		}
	}
	if strings.Contains(strings.ToLower(v.Scenes[0].Voiceover), "free") || v.Scenes[0].Voiceover != "Duka POS counts every sale." {
		t.Fatalf("voiceover: %q", v.Scenes[0].Voiceover)
	}
	if v.Scenes[0].Caption != "" || v.CTA != "" || len(v.Hooks) != 1 || v.Scenes[1].Voiceover != "Chat with us on WhatsApp." {
		t.Fatalf("script: %+v", v)
	}
}

func TestGuardOffersKeepsOffersTheBriefMakes(t *testing.T) {
	v := VideoScript{CTA: "Claim your free delivery", Scenes: []Scene{{Voiceover: "Get 10% off with code ALOE10."}}}
	if found := GuardOffers(&v, "Aloe gel\nPromotion code: ALOE10 for a 10% discount\nFree delivery in Nairobi"); len(found) != 0 {
		t.Fatalf("backed offers flagged: %+v", found)
	}
	if v.CTA == "" || v.Scenes[0].Voiceover == "" {
		t.Fatal("backed offers removed")
	}
}

func TestDigestSiteTextDropsClientsAndBoilerplate(t *testing.T) {
	site := strings.Join([]string{
		"Home | About | Services | Contact",
		"Codevertex builds point of sale and accounting software for Kenyan shops.",
		"Our clients",
		"The Urban Loft",
		"Glow Cosmetics",
		"We integrate M-Pesa and KRA eTIMS in every system we ship.",
		"We integrate M-Pesa and KRA eTIMS in every system we ship.",
		"We use cookies to improve your experience on this site.",
		"© 2026 Codevertex. All rights reserved.",
	}, "\n")
	got := DigestSiteText(site)
	for _, bad := range []string{"Urban Loft", "Glow", "cookies", "rights reserved", "Home | About"} {
		if strings.Contains(got, bad) {
			t.Fatalf("digest keeps %q:\n%s", bad, got)
		}
	}
	if strings.Count(got, "M-Pesa") != 1 || !strings.Contains(got, "point of sale") {
		t.Fatalf("digest lost facts:\n%s", got)
	}
	long := strings.Repeat("We sell aloe vera gel in Nairobi at fair prices. ", 60)
	if d := DigestSiteText(long); d == "" || len(d) > siteDigestChars {
		t.Fatalf("long text digest: %d chars", len(d))
	}
}
