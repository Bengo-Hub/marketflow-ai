# MarketFlow AI — Master Implementation Plan

**Last updated:** 2026-10-22 (Marketing OS, Sprint 21, in progress; the sprint sections below are history)

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
- Was meant to be configured via `MF_AI_PRIMARY_PROVIDER=anthropic`; that variable is never read (Claude was only ever the last fallback). Sprint 21 replaces it with the routing policy.

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

## Sprint 7 — Vera AI Security Hardening ✅ COMPLETE

**Deployed:** 2026-05-22

### Phase 1 — Jailbreak Defense ✅
- `internal/rag/guard.go` (new) — `ScreenInput()` (18 regex patterns) + `ScreenOutput()` + `identityAnchor()`
- Identity anchor prepended to all 6 system prompt builders (`buildSystemPrompt`, `buildCodevertexSystemPrompt`, `buildGenericMarketingPrompt`, `buildAgentSystemPrompt`, `buildCodevertexAgentSystemPrompt`, `buildGenericMarketingAgentPrompt`)
- `ScreenInput()` called before LLM in both `Chat()` and `ChatStream()` — returns fixed identity rejection without burning tokens
- `ScreenOutput()` called on all LLM responses (agent + single-shot) before compliance filter

### Phase 2 — Rate Limiting ✅
- `internal/middleware/ratelimit.go` (new) — Redis sorted-set sliding-window, Lua atomic script
- Limits: 15 req/min per IP, 100 req/min per tenant on `/api/v1/public`
- Input length cap: 1000 chars in `internal/httpapi/handlers/chat.go` (returns 400 before LLM call)

### Phase 5 — Agent Tool Safety ✅
- `internal/agent/types.go` — added `PublicMode bool` to `Input`
- `internal/agent/loop.go` — max 5 tool calls per LLM turn; mutation tools blocked in `PublicMode` (create_deal, create_campaign, book_meeting, create_task, create_activity, create_ticket, draft_email, move_pipeline_stage, enrich_contact)

### Phase 6 — Compliance Filter Hardening ✅
- `internal/compliance/filter.go` — all string matching replaced with `*regexp.Regexp` patterns covering all tense/form variants (`curing`, `healed`, `treats`, etc.)

---

## Environment Variables

```env
# Names as read by internal/config/config.go (prefix MF_AI_ unless noted).
# Platform values saved on /platform/providers (ProviderSetting) win over these.
MF_AI_GROQ_API_KEY=<REPLACE_ME>
MF_AI_GROQ_MAIN_MODEL=llama-3.3-70b-versatile
MF_AI_GROQ_FALLBACK_MODEL=llama-3.1-8b-instant
MF_AI_GROQ_ROUTER_MODEL=llama-3.1-8b-instant
MF_AI_OLLAMA_URL=http://ollama.shared-infra.svc.cluster.local:11434
MF_AI_CLAUDE_API_KEY=<REPLACE_ME>
MF_AI_CLAUDE_MODEL=claude-haiku-5-5        # premium class default from Sprint 21
MF_AI_MARKETFLOW_API_URL=http://marketflow-api.marketflow.svc.cluster.local:4000
MF_AI_MARKETFLOW_API_KEY=<REPLACE_ME>
MF_AI_TICKETING_URL=http://ticketing-api.ticketing.svc.cluster.local:4009
INTERNAL_SERVICE_KEY=<REPLACE_ME>          # no prefix; shared S2S key
SUBSCRIPTION_BASE_URL=http://subscription-api.subscriptions.svc.cluster.local:4000
MF_SECURITY_SUBSCRIPTIONS_API_KEY=<REPLACE_ME>
REDIS_ADDR=redis.redis.svc.cluster.local:6379
POSTGRES_URL=<REPLACE_ME>
```

`MF_AI_PRIMARY_PROVIDER` is defined but never read; from Sprint 21 the routing policy decides providers per model class.

---

## S2S Integration

