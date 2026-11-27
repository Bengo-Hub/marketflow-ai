package support

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Routes a request can take, in the order they are tried.
const (
	RoutePlatformTicket = "platform_ticket"
	RouteTenantTicket   = "tenant_ticket"
	RouteStaffNotified  = "staff_notified"
	// RouteContactOnly means nothing could be sent; the visitor gets the business's contact.
	RouteContactOnly = "contact_only"
)

// Source is the ticketing source every ticket from this flow carries.
const Source = "vera_support"

// Tenant is what the router needs to know about a tenant.
type Tenant struct {
	ID              uuid.UUID
	Slug            string
	Name            string
	SupportEmail    string
	SupportWhatsApp string
}

// TenantLookup reads a tenant by slug (auth-api's public tenant, cached).
type TenantLookup func(ctx context.Context, slug string) (Tenant, error)

// PlanChecker answers whether a tenant's plan includes a service: auth-client's shared
// EntitlementClient (subscriptions-api, cached a minute, open when it cannot be reached).
type PlanChecker interface {
	UsesService(ctx context.Context, tenantID, serviceTag string) bool
}

// TicketCreator opens a ticket (Ticketing).
type TicketCreator interface {
	Create(ctx context.Context, tenantID uuid.UUID, in TicketRequest) (Ticket, error)
}

// MessageSender queues a message (Notifier).
type MessageSender interface {
	Send(ctx context.Context, tenant string, msg MessageRequest, idempotencyKey string) error
}

// AdminDirectory lists a tenant's admin phones (AdminReach).
type AdminDirectory interface {
	Admins(ctx context.Context, tenantID uuid.UUID, max int) ([]Admin, error)
}

// Budgets. Every step has its own limit and the whole request has one, so the slowest possible
// path (platform ticket fails, gate, tenant ticket fails, staff notice) still answers well under
// the widget's wait. A step with less than MinStep left is skipped for the next fallback.
const (
	DefaultTotalBudget = 22 * time.Second
	DefaultStepBudget  = 6 * time.Second
	DefaultGateBudget  = 2 * time.Second
	DefaultLookup      = 3 * time.Second
	MinStep            = 1500 * time.Millisecond
)

// Config wires a Router.
type Config struct {
	Classifier   *Classifier
	Settings     SettingsSource
	Tenants      TenantLookup
	PlatformSlug string // the platform tenant (codevertex) that holds Codevertex's own tickets
	Plans        PlanChecker
	Tickets      TicketCreator
	Messages     MessageSender
	Admins       AdminDirectory
	Logger       *zap.Logger

	TotalBudget time.Duration
	StepBudget  time.Duration
	GateBudget  time.Duration
}

// Router routes support requests.
type Router struct{ cfg Config }

// NewRouter builds a router; zero budgets take the defaults.
func NewRouter(cfg Config) *Router {
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	cfg.Logger = cfg.Logger.Named("support")
	if cfg.TotalBudget <= 0 {
		cfg.TotalBudget = DefaultTotalBudget
	}
	if cfg.StepBudget <= 0 {
		cfg.StepBudget = DefaultStepBudget
	}
	if cfg.GateBudget <= 0 {
		cfg.GateBudget = DefaultGateBudget
	}
	if cfg.PlatformSlug == "" {
		cfg.PlatformSlug = "codevertex"
	}
	return &Router{cfg: cfg}
}

// Request is one visitor's support request, with the email already verified.
type Request struct {
	TenantID   uuid.UUID
	TenantSlug string
	// RequestID makes every downstream call idempotent (ticket source_ref, message keys).
	RequestID string
	Name      string
	Email     string
	Phone     string
	Subject   string
	Message   string
	Priority  string
	// Page is the page the widget runs on, for context in the ticket or notice.
	Page string
}

// Contact is how the visitor can reach the business directly.
type Contact struct {
	Email    string `json:"email,omitempty"`
	WhatsApp string `json:"whatsapp,omitempty"`
}

