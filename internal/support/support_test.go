package support

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/decide"
)

var (
	hadiaID    = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	platformID = uuid.MustParse("22222222-2222-4222-8222-222222222222")
)

func init() {
	// Retries in tests do not wait.
	sleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
}

// ── classifier ──────────────────────────────────────────────────────────────

type fakeDecider struct {
	choice   string
	conf     float64
	fallback bool
	err      error
	calls    atomic.Int32
	delay    time.Duration
}

func (f *fakeDecider) Decide(ctx context.Context, name, state string, qs map[string]decide.Question) (decide.Answers, decide.Usage, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, decide.Usage{}, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if f.err != nil {
		return nil, decide.Usage{}, f.err
	}
	if _, ok := qs["route"]; !ok || name != "support_route" {
		return nil, decide.Usage{}, errors.New("unexpected question")
	}
	return decide.Answers{"route": {Type: decide.KindChoice, Choice: f.choice, Confidence: f.conf}}, decide.Usage{Fallback: f.fallback}, nil
}

func TestClassifierRules(t *testing.T) {
	c := NewClassifier(nil)
	cases := []struct {
		text string
		want Kind
		src  string
	}{
		{"The site won't load on my phone, just a blank page", KindTechnical, SourceRules},
		{"I can't log in, the verification code never arrives", KindTechnical, SourceRules},
		{"Checkout failed with an error when I tried to pay", KindTechnical, SourceRules},
		{"When will my payout for the registry arrive?", KindBusiness, SourceRules},
		{"I want a refund for a gift I sent twice", KindBusiness, SourceRules},
		{"What is your privacy policy on guest details?", KindBusiness, SourceRules},
		{"How do I change my owner account email?", KindBusiness, SourceRules},
		// Both signals and no model: the safe default.
		{"My payment failed and I want a refund", KindBusiness, SourceDefault},
		// No signal at all: the safe default.
		{"Hello, can someone help me please", KindBusiness, SourceDefault},
	}
	for _, tc := range cases {
		got := c.Classify(context.Background(), tc.text)
		if got.Kind != tc.want || got.Source != tc.src {
			t.Errorf("%q: got %s by %s (tech %d, biz %d), want %s by %s", tc.text, got.Kind, got.Source, got.Technical, got.Business, tc.want, tc.src)
		}
	}
}

func TestClassifierModelOnlyWhenRulesCannotTell(t *testing.T) {
	d := &fakeDecider{choice: "technical", conf: 0.9}
	c := NewClassifier(d)
	if got := c.Classify(context.Background(), "The site won't load"); got.Source != SourceRules || d.calls.Load() != 0 {
		t.Fatalf("a clear rule answer must not call the model: %+v, calls %d", got, d.calls.Load())
	}
	got := c.Classify(context.Background(), "My payment failed and I want a refund")
	if got.Kind != KindTechnical || got.Source != SourceJev || d.calls.Load() != 1 {
		t.Fatalf("ambiguous text should take a confident model answer: %+v", got)
	}
}

func TestClassifierLowConfidenceAndFailureTakeSafeDefault(t *testing.T) {
	for name, d := range map[string]*fakeDecider{
		"low confidence":           {choice: "technical", conf: 0.5},
		"fallback under its bar":   {choice: "technical", conf: 0.55, fallback: true},
		"model failed":             {err: errors.New("down")},
		"model slower than budget": {choice: "technical", conf: 0.99, delay: time.Second},
	} {
		c := NewClassifier(d)
		c.budget = 50 * time.Millisecond
		start := time.Now()
		got := c.Classify(context.Background(), "Hello, something is off with my thing")
		if got.Kind != KindBusiness || got.Source != SourceDefault {
			t.Errorf("%s: got %+v, want the business default", name, got)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Errorf("%s: the classifier waited past its budget", name)
		}
	}
}

// ── settings ────────────────────────────────────────────────────────────────

