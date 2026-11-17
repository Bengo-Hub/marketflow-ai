package rag

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// FormPrefill tells the widget to open one of its in-chat forms with the details the visitor
// already typed. Vera never submits it: the visitor checks the fields and taps Submit, and the
// widget posts the form to the service that owns the record (auth-api for integration
// requests). That keeps every write out of the public tool allowlist.
type FormPrefill struct {
	Kind    string            `json:"kind"`
	Prefill map[string]string `json:"prefill,omitempty"`
}

// FormIntegration is the "Request an Integration" form.
const FormIntegration = "integration"

// Integration request types offered on the widget's form (marketflow-ui public/widget/chat.js
// INTEGRATION_TYPES). auth-api stores any lowercase request_type, so this list and the widget's
// are the whole contract. arduino_iot is the retired "IoT" choice: an old prefill of it is read
// as plc_automation (see legacyIntegrationTypes).
var (
	integrationTypes = map[string]bool{
		"etims_integration": true, "payments_integration": true, "notifications_integration": true,
		"sso_integration": true, "api_integration": true, "data_import": true,
		"printing_hardware": true, "truload_weighing": true, "camera_systems": true,
		"plc_automation": true, "other_integration": true,
	}
	legacyIntegrationTypes = map[string]string{"arduino_iot": "plc_automation"}
	// Checked in order, first match wins: eTIMS before payments ("KRA invoices paid by M-Pesa"),
	// weighing before cameras (a number-plate camera belongs to the weighbridge). Words are
	// picked so they do not hide inside common words: "email" would match every pasted summary,
	// "api" sits inside "rapid" and "import" inside "important". A leading space marks a short
	// word that must start a word (typeFromWords pads the text with one).
	integrationTypeWords = []struct{ word, requestType string }{
		{"etims", "etims_integration"}, {" kra", "etims_integration"}, {"fiscal", "etims_integration"},
		{"mpesa", "payments_integration"}, {"m-pesa", "payments_integration"}, {"airtel money", "payments_integration"},
		{"paystack", "payments_integration"}, {"payhero", "payments_integration"}, {"stk push", "payments_integration"},
		{"whatsapp", "notifications_integration"}, {"bulk sms", "notifications_integration"}, {"send sms", "notifications_integration"},
		{"single sign", "sso_integration"}, {" sso", "sso_integration"}, {"oidc", "sso_integration"},
		{"webhook", "api_integration"}, {"rest api", "api_integration"}, {"your api", "api_integration"},
		{"migrat", "data_import"}, {"data import", "data_import"}, {"import my", "data_import"}, {"import our", "data_import"},
		{"printer", "printing_hardware"}, {"barcode", "printing_hardware"}, {"label print", "printing_hardware"},
		{"truload", "truload_weighing"}, {"weighbridge", "truload_weighing"}, {"weighing", "truload_weighing"}, {"axle", "truload_weighing"},
		{"anpr", "truload_weighing"}, {"number plate", "truload_weighing"}, {"number-plate", "truload_weighing"},
		{"camera", "camera_systems"}, {"cctv", "camera_systems"},
		{"plc", "plc_automation"}, {"sensor", "plc_automation"},
		{"arduino", "plc_automation"}, {" iot", "plc_automation"},
	}
	// requestWords mark a message that asks for an integration, not one that only names one
	// ("does POS work with eTIMS?" names it; "connect our cameras" asks for it).
	requestWords = []string{"integrat", "connect", "request", "submit", "set up", "setup", "hook up", "install"}

	// Field limits match auth-api's validation, so a prefilled form always passes it.
	integrationFieldMax = map[string]int{
		"requester_name": 120, "requester_email": 254, "requester_phone": 32,
		"company_name": 160, "notes": 2000,
	}
	integrationKeys = map[string]string{
		"type": "request_type", "request_type": "request_type", "integration": "request_type",
		"mode": "integration_mode", "integration_mode": "integration_mode",
		"name": "requester_name", "requester_name": "requester_name", "full_name": "requester_name",
		"email": "requester_email", "requester_email": "requester_email",
		"phone": "requester_phone", "requester_phone": "requester_phone", "whatsapp": "requester_phone",
		"company": "company_name", "company_name": "company_name", "business": "company_name",
		"notes": "notes", "note": "notes", "message": "notes", "details": "notes",
	}

	// keyValueRe matches only the known field names, so "request: type=camera_systems" reads
	// type, not "request" swallowing the pair after it.
	keyValueRe = regexp.MustCompile(`(?i)\b(` + keyAlternation() + `)\s*[=:]\s*([^,;\n]+)`)
	emailRe    = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	phoneRe    = regexp.MustCompile(`(?:\+?254|\b0)[17]\d{8}\b`)
	myNameRe   = regexp.MustCompile(`(?i)\bmy name is ([a-z][a-z'\-]+(?: [a-z][a-z'\-]+){0,3})`)
)