// Outcome is what the visitor is told.
type Outcome struct {
	Route        string   `json:"route"`
	Kind         Kind     `json:"kind"`
	ClassifiedBy string   `json:"classified_by"`
	TicketNumber string   `json:"ticket_number,omitempty"`
	Channels     []string `json:"channels,omitempty"`
	Message      string   `json:"message"`
	Contact      *Contact `json:"contact,omitempty"`
}

// Settings loads the tenant's routing settings.
func (r *Router) Settings(ctx context.Context, slug string) Settings {
	ctx, cancel := context.WithTimeout(ctx, DefaultLookup)
	defer cancel()
	return LoadSettings(ctx, r.cfg.Settings, slug)
}

// Route runs the flow: classify, then the platform ticket for a technical request, the tenant
// ticket when the plan includes ticketing, the staff notice otherwise, and the business's own
// contact details when nothing could be sent. It never returns an error: every failure moves to
// the next step, and the last step needs no network.
func (r *Router) Route(ctx context.Context, req Request, s Settings) Outcome {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.TotalBudget)
	defer cancel()
	if req.RequestID == "" {
		req.RequestID = uuid.NewString()
	}
	if req.Subject == "" {
		req.Subject = SubjectFrom(req.Message)
	}
	// auth-api owns tenant UUIDs, and ticketing-api and subscriptions-api key on them, so the
	// looked-up id wins; the caller's id is the fallback when auth-api cannot be read.
	tenant := r.lookup(ctx, req.TenantSlug)
	if tenant.ID == uuid.Nil {
		tenant.ID = req.TenantID
	}
	tenant.Slug = req.TenantSlug
	if tenant.Name == "" {
		tenant.Name = req.TenantSlug
	}
	if s.SupportEmail != "" {
		tenant.SupportEmail = s.SupportEmail
	}

	cls := r.cfg.Classifier.Classify(ctx, req.Subject+"\n"+req.Message)
	log := r.cfg.Logger.With(zap.String("tenant", req.TenantSlug), zap.String("request_id", req.RequestID),
		zap.String("kind", string(cls.Kind)), zap.String("classified_by", cls.Source))

	if cls.Kind == KindTechnical && s.PlatformTickets {
		if t, err := r.CreatePlatformTicket(ctx, req, tenant, cls); err == nil {
			log.Info("support request: platform ticket", zap.String("ticket", t.TicketNumber))
			return Outcome{Route: RoutePlatformTicket, Kind: cls.Kind, ClassifiedBy: cls.Source, TicketNumber: t.TicketNumber,
				Message: platformTicketMessage(t.TicketNumber, req.Email, s.ReplyTime)}
		} else {
			log.Warn("support request: platform ticket failed, trying the business's team", zap.Error(err))
		}
	}

	if r.TicketingIncluded(ctx, tenant.ID, s.GateTag) {
		if t, err := r.CreateTenantTicket(ctx, req, tenant, cls); err == nil {
			log.Info("support request: tenant ticket", zap.String("ticket", t.TicketNumber))
			return Outcome{Route: RouteTenantTicket, Kind: cls.Kind, ClassifiedBy: cls.Source, TicketNumber: t.TicketNumber,
				Message: tenantTicketMessage(tenant.Name, t.TicketNumber, req.Email, s.ReplyTime)}
		} else {
			log.Warn("support request: tenant ticket failed, notifying the team instead", zap.Error(err))
		}
	}

	if channels := r.NotifyTenantStaff(ctx, req, tenant, s); len(channels) > 0 {
		log.Info("support request: staff notified", zap.Strings("channels", channels))
		return Outcome{Route: RouteStaffNotified, Kind: cls.Kind, ClassifiedBy: cls.Source, Channels: channels,
			Message: staffNotifiedMessage(tenant.Name, channels, req.Email, s.ReplyTime)}
	}

	log.Error("support request: nothing could be sent; the visitor got the contact details")
	return r.contactOnly(tenant, cls)
}