func TestLoadSettingsMergesTenantOverDefault(t *testing.T) {
	rows := map[string]map[string]string{
		"default": {"enabled": "false", "fallback_channels": "email", "reply_time": ""},
		"hadia": {"enabled": "true", "fallback_channels": "email, whatsapp, sms", "admin_whatsapp_source": "configured",
			"admin_whatsapp_numbers": "+254700000001,0700000002", "max_admins": "2", "default_dial_code": "+254"},
	}
	src := func(_ context.Context, typ, name string) (map[string]string, error) {
		if typ != SettingsType {
			t.Fatalf("type %q", typ)
		}
		return rows[name], nil
	}
	s := LoadSettings(context.Background(), src, "Hadia")
	if !s.Enabled || s.AdminSource != AdminSourceConfigured || len(s.AdminNumbers) != 2 || s.MaxAdmins != 2 || s.DialCode != "254" {
		t.Fatalf("tenant row not applied: %+v", s)
	}
	if strings.Join(s.FallbackChannels, ",") != "email,whatsapp" {
		t.Fatalf("unknown channels must be dropped: %v", s.FallbackChannels)
	}
	if s.GateTag != "ticketing" || s.ReplyTime != "" || !s.PlatformTickets {
		t.Fatalf("defaults lost: %+v", s)
	}
	other := LoadSettings(context.Background(), src, "someone-else")
	if other.Enabled || strings.Join(other.FallbackChannels, ",") != "email" {
		t.Fatalf("default row not applied: %+v", other)
	}
	failing := LoadSettings(context.Background(), func(context.Context, string, string) (map[string]string, error) {
		return nil, errors.New("marketflow-api down")
	}, "hadia")
	if failing.Enabled {
		t.Fatal("a failed read must fall back to the code default (off)")
	}
}

// ── fakes that decode the exact wire shapes ─────────────────────────────────

// ticketingBody mirrors ticketing-api's internal create body; DisallowUnknownFields makes a
// misnamed field fail the test instead of being dropped silently.
type ticketingBody struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Priority    string `json:"priority"`
	Channel     string `json:"channel"`
	Category    string `json:"category"`
	SourceRef   *struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		Parent *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"parent"`
	} `json:"source_ref"`
	RequesterName  string `json:"requester_name"`
	RequesterEmail string `json:"requester_email"`
	RequesterPhone string `json:"requester_phone"`
	ContactHash    string `json:"contact_hash"`
	PhoneNumberID  string `json:"phone_number_id"`
	ReceivedAt     string `json:"received_at"`
}

type ticketCall struct {
	tenant string
	key    string
	body   ticketingBody
}

type ticketingFake struct {
	mu     sync.Mutex
	calls  []ticketCall
	status func(n int) int
	hang   bool
}

func (f *ticketingFake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/internal/tickets" {
			t.Errorf("ticketing: %s %s", r.Method, r.URL.Path)
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var b ticketingBody
		if err := dec.Decode(&b); err != nil {
			t.Errorf("ticketing body does not match the contract: %v", err)
		}
		if b.Subject == "" || b.Description == "" || b.Source == "" {
			t.Errorf("ticketing body misses a required field: %+v", b)
		}
		if _, err := uuid.Parse(r.Header.Get("X-Tenant-ID")); err != nil {
			t.Errorf("X-Tenant-ID must be the tenant UUID, got %q", r.Header.Get("X-Tenant-ID"))
		}
		f.mu.Lock()
		f.calls = append(f.calls, ticketCall{tenant: r.Header.Get("X-Tenant-ID"), key: r.Header.Get("X-API-Key"), body: b})
		n := len(f.calls)
		f.mu.Unlock()
		if f.hang {
			<-r.Context().Done()
			return
		}
		status := http.StatusCreated
		if f.status != nil {
			status = f.status(n)
		}
		w.WriteHeader(status)
		if status < 300 {
			_, _ = w.Write([]byte(`{"id":"t-1","ticket_number":"TKT-000042","status":"open"}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}
	}
}

// notificationsBody mirrors notifications-api's CreateMessageRequest.
type notificationsBody struct {
	Channel     string         `json:"channel"`
	Tenant      string         `json:"tenant"`
	Template    string         `json:"template"`
	Data        map[string]any `json:"data"`
	To          []string       `json:"to"`
	Cc          []string       `json:"cc"`
	Metadata    map[string]any `json:"metadata"`
	Attachments []any          `json:"attachments"`
}

