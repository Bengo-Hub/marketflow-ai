package ai

import "testing"

func TestRecoverToolUseFailed(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantOK    bool
		wantName  string
		wantArgs  string
		wantCalls int
	}{
		{
			name:      "equals-form single call",
			body:      `{"error":{"code":"tool_use_failed","failed_generation":"<function=kb_search={\"category\": \"product\", \"query\": \"Forever Aloe Vera Gel\", \"topK\": 1}</function>"}}`,
			wantOK:    true,
			wantName:  "kb_search",
			wantArgs:  `{"category": "product", "query": "Forever Aloe Vera Gel", "topK": 1}`,
			wantCalls: 1,
		},
		{
			name:      "angle-form with nested braces",
			body:      `{"error":{"code":"tool_use_failed","failed_generation":"<function=calculate_ad_cost>{\"budget\":{\"daily\":10,\"total\":300}}</function>"}}`,
			wantOK:    true,
			wantName:  "calculate_ad_cost",
			wantArgs:  `{"budget":{"daily":10,"total":300}}`,
			wantCalls: 1,
		},
		{
			name:      "two calls",
			body:      `{"error":{"code":"tool_use_failed","failed_generation":"<function=kb_search={\"q\":\"a\"}</function><function=search_images={\"q\":\"b\"}</function>"}}`,
			wantOK:    true,
			wantCalls: 2,
		},
		{
			name:   "different error code is not recoverable",
			body:   `{"error":{"code":"rate_limit_exceeded","failed_generation":"<function=kb_search={\"q\":\"a\"}</function>"}}`,
			wantOK: false,
		},
		{
			name:   "invalid json args yields no recovery",
			body:   `{"error":{"code":"tool_use_failed","failed_generation":"<function=kb_search={not json}</function>"}}`,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := recoverToolUseFailed([]byte(tc.body), "llama-3.3-70b-versatile")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			calls := got.Choices[0].Message.ToolCalls
			if len(calls) != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", len(calls), tc.wantCalls)
			}
			if tc.wantName != "" && calls[0].Function.Name != tc.wantName {
				t.Errorf("name = %q, want %q", calls[0].Function.Name, tc.wantName)
			}
			if tc.wantArgs != "" && calls[0].Function.Arguments != tc.wantArgs {
				t.Errorf("args = %q, want %q", calls[0].Function.Arguments, tc.wantArgs)
			}
			if got.Choices[0].Finish != "tool_calls" {
				t.Errorf("finish = %q, want tool_calls", got.Choices[0].Finish)
			}
		})
	}
}