// ContactOnly is the answer when the flow cannot run at all (for example the routing is on but
// every service is down before it starts). It needs no network beyond the tenant lookup.
func (r *Router) ContactOnly(ctx context.Context, slug string) Outcome {
	ctx, cancel := context.WithTimeout(ctx, DefaultLookup)
	defer cancel()
	t := r.lookup(ctx, slug)
	if t.Name == "" {
		t.Name = slug
	}
	return r.contactOnly(t, Classification{Kind: KindBusiness, Source: SourceDefault})
}

func (r *Router) contactOnly(t Tenant, cls Classification) Outcome {
	out := Outcome{Route: RouteContactOnly, Kind: cls.Kind, ClassifiedBy: cls.Source}
	if t.SupportEmail != "" || t.SupportWhatsApp != "" {
		out.Contact = &Contact{Email: t.SupportEmail, WhatsApp: t.SupportWhatsApp}
	}
	out.Message = contactOnlyMessage(t.Name, out.Contact)
	return out
}

// TicketingIncluded is the plan gate: does the tenant's plan include the ticketing service?
// No checker means no gate (local development). The check has its own short budget; when
// subscriptions-api cannot answer in time the shared client fails open, as everywhere else.
func (r *Router) TicketingIncluded(ctx context.Context, tenantID uuid.UUID, tag string) bool {
	if r.cfg.Plans == nil || tenantID == uuid.Nil {
		return r.cfg.Plans == nil
	}
	if tag == "" {
		tag = "ticketing"
	}
	gctx, cancel := context.WithTimeout(ctx, r.cfg.GateBudget)
	defer cancel()
	return r.cfg.Plans.UsesService(gctx, tenantID.String(), tag)
}

// CreatePlatformTicket opens a ticket for Codevertex technical support on the platform tenant.
// It is always allowed: Codevertex's own support is not part of the business's plan.
func (r *Router) CreatePlatformTicket(ctx context.Context, req Request, t Tenant, cls Classification) (Ticket, error) {
	if r.cfg.Tickets == nil || r.cfg.Tenants == nil {
		return Ticket{}, fmt.Errorf("platform tickets are not configured")
	}
	sctx, cancel, ok := r.step(ctx)
	if !ok {
		return Ticket{}, context.DeadlineExceeded
	}
	defer cancel()
	lctx, lcancel := context.WithTimeout(sctx, DefaultLookup)
	platform, err := r.cfg.Tenants(lctx, r.cfg.PlatformSlug)
	lcancel()
	if err != nil || platform.ID == uuid.Nil {
		return Ticket{}, fmt.Errorf("platform tenant %q not resolved: %v", r.cfg.PlatformSlug, err)
	}
	desc := req.Message + "\n\n" + fmt.Sprintf(
		"Raised by Vera, the chat assistant on %s, for %s (tenant %s, id %s). Vera read it as a technical problem with the platform (%s).",
		pageHost(req.Page, t.Slug), t.Name, t.Slug, t.ID, cls.Source) + "\n" + requesterLine(req)
	return r.cfg.Tickets.Create(sctx, platform.ID, TicketRequest{
		Subject:     clipRunes(t.Name+": "+req.Subject, 200),
		Description: desc,
		Source:      Source,
		Priority:    Priority(req.Priority),
		Channel:     "chat",
		Category:    "technical",
		SourceRef: &SourceRef{Type: "vera_support_request", ID: req.RequestID,
			Parent: &ParentRef{Type: "tenant", ID: t.ID.String()}},
		RequesterName: req.Name, RequesterEmail: req.Email, RequesterPhone: req.Phone,
	})
}

