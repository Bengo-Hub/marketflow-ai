package support

import (
	"context"
	"strings"
)

// SettingsType is the MarketFlow ProviderSetting type that holds Vera's support routing. The
// provider name is "default" for the platform-wide values and the tenant slug for one tenant's
// own values, which override the default key by key. Both are platform rows, saved by a platform
// owner through POST /api/v1/admin/providers/configure (which replaces every key of that row,
// so send them all) and read through GET /api/v1/admin/providers/resolve, cached five minutes.
const SettingsType = "vera_support"

// Admin WhatsApp number sources.
const (
	// AdminSourceUsers reads the tenant's owner and admin phones from auth-api
	// (GET /api/v1/s2s/tenants/reach).
	AdminSourceUsers = "admin_users"
	// AdminSourceConfigured uses admin_whatsapp_numbers from the settings.
	AdminSourceConfigured = "configured"
	// AdminSourceSupport uses the tenant's support contact (auth-api metadata "support").
	AdminSourceSupport = "support_contact"
)

// Channels for the staff notice.
const (
	ChannelEmail    = "email"
	ChannelWhatsApp = "whatsapp"
)

// Settings is one tenant's support routing.
type Settings struct {
	// Enabled turns the routing on. Off, the widget's support form keeps the old behaviour: a
	// ticket on the tenant's own desk through ticketing-api's public route.
	Enabled bool
	// PlatformTickets lets technical requests become Codevertex platform tickets. Off, they are
	// handled like business requests.
	PlatformTickets bool
	// GateTag is the service tag the tenant's plan must include for a ticket in its own queue.
	GateTag string
	// FallbackChannels are the staff notice channels, in order, when the plan has no ticketing.
	FallbackChannels []string
	// AdminSource picks where the WhatsApp numbers come from (see the AdminSource constants).
	AdminSource string
	// AdminNumbers are the configured WhatsApp numbers (AdminSourceConfigured).
	AdminNumbers []string
	// MaxAdmins caps how many admins one notice goes to.
	MaxAdmins int
	// SupportEmail overrides the tenant's support contact email for the staff notice.
	SupportEmail string
	// WhatsAppTemplate is the approved platform template the WhatsApp notice uses. Its params
	// are first name, sender name and the message (broadcast_service_notice_v1 by default).
	WhatsAppTemplate string
	// DialCode reads local numbers (0712...) as international ones.
	DialCode string
	// ReplyTime is a reply time the business has set, shown to the visitor as is ("within one
	// business day"). Empty means no reply time is promised.
	ReplyTime string
}

// DefaultSettings is the code default under any saved row: routing off, so only tenants that
// were switched on (Hadia, 2026-11-28) change behaviour.
func DefaultSettings() Settings {
	return Settings{
		Enabled:          false,
		PlatformTickets:  true,
		GateTag:          "ticketing",
		FallbackChannels: []string{ChannelEmail, ChannelWhatsApp},
		AdminSource:      AdminSourceUsers,
		MaxAdmins:        3,
		WhatsAppTemplate: "broadcast_service_notice_v1",
		DialCode:         "254",
	}
}

// SettingsSource reads one ProviderSetting row (type, name); a missing row is an empty map.
type SettingsSource func(ctx context.Context, providerType, providerName string) (map[string]string, error)

// LoadSettings merges the tenant's row over the default row over the code defaults. A failed
// read is skipped (the next layer stands), so a marketflow-api outage never blocks a visitor.
func LoadSettings(ctx context.Context, src SettingsSource, tenantSlug string) Settings {
	s := DefaultSettings()
	if src == nil {
		return s
	}
	if m, err := src(ctx, SettingsType, "default"); err == nil {
		s = s.apply(m)
	}
	if slug := strings.ToLower(strings.TrimSpace(tenantSlug)); slug != "" && slug != "default" {
		if m, err := src(ctx, SettingsType, slug); err == nil {
			s = s.apply(m)
		}
	}
	return s
}

// apply sets every key present in m. Unknown keys and unparseable values are ignored.
func (s Settings) apply(m map[string]string) Settings {
	get := func(k string) (string, bool) {
		v, ok := m[k]
		return strings.TrimSpace(v), ok
	}
	if v, ok := get("enabled"); ok {
		if b, ok := parseBool(v); ok {
			s.Enabled = b
		}
	}
	if v, ok := get("platform_tickets"); ok {
		if b, ok := parseBool(v); ok {
			s.PlatformTickets = b
		}
	}
	if v, ok := get("gate_service_tag"); ok && v != "" {
		s.GateTag = strings.ToLower(v)
	}
	if v, ok := get("fallback_channels"); ok {
		var chans []string
		for _, c := range splitList(v) {
			c = strings.ToLower(c)
			if (c == ChannelEmail || c == ChannelWhatsApp) && !contains(chans, c) {
				chans = append(chans, c)
			}
		}
		s.FallbackChannels = chans
	}
	if v, ok := get("admin_whatsapp_source"); ok {
		switch v = strings.ToLower(v); v {
		case AdminSourceUsers, AdminSourceConfigured, AdminSourceSupport:
			s.AdminSource = v
		}
	}
	if v, ok := get("admin_whatsapp_numbers"); ok {
		s.AdminNumbers = splitList(v)
	}
	if v, ok := get("max_admins"); ok {
		if n := atoiPositive(v); n > 0 && n <= 10 {
			s.MaxAdmins = n
		}
	}
	if v, ok := get("support_email"); ok {
		s.SupportEmail = v
	}
	if v, ok := get("whatsapp_template"); ok && v != "" {
		s.WhatsAppTemplate = v
	}
	if v, ok := get("default_dial_code"); ok && v != "" {
		s.DialCode = strings.TrimPrefix(v, "+")
	}
	if v, ok := get("reply_time"); ok {
		s.ReplyTime = v
	}
	return s
}

func parseBool(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}

func atoiPositive(v string) int {
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 1000 {
			return 0
		}
	}
	return n
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
