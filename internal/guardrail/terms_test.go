package guardrail

import (
	"strings"
	"testing"
)

var testTerms = []Term{{Wrong: "M-Pay", Right: "M-Pesa"}, {Wrong: "Mpesa", Right: "M-Pesa"}}

func TestFixTerms(t *testing.T) {
	cases := map[string]string{
		"Pay with M-Pay today":              "Pay with M-Pesa today",
		"Lipa kwa m-pay au MPESA.":          "Lipa kwa M-Pesa au M-Pesa.",
		"M-Pay, M-Pay":                      "M-Pesa, M-Pesa",
		`{"cta":"Pay by mpesa"}`:            `{"cta":"Pay by M-Pesa"}`,
		"We already say M-Pesa":             "We already say M-Pesa",
		"M-Payment and SuperM-Pay stay put": "M-Payment and SuperM-Pay stay put",
	}
	for in, want := range cases {
		if got := FixTerms(in, testTerms); got != want {
			t.Errorf("FixTerms(%q) = %q, want %q", in, got, want)
		}
	}
	if FixTerms("M-Pay", nil) != "M-Pay" {
		t.Fatal("no glossary changes nothing")
	}
}

func TestTermsBlock(t *testing.T) {
	b := TermsBlock([]string{"M-Pesa", "cash"}, testTerms)
	for _, want := range []string{"M-Pesa, cash", `never "M-Pay"`, "Never invent a brand", "label it as an assumption"} {
		if !strings.Contains(b, want) {
			t.Errorf("block lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(TermsBlock(nil, nil), "takes payment by") {
		t.Fatal("no payment methods, no payment line")
	}
}
