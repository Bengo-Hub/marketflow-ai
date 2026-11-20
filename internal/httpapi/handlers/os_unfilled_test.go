package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
)

// A template brief with a raw ${field} never reaches the team or the preview: it is refused with
// the field names, before any credit is charged.
func TestAskRefusesUnfilledTemplateFields(t *testing.T) {
	h := &OSHandler{}
	claims := &authclient.Claims{TenantID: uuid.NewString()}
	body := `{"text":"Write ads for ${offering} on ${platforms} this week, budget ${budget}"}`
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){"ask": h.Ask, "optimize": h.Optimize} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/os/"+name, strings.NewReader(body))
		r = r.WithContext(authclient.ContextWithClaims(r.Context(), claims))
		w := httptest.NewRecorder()
		call(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", name, w.Code)
		}
		if !strings.Contains(w.Body.String(), "offering, platforms, budget") {
			t.Fatalf("%s: message should name the fields, got %s", name, w.Body.String())
		}
	}
}