// CreateTenantTicket opens a ticket in the business's own queue. The caller checks the plan
// gate first (TicketingIncluded); ticketing-api's internal create does not gate on the plan.
func (r *Router) CreateTenantTicket(ctx context.Context, req Request, t Tenant, cls Classification) (Ticket, error) {
	if r.cfg.Tickets == nil {
		return Ticket{}, fmt.Errorf("tickets are not configured")
	}
	sctx, cancel, ok := r.step(ctx)
	if !ok {
		return Ticket{}, context.DeadlineExceeded
	}
	defer cancel()
	note := "Vera, the chat assistant, read it as a question for your team."
	category := "general"
	if cls.Kind == KindTechnical {
		note = "Vera read it as a technical problem with the site, but could not reach Codevertex technical support. Forward it to support@codevertexafrica.com if it needs them."
		category = "technical"
	}
	desc := req.Message + "\n\n" + fmt.Sprintf("Sent from the chat on %s. %s", pageHost(req.Page, t.Slug), note) + "\n" + requesterLine(req)
	return r.cfg.Tickets.Create(sctx, t.ID, TicketRequest{
		Subject:       clipRunes(req.Subject, 200),
		Description:   desc,
		Source:        Source,
		Priority:      Priority(req.Priority),
		Channel:       "chat",
		Category:      category,
		SourceRef:     &SourceRef{Type: "vera_support_request", ID: req.RequestID},
		RequesterName: req.Name, RequesterEmail: req.Email, RequesterPhone: req.Phone,
	})
}

// NotifyTenantStaff sends the request to the business's team on each configured channel: an
// email to its support address and a WhatsApp message to its admins, both sent as the platform
// tenant, so they go from Codevertex's address and number and the business pays nothing. It
// returns the channels that were queued; a channel that fails is skipped, never retried past
// its step budget.
func (r *Router) NotifyTenantStaff(ctx context.Context, req Request, t Tenant, s Settings) []string {
	if r.cfg.Messages == nil || r.cfg.Tenants == nil {
		return nil
	}
	lctx, lcancel := context.WithTimeout(ctx, DefaultLookup)
	platform, err := r.cfg.Tenants(lctx, r.cfg.PlatformSlug)
	lcancel()
	if err != nil || platform.ID == uuid.Nil {
		r.cfg.Logger.Warn("staff notice: platform tenant not resolved", zap.Error(err))
		return nil
	}
	sender := platform.ID.String()
	var sent []string
	for _, ch := range s.FallbackChannels {
		switch ch {
		case ChannelEmail:
			if r.notifyEmail(ctx, sender, req, t) {
				sent = append(sent, ChannelEmail)
			}
		case ChannelWhatsApp:
			if r.notifyWhatsApp(ctx, sender, req, t, s) {
				sent = append(sent, ChannelWhatsApp)
			}
		}
	}
	return sent
}

func (r *Router) notifyEmail(ctx context.Context, sender string, req Request, t Tenant) bool {
	if t.SupportEmail == "" {
		r.cfg.Logger.Info("staff notice: no support email for the tenant", zap.String("tenant", t.Slug))
		return false
	}
	sctx, cancel, ok := r.step(ctx)
	if !ok {
		return false
	}
	defer cancel()
	text := StaffEmailText(t.Name, req)
	err := r.cfg.Messages.Send(sctx, sender, MessageRequest{
		Channel:  ChannelEmail,
		Template: "broadcast/message",
		To:       []string{t.SupportEmail},
		Data:     map[string]any{"body": text, "paragraphs": paragraphs(text)},
		Metadata: map[string]any{"subject": clipRunes("Website support request from "+firstNonEmpty(req.Name, req.Email), 150)},
	}, "vera-support:"+req.RequestID+":email")
	if err != nil {
		r.cfg.Logger.Warn("staff notice: email not queued", zap.String("tenant", t.Slug), zap.Error(err))
		return false
	}
	return true
}