All services (including codevertex-website) must configure:
```
INTERNAL_SERVICE_KEY=<shared-key>
MARKETFLOW_AI_URL=https://marketflowai.codevertexafrica.com
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

---

## Ads Autopilot (planned 2026-10-22)

Design: `.claude/plans/marketflow-ads-autopilot-2026-10-22.md`. Sprint files: `docs/sprint-plans/sprint-14` to `sprint-20` in the marketflow docs folder.

| Phase | marketflow-ai scope |
|---|---|
| 0 Foundations (Sprint 14) | Compliance filter for every tenant, with FLP rules on top (`internal/compliance/filter.go`); 30-day `chat_sessions` TTL with a daily RunOnce prune and a rolling memory summary (`internal/sessions/service.go`) |
| 1 Creative studio (Sprint 15) | Provider port (Quote, Submit, Status, Fetch, Cancel); adapters for Creatomate, Shotstack, HeyGen, Creatify, fal.ai, Azure and ElevenLabs TTS; model registry in platform ProviderSetting rows with output hosting fields; router with fallback and canary; shutdown-date watcher; JSON script generation with hooks; JSON Patch revisions with scene hash cache; bounded context; golden-brief eval; output hosting decision (provider public, provider share, public R2 copy, private option); cost and margin report |
| 3 WhatsApp assistant (Sprint 17) | Consumer for `notifications.whatsapp.inbound`; verified-phone identity and permissions; brief, quote, review and publish flow with buttons; proposals with single-use confirmation tokens (15 min, action-hash bound); OTP above a threshold |
| 6 Autopilot (Sprint 20) | "Describe your agent" builder; proposal-style tools (`generate_ad_brief`, `generate_script`, `render_creative`, `schedule_publish`, `launch_campaign`, `pause_campaign`, `get_campaign_insights`); `content_planner` weekly calendar |

Security rules for this work: the LLM never mutates spend or publishing directly. Third-party text (lead messages, comments, fetched pages) goes through a quarantined summariser with no tools. Outputs are schema-validated. URL fetches pass an SSRF guard. Lead PII never goes to generation providers.

## Marketing OS (Sprint 21, in progress since 2026-10-22)

Design: `.claude/plans/marketflow-marketing-os-2026-10-22.md`. Checklist: `marketflow/docs/sprint-plans/sprint-21-marketing-os-brain.md`. Modules that use it later: Sprints 22 (social inbox), 23 (sales follow-up) and 24 (competitor ad intelligence).

| Package | Purpose | Status |
|---|---|---|
| `internal/ai` | `ChatClient` interface for every provider; Anthropic prompt caching, effort and cache usage; JSON response format | Written, not committed |
| `internal/llm` | Model router: classes nano, standard, premium, premium_plus; platform policy `ai_routing/policy`; breaker per model; usage meter and Prometheus cost counters; class-bound clients for legacy callers | Written, not committed |
| `internal/decide` | Jev decide port (TypeSafe, OpenRouter, Groq fallback) for Choice, Score and Noul decisions | Planned in this sprint |
| `internal/library` | Embedded skill playbooks (from the marketingskills plugin, MIT) and a curated prompts.chat subset (CC0), with NOTICE | Planned in this sprint |
| `internal/promptopt` | Prompt optimiser: deterministic parsing plus decide, nano only for wording | Planned in this sprint |
| `internal/team` | Marketing Manager plus specialists, manager routing or planning, Reviewer | Planned in this sprint |
| `internal/registry` | Model registry read side and the credit cost engine shared with Sprint 15; Creative Plan quote | Planned in this sprint |

New routes: `POST /api/v1/os/ask`, `POST /api/v1/os/optimize`, `POST /api/v1/os/plan/quote`, `GET /api/v1/os/library/templates`, `POST /api/v1/agent/run` (S2S; the route the worker always called), `POST /api/v1/internal/os/daily-brief` (S2S).

Fixes folded in: the public widget gets a read-only tool allowlist (it could reach `query_service_data`, `get_contact_360`, `analyze_deal`, `meeting_prep`, `score_lead` and `request_integration`); the duplicated fallback ladder in `rag/flow.go` and `rag/stream.go` moves into the router; CRM tool model calls are metered.
