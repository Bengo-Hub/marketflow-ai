package compliance

import (
	"strings"
	"testing"
)

// TestCatalogLoads guards the embedded data: every type has a label and description, the owner's
// list from 2026-11-14 is complete, and every block compiled (mustLoad would have panicked).
func TestCatalogLoads(t *testing.T) {
	want := []string{"generic", "flp", "restaurant", "retail", "ecommerce", "services", "saas", "salon", "pharmacy", "hospitality"}
	got := map[string]TypeInfo{}
	for _, ti := range Types() {
		got[ti.Key] = ti
	}
	if len(got) != len(want) {
		t.Fatalf("got %d business types, want %d: %v", len(got), len(want), Types())
	}
	for _, k := range want {
		ti, ok := got[k]
		if !ok {
			t.Fatalf("business type %q is missing", k)
		}
		if ti.Label == "" || ti.Description == "" {
			t.Fatalf("business type %q needs a label and a description", k)
		}
		if strings.Contains(ti.Description, "\u2014") || strings.Contains(ti.Label, "\u2014") {
			t.Fatalf("business type %q uses an em dash", k)
		}
	}
}

func TestParseCatalogRejectsBadData(t *testing.T) {
	cases := map[string]string{
		"bad regex":      "types:\n  - {key: generic, label: G, description: d}\n  - {key: flp, label: F, description: d, blocks: [{pattern: '(', reason: r}]}\n",
		"no reason":      "types:\n  - {key: generic, label: G, description: d, blocks: [{pattern: 'x'}]}\n  - {key: flp, label: F, description: d}\n",
		"duplicate":      "types:\n  - {key: generic, label: G, description: d, aliases: [flp]}\n  - {key: flp, label: F, description: d}\n",
		"missing flp":    "types:\n  - {key: generic, label: G, description: d}\n",
		"upper key":      "types:\n  - {key: Generic, label: G, description: d}\n  - {key: flp, label: F, description: d}\n",
		"unknown strict": "types:\n  - {key: generic, label: G, description: d, strict: other}\n  - {key: flp, label: F, description: d}\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCatalog([]byte(raw)); err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"":           "generic",
		"codevertex": "generic",
		"something":  "generic",
		"flp":        "flp",
		" FLP ":      "flp",
		"cafe":       "restaurant",
		"Restaurant": "restaurant",
		"e-commerce": "ecommerce",
		"hotel":      "hospitality",
		"guesthouse": "hospitality",
		"beauty":     "salon",
		"chemist":    "pharmacy",
		"saas":       "saas",
		"services":   "services",
		"retail":     "retail",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if Known("") || Known("codevertex") || !Known("cafe") || !Known("pharmacy") {
		t.Fatal("Known is wrong for empty, legacy or alias values")
	}
}

// TestTypeRules has, for every type, copy that breaks its rules and copy that follows them.
func TestTypeRules(t *testing.T) {
	f := NewFilter()
	cases := []struct {
		typ     string
		blocked []string
		clean   []string
	}{
		{"generic",
			[]string{"This tea cures headaches.", "Double your money in 30 days."},
			[]string{"Treat yourself to our new pastries.", "Satisfaction guaranteed or your money back."}},
		{"restaurant",
			[]string{"Our menu is 100% nut-free.", "Allergy-free cooking for the whole family.", "This green smoothie burns belly fat.", "Our soup boosts your immune system."},
			[]string{"Healthy lunch bowls from KES 450. Ask our staff about allergens.", "Fresh mandazi every morning from 7am."}},
		{"retail",
			[]string{"Cheapest in Nairobi, come see!", "Lowest prices guaranteed on all shoes.", "No refunds, no returns."},
			[]string{"School shoes KES 1,500. Exchange within 7 days with a receipt.", "Was KES 2,000, now KES 1,600 this week."}},
		{"ecommerce",
			[]string{"Lowest price in Kenya, order now.", "No returns or refunds on any item."},
			[]string{"Leather sandals KES 1,800. Delivery in Nairobi KES 200, returns within 7 days.", "Pay with M-Pesa on delivery."}},
		{"services",
			[]string{"Guaranteed visa approval in two weeks.", "100% pass rate for every student.", "We are guaranteed to win your case."},
			[]string{"Company registration in 5 working days from KES 15,000.", "Our NCA 4 registered team builds to plan."}},
		{"saas",
			[]string{"100% uptime, always.", "Your data is completely secure with our unhackable platform.", "Bank-grade encryption for every invoice."},
			[]string{"Free 14-day trial, then KES 2,500 a month. Cancel any time in settings.", "99.9% uptime under our published SLA."}},
		{"salon",
			[]string{"Guaranteed results after one session.", "Skin lightening cream now in stock.", "Our oil regrows your hair in weeks.", "Overnight results for dull skin."},
			[]string{"Box braids from KES 2,500 at our Westlands salon. Book a slot.", "See how our client Wanjiru's knotless braids turned out (shared with her permission)."}},
		{"pharmacy",
			[]string{"No prescription needed for any medicine.", "Safe painkiller with no side effects.", "Get 20% off amoxicillin this week.", "Ozempic now on offer!", "This supplement treats diabetes.", "Guaranteed relief in one hour."},
			[]string{"We fill prescriptions from any doctor. Talk to our pharmacist about your medicines.", "Paracetamol 500mg KES 50. Read the label and ask the pharmacist if pain lasts."}},
		{"hospitality",
			[]string{"Only 2 rooms left for December!", "Hurry, we are almost fully booked.", "Best rates guaranteed when you book direct."},
			[]string{"Deluxe room KES 8,500 a night, VAT and catering levy included.", "Check-in from 2pm. Free cancellation up to 48 hours before."}},
	}
	for _, c := range cases {
		for _, in := range c.blocked {
			t.Run(c.typ+"/blocked/"+in, func(t *testing.T) {
				if _, blocked, _ := f.CheckForTenant(in, c.typ); !blocked {
					t.Fatalf("%s: expected %q to be blocked", c.typ, in)
				}
			})
		}
		for _, in := range c.clean {
			t.Run(c.typ+"/clean/"+in, func(t *testing.T) {
				out, blocked, reason := f.CheckForTenant(in, c.typ)
				if blocked {
					t.Fatalf("%s: clean copy %q was blocked (%s)", c.typ, in, reason)
				}
				if out != in {
					t.Fatalf("%s: copy must pass unchanged, got %q", c.typ, out)
				}
			})
		}
	}
}

// TestFilterSelection: each type gets its own rules only, FLP keeps its strict filter, and
// unknown or empty types get the generic rules.
func TestFilterSelection(t *testing.T) {
	f := NewFilter()
	pharmacyOnly := "Get 20% off amoxicillin this week."
	if _, blocked, _ := f.CheckForTenant(pharmacyOnly, "restaurant"); blocked {
		t.Fatal("a pharmacy rule must not block a restaurant")
	}
	if _, blocked, reason := f.CheckForTenant(pharmacyOnly, "pharmacy"); !blocked || !strings.Contains(reason, "Pharmacy") {
		t.Fatalf("pharmacy rule not applied or reason unclear: %q", reason)
	}
	// FLP: "treats" is blocked outright (strict filter), and a clean answer gets the disclaimer.
	if _, blocked, _ := f.CheckForTenant("Aloe treats ulcers.", "flp"); !blocked {
		t.Fatal("flp must keep its strict rules")
	}
	if _, blocked, _ := f.CheckForTenant("Aloe treats ulcers.", "generic"); blocked {
		t.Fatal("generic must not get the flp rules")
	}
	out, _, _ := f.CheckForTenant("Our aloe supports digestive wellness.", "FLP")
	if !strings.Contains(out, "Food and Drug Administration") {
		t.Fatalf("flp disclaimer missing: %q", out)
	}
	for _, bt := range []string{"", "codevertex", "unknown-type"} {
		if _, blocked, _ := f.CheckForTenant("This tea cures headaches.", bt); !blocked {
			t.Fatalf("type %q must get the generic baseline", bt)
		}
		if _, blocked, _ := f.CheckForTenant("Only 2 rooms left!", bt); blocked {
			t.Fatalf("type %q must not get another type's rules", bt)
		}
	}
	// The legacy cafe persona is the restaurant type.
	if _, blocked, _ := f.CheckForTenant("Our menu is 100% nut-free.", "cafe"); !blocked {
		t.Fatal("cafe must get the restaurant rules")
	}
}

func TestPromptRules(t *testing.T) {
	p := PromptRules("pharmacy")
	for _, want := range []string{"Pharmacy or chemist", "prescription-only", "Pharmacy and Poisons Board", "real earlier price"} {
		if !strings.Contains(p, want) {
			t.Fatalf("pharmacy prompt rules miss %q:\n%s", want, p)
		}
	}
	g := PromptRules("")
	if !strings.Contains(g, "Other business") || strings.Contains(g, "prescription") {
		t.Fatalf("empty type must get only the generic rules:\n%s", g)
	}
	if !strings.HasPrefix(g, "\n") {
		t.Fatal("prompt rules must start on a new line so they can be appended")
	}
	if strings.Contains(PromptRules("hospitality"), "\u2014") {
		t.Fatal("prompt rules must not use em dashes")
	}
}
