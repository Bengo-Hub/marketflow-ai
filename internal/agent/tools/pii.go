package tools

import (
	"regexp"
	"strings"
)

// Service data reaches the model as marketing facts, never as people's details. These word lists
// decide which JSON keys are dropped before a response is shown to the model (decision 61): a
// key is split into lower-case words (camelCase, snake_case and kebab-case alike) and dropped when
// any word, or the whole key, matches.
var (
	// piiWords mark a field that holds a person's details or a secret.
	piiWords = wordSet(
		"email", "phone", "mobile", "msisdn", "whatsapp", "telephone", "tel",
		"address", "street", "postcode", "postal", "zip",
		"password", "passcode", "secret", "token", "apikey", "otp", "pin",
		"passport", "national", "nationalid", "idnumber", "kra", "dob", "birth", "birthday", "gender",
		"ip", "useragent", "device", "imei",
		"card", "iban", "mpesa", "msisdn", "account", "bank",
		"firstname", "lastname", "fullname", "surname", "signature",
	)
	// personWords mark a field (or a whole object) about a person rather than the business: a
	// customer, a member of staff, a rider. The value is dropped unless it is a plain number or
	// true/false, so "new_customers": 12 stays and "customer": {...} goes.
	personWords = wordSet(
		"customer", "customers", "buyer", "client", "recipient", "receiver", "guest", "user", "users",
		"staff", "employee", "cashier", "waiter", "rider", "driver", "courier",
		"student", "contact", "contacts", "billing", "shipping", "createdby", "updatedby", "servedby",
		"approvedby", "owner",
	)
)

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// keyWords splits a JSON key into lower-case words, plus the whole key with separators removed.
func keyWords(k string) []string {
	k = camelBoundary.ReplaceAllString(k, "${1}_${2}")
	parts := splitKey.Split(strings.ToLower(k), -1)
	out := make([]string, 0, len(parts)+1)
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return append(out, strings.Join(out, ""))
}

var splitKey = regexp.MustCompile(`[^a-z0-9]+`)

func sensitiveKey(k string, val any) bool {
	scalar := false
	switch val.(type) {
	case float64, bool, nil:
		scalar = true
	}
	for _, w := range keyWords(k) {
		if piiWords[w] || (personWords[w] && !scalar) {
			return true
		}
	}
	return false
}

// redactPII returns v without the keys that hold personal details, recursively. It counts what
// it dropped so the tool can tell the model that details were held back.
func redactPII(v any, dropped *int) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if sensitiveKey(k, val) {
				*dropped++
				continue
			}
			out[k] = redactPII(val, dropped)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redactPII(val, dropped)
		}
		return out
	default:
		return v
	}
}

func wordSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}
