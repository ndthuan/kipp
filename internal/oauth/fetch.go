package oauth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// fetchTimeout bounds the fetch of a client's metadata document.
const fetchTimeout = 5 * time.Second

// refused are the addresses a client's metadata document may not be fetched
// from beyond what netip already calls private, loopback or link-local:
// the ranges that name this host or another network's inside.
var refused = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "2002::/16",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// blocked reports whether an address is one that kipp, which is likely
// inside somebody's network, must not be made to call by a stranger's
// client_id.
func blocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || !ip.IsValid() {
		return true
	}
	for _, p := range refused {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// errBlocked is a dial the guard stopped.
var errBlocked = errors.New("refusing to connect to a private, loopback or link-local address")

// refuseBlocked is the dialer's check, made on the address the name
// resolved to, at the moment of the connection: a name that answers a
// public address to the check and a private one to the connection (DNS
// rebinding) gets no connection.
func refuseBlocked(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q is not an address", errBlocked, host)
	}
	if blocked(ip) {
		return fmt.Errorf("%w (%s)", errBlocked, ip)
	}
	return nil
}

// GuardedClient is the HTTP client a metadata document is fetched with: it
// uses no proxy (one would be the address connected to), connects only to
// public addresses, and gives up after a few seconds.
func GuardedClient() *http.Client {
	d := &net.Dialer{Timeout: fetchTimeout, Control: refuseBlocked}
	return &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			Proxy: nil, DialContext: d.DialContext, TLSHandshakeTimeout: fetchTimeout,
			ResponseHeaderTimeout: fetchTimeout, DisableKeepAlives: true, ForceAttemptHTTP2: true,
		},
	}
}
