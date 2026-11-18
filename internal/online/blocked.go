package online

import "net/netip"

// blockedPrefixes are not reachable on the internet (besides what
// netip's predicates cover): CGNAT and Tailscale, "this network", IETF
// protocol assignments, benchmarking, reserved, IPv4-compatible IPv6,
// site-local IPv6.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"), netip.MustParsePrefix("fec0::/10"),
}

// embedsIPv4: NAT64 (64:ff9b::/96, 64:ff9b:1::/48) and 6to4 (2002::/16)
// addresses carry an IPv4 address that is checked as well.
var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour  = netip.MustParsePrefix("2002::/16")
)

// blockedAddr reports addresses an upstream fetch may not reach unless the
// admin allows the home network: anything that is not global unicast
// (loopback, private, link-local and cloud metadata, multicast,
// unspecified), the non-global ranges above, and v6 forms embedding such
// an IPv4 address.
func blockedAddr(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return true
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	if a.Is6() {
		b := a.As16()
		var v4 [4]byte
		switch {
		case nat64.Contains(a):
			copy(v4[:], b[12:16])
		case nat64Local.Contains(a):
			copy(v4[:], b[12:16])
		case sixToFour.Contains(a):
			copy(v4[:], b[2:6])
		default:
			return false
		}
		return blockedAddr(netip.AddrFrom4(v4).String())
	}
	return false
}
