package middleware

import (
	"encoding/json"
	"net/http"
	"strings"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

// DenyRestrictedRoles refuses signed-in callers whose only MarketFlow role is a restricted one,
// such as marketflow_reviewer (the Meta App Review account): that role may connect, boost, post
// and reply in marketflow-api, but never use AI chat or make media here, which cost credits.
// Callers without user claims (the platform service key) and platform owners pass. The roles
// come from config (MF_SECURITY_RESTRICTED_ROLES); an empty list switches the guard off.
func DenyRestrictedRoles(restricted []string) func(http.Handler) http.Handler {
	return denyRestrictedRoles(restricted, func(r *http.Request) (*authclient.Claims, bool) {
		return authclient.ClaimsFromContext(r.Context())
	})
}

func denyRestrictedRoles(restricted []string, claimsOf func(*http.Request) (*authclient.Claims, bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(restricted) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c, ok := claimsOf(r); ok && restrictedOnly(c, restricted) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error":   "role_not_allowed",
					"message": "This account cannot use AI features.",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// restrictedOnly reports whether every role the caller holds is restricted.
func restrictedOnly(c *authclient.Claims, restricted []string) bool {
	if c == nil || c.IsPlatformOwner || len(c.Roles) == 0 {
		return false
	}
	set := make(map[string]bool, len(restricted))
	for _, r := range restricted {
		set[strings.ToLower(strings.TrimSpace(r))] = true
	}
	for _, r := range c.Roles {
		if !set[strings.ToLower(strings.TrimSpace(r))] {
			return false
		}
	}
	return true
}
