package relayserver

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Snapshot checked 2026-10-07. Update these lists when the providers change their
// published ranges; no network dependency or background refresh in the relay.
// https://docs.cloud.google.com/load-balancing/docs/firewall-rules
// GFE backends: instance groups and zonal GCE_VM_IP_PORT NEGs, not internet NEGs.
var gfeProxyRanges = ipPrefixes("35.191.0.0/16 130.211.0.0/22 2600:2d00:1:1::/64")

// https://www.cloudflare.com/ips-v4 and https://www.cloudflare.com/ips-v6
var cloudflareProxyRanges = ipPrefixes(`
173.245.48.0/20 103.21.244.0/22 103.22.200.0/22 103.31.4.0/22
141.101.64.0/18 108.162.192.0/18 190.93.240.0/20 188.114.96.0/20
197.234.240.0/22 198.41.128.0/17 162.158.0.0/15 104.16.0.0/13
104.24.0.0/14 172.64.0.0/13 131.0.72.0/22
2400:cb00::/32 2606:4700::/32 2803:f800::/32 2405:b500::/32
2405:8100::/32 2a06:98c0::/29 2c0f:f248::/32`)

func ipPrefixes(raw string) []netip.Prefix {
	var ranges []netip.Prefix
	for _, s := range strings.Fields(raw) {
		ranges = append(ranges, netip.MustParsePrefix(s))
	}
	return ranges
}
func ipInRanges(ip netip.Addr, ranges []netip.Prefix) bool {
	for _, prefix := range ranges {
		if prefix.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}
func plainIP(raw string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	return ip.Unmap(), err == nil && ip.Zone() == ""
}

// registrationClientIP only selects an abuse-limiting bucket, never an identity
// or authorization principal. Direct mode ignores all forwarding headers.
func registrationClientIP(r *http.Request, mode string, forwardingIP netip.Addr) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	socket, ok := plainIP(host)
	if !ok {
		return host
	}
	host = socket.String()
	if mode != "gclb" || !forwardingIP.IsValid() || !ipInRanges(socket, gfeProxyRanges) {
		return host
	}

	// GCLB appends <connecting-client>,<forwarding-rule-IP>. Earlier entries are
	// caller-controlled. Only inspect the suffix, and only behind a verified GFE.
	// https://docs.cloud.google.com/load-balancing/docs/https#x-forwarded-for_header
	forwarded := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	last := strings.LastIndexByte(forwarded, ',')
	if last < 0 {
		return host
	}
	if lastIP, valid := plainIP(forwarded[last+1:]); !valid || lastIP != forwardingIP {
		return host
	}
	before := forwarded[:last]
	source, ok := plainIP(before[strings.LastIndexByte(before, ',')+1:])
	if !ok {
		return host
	}
	// A direct-origin caller gets its GCLB-recorded address, never its supplied
	// Cloudflare header. Only a Cloudflare connecting source vouches for that header.
	if ipInRanges(source, cloudflareProxyRanges) {
		values := r.Header.Values("CF-Connecting-IP")
		if len(values) == 1 {
			if client, valid := plainIP(values[0]); valid {
				return client.String()
			}
		}
	}
	return source.String()
}
