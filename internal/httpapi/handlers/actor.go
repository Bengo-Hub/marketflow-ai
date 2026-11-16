package handlers

import (
	"net/http"
	"strings"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/guardrail"
)

// actorFor is who an AI run acts for: the signed-in person from the token, never a value the
// request body or a model supplied. Service callers and the public widget have no user, only an
// origin, so marketflow-api applies the narrower gate for them.
// visitorSaid joins what a public visitor typed in this conversation (the question and their
// earlier messages), for guardrail.Actor.Said.
func visitorSaid(question string, history []ai.Message) string {
	var b strings.Builder
	for _, m := range history {
		if m.Role == "user" {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
	}
	b.WriteString(question)
	return b.String()
}

func actorFor(r *http.Request, origin string) guardrail.Actor {
	a := guardrail.Actor{Origin: origin}
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil && !c.IsService {
		if id, err := uuid.Parse(c.Subject); err == nil {
			a.UserID = id
		}
	}
	return a
}
