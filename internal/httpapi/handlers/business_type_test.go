package handlers

import "testing"

// TestChooseBusinessType: the tenant's own type always wins; auto-detect (an empty result) only
// runs when neither the tenant, the request nor the widget header names a type.
func TestChooseBusinessType(t *testing.T) {
	cases := []struct {
		name, tenant string
		chosen       bool
		requested    string
		persona      string
		want         string
	}{
		{"tenant wins over request", "pharmacy", false, "generic", "", "pharmacy"},
		{"tenant wins over persona", "salon", true, "", "urban-loft-cafe", "salon"},
		{"same type keeps the widget persona", "restaurant", true, "cafe", "", "cafe"},
		{"flp tenant", "flp", false, "", "", "flp"},
		{"tenant alias is normalised", "hotel", false, "", "", "hospitality"},
		{"unchosen generic leaves the request", "generic", false, "cafe", "", "cafe"},
		{"unchosen generic leaves the persona", "generic", false, "", "codevertex-site", "codevertex"},
		{"chosen generic is final over the request", "generic", true, "cafe", "", "generic"},
		{"chosen generic is final over the persona", "generic", true, "", "urban-loft", "generic"},
		{"chosen generic stops auto-detect", "generic", true, "", "", "generic"},
		{"unknown tenant type leaves the request", "codevertex", false, "codevertex", "", "codevertex"},
		{"nothing known: auto-detect", "", false, "", "", ""},
		{"no tenant: request", "", false, "flp", "", "flp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := chooseBusinessType(c.tenant, c.chosen, c.requested, c.persona); got != c.want {
				t.Fatalf("chooseBusinessType(%q, %v, %q, %q) = %q, want %q", c.tenant, c.chosen, c.requested, c.persona, got, c.want)
			}
		})
	}
}

func TestPersonaToBusinessType(t *testing.T) {
	cases := map[string]string{
		"urban-loft":  "cafe",
		"forever":     "flp",
		"codevertex":  "codevertex",
		"Salon":       "salon",
		"hotel":       "hospitality",
		"pharmacy":    "pharmacy",
		"some-widget": "generic",
	}
	for in, want := range cases {
		if got := personaToBusinessType(in); got != want {
			t.Errorf("personaToBusinessType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOwnBusinessType(t *testing.T) {
	for in, want := range map[string]string{"": "generic", "codevertex": "generic", "retail": "retail", "flp": "flp"} {
		if got := ownBusinessType(in); got != want {
			t.Errorf("ownBusinessType(%q) = %q, want %q", in, got, want)
		}
	}
}
