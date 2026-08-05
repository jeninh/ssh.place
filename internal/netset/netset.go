// Package netset matches client addresses against a list of networks.
//
// It exists because a fingerprint list cannot win an arms race against key
// rotation. Measured on the live canvas: 6,173 placements came from 2,642
// distinct keys, about 2.3 placements each, which means a fresh key per
// placement. Six networks accounted for all of it. Keys are free; a subnet is
// not, and it is the one thing an operator cannot regenerate on a whim.
package netset

import (
	"fmt"
	"net/netip"
	"strings"
)

// Set is an immutable list of networks. The zero value is empty and matches
// nothing, so an unconfigured Set is safe rather than silently blocking.
type Set struct {
	prefixes []netip.Prefix
	raw      []string
}

// Parse builds a Set from a comma separated list of CIDR blocks or bare
// addresses. A bare address is treated as a single host.
//
// Entries that do not parse are returned rather than rejected, so one typo in a
// config cannot stop the server coming up. The caller is expected to log them.
func Parse(list string) (*Set, []string) {
	if strings.TrimSpace(list) == "" {
		return &Set{}, nil
	}
	s := &Set{}
	var bad []string
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		p, err := parseOne(item)
		if err != nil {
			bad = append(bad, item)
			continue
		}
		s.prefixes = append(s.prefixes, p)
		s.raw = append(s.raw, item)
	}
	return s, bad
}

func parseOne(item string) (netip.Prefix, error) {
	if strings.Contains(item, "/") {
		p, err := netip.ParsePrefix(item)
		if err != nil {
			return netip.Prefix{}, err
		}
		// Masked so that 10.0.0.5/24 behaves as 10.0.0.0/24 rather than never
		// matching, which is the mistake anyone writing these by hand will make.
		return p.Masked(), nil
	}
	addr, err := netip.ParseAddr(item)
	if err != nil {
		return netip.Prefix{}, err
	}
	addr = addr.WithZone("").Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// Len is how many networks the set holds.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.prefixes)
}

// String lists the networks as configured, for logging.
func (s *Set) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(s.raw, ",")
}

// Contains reports whether who falls inside any network in the set.
//
// who may be a plain address, or the grouped form ratelimit.NetKey produces: an
// exact address for IPv4 and a /64 prefix for IPv6. Both are accepted because
// that grouped form is what a session actually carries, and refusing it would
// mean this never matched an IPv6 client at all.
func (s *Set) Contains(who string) bool {
	if s == nil || len(s.prefixes) == 0 || who == "" {
		return false
	}

	if addr, err := netip.ParseAddr(who); err == nil {
		addr = addr.WithZone("").Unmap()
		for _, p := range s.prefixes {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}

	// A prefix: match if the two overlap at all. A configured /48 has to catch a
	// client's /64 inside it, and a configured /64 has to catch that same /64.
	if got, err := netip.ParsePrefix(who); err == nil {
		got = got.Masked()
		for _, p := range s.prefixes {
			if p.Overlaps(got) {
				return true
			}
		}
	}
	return false
}

// Which returns the first matching network, for logging why something was
// refused. The empty string means no match.
func (s *Set) Which(who string) string {
	if s == nil || who == "" {
		return ""
	}
	for i, p := range s.prefixes {
		one := &Set{prefixes: []netip.Prefix{p}}
		if one.Contains(who) {
			return s.raw[i]
		}
	}
	return ""
}

// MustParse is Parse for tests and for a caller that has already validated.
func MustParse(list string) *Set {
	s, bad := Parse(list)
	if len(bad) > 0 {
		panic(fmt.Sprintf("netset: bad entries %v", bad))
	}
	return s
}
