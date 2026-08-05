package netset

import "testing"

func TestContainsIPv4(t *testing.T) {
	// The six networks measured drawing watchgoose.com across the canvas.
	s := MustParse("115.190.40.0/24, 210.22.150.0/24, 188.40.122.0/24")

	in := []string{
		"115.190.40.10", "115.190.40.15", "115.190.40.0", "115.190.40.255",
		"210.22.150.146", "188.40.122.1",
	}
	for _, ip := range in {
		if !s.Contains(ip) {
			t.Errorf("Contains(%q) = false, want true", ip)
		}
	}
	out := []string{
		"115.190.41.10", "115.190.39.255", "210.22.151.1", "188.40.123.1",
		"8.8.8.8", "127.0.0.1", "", "not-an-ip",
	}
	for _, ip := range out {
		if s.Contains(ip) {
			t.Errorf("Contains(%q) = true, want false", ip)
		}
	}
}

func TestUnmaskedPrefixStillMatches(t *testing.T) {
	// Anyone writing these by hand will paste a host address with a /24 on it.
	// Masking on parse is what stops that entry silently matching nothing.
	s := MustParse("115.190.40.10/24")
	if !s.Contains("115.190.40.200") {
		t.Error("an unmasked prefix did not match its own network")
	}
}

func TestBareAddressIsASingleHost(t *testing.T) {
	s := MustParse("203.0.113.5")
	if !s.Contains("203.0.113.5") {
		t.Error("bare address did not match itself")
	}
	if s.Contains("203.0.113.6") {
		t.Error("bare address matched a neighbour, want host-only")
	}
}

func TestIPv6PrefixForm(t *testing.T) {
	// A session carries the grouped form from ratelimit.NetKey: a /64 for IPv6.
	// If that were not accepted this would never match an IPv6 client at all.
	s := MustParse("2001:db8::/32")
	for _, who := range []string{
		"2001:db8:1:2::1",   // plain address inside
		"2001:db8:1:2::/64", // the grouped form
		"2001:db8:ffff:ff::/64",
	} {
		if !s.Contains(who) {
			t.Errorf("Contains(%q) = false, want true", who)
		}
	}
	for _, who := range []string{"2001:db9::1", "2001:db9:1:2::/64"} {
		if s.Contains(who) {
			t.Errorf("Contains(%q) = true, want false", who)
		}
	}
}

func TestConfiguredNarrowerThanTheClientGroup(t *testing.T) {
	// A /64 configured against a client group of exactly that /64 must match.
	s := MustParse("2001:db8:0:1::/64")
	if !s.Contains("2001:db8:0:1::/64") {
		t.Error("a /64 did not match itself in prefix form")
	}
	if s.Contains("2001:db8:0:2::/64") {
		t.Error("a /64 matched a different /64")
	}
}

func TestIPv4MappedIPv6IsUnmapped(t *testing.T) {
	// ::ffff:1.2.3.4 and 1.2.3.4 are the same client, and a ban on one has to
	// hold for the other or it is trivially bypassed.
	s := MustParse("1.2.3.0/24")
	if !s.Contains("::ffff:1.2.3.4") {
		t.Error("mapped IPv4 escaped an IPv4 network")
	}
	s2 := MustParse("::ffff:1.2.3.4")
	if !s2.Contains("1.2.3.4") {
		t.Error("a mapped entry did not match the plain form")
	}
}

func TestZoneIsIgnored(t *testing.T) {
	s := MustParse("fe80::/10")
	if !s.Contains("fe80::1%eth0") {
		t.Error("a zone stopped the match")
	}
}

func TestEmptyAndNilSetMatchNothing(t *testing.T) {
	// An unconfigured set must be inert. Failing the other way would block
	// everybody the moment someone forgot the flag.
	empty, bad := Parse("")
	if len(bad) != 0 || empty.Len() != 0 {
		t.Fatalf("Parse(\"\") = %d nets, %v bad", empty.Len(), bad)
	}
	var nilSet *Set
	for _, s := range []*Set{empty, nilSet} {
		if s.Contains("8.8.8.8") {
			t.Error("an empty set matched something")
		}
		if s.Len() != 0 {
			t.Error("an empty set reported a length")
		}
		if s.Which("8.8.8.8") != "" {
			t.Error("an empty set named a network")
		}
	}
}

func TestBadEntriesAreReportedNotFatal(t *testing.T) {
	s, bad := Parse("115.190.40.0/24, nonsense, 10.0.0.0/99, , 8.8.8.8")
	if s.Len() != 2 {
		t.Errorf("parsed %d nets, want 2", s.Len())
	}
	want := []string{"nonsense", "10.0.0.0/99"}
	if len(bad) != len(want) {
		t.Fatalf("bad = %v, want %v", bad, want)
	}
	for i := range want {
		if bad[i] != want[i] {
			t.Errorf("bad[%d] = %q, want %q", i, bad[i], want[i])
		}
	}
	// The good entries still work.
	if !s.Contains("115.190.40.7") || !s.Contains("8.8.8.8") {
		t.Error("a bad entry broke the good ones")
	}
}

func TestWhichNamesTheMatchingNetwork(t *testing.T) {
	s := MustParse("115.190.40.0/24,210.22.150.0/24")
	if got, want := s.Which("210.22.150.146"), "210.22.150.0/24"; got != want {
		t.Errorf("Which = %q, want %q", got, want)
	}
	if got := s.Which("8.8.8.8"); got != "" {
		t.Errorf("Which = %q, want empty", got)
	}
}

func TestStringRoundTripsForLogging(t *testing.T) {
	s := MustParse("115.190.40.0/24,8.8.8.8")
	if got, want := s.String(), "115.190.40.0/24,8.8.8.8"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}