type messageCall struct {
	path, tenantHeader, idem string
	body                     notificationsBody
}

type notificationsFake struct {
	mu     sync.Mutex
	calls  []messageCall
	status int
}

func (f *notificationsFake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var b notificationsBody
		if err := dec.Decode(&b); err != nil {
			t.Errorf("notifications body does not match the contract: %v", err)
		}
		if b.Channel == "" || b.Tenant == "" || b.Template == "" || len(b.To) == 0 || b.Data == nil {
			t.Errorf("notifications body misses a required field: %+v", b)
		}
		f.mu.Lock()
		f.calls = append(f.calls, messageCall{path: r.URL.Path, tenantHeader: r.Header.Get("X-Tenant-ID"), idem: r.Header.Get("Idempotency-Key"), body: b})
		f.mu.Unlock()
		status := f.status
		if status == 0 {
			status = http.StatusAccepted
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"queued","requestId":"r1"}`))
	}
}

type fakePlans struct {
	yes   bool
	asked []string
}

func (p *fakePlans) UsesService(_ context.Context, tenantID, tag string) bool {
	p.asked = append(p.asked, tenantID+"|"+tag)
	return p.yes
}

func tenants(_ context.Context, slug string) (Tenant, error) {
	switch slug {
	case "hadia":
		return Tenant{ID: hadiaID, Slug: "hadia", Name: "Hadia", SupportEmail: "support@hadiafrica.com", SupportWhatsApp: "+254711000000"}, nil
	case "codevertex":
		return Tenant{ID: platformID, Slug: "codevertex", Name: "Codevertex"}, nil
	}
	return Tenant{}, errors.New("not found")
}

type rig struct {
	router *Router
	tick   *ticketingFake
	notif  *notificationsFake
	plans  *fakePlans
	reach  *atomic.Int32
}

func newRig(t *testing.T, plansYes bool) *rig {
	t.Helper()
	rg := &rig{tick: &ticketingFake{}, notif: &notificationsFake{}, plans: &fakePlans{yes: plansYes}, reach: &atomic.Int32{}}
	ts := httptest.NewServer(rg.tick.handler(t))
	ns := httptest.NewServer(rg.notif.handler(t))
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rg.reach.Add(1)
		if r.URL.Path != "/api/v1/s2s/tenants/reach" || r.URL.Query().Get("tenant_ids") != hadiaID.String() ||
			r.URL.Query().Get("purpose") != "broadcast" || r.Header.Get("X-API-Key") != "k" {
			t.Errorf("auth reach: %s %s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"data":[{"tenant_id":"` + hadiaID.String() + `","phones":[
			{"address":"+254700000009","source":"contact","verified":true},
			{"address":"+254700000001","source":"owner","verified":false,"first_name":"Amina"},
			{"address":"+254700000002","source":"admin","verified":true,"first_name":"Tom"}]}],"next":""}`))
	}))
	t.Cleanup(func() { ts.Close(); ns.Close(); as.Close() })
	rg.router = NewRouter(Config{
		Classifier:   NewClassifier(nil),
		Tenants:      tenants,
		PlatformSlug: "codevertex",
		Plans:        rg.plans,
		Tickets:      &Ticketing{BaseURL: ts.URL, ServiceKey: "k", Backoff: Backoff{Attempts: 3, Base: time.Millisecond, Max: time.Millisecond, PerTry: 300 * time.Millisecond}},
		Messages:     &Notifier{BaseURL: ns.URL, ServiceKey: "k", Backoff: Backoff{Attempts: 2, Base: time.Millisecond, Max: time.Millisecond, PerTry: 300 * time.Millisecond}},
		Admins:       &AdminReach{BaseURL: as.URL, ServiceKey: "k"},
		TotalBudget:  3 * time.Second,
		StepBudget:   time.Second,
		GateBudget:   200 * time.Millisecond,
	})
	return rg
}

func hadiaSettings() Settings {
	s := DefaultSettings()
	s.Enabled = true
	return s
}

