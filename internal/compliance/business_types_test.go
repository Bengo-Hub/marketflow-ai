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
		"exempt without med group": "types:\n  - {key: generic, label: G, description: d}\n  - {key: flp, label: F, description: d, blocks: [{pattern: 'dawa', reason: r, exempt: 'dawa ya'}]}\n",
		"negation without promo group":"types:\n  - {key: generic, label: G, description: d}\n  - {key: flp, label: F, description: d, blocks: [{pattern: 'discount', reason: r, allow_negated: true}]}\n",
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
		// Pharmacy rules rest on the Pharmacy and Poisons Act (Cap 244) ss. 36 to 39 and Schedule,
		// L.N. 98 of 2022 rules 22 to 38 and the PPB advertising guideline Rev. 3 (July 2025); the
		// clause for each block is in business_types.yaml.
		{"pharmacy",
			[]string{
				// prescription or doctor not needed (guideline 6.0 (m))
				"No prescription needed for any medicine.",
				"No need to see a doctor, just order online.",
				// no side effects or complete safety (guideline 16.0 (c), (d))
				"Safe painkiller with no side effects.",
				"No harmful effects at all.",
				"Completely safe for the whole family.",
				"Our herbal capsules are natural and safe.",
				// guaranteed effects (guideline 10.0 (e))
				"Guaranteed relief in one hour.",
				"This syrup works every time.",
				// superlatives and hanging comparisons (guideline 10.0 (e), (f), 16.0)
				"The strongest painkiller in town.",
				"Better than any other painkiller.",
				// Schedule diseases (Act s. 37)
				"This supplement treats diabetes.",
				"Our tea prevents cancer.",
				"This herb gets rid of kidney stones.",
				"Boost your libido with our new capsules.",
				"Last longer in bed tonight.",
				// abortion (Act s. 38)
				"Abortion pills available, inbox us.",
				// PPB or WHO approval claims (guideline 17.0 (d))
				"PPB approved for your whole family.",
				"Approved by WHO.",
				// health worker endorsements (guideline 17.0 (c))
				"Doctor-recommended cough syrup.",
				"Recommended by top doctors in Nairobi.",
				"9 out of 10 pharmacists choose it.",
				// inducements on medicines (guideline 10.0 (b), (c), 12.0 (g))
				"Buy one get one free on all vitamins.",
				"Giveaway: win a month of free supplements!",
				// discount or offer copy on any medicine, general sale ones included (owner, 2026-11-14)
				"20% off Panadol this weekend.",
				"Discounted cough syrup for the whole family.",
				"Multivitamins now on offer!",
				"Half price ORS sachets today.",
				"Save KES 50 on paracetamol tablets.",
				"Punguzo kwa dawa zote leo!",
				"Punguzo kwa dawa ya kikohozi leo!",
				// a household phrase does not hide a real medicine later in the sentence
				"Punguzo kwa dawa ya meno na Panadol",
				"Dawa ya malaria ofa ya wiki hii.",
				"Big discount on Ozempic today.",
				// a negation that is part of a sales line does not make a refusal
				"Don't miss 20% off paracetamol.",
				"Antimalarials have never been cheaper.",
				"We can't wait to give you 20% off Panadol.",
				// a negation in an earlier clause does not reach the offer
				"No prescription? 20% off Panadol.",
				"We cannot discount Ozempic, but Panadol is 20% off.",
				// prescription-only and controlled medicines (rule 26(1)(c), (d), rule 38)
				"Get 20% off amoxicillin this week.",
				"Ozempic now on offer!",
				"Order now: Mounjaro pens delivered to your door.",
				"Tramadol with free delivery today.",
				// weight loss without diet and exercise (rule 31)
				"Lose 10kg in 2 weeks with our slimming tea.",
				"Lose weight without diet or exercise.",
			},
			[]string{
				"We fill prescriptions from any doctor. Talk to our pharmacist about your medicines.",
				// a general sale product with the content rule 34 and guideline 9.0 ask for
				"Panadol (paracetamol) 500mg, KES 50 for 10 tablets. For pain and fever. Do not take with other paracetamol products. If symptoms persist, seek medical advice.",
				// naming a stocked prescription-only medicine in a factual chat answer is not promotion
				"Yes, we stock amoxicillin. It needs a valid prescription, so bring yours and our pharmacist will help.",
				"Ask our pharmacist in private about erectile dysfunction.",
				"Condoms help protect against HIV and other STIs.",
				// a weight claim made the way rule 31 allows
				"Slimming tea KES 900. Use it with a balanced diet and exercise. This product has not been proven to burn fat or block starch.",
				// offers on non-medical stock and free services stay allowed
				"Buy 2 body lotions and get 1 free this weekend.",
				"Free blood pressure checks every Saturday morning.",
				"20% off all body lotions and sunscreen this weekend.",
				"Discounts on BP machines and glucometers this month.",
				"20% off vitamin C serum.",
				// refusals that name a medicine (owner, 2026-11-14)
				"We can't offer discounts on Ozempic.",
				"Sorry, we do not give discounts on medicines.",
				"Hatutoi punguzo kwa dawa za prescription.",
				"Hatuwezi kutoa ofa kwa dawa.",
				"Panadol is not on offer, but body lotions are 20% off this weekend.",
				// household and cosmetic "dawa" is not medicine (owner, 2026-11-14)
				"Punguzo kwa dawa ya meno leo!",
				"Dawa ya mbu ofa ya wiki hii.",
				"20% off dawa ya wadudu na dawa ya nywele.",
				"Punguzo kwa dawa ya meno na dawa ya mbu",
				"Ask your doctor whether this is safe during pregnancy.",
				// the advert approval reference rule 22(5) asks for, and "who" in ordinary use
				"PPB advert approval ref. PPB/ADV/0000.",
				"The pharmacist who approved your refill will call you.",
				"Take it as recommended by your doctor.",
			}},
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

