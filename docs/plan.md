# MarketFlow AI — Master Implementation Plan

**Last updated:** 2026-05-12

## Overview

MarketFlow AI is the intelligence layer powering the Codevertex platform. It serves as:
- **Vera** — the embeddable AI assistant widget deployed on any tenant website or the Codevertex website itself
- **CRM AI** — enriching the MarketFlow CRM with automated lead scoring, campaign generation, and pipeline insights
- **Platform Knowledge Hub** — answering questions about all Codevertex services for tenants, operators, and end users
- **Ticketing Intelligence** — auto-resolving support tickets and routing unresolvable ones to human agents

---

## Current State (Sprint 2 Complete)

| Component | Status |
|-----------|--------|
| Groq LLM (primary) + Ollama (embeddings) | ✅ Live |
| 9-layer RAG pipeline | ✅ Live |
| Intent router (11 intents) | ✅ Live |
| Agentic tool loop (14 tools) | ✅ Live |
| FLP/ForeverLiving compliance filter | ✅ Live |
| Provider resolver (3-tier) | ✅ Live |
| Anthropic native Messages API client | ✅ Implemented |
| Session management (Redis) | ✅ Live |
| KB search + category tools | ✅ Live |
| Book meeting tool | ✅ Live |
| Web search (Google + Tavily fallback) | ✅ Live |
| Vera widget (chat.js v3.1 — Codevertex branded) | ✅ Done |
| Platform-wide KB seed JSON (32 entries) | ✅ Done |
| KB seed script (marketflow-ui/scripts/seed-kb.ts) | ✅ Done |
| Anthropic as configurable primary provider | ✅ Implemented |
| 4 new intents: service_inquiry, course_inquiry, support_request, data_query | ✅ Done |
| create_ticket agent tool | ✅ Implemented |
| query_service_data agent tool | ✅ Implemented |
| Dynamic in-chat forms (chat.js) | ✅ Done |
| Tenant-configurable WhatsApp/phone (data-whatsapp, data-phone) | ✅ Done |
| Codevertex website ChatBot.tsx linked to marketflow-ai | ✅ Done |
| Ticketing AI triage endpoint | ✅ Done (Sprint 5) |
| MarketFlow CRM AI enhancements (lead scoring, pipeline insights) | ✅ Done (Sprint 6) |

---

## Sprint 2 — COMPLETED

### 2.1 Vera Widget Redesign ✅

- Rebranded from FLP amber to Codevertex primary colour scheme
- `data-whatsapp`, `data-phone`, `data-call-url` tenant-configurable attributes
- `data-mode="platform|tenant"` — drives FAQ chips and behaviour
- `buildForm(config, onSuccess)` — dynamic in-chat form factory
- Forms: support ticket, enrollment, book-a-call, contact
- `wa.me` links (inbox format) — never WhatsApp Business API
- File: `marketflow-ui/public/widget/chat.js` (v3.1)

### 2.2 Platform Knowledge Base ✅

- Seed JSON: `marketflow-ui/public/kb/platform-seed.json` (32 entries)
- Categories: platform_overview, marketflow_crm, auth_sso, ordering_pos, finance_treasury, logistics_tracking, isp_billing, subscriptions, notifications, erp, ticketing, digitika_courses, digitika_bootcamp, pricing_plans, onboarding_howto
- Seed script: `marketflow-ui/scripts/seed-kb.ts` — canonical KB seed location
- Scope: `"platform"` — accessible to all tenants

---

## Sprint 3 — AI Provider Expansion ✅

### 3.1 Anthropic Native Client ✅

- File: `internal/ai/anthropic.go`
- Native Messages API (not OpenAI-compatible)
- `AnthropicClient` implementing same `Chat()` / `ChatStream()` interface
- Tool-use content blocks for agentic use
- Configured via `MF_AI_PRIMARY_PROVIDER=anthropic`

### 3.2 New Intents ✅

Added to `internal/ai/router.go`:
```go
IntentServiceInquiry  Intent = "service_inquiry"
IntentCourseInquiry   Intent = "course_inquiry"
IntentSupportRequest  Intent = "support_request"
IntentDataQuery       Intent = "data_query"
```

System prompt updated from FLP-specific to Codevertex platform-aware.

