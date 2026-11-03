package rag

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

func TestIntegrationFormPrefill(t *testing.T) {
	cases := []struct {
		name     string
		business string
		intent   ai.Intent
		question string
		want     map[string]string // nil means no form
	}{
		{
			name:     "client report, typed as key=value",
			business: "codevertex",
			intent:   ai.IntentIntegrationInterest,
			question: "Please submit an integration request: type=camera_systems, mode=self_serve, name=Sharley Mwajuma, email=sharley@example.com, notes=Hello World!",
			want: map[string]string{
				"request_type": "camera_systems", "integration_mode": "self_serve",
				"requester_name": "Sharley Mwajuma", "requester_email": "sharley@example.com", "notes": "Hello World!",
			},
		},
		{
			name:     "plain words",
			business: "codevertex",
			intent:   ai.IntentIntegrationInterest,
			question: "Hi, my name is Jane Akinyi. We want to connect our CCTV cameras, I'm on 0712345678 or jane@shop.co.ke",
			want: map[string]string{
				"request_type": "camera_systems", "requester_name": "Jane Akinyi",
				"requester_phone": "0712345678", "requester_email": "jane@shop.co.ke",
			},
		},
		{
			name:     "intent from keywords only when the message asks",
			business: "codevertex",
			intent:   ai.IntentServiceInquiry,
			question: "Can you set up eTIMS for my shop?",
			want:     map[string]string{"request_type": "etims_integration"},
		},
		{
			name:     "naming an integration is not asking for one",
			business: "codevertex",
			intent:   ai.IntentServiceInquiry,
			question: "Does your POS support eTIMS?",
			want:     nil,
		},
		{
			name:     "other personas never get the form",
			business: "flp",
			intent:   ai.IntentIntegrationInterest,
			question: "integrate my arduino",
			want:     nil,
		},
		{
			name:     "values outside auth-api's rules are dropped, not guessed",
			business: "codevertex",
			intent:   ai.IntentIntegrationInterest,
			question: "type=teleporter, mode=whatever, email=not-an-email, notes=" + strings.Repeat("x", 2001),
			want:     map[string]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IntegrationForm(tc.business, tc.intent, tc.question)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("want no form, got %+v", got)
				}
				return
			}
			if got == nil || got.Kind != FormIntegration {
				t.Fatalf("want the integration form, got %+v", got)
			}
			if !reflect.DeepEqual(got.Prefill, tc.want) {
				t.Fatalf("prefill\n got %v\nwant %v", got.Prefill, tc.want)
			}
		})
	}
}
