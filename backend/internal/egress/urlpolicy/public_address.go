package urlpolicy

import "net/netip"

// public_address.go classifies resolved addresses before a public RPC client
// chooses its dial target. A hostname can return several answers. The caller
// checks every answer with this policy and connects only to an address from
// that checked set. The ranges follow the pinned cryptopay implementation.

var (
	// These ranges include special-purpose routes that the standard global
	// unicast predicate accepts, including documentation and transition networks.
	publicRPCExcludedRanges = [...]netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("168.63.129.16/32"), netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
	}
	publicRPCGlobalIPv6 = netip.MustParsePrefix("2000::/3")
)

// IsPublicResolvedIP reports whether one DNS result may be dialed by a public
// RPC client. Mapped IPv4 addresses use the same range rules as their native
// IPv4 representation. Scoped and special-purpose addresses are rejected.
func IsPublicResolvedIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.Is6() && !publicRPCGlobalIPv6.Contains(ip) {
		return false
	}
	for _, prefix := range publicRPCExcludedRanges {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
