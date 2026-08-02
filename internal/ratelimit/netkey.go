package ratelimit

import "net/netip"

// IPv6Prefix is the prefix length IPv6 clients are grouped by.
//
// A /64 is the smallest block an IPv6 customer is normally assigned, and many
// are handed something larger still.
const IPv6Prefix = 64

// NetKey reduces a client address to the network whose budget it shares.
//
// This is what makes the per-address limit mean anything. An IPv4 address is its
// own network, but IPv6 is not: one residential customer is routinely handed a
// whole /64, which is 18 quintillion source addresses. Keying a budget on the
// full address would hand a fresh one to every address in that block, so an
// attacker with any ordinary IPv6 allocation could rotate addresses as freely as
// they rotate keys and never hit a limit at all. Group IPv6 by /64 instead.
//
// IPv4-mapped IPv6 addresses are unmapped first, so ::ffff:198.51.100.7 and
// 198.51.100.7 land on the same key rather than being handed one budget each.
// An address that will not parse is returned unchanged: it is better to limit an
// unrecognised client too strictly than not at all.
func NetKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	// A zone (%eth0) is not part of the identity for limiting purposes.
	addr = addr.WithZone("").Unmap()

	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(IPv6Prefix)
	if err != nil {
		return addr.String()
	}
	return prefix.String()
}
