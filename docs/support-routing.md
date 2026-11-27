# Support routing from the Vera widget

Added 28 November 2026 for Hadia's site (hadiafrica.com, tenant `hadia`). Code: `internal/support`, the route `POST /api/v1/public/support` in `internal/httpapi/handlers/public_support.go`, wiring in `internal/app/support.go`. The widget side is marketflow-ui `public/widget/chat.js`. The wider picture is in marketflow `docs/agents-architecture.md`, "Support routing from the widget".

## What it does

A visitor who needs help fills in the widget's "Get help" form and verifies their email. I then send the request to whoever can answer it:

| The request | Where it goes |
|---|---|
| A technical or platform problem (site or app not loading, sign-in or codes failing, a payment failing because of an error, a bug) | A ticket for Codevertex technical support on the platform tenant (`CreatePlatformTicket`) |
| A question for the business (for Hadia: a registry, a gift, a payout, a refund, the owner's account, a policy), and the business's plan includes ticketing | A ticket in the business's own queue (`CreateTenantTicket`) |
| The same, and the plan has no ticketing | An email to the business's support address and a WhatsApp message to its admins, sent through notifications-api as the platform tenant (`NotifyTenantStaff`) |
| Nothing could be sent | The business's own email and WhatsApp number, shown to the visitor (`contact_only`) |

A technical request whose platform ticket fails goes down the business path, and a tenant ticket that fails becomes a staff notice. The visitor always gets a reference, a plain confirmation of where the request went, or the contact details. No reply time is promised unless `reply_time` is set.

The model never triggers any of this. It stays out of the public tool allowlist like every widget write; the visitor checks the form and taps Send.

## Classifier

`Classifier.Classify`: rules first (`RuleScores` counts fault phrases and business words). Only technical signals, with a fault phrase or two signals, is technical; only business words is business. Both or neither asks the decide layer one Choice question (`support_route`, Jev then the free fallback model, 4s budget). A confident answer (0.65 for Jev, 0.58 for the fallback) wins; anything else, a failure or a timeout gives business, the safe default, because the business's staff can forward a technical problem to Codevertex.

## Budgets and retries

| Layer | Limit |
|---|---|
| The whole request | 22s |
| One step (a ticket, the email, the WhatsApp messages) | 6s, or what is left of the 22s |
| Plan check | 2s; auth-client fails open on an outage |
| Tenant and settings lookups | 3s each |
| A step with under 1.5s left | skipped for the next fallback |

Ticket creates, message sends and the admin phone read retry a 429, a 5xx or a network error up to three times, with exponential backoff and jitter (300ms base, 2s cap, 5s per try). A 4xx is never retried. Retries are safe: each ticket has a `source_ref` keyed on the request, so ticketing-api answers a repeat with the same ticket, and each message has an `Idempotency-Key`. The email code check is one try, because a code works once.

## Calls and their exact shapes

**auth-api, email code.** `GET {AUTH_API_URL}/api/v1/s2s/otp/validate?token=<X-OTP-Token>` with `X-API-Key`. Reads `{valid, email}`. Invalid answers 401 to the widget; no answer gives `contact_only`.

**auth-api, tenant.** `GET {AUTH_API_URL}/api/v1/tenants/by-slug/{slug}` through the shared cache module (`cache.GetTenantDetails`, Redis), with `cache.ResolveSupportContact` for the support email and WhatsApp (metadata `support`, else the contact email and phone). The platform tenant is read the same way (`MF_PLATFORM_TENANT_SLUG`, default `codevertex`).

**auth-api, admin phones.** `GET {AUTH_API_URL}/api/v1/s2s/tenants/reach?purpose=broadcast&tenant_ids=<id>&include_demo=true&limit=5` with `X-API-Key`. Reads `data[].phones[] {address, source, verified, first_name}`; keeps `owner` and `admin`, verified first.

**subscriptions-api, plan.** auth-client `EntitlementClient.UsesService(tenant, "ticketing")`, which calls `GET {SUBSCRIPTION_BASE_URL}/api/v1/tenants/{id}/subscription?include_usage=false` and caches a minute.

**ticketing-api, ticket.** `POST {TICKETING_URL}/api/v1/internal/tickets` with `X-API-Key`, `X-Tenant-ID` (the platform tenant for a platform ticket, the business for a tenant ticket):

```json
{
  "subject": "Hadia: I can't log in",
  "description": "<the question>\n\nRaised by Vera, the chat assistant on hadiafrica.com, ...\nName: Jane\nEmail: jane@example.com\nPhone: 0712345678",
  "source": "vera_support",
  "priority": "high",
  "channel": "chat",
  "category": "technical",
  "source_ref": {"type": "vera_support_request", "id": "<request id>", "parent": {"type": "tenant", "id": "<hadia id>"}},
  "requester_name": "Jane",
  "requester_email": "jane@example.com",
  "requester_phone": "0712345678"
}
```

A tenant ticket has no `parent` and category `general` (or `technical` when the platform ticket could not be opened). Reads `{id, ticket_number, status}` on 201 or 200.

**notifications-api, staff notice.** `POST {NOTIFICATIONS_URL}/api/v1/{platform tenant id}/notifications/messages` with `X-API-Key`, `X-Tenant-ID` the platform tenant, `Idempotency-Key` `vera-support:<request id>:email` or `vera-support:<request id>:wa:<phone>`:

```json
{"channel": "email", "tenant": "<platform id>", "template": "broadcast/message",
 "to": ["support@hadiafrica.com"],
 "data": {"body": "<text>", "paragraphs": ["..."]},
 "metadata": {"subject": "Website support request from Jane"}}
```

```json
{"channel": "whatsapp", "tenant": "<platform id>", "template": "broadcast/message",
 "to": ["+254700000002"],
 "data": {"body": "<one line notice>"},
 "metadata": {"template_name": "broadcast_service_notice_v1",
              "template_params": ["Tom", "Vera on the Hadia website", "<one line notice>"],
              "default_dial_code": "254"}}
```

Sending as the platform tenant is the platform route for a Codevertex notice to a tenant's admins: it goes from the platform number and address, the platform pays, and the business needs no WhatsApp number or credits. The template parameter is one line, because Meta refuses line breaks in a parameter.

## Settings

ProviderSetting type `vera_support`, name `default` for the platform values and the tenant slug for one tenant (it wins key by key). A platform owner saves a row with `POST /api/v1/admin/providers/configure` on marketflow-api, which replaces every key of that row, so I send them all. I read it through `GET /api/v1/admin/providers/resolve` (cached 5 minutes). Values are strings.

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Routing on. Off, the form keeps the old ticket: ticketing-api's public route on the business's own desk |
| `platform_tickets` | `true` | Technical requests become platform tickets |
| `gate_service_tag` | `ticketing` | The service the plan must include for a tenant ticket |
| `fallback_channels` | `email,whatsapp` | Staff notice channels |
| `admin_whatsapp_source` | `admin_users` | `admin_users`, `configured` or `support_contact`; an empty source falls through to the others in the order configured, admin users, support contact |
| `admin_whatsapp_numbers` | empty | Comma separated, for `configured` |
| `max_admins` | `3` | Most admins one notice reaches |
| `support_email` | empty | Overrides the tenant's support email |
| `whatsapp_template` | `broadcast_service_notice_v1` | The approved platform template (first name, sender name, message) |
| `default_dial_code` | `254` | Reads local numbers as international |
| `reply_time` | empty | Shown as is, for example "within one business day" |

Hadia's row is `vera_support/hadia` with `enabled=true`.

## Env

`NOTIFICATIONS_URL` (new, default `http://notifications-api.notifications.svc.cluster.local:4000`), `TICKETING_URL`, `AUTH_API_URL`, `SUBSCRIPTION_BASE_URL`, `MF_SECURITY_SUBSCRIPTIONS_API_KEY` (else `INTERNAL_SERVICE_KEY`), `INTERNAL_SERVICE_KEY`, `MF_PLATFORM_TENANT_SLUG`.

## Tests

`internal/support/support_test.go` covers the classifier cases, the settings merge, each route, the fallbacks (platform failure, tenant ticket 400, everything down, a hanging service inside the budget), the retry bounds, and the exact ticketing-api and notifications-api bodies, decoded with unknown fields refused. `internal/httpapi/handlers/public_support_test.go` covers the old ticket path with routing off, the verified email, an expired code and an auth-api outage.
