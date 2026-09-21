package netpolicy

import (
	"net"
	"net/netip"
)

// nonPublic is every IPv4 range that is not the public internet. The proxy dials
// from the host, so its connections skip the msbr0 forward-chain isolation that
// protects these ranges on the kernel path; this list is that isolation, redone.
var nonPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",      // "this network"
		"10.0.0.0/8",     // private; also the sprite network and podman
		"100.64.0.0/10",  // CGNAT, i.e. the tailnet
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local, cloud metadata
		"172.16.0.0/12",  // private
		"192.0.0.0/24",   // IETF protocol assignments
		"192.168.0.0/16", // private
		"198.18.0.0/15",  // benchmarking
		"224.0.0.0/4",    // multicast
		"240.0.0.0/4",    // reserved, and limited broadcast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// NonPublic reports whether a is anything other than a public IPv4 address.
func NonPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() {
		return true // guests have no IPv6, so nothing legitimate arrives as v6
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Blocked is NonPublic plus the host's own addresses: a host with a public
// address must not have its services exposed to sprites through the proxy.
// Interfaces are read on every call because they change (VPNs, DHCP).
func Blocked(a netip.Addr) bool {
	if NonPublic(a) {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true
	}
	for _, ifa := range addrs {
		if n, ok := ifa.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == a.Unmap() {
				return true
			}
		}
	}
	return false
}