// IntegrationForm returns the integration form to open for this message, or nil. It applies to
// the Codevertex persona only, because the integrations on the form are Codevertex products. An
// empty business type is detected from the question the same way the pipeline does it, so the
// handler and the pipeline always agree. The form opens on the integration intent, or when the
// message itself names an integration and asks for it.
func IntegrationForm(businessType string, intent ai.Intent, question string) *FormPrefill {
	if businessType == "" {
		businessType = detectNiche(question)
	}
	if businessType != "" && businessType != "codevertex" {
		return nil
	}
	prefill := parseIntegrationPrefill(question)
	if intent != ai.IntentIntegrationInterest && (prefill["request_type"] == "" || !asksForIntegration(question)) {
		return nil
	}
	return &FormPrefill{Kind: FormIntegration, Prefill: prefill}
}

// keyAlternation lists the field names longest first, for keyValueRe.
func keyAlternation() string {
	keys := make([]string, 0, len(integrationKeys))
	for k := range integrationKeys {
		keys = append(keys, regexp.QuoteMeta(k))
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	return strings.Join(keys, "|")
}

func asksForIntegration(question string) bool {
	q := strings.ToLower(question)
	for _, w := range requestWords {
		if strings.Contains(q, w) {
			return true
		}
	}
	return false
}

// parseIntegrationPrefill reads only what the visitor wrote: key=value pairs first (the widget's
// own summary format and people who copy it), then a bare email and Kenyan phone number, the
// integration named in the message, and "my name is ...". Values that do not fit auth-api's
// enums or limits are dropped, never guessed.
func parseIntegrationPrefill(question string) map[string]string {
	out := map[string]string{}
	for _, m := range keyValueRe.FindAllStringSubmatch(question, -1) {
		field, ok := integrationKeys[strings.ToLower(m[1])]
		if !ok || out[field] != "" {
			continue
		}
		out[field] = strings.TrimSpace(m[2])
	}
	if out["requester_email"] == "" {
		out["requester_email"] = emailRe.FindString(question)
	}
	if out["requester_phone"] == "" {
		out["requester_phone"] = phoneRe.FindString(question)
	}
	if out["requester_name"] == "" {
		if m := myNameRe.FindStringSubmatch(question); m != nil {
			out["requester_name"] = m[1]
		}
	}

	if t := strings.ToLower(strings.ReplaceAll(out["request_type"], " ", "_")); integrationTypes[t] {
		out["request_type"] = t
	} else if legacy, ok := legacyIntegrationTypes[t]; ok {
		out["request_type"] = legacy
	} else {
		out["request_type"] = typeFromWords(out["request_type"] + " " + question)
	}
	out["integration_mode"] = normaliseMode(out["integration_mode"] + " " + question)

	if !emailRe.MatchString(out["requester_email"]) || emailRe.FindString(out["requester_email"]) != out["requester_email"] {
		out["requester_email"] = ""
	}
	for field, max := range integrationFieldMax {
		if utf8.RuneCountInString(out[field]) > max {
			out[field] = ""
		}
	}
	for k, v := range out {
		if v == "" {
			delete(out, k)
		}
	}
	return out
}

func typeFromWords(text string) string {
	t := " " + strings.ToLower(text)
	for _, w := range integrationTypeWords {
		if strings.Contains(t, w.word) {
			return w.requestType
		}
	}
	return ""
}

func normaliseMode(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "self_serve"), strings.Contains(t, "self serve"), strings.Contains(t, "self-serve"),
		strings.Contains(t, "own developer"), strings.Contains(t, "our developer"), strings.Contains(t, "in-house"):
		return "self_serve"
	case strings.Contains(t, "assisted"), strings.Contains(t, "your team"), strings.Contains(t, "codevertex team"):
		return "assisted"
	}
	return ""
}
