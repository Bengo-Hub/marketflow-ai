package guardrail

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	"github.com/Bengo-Hub/httpware/netguard"
)

// Outbound fetches of URLs that came from data (scene, actor and music URLs, tenant uploads) use
// the shared httpware/netguard guard, the same rule marketflow-api's publicweb uses: http or
// https on ports 80 and 443, at most 5 redirects, and every connection checked at dial time
// against private, loopback, link-local (cloud metadata), shared and reserved ranges, so DNS
// rebinding cannot reach them.

// ErrBlockedAddress is returned when a fetch would reach a non-public address.
var ErrBlockedAddress = netguard.ErrBlocked

// PublicAddr reports whether ip is a public unicast address.
func PublicAddr(ip netip.Addr) bool { return netguard.IsPublic(ip) }

func fetchOptions(timeout time.Duration, allowPrivate bool) netguard.Options {
	return netguard.Options{Timeout: timeout, AllowHTTP: true, AllowPrivate: allowPrivate}
}

// SafeHTTPClient returns a client for fetching URLs that came from data. allowPrivate turns the
// address check off and exists for tests that serve files from 127.0.0.1.
func SafeHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	return netguard.Client(fetchOptions(timeout, allowPrivate))
}

// CheckFetchURL refuses a URL that is not http(s), carries credentials, names another port or
// is a non-public IP literal, before any request.
func CheckFetchURL(ctx context.Context, raw string) error {
	return CheckFetchURLWith(ctx, raw, false)
}

// CheckFetchURLWith is CheckFetchURL with the test switch SafeHTTPClient has: allowPrivate skips
// the address and port checks (scheme and credentials are still checked).
func CheckFetchURLWith(ctx context.Context, raw string, allowPrivate bool) error {
	_, err := fetchOptions(0, allowPrivate).CheckURL(ctx, raw)
	return err
}
