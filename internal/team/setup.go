package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/library"
)

// Team setup through Vera (owner decision 2026-11-08). The owner asks in the portal chat or on
// WhatsApp to set up or show the marketing team; marketflow-api fits the prebuilt team to the
// business with rules (marketingos/team_setup.go) and this package turns the outcome into an org
// chart reply. No model is called: the reply is built from the roster in team.yaml.

// MemberFit is marketflow-api's verdict on one member.
type MemberFit struct {
	Key      string   `json:"key"`
	Enabled  bool     `json:"enabled"`
	Reason   string   `json:"reason,omitempty"`
	Limits   []string `json:"limits,omitempty"`
	Changed  bool     `json:"changed,omitempty"`
	OwnerSet bool     `json:"owner_set,omitempty"`
}

// TeamSetup is marketflow-api's setup outcome.
type TeamSetup struct {
	First   bool        `json:"first"`
	Members []MemberFit `json:"members"`
}

// TeamSetupper runs the setup in marketflow-api. bearer is the portal user's own token (the
// portal route checks their role); empty means the internal route, used for a WhatsApp sender
// whose identity and role the assistant already verified.
type TeamSetupper interface {
	SetupTeam(ctx context.Context, tenantID uuid.UUID, bearer string) (TeamSetup, error)
}