// TestPharmacyRefusals: a negation before the promotion phrase in the same clause lets a refusal
// through, in English and Swahili, and Violations agrees with CheckForTenant (owner, 2026-11-14).
func TestPharmacyRefusals(t *testing.T) {
	f := NewFilter()
	cases := []struct {
		text    string
		blocked bool
	}{
		{"We can't offer discounts on Ozempic.", false},
		{"We can’t offer discounts on Ozempic.", false},
		{"We cannot give you 20% off amoxicillin.", false},
		{"We can not give discounts on Panadol.", false},
		{"We are unable to offer promotions on prescription medicines.", false},
		{"There are no discounts on antibiotics.", false},
		{"We never discount medicines.", false},
		{"Ozempic is not on offer.", false},
		{"Hatutoi punguzo kwa dawa za prescription.", false},
		{"Hakuna punguzo kwa dawa.", false},
		{"Bei ya Panadol si punguzo, ni bei ya kawaida.", false},
		{"Big discount on Ozempic today.", true},
		{"Ozempic on offer now.", true},
		{"Punguzo kwa Panadol leo!", true},
		{"Don't miss our discount on Panadol.", true},
		{"No queues. 20% off Panadol.", true},
		{"We can't discount Ozempic. Panadol 20% off though!", true},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			_, blocked, reason := f.CheckForTenant(c.text, "pharmacy")
			if blocked != c.blocked {
				t.Fatalf("blocked = %v (%s), want %v", blocked, reason, c.blocked)
			}
			if v := f.Violations(c.text, "pharmacy"); (len(v) > 0) != c.blocked {
				t.Fatalf("Violations disagrees with CheckForTenant: %v", v)
			}
		})
	}
	// Other types never get the pharmacy blocks, refusal or not.
	if _, blocked, _ := f.CheckForTenant("Big discount on Ozempic today.", "retail"); blocked {
		t.Fatal("a pharmacy rule must not block a retail shop")
	}
}

// TestBlockFindSkipsNegatedMatch checks the search goes on after a negated match, so a refusal
// does not hide a later offer in the same text.
func TestBlockFindSkipsNegatedMatch(t *testing.T) {
	blocks := []Block{{Pattern: `(?i)\b(?P<promo>discount)\b[^.!?\n]{0,20}?\bpills?\b|\bpills?\b[^.!?\n]{0,20}?\b(?P<promo>discount)\b`, Reason: "r", AllowNegated: true}}
	if err := compileBlocks(blocks, "test"); err != nil {
		t.Fatal(err)
	}
	b := &blocks[0]
	if len(b.promo) != 2 {
		t.Fatalf("want both promo groups found, got %v", b.promo)
	}
	if m, hit := b.find("no discount on pills"); hit {
		t.Fatalf("negated match reported: %q", m)
	}
	if m, hit := b.find("no discount on pills. pills at a discount"); !hit || m != "pills at a discount" {
		t.Fatalf("later offer missed: %q %v", m, hit)
	}
	if _, hit := b.find("pills"); hit {
		t.Fatal("no promotion phrase, no match")
	}
	plain := []Block{{Pattern: `(?i)\bdiscount\b`, Reason: "r"}}
	if err := compileBlocks(plain, "test"); err != nil {
		t.Fatal(err)
	}
	if _, hit := plain[0].find("no discount"); !hit {
		t.Fatal("a block without allow_negated ignores negations")
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
