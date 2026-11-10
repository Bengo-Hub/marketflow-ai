package tools

import (
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// idPath is the only form a model-supplied ID takes in a marketflow-api URL path: its canonical
// UUID, or "invalid-id" (the API answers 404) so no tool argument can add path segments or a
// query to a request made with the service key (audit MF #8).
func idPath(v string) string {
	id, err := uuid.Parse(strings.TrimSpace(v))
	if err != nil {
		return "invalid-id"
	}
	return id.String()
}

var filterKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,39}$`)

// blockedFilterKeys can never come from a model: the row cap and scope switches.
var blockedFilterKeys = map[string]bool{"limit": true, "all": true, "include_deleted": true}

// safeFilterKey reports whether a model-supplied filter may be forwarded: a plain
// name that is not about the tenant (scoping is set by the tool, never by the model) and not a
// cap or scope switch (audit MF #8).
func safeFilterKey(k string) bool {
	if !filterKeyPattern.MatchString(k) || blockedFilterKeys[strings.ToLower(k)] {
		return false
	}
	return !strings.Contains(strings.ToLower(k), "tenant")
}