func (r *Router) notifyWhatsApp(ctx context.Context, sender string, req Request, t Tenant, s Settings) bool {
	sctx, cancel, ok := r.step(ctx)
	if !ok {
		return false
	}
	defer cancel()
	admins := r.adminNumbers(sctx, t, s)
	if len(admins) == 0 {
		r.cfg.Logger.Info("staff notice: no admin WhatsApp number for the tenant", zap.String("tenant", t.Slug), zap.String("source", s.AdminSource))
		return false
	}
	notice := StaffWhatsAppText(req)
	queued := false
	for _, a := range admins {
		err := r.cfg.Messages.Send(sctx, sender, MessageRequest{
			Channel:  ChannelWhatsApp,
			Template: "broadcast/message",
			To:       []string{a.Phone},
			Data:     map[string]any{"body": notice},
			Metadata: map[string]any{
				"template_name":     s.WhatsAppTemplate,
				"template_params":   []string{templateParam(firstNonEmpty(a.FirstName, "there"), 60), templateParam("Vera on the "+t.Name+" website", 80), notice},
				"default_dial_code": s.DialCode,
			},
		}, "vera-support:"+req.RequestID+":wa:"+a.Phone)
		if err != nil {
			r.cfg.Logger.Warn("staff notice: WhatsApp not queued", zap.String("tenant", t.Slug), zap.Error(err))
			continue
		}
		queued = true
	}
	return queued
}

