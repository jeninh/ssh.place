package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestNetKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ipv4 is its own network", "198.51.100.7", "198.51.100.7"},
		{"another ipv4", "10.0.0.1", "10.0.0.1"},
		{"ipv6 groups by /64", "2001:db8:dead:beef::1", "2001:db8:dead:beef::/64"},
		{"same /64, different host", "2001:db8:dead:beef:ffff:ffff:ffff:ffff", "2001:db8:dead:beef::/64"},
		{"neighbouring /64 differs", "2001:db8:dead:beee::1", "2001:db8:dead:beee::/64"},
		{"ipv4-mapped unmaps", "::ffff:198.51.100.7", "198.51.100.7"},
		{"loopback v6", "::1", "::/64"},
		{"zone is ignored", "fe80::1%eth0", "fe80::/64"},
		{"garbage passes through", "not-an-address", "not-an-address"},
		{"empty passes through", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NetKey(tc.in); got != tc.want {
				t.Errorf("NetKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The whole point: every address in one /64 has to share a single budget.
func TestNetKeyCollapsesAWholeIPv6Block(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 5000; i++ {
		seen[NetKey(fmt.Sprintf("2001:db8:dead:beef::%x", i))] = true
	}
	if len(seen) != 1 {
		t.Errorf("5000 addresses in one /64 produced %d keys, want 1", len(seen))
	}

	// An IPv4-mapped form must not be a second budget for the same host.
	if NetKey("::ffff:203.0.113.9") != NetKey("203.0.113.9") {
		t.Error("an IPv4 address and its mapped form get separate budgets")
	}
}

// End to end through the limiter: rotating addresses inside a /64 must not buy
// more placements, which is the bypass this exists to close.
func TestLimiterContainsIPv6AddressRotation(t *testing.T) {
	const burst = 5
	l := New(0, burst, time.Hour)

	allowed := 0
	for i := 0; i < 500; i++ {
		ip := fmt.Sprintf("2001:db8:dead:beef::%x", i)
		if ok, _, _ := l.Reserve(fmt.Sprintf("throwaway-key-%d", i), NetKey(ip), base); ok {
			allowed++
		}
	}
	if allowed != burst {
		t.Errorf("%d placements from one /64 with 500 keys, want exactly %d", allowed, burst)
	}

	// A genuinely different /64 still gets its own budget.
	if ok, _, _ := l.Reserve("other", NetKey("2001:db8:dead:bee0::1"), base); !ok {
		t.Error("a different /64 was refused; the grouping is too broad")
	}
}