// SetupTeam implements TeamSetupper and drops the cached context, so the next request sees the
// changed members at once.
func (s *APISource) SetupTeam(ctx context.Context, tenantID uuid.UUID, bearer string) (TeamSetup, error) {
	var out TeamSetup
	if s == nil || s.BaseURL == "" {
		return out, fmt.Errorf("team setup: marketflow-api not configured")
	}
	base := strings.TrimSuffix(s.BaseURL, "/")
	var req *http.Request
	var err error
	if bearer != "" {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/agents/team/setup", nil)
		if err == nil {
			req.Header.Set("Authorization", bearer)
			req.Header.Set("X-Tenant-ID", tenantID.String())
		}
	} else {
		if s.Key == "" {
			return out, fmt.Errorf("team setup: no service key")
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/internal/os/team/setup?tenant_id="+tenantID.String(), nil)
		if err == nil {
			req.Header.Set("X-API-Key", s.Key)
		}
	}
	if err != nil {
		return out, err
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("team setup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return out, ErrTeamForbidden
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("team setup: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return out, fmt.Errorf("team setup: %w", err)
	}
	if s.Redis != nil {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 300*time.Millisecond)
		_ = s.Redis.Del(dctx, s.Namespace+":os:ctx:"+tenantID.String()).Err()
		cancel()
	}
	return out, nil
}

// ErrTeamForbidden is a caller who may not change the team (not the owner or an admin).
var ErrTeamForbidden = fmt.Errorf("only the account owner or an admin can set up the marketing team")

// teamRequest matches asks to set up, show or review the team, in English and Swahili. A request
// for marketing work that only mentions the team ("ask my team to plan posts") does not match.
var teamRequest = regexp.MustCompile(`(?i)^\W*(please\s+)?(` +
	`(set\s*up|setup|create|build|make|form|assemble|hire|review|check|update|refresh|show|list)\s+(me\s+)?(a|my|our|the)?\s*(marketing\s+)?team\b` +
	`|(who\s+is|who's|what\s+is)\s+(on|in)\s+(my|our|the)\s+(marketing\s+)?team` +
	`|(my|our|the)?\s*(marketing\s+)?team\s+(structure|chart|members|org\s*chart|setup)` +
	`|org(anisation|anization)?\s*chart|organogram` +
	`|(unda|tengeneza|anzisha|panga|nionyeshe|onyesha|kagua)\s+(timu|kikundi)` +
	`|timu\s+yangu)`)

// IsTeamRequest reports whether the owner is asking about the team itself.
func IsTeamRequest(text string) bool { return teamRequest.MatchString(strings.TrimSpace(text)) }

// toolLabels say what each tool lets a member do, in the owner's terms. They name no endpoint,
// key or service, and every one is read only or a proposal.
var toolLabels = map[string]string{
	"kb_search":            "searches your knowledge base",
	"get_product":          "reads your product and service catalogue",
	"query_service_data":   "reads your sales, stock and order figures (read only)",
	"web_search":           "searches the public web",
	"calculate_ad_cost":    "prices ads before anything is spent",
	"check_integrations":   "checks which pages and ad accounts are connected",
	"search_images":        "finds pictures you can use",
	"generate_links":       "makes your shop and tracking links",
	"get_contact_360":      "reads a customer's history with you",
	"meeting_prep":         "prepares notes before a meeting",
	"get_compliance_rules": "checks your banned claims and advertising rules",
}

// OrgChart writes the team as a short plain-text org chart, fit for WhatsApp (no tables, no
// markdown) and the portal chat alike. overrides carry the names the owner chose.
func OrgChart(lib *library.Library, setup TeamSetup, overrides []MemberOverride) string {
	names := map[string]string{}
	for _, o := range overrides {
		if strings.TrimSpace(o.Name) != "" {
			names[o.Key] = o.Name
		}
	}
	fit := map[string]MemberFit{}
	for _, f := range setup.Members {
		fit[f.Key] = f
	}
	nameOf := func(m library.Member) string {
		if n := names[m.Key]; n != "" {
			return n
		}
		return m.Name
	}
	var b strings.Builder
	var manager library.Member
	var reports []library.Member
	for _, m := range lib.Members() {
		if m.ReportsTo == "" {
			manager = m
		} else {
			reports = append(reports, m)
		}
	}
	if setup.First {
		b.WriteString("Your marketing team is ready.\n\n")
	} else {
		b.WriteString("I reviewed your marketing team.\n\n")
	}
	fmt.Fprintf(&b, "%s, %s. You talk to me; I plan the work, give it to the right person and check it before you see it. I never post or spend without your approval.\n\n", nameOf(manager), manager.Role)

	var on, off []library.Member
	for _, m := range reports {
		if f, ok := fit[m.Key]; ok && !f.Enabled {
			off = append(off, m)
		} else {
			on = append(on, m)
		}
	}
	fmt.Fprintf(&b, "Reporting to %s:\n", nameOf(manager))
	for i, m := range on {
		fmt.Fprintf(&b, "%d. %s, %s. %s\n", i+1, nameOf(m), m.Role, m.Description)
		if skills := playbookNames(lib, m.Playbooks, 4); skills != "" {
			fmt.Fprintf(&b, "   Skills: %s.\n", skills)
		}
		if tools := toolSummary(m.Tools); tools != "" {
			fmt.Fprintf(&b, "   Can: %s.\n", tools)
		}
		for _, l := range fit[m.Key].Limits {
			fmt.Fprintf(&b, "   For now it %s.\n", l)
		}
	}
	if len(off) > 0 {
		b.WriteString("\nOff for now:\n")
		for _, m := range off {
			fmt.Fprintf(&b, "- %s: %s.\n", nameOf(m), fit[m.Key].Reason)
		}
	}
	var changed []string
	for _, f := range setup.Members {
		if f.Changed {
			state := "switched off"
			if f.Enabled {
				state = "switched back on"
			}
			for _, m := range reports {
				if m.Key == f.Key {
					changed = append(changed, nameOf(m)+" "+state)
				}
			}
		}
	}
	if len(changed) > 0 {
		fmt.Fprintf(&b, "\nWhat I changed: %s. Tell me to switch anyone back on and I will.\n", strings.Join(changed, "; "))
	}
	b.WriteString("\nTo give the team work, tell me what you need, for example \"plan this month's posts\" or \"write three ads for our best seller\". I quote credits before anything is made.")
	return b.String()
}

func playbookNames(lib *library.Library, keys []string, max int) string {
	var out []string
	for _, k := range keys {
		if len(out) == max {
			break
		}
		if p, ok := lib.Playbook(k); ok && p.Name != "" {
			out = append(out, strings.ToLower(p.Name))
		}
	}
	return strings.Join(out, ", ")
}

func toolSummary(tools []string) string {
	var out []string
	for _, t := range tools {
		if l, ok := toolLabels[t]; ok {
			out = append(out, l)
		}
	}
	return strings.Join(out, "; ")
}