// adminNumbers picks the WhatsApp recipients: the configured source first, then the other
// sources in a fixed order (configured numbers, admin users, support contact), so a tenant that
// has no admin phone on file still reaches its support WhatsApp number.
func (r *Router) adminNumbers(ctx context.Context, t Tenant, s Settings) []Admin {
	order := []string{s.AdminSource}
	for _, src := range []string{AdminSourceConfigured, AdminSourceUsers, AdminSourceSupport} {
		if src != s.AdminSource {
			order = append(order, src)
		}
	}
	max := s.MaxAdmins
	if max <= 0 {
		max = 3
	}
	for _, src := range order {
		var out []Admin
		switch src {
		case AdminSourceConfigured:
			for _, n := range s.AdminNumbers {
				out = append(out, Admin{Phone: n})
			}
		case AdminSourceUsers:
			if r.cfg.Admins != nil && t.ID != uuid.Nil {
				admins, err := r.cfg.Admins.Admins(ctx, t.ID, max)
				if err != nil {
					r.cfg.Logger.Warn("staff notice: admin phones not read", zap.String("tenant", t.Slug), zap.Error(err))
				}
				out = admins
			}
		case AdminSourceSupport:
			if t.SupportWhatsApp != "" {
				out = []Admin{{Phone: t.SupportWhatsApp}}
			}
		}
		if len(out) > max {
			out = out[:max]
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// step gives one step its budget: the step limit or what is left of the total, whichever is
// shorter. ok is false when less than MinStep is left.
func (r *Router) step(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	budget := r.cfg.StepBudget
	if dl, ok := ctx.Deadline(); ok {
		left := time.Until(dl)
		if left < MinStep {
			return ctx, func() {}, false
		}
		if left < budget {
			budget = left
		}
	}
	c, cancel := context.WithTimeout(ctx, budget)
	return c, cancel, true
}

func (r *Router) lookup(ctx context.Context, slug string) Tenant {
	if r.cfg.Tenants == nil || slug == "" {
		return Tenant{}
	}
	lctx, cancel := context.WithTimeout(ctx, DefaultLookup)
	defer cancel()
	t, err := r.cfg.Tenants(lctx, slug)
	if err != nil {
		r.cfg.Logger.Warn("support: tenant details not read", zap.String("tenant", slug), zap.Error(err))
		return Tenant{}
	}
	return t
}

// ── wording ─────────────────────────────────────────────────────────────────
// No reply time is ever promised unless the business set one (Settings.ReplyTime).

func replyLine(email, replyTime string) string {
	line := "They will reply to you"
	if email != "" {
		line += " at " + email
	}
	if replyTime != "" {
		line += ", usually " + replyTime
	}
	return line + "."
}

func platformTicketMessage(ref, email, replyTime string) string {
	return "This looks like a technical problem with the site, so I have passed it to Codevertex technical support, who run the platform. " +
		"Your reference is " + ref + ". " + replyLine(email, replyTime)
}

func tenantTicketMessage(name, ref, email, replyTime string) string {
	return "I have passed this to the " + name + " team. Your reference is " + ref + ". " + replyLine(email, replyTime)
}

func staffNotifiedMessage(name string, channels []string, email, replyTime string) string {
	how := "by email"
	switch {
	case contains(channels, ChannelEmail) && contains(channels, ChannelWhatsApp):
		how = "by email and WhatsApp"
	case contains(channels, ChannelWhatsApp):
		how = "on WhatsApp"
	}
	return name + " handles this directly, so I have sent your question to their team " + how + ". " + replyLine(email, replyTime)
}

func contactOnlyMessage(name string, c *Contact) string {
	switch {
	case c != nil && c.Email != "" && c.WhatsApp != "":
		return "I could not pass this on right now. You can reach the " + name + " team directly at " + c.Email + " or on WhatsApp at " + c.WhatsApp + "."
	case c != nil && c.Email != "":
		return "I could not pass this on right now. You can reach the " + name + " team directly at " + c.Email + "."
	case c != nil && c.WhatsApp != "":
		return "I could not pass this on right now. You can reach the " + name + " team directly on WhatsApp at " + c.WhatsApp + "."
	}
	return "I could not pass this on right now. Please try again in a few minutes."
}

// StaffEmailText is the email body the business's support address gets.
func StaffEmailText(tenantName string, req Request) string {
	var b strings.Builder
	b.WriteString("Hello " + tenantName + " team,\n\n")
	b.WriteString("A visitor on your website asked for help through Vera, the chat assistant, and their question is for your team.\n\n")
	b.WriteString(requesterLine(req) + "\n")
	if req.Page != "" {
		b.WriteString("Page: " + req.Page + "\n")
	}
	b.WriteString("\nTheir question:\n\n" + req.Message + "\n\n")
	b.WriteString("Please reply to them directly")
	if req.Email != "" {
		b.WriteString(" at " + req.Email)
	}
	b.WriteString(".")
	return b.String()
}

// StaffWhatsAppText is the one-line notice that goes into the WhatsApp template's message
// parameter. Meta refuses a parameter with line breaks, tabs or long runs of spaces, so it is a
// single line, capped.
func StaffWhatsAppText(req Request) string {
	who := firstNonEmpty(req.Name, "A visitor")
	var contact []string
	if req.Email != "" {
		contact = append(contact, req.Email)
	}
	if req.Phone != "" {
		contact = append(contact, req.Phone)
	}
	s := who
	if len(contact) > 0 {
		s += " (" + strings.Join(contact, ", ") + ")"
	}
	s += " asked on your website: " + req.Message + " Please reply to them directly."
	return templateParam(s, 900)
}

func requesterLine(req Request) string {
	parts := []string{"Name: " + firstNonEmpty(req.Name, "not given"), "Email: " + firstNonEmpty(req.Email, "not given")}
	if req.Phone != "" {
		parts = append(parts, "Phone: "+req.Phone)
	}
	return strings.Join(parts, "\n")
}

// templateParam makes text safe for a WhatsApp template parameter: one line, single spaces, at
// most n characters.
func templateParam(s string, n int) string {
	return clipRunes(strings.Join(strings.Fields(s), " "), n)
}

// SubjectFrom is the first line of the message, at most 120 characters.
func SubjectFrom(message string) string {
	line := strings.TrimSpace(message)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		line = "Support request"
	}
	return clipRunes(line, 120)
}

// Priority maps the widget's choice (Low, Medium, High (urgent)) or a ticketing priority to
// ticketing-api's values; anything else is normal.
func Priority(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch {
	case p == "low":
		return "low"
	case strings.HasPrefix(p, "high"), p == "urgent":
		return "high"
	default:
		return "normal"
	}
}

func pageHost(page, slug string) string {
	if u, err := url.Parse(page); err == nil && u.Host != "" {
		return u.Host
	}
	return "the " + slug + " website"
}

func paragraphs(text string) []string {
	var out []string
	for _, p := range strings.Split(text, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-3])) + "..."
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