### 3.3 Config Additions ✅

Added to `internal/config/config.go`:
```
ClaudeBaseURL string   // default: https://api.anthropic.com/v1
ClaudeVersion string   // default: 2023-06-01
PrimaryProvider string // groq | anthropic | ollama
TicketingURL string
InternalServiceKey string
```

---

## Sprint 4 — Agent Tool Expansion ✅

### 4.1 create_ticket Tool ✅

- File: `internal/agent/tools/create_ticket.go`
- S2S POST to `{TicketingURL}/api/v1/internal/tickets`
- `X-API-Key: INTERNAL_SERVICE_KEY`
- Ticket ref format: `CVX-{id[:8]}`
- Triggered: intent = escalate/support_request OR confidence < 0.4

### 4.2 query_service_data Tool ✅

- File: `internal/agent/tools/query_service_data.go`
- Service URL registry: 8 services → k8s internal URLs
- Read-only endpoint paths per (service, resource)
- Tenant scoping enforced: `tenant_id` always injected
- Response capped at 8000 bytes
- Uses Go 1.24.11 built-in `min`

---

## Sprint 5 — Ticketing Service AI Layer ✅ COMPLETE

**Deployed:** 2026-05-22

- `internal/httpapi/handlers/triage.go` — TriageHandler with Triage + Classify endpoints
- `POST /api/v1/internal/triage` (X-API-Key) — full classification with confidence + human_review flag
- `POST /api/v1/classify` (X-API-Key) — simpler response for ticketing-api
- Groq 8B intent-router, fail-open on error (returns general/normal rather than blocking)
- Returns: `{category, priority, confidence, reasoning, requires_human_review}`

---

## Sprint 6 — MarketFlow CRM AI Enhancement ✅ COMPLETE

**Deployed:** 2026-05-22

- `internal/httpapi/handlers/crm_ai.go` — CRMAIHandler
- `POST /api/v1/ai/pipeline-summary` — fetches open deals S2S, Groq 70B analysis, returns deal_health/bottlenecks/at_risk_deals/recommended_actions/win_rate_estimate
- `POST /api/v1/ai/score-lead` — fetches lead S2S, returns score 0-100 with grade/reasoning/strengths/weaknesses/recommended_action

---

## Environment Variables

```env
# Existing
MF_GROQ_API_KEY=<REPLACE_ME>
MF_OLLAMA_URL=http://ollama.ollama.svc.cluster.local:11434
MF_REDIS_URL=redis://redis.redis.svc.cluster.local:6379

# Sprint 3–4 additions
MF_AI_PRIMARY_PROVIDER=groq          # groq | anthropic | ollama
MF_AI_CLAUDE_BASE_URL=https://api.anthropic.com/v1
MF_AI_CLAUDE_VERSION=2023-06-01
MF_ANTHROPIC_API_KEY=<REPLACE_ME>
MF_TICKETING_URL=http://ticketing-api.ticketing.svc.cluster.local:4000
MF_INTERNAL_SERVICE_KEY=<REPLACE_ME>  # shared INTERNAL_SERVICE_KEY
```

---

## S2S Integration

All services (including codevertex-website) must configure:
```
INTERNAL_SERVICE_KEY=<shared-key>
MARKETFLOW_AI_URL=https://marketflowai.codevertexitsolutions.com
```

The codevertex-website `/api/chat` route proxies to marketflow-ai first, falls back to direct Anthropic if unavailable.

---

## Key Design Decisions

| Decision | Choice | Reason |
|----------|--------|--------|
| Anthropic format | Native Messages API | Tool-use format differs; better performance natively |
| Ticket creation | S2S to ticketing-service | Single source of truth; no ticket data in marketflow-ai |
| Service data queries | S2S with tenant scoping | Security; no cross-tenant leakage |
| KB scope | platform vs tenant_id | Platform-wide + tenant-specific KB in same table |
| Widget mode | `data-mode="platform\|tenant"` | Single file; behaviour driven by attribute |
| Provider config | env primary + DB ProviderSetting overrides | Tenants can bring own Claude/Groq keys |
| wa.me format | `https://wa.me/{number}` | Inbox link (not WhatsApp Business API) |