func request(msg string) Request {
	return Request{TenantID: hadiaID, TenantSlug: "hadia", RequestID: "req-1", Name: "Jane Doe", Email: "jane@example.com",
		Phone: "0712345678", Message: msg, Priority: "High (urgent)", Page: "https://hadiafrica.com/registry/abc"}
}

// ── routes ──────────────────────────────────────────────────────────────────

func TestTechnicalGoesToPlatformTicket(t *testing.T) {
	rg := newRig(t, false)
	out := rg.router.Route(context.Background(), request("I can't log in, the page keeps loading"), hadiaSettings())
	if out.Route != RoutePlatformTicket || out.TicketNumber != "TKT-000042" || out.Kind != KindTechnical {
		t.Fatalf("got %+v", out)
	}
	if len(rg.tick.calls) != 1 {
		t.Fatalf("want one ticket call, got %d", len(rg.tick.calls))
	}
	c := rg.tick.calls[0]
	if c.tenant != platformID.String() || c.key != "k" {
		t.Fatalf("platform ticket must go to the platform tenant: %+v", c)
	}
	b := c.body
	if b.Source != Source || b.Channel != "chat" || b.Category != "technical" || b.Priority != "high" ||
		b.RequesterEmail != "jane@example.com" || b.RequesterPhone != "0712345678" {
		t.Fatalf("body: %+v", b)
	}
	if b.SourceRef == nil || b.SourceRef.ID != "req-1" || b.SourceRef.Parent == nil || b.SourceRef.Parent.ID != hadiaID.String() {
		t.Fatalf("source_ref must make the create idempotent and name the tenant: %+v", b.SourceRef)
	}
	if !strings.Contains(out.Message, "TKT-000042") || strings.Contains(strings.ToLower(out.Message), "hour") {
		t.Fatalf("message must carry the reference and no reply time: %q", out.Message)
	}
	if len(rg.plans.asked) != 0 || len(rg.notif.calls) != 0 {
		t.Fatal("a platform ticket needs no plan check and no staff notice")
	}
}

func TestBusinessWithTicketingGoesToTenantTicket(t *testing.T) {
	rg := newRig(t, true)
	out := rg.router.Route(context.Background(), request("When will my registry payout arrive?"), hadiaSettings())
	if out.Route != RouteTenantTicket || out.TicketNumber != "TKT-000042" {
		t.Fatalf("got %+v", out)
	}
	if len(rg.plans.asked) != 1 || rg.plans.asked[0] != hadiaID.String()+"|ticketing" {
		t.Fatalf("gate must ask for the tenant's ticketing tag: %v", rg.plans.asked)
	}
	c := rg.tick.calls[0]
	if c.tenant != hadiaID.String() || c.body.Category != "general" || c.body.SourceRef == nil || c.body.SourceRef.Parent != nil {
		t.Fatalf("tenant ticket: %+v", c)
	}
	if !strings.Contains(out.Message, "Hadia team") {
		t.Fatalf("message: %q", out.Message)
	}
}

