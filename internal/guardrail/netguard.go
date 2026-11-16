package guardrail

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// Outbound fetches of URLs that came from data (scene, actor and music URLs, tenant uploads) go
// through SafeHTTPClient: http or https only, ports 80 and 443, at most 5 redirects, and every
// connection is checked at dial time against private, loopback, link-local (the cloud metadata
// address 169.254.169.254), shared, benchmark, documentation, multicast and reserved ranges. The
// check runs on the address actually dialled, after DNS, so a name that resolves to a public
// address first and a private one later (DNS rebinding) is still refused.
//
// marketflow-api has the same rule for its own fetches (internal/platform/publicweb); moving both
// into the shared httpware module is a recorded follow-up.

// ErrBlockedAddress is returned when a fetch would reach a non-public address.
var ErrBlockedAddress = errors.New("destination address is not allowed")

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"255.255.255.255/32",
		"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
		"2001::/23", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// PublicAddr reports whether ip is a public unicast address.
func PublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// SafeHTTPClient returns a client for fetching URLs that came from data. allowPrivate turns the
// address check off and exists for tests that serve files from 127.0.0.1.
func SafeHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return ErrBlockedAddress
			}
			if !allowPrivate {
				if port != "80" && port != "443" {
					return fmt.Errorf("%w: port %s", ErrBlockedAddress, port)
				}
				ip, err := netip.ParseAddr(host)
				if err != nil || !PublicAddr(ip) {
					return ErrBlockedAddress
				}
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:                 nil, // a proxy would make the dial check see the proxy, not the target
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("%w: scheme %s", ErrBlockedAddress, req.URL.Scheme)
			}
			return nil
		},
	}
}

// CheckFetchURL refuses a URL that is not http(s) or carries credentials, before any request.
func CheckFetchURL(ctx context.Context, raw string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q", ErrBlockedAddress, req.URL.Scheme)
	}
	if req.URL.User != nil || req.URL.Hostname() == "" {
		return ErrBlockedAddress
	}
	return nil
}
