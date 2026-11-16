package guardrail

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// Origins of an AI action, sent to marketflow-api so it can apply the right gate.
const (
	OriginChat     = "chat"      // a signed-in person in the dashboard or the OS console
	OriginPublic   = "public"    // the public widget or a lead reply: an anonymous visitor
	OriginAgentRun = "agent_run" // a custom agent run started by an event
	OriginWhatsApp = "whatsapp"  // a verified owner or admin on WhatsApp
)

// Actor is who an AI action is done for. UserID is the SSO user from the token (or the verified
// WhatsApp sender), never a value the model or the message supplied.
type Actor struct {
	UserID uuid.UUID
	Origin string
	// Said is what an anonymous visitor typed in this conversation (public origin only). A tool
	// that acts on a visitor's detail, such as an email for a booking, takes it only when the
	// visitor wrote it, never from a web page or a tool result.
	Said string
}

// VisitorWrote reports whether the visitor typed value (case-insensitive). It is true for every
// non-public origin, where a signed-in person or the business itself is acting.
func (a Actor) VisitorWrote(value string) bool {
	if a.Origin != OriginPublic {
		return true
	}
	value = strings.ToLower(strings.TrimSpace(value))
	return value != "" && strings.Contains(strings.ToLower(a.Said), value)
}

type actorKey struct{}

// WithActor records who the current AI run acts for.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor recorded on ctx, if any.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok && a.Origin != ""
}

// Header names marketflow-api reads on service-key calls.
const (
	HeaderActingUser   = "X-Acting-User-ID"
	HeaderActingOrigin = "X-Acting-Origin"
)

// ActingTransport adds the actor headers to every service-key request made with ctx, so
// marketflow-api applies the person's own role instead of trusting the shared key. A request
// without an actor goes out as before.
type ActingTransport struct {
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t ActingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if a, ok := ActorFrom(req.Context()); ok && req.Header.Get("X-API-Key") != "" {
		req = req.Clone(req.Context())
		req.Header.Set(HeaderActingOrigin, strings.ToLower(a.Origin))
		if a.UserID != uuid.Nil {
			req.Header.Set(HeaderActingUser, a.UserID.String())
		}
	}
	return base.RoundTrip(req)
}