func TestBusinessWithoutTicketingNotifiesStaffByEmailAndWhatsApp(t *testing.T) {
	rg := newRig(t, false)
	out := rg.router.Route(context.Background(), request("I need a refund for a gift\nI paid twice."), hadiaSettings())
	if out.Route != RouteStaffNotified || strings.Join(out.Channels, ",") != "email,whatsapp" {
		t.Fatalf("got %+v", out)
	}
	if len(rg.tick.calls) != 0 {
		t.Fatal("no ticket when the plan has no ticketing")
	}
	if !strings.Contains(out.Message, "by email and WhatsApp") || out.TicketNumber != "" {
		t.Fatalf("message: %q", out.Message)
	}
	// One email and two WhatsApp messages (owner and admin; the contact number is not an admin).
	if len(rg.notif.calls) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(rg.notif.calls), rg.notif.calls)
	}
	email := rg.notif.calls[0]
	if email.path != "/api/v1/"+platformID.String()+"/notifications/messages" || email.tenantHeader != platformID.String() ||
		email.body.Tenant != platformID.String() {
		t.Fatalf("staff notices go as the platform tenant: %+v", email)
	}
	if email.body.Channel != "email" || email.body.Template != "broadcast/message" || email.body.To[0] != "support@hadiafrica.com" ||
		email.idem != "vera-support:req-1:email" || email.body.Metadata["subject"] == nil {
		t.Fatalf("email: %+v", email)
	}
	if body, _ := email.body.Data["body"].(string); !strings.Contains(body, "jane@example.com") || !strings.Contains(body, "I paid twice.") {
		t.Fatalf("email body must carry the visitor's question and contact: %q", body)
	}
	seen := map[string]bool{}
	for _, wa := range rg.notif.calls[1:] {
		if wa.body.Channel != "whatsapp" || wa.body.Metadata["template_name"] != "broadcast_service_notice_v1" ||
			wa.body.Metadata["default_dial_code"] != "254" {
			t.Fatalf("whatsapp: %+v", wa)
		}
		params, _ := wa.body.Metadata["template_params"].([]any)
		if len(params) != 3 {
			t.Fatalf("template params: %v", wa.body.Metadata["template_params"])
		}
		if msg, _ := params[2].(string); strings.ContainsAny(msg, "\n\t") || !strings.Contains(msg, "Jane Doe") {
			t.Fatalf("template message must be one line with the visitor: %q", msg)
		}
		seen[wa.body.To[0]] = true
	}
	// Verified admin first.
	if !seen["+254700000002"] || !seen["+254700000001"] || rg.notif.calls[1].body.To[0] != "+254700000002" {
		t.Fatalf("admin phones: %v", seen)
	}
}

func TestConfiguredNumbersAndChannels(t *testing.T) {
	rg := newRig(t, false)
	s := hadiaSettings()
	s.FallbackChannels = []string{ChannelWhatsApp}
	s.AdminSource = AdminSourceConfigured
	s.AdminNumbers = []string{"+254799999999"}
	out := rg.router.Route(context.Background(), request("Question about the cash fund policy"), s)
	if out.Route != RouteStaffNotified || strings.Join(out.Channels, ",") != "whatsapp" || len(rg.notif.calls) != 1 ||
		rg.notif.calls[0].body.To[0] != "+254799999999" || rg.reach.Load() != 0 {
		t.Fatalf("configured number only: %+v, calls %+v, reach %d", out, rg.notif.calls, rg.reach.Load())
	}
	if !strings.Contains(out.Message, "on WhatsApp") {
		t.Fatalf("message: %q", out.Message)
	}
}

func TestReplyTimeOnlyWhenConfigured(t *testing.T) {
	rg := newRig(t, true)
	s := hadiaSettings()
	s.ReplyTime = "within one business day"
	out := rg.router.Route(context.Background(), request("A question about my registry"), s)
	if !strings.Contains(out.Message, "usually within one business day") {
		t.Fatalf("configured reply time missing: %q", out.Message)
	}
}

func TestPlatformFailureFallsBackToTheBusinessTeam(t *testing.T) {
	rg := newRig(t, false)
	rg.tick.status = func(int) int { return http.StatusServiceUnavailable }
	out := rg.router.Route(context.Background(), request("The website is down, error 500"), hadiaSettings())
	if out.Route != RouteStaffNotified || out.Kind != KindTechnical {
		t.Fatalf("got %+v", out)
	}
	// 503 is retried with backoff up to the attempt cap, never more.
	if n := len(rg.tick.calls); n != 3 {
		t.Fatalf("want 3 bounded attempts, got %d", n)
	}
}

func TestTenantTicketFailureNotifiesStaff(t *testing.T) {
	rg := newRig(t, true)
	rg.tick.status = func(int) int { return http.StatusBadRequest }
	out := rg.router.Route(context.Background(), request("A question about my gift"), hadiaSettings())
	if out.Route != RouteStaffNotified {
		t.Fatalf("got %+v", out)
	}
	if len(rg.tick.calls) != 1 {
		t.Fatalf("a 400 is our bug and must not be retried, got %d calls", len(rg.tick.calls))
	}
}

func TestEverythingDownGivesContactOnly(t *testing.T) {
	rg := newRig(t, true)
	rg.tick.status = func(int) int { return http.StatusInternalServerError }
	rg.notif.status = http.StatusInternalServerError
	out := rg.router.Route(context.Background(), request("A question about my registry"), hadiaSettings())
	if out.Route != RouteContactOnly || out.Contact == nil || out.Contact.Email != "support@hadiafrica.com" ||
		!strings.Contains(out.Message, "support@hadiafrica.com") || !strings.Contains(out.Message, "+254711000000") {
		t.Fatalf("got %+v", out)
	}
}

func TestHangingServiceNeverStalls(t *testing.T) {
	rg := newRig(t, true)
	rg.tick.hang = true
	rg.notif.status = http.StatusInternalServerError
	rg.router.cfg.TotalBudget = 2 * time.Second
	start := time.Now()
	out := rg.router.Route(context.Background(), request("The site won't load, blank page"), hadiaSettings())
	if el := time.Since(start); el > 2500*time.Millisecond {
		t.Fatalf("the flow took %v, past its total budget", el)
	}
	if out.Route != RouteContactOnly || out.Message == "" {
		t.Fatalf("got %+v", out)
	}
}

func TestBackoffStopsOnSuccessAndBounds(t *testing.T) {
	var n atomic.Int32
	b := Backoff{Attempts: 4, Base: time.Millisecond, Max: 2 * time.Millisecond, PerTry: 100 * time.Millisecond}
	err := b.do(context.Background(), func(context.Context) error {
		if n.Add(1) < 2 {
			return &statusError{Status: 503}
		}
		return nil
	})
	if err != nil || n.Load() != 2 {
		t.Fatalf("want success on the second try, got %v after %d", err, n.Load())
	}
	n.Store(0)
	err = b.do(context.Background(), func(context.Context) error { n.Add(1); return &statusError{Status: 429} })
	if err == nil || n.Load() != 4 {
		t.Fatalf("want 4 bounded tries, got %d", n.Load())
	}
	for i := 0; i < 10; i++ {
		if d := b.delay(i); d <= 0 || d > b.Max {
			t.Fatalf("delay %d = %v outside (0, %v]", i, d, b.Max)
		}
	}
}

func TestTicketWithoutSourceRefIsSentOnce(t *testing.T) {
	f := &ticketingFake{status: func(int) int { return http.StatusBadGateway }}
	ts := httptest.NewServer(f.handler(t))
	defer ts.Close()
	tk := &Ticketing{BaseURL: ts.URL, ServiceKey: "k", Backoff: Backoff{Attempts: 3, Base: time.Millisecond, PerTry: time.Second}}
	_, err := tk.Create(context.Background(), hadiaID, TicketRequest{Subject: "s", Description: "d", Source: "vera_ai"})
	if err == nil || len(f.calls) != 1 {
		t.Fatalf("a create without source_ref is not idempotent and must be sent once: %d calls", len(f.calls))
	}
}

func TestOTPValidatorShape(t *testing.T) {
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/s2s/otp/validate" || r.Header.Get("X-API-Key") != "k" {
			t.Errorf("otp: %s", r.URL.String())
		}
		if r.URL.Query().Get("token") == "good" {
			_, _ = w.Write([]byte(`{"valid":true,"email":"jane@example.com"}`))
			return
		}
		_, _ = w.Write([]byte(`{"valid":false}`))
	}))
	defer as.Close()
	o := &OTPValidator{BaseURL: as.URL, ServiceKey: "k"}
	if email, err := o.Validate(context.Background(), "good"); err != nil || email != "jane@example.com" {
		t.Fatalf("got %q %v", email, err)
	}
	if _, err := o.Validate(context.Background(), "used"); !errors.Is(err, ErrOTPInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestPriorityAndSubject(t *testing.T) {
	for in, want := range map[string]string{"Low": "low", "Medium": "normal", "High (urgent)": "high", "": "normal", "urgent": "high"} {
		if got := Priority(in); got != want {
			t.Errorf("Priority(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SubjectFrom("  First line here\nsecond"); got != "First line here" {
		t.Fatalf("subject %q", got)
	}
	if got := SubjectFrom(strings.Repeat("a", 300)); len([]rune(got)) != 120 {
		t.Fatalf("subject length %d", len([]rune(got)))
	}
}
