package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/jeninh/ssh.place/internal/app"
	"github.com/jeninh/ssh.place/internal/canvas"
	"github.com/jeninh/ssh.place/internal/hub"
	"github.com/jeninh/ssh.place/internal/ratelimit"
)

func TestDerivedIPRefill(t *testing.T) {
	cases := []struct {
		cooldown time.Duration
		maxPerIP int
		want     time.Duration
	}{
		{15 * time.Second, 5, 3 * time.Second},
		{15 * time.Second, 1, 15 * time.Second},
		{30 * time.Second, 3, 10 * time.Second},
		{15 * time.Second, 0, 15 * time.Second}, // no per-network session cap
		{0, 5, time.Second},                     // cooldown disabled
	}
	for _, tc := range cases {
		if got := derivedIPRefill(tc.cooldown, tc.maxPerIP); got != tc.want {
			t.Errorf("derivedIPRefill(%s, %d) = %s, want %s", tc.cooldown, tc.maxPerIP, got, tc.want)
		}
	}
}

// The headline promise is one cell per cooldown, per person. That has to hold for
// people sharing an address, which is most people: campus wifi, an office, a
// carrier-grade NAT, or one household on a /64. Sizing the network budget at one
// player's rate silently starved everybody but the fastest asker.
func TestSharedNetworkUsersEachGetTheirFullRate(t *testing.T) {
	const (
		cooldown = 15 * time.Second
		maxPerIP = 5
		minutes  = 4
	)
	refill := derivedIPRefill(cooldown, maxPerIP)

	a := &app.App{
		Canvas:     canvas.New(60, 20),
		Hub:        hub.New(500, maxPerIP),
		Limiter:    ratelimit.New(cooldown, maxPerIP, refill),
		BlocksOnly: true,
	}

	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	ticks := minutes * 60 / int(cooldown.Seconds())

	landed := make([]int, maxPerIP)
	for tick := 0; tick < ticks; tick++ {
		now := start.Add(time.Duration(tick) * cooldown)
		for i := 0; i < maxPerIP; i++ {
			s, err := a.Hub.Add("198.51.100.7", fmt.Sprintf("key-%d", i), true)
			if err != nil {
				t.Fatalf("add session: %v", err)
			}
			if _, err := a.Place(s, i, tick%20, canvas.Block(1), now); err == nil {
				landed[i]++
			}
			a.Hub.Remove(s)
		}
	}

	for i, n := range landed {
		if n != ticks {
			t.Errorf("user %d placed %d cells in %d minutes, want %d: sharing a network must not cost you your own rate",
				i, n, minutes, ticks)
		}
	}
}

// Sharing must not become a way to gain, either: more sessions than the network
// is allowed to hold cannot squeeze out extra placements.
func TestNetworkBudgetStillCapsKeyRotation(t *testing.T) {
	const (
		cooldown = 15 * time.Second
		maxPerIP = 5
	)
	refill := derivedIPRefill(cooldown, maxPerIP)

	a := &app.App{
		Canvas:     canvas.New(60, 20),
		Hub:        hub.New(500, 0), // session cap off, so only the budget can refuse
		Limiter:    ratelimit.New(cooldown, maxPerIP, refill),
		BlocksOnly: true,
	}

	// 200 throwaway keys all firing at the same instant from one network.
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	landed := 0
	for i := 0; i < 200; i++ {
		s, err := a.Hub.Add("198.51.100.7", fmt.Sprintf("throwaway-%d", i), true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Place(s, i%60, i/60, canvas.Block(1), now); err == nil {
			landed++
		}
		a.Hub.Remove(s)
	}
	if landed > maxPerIP {
		t.Errorf("%d placements landed at one instant from one network, want at most %d", landed, maxPerIP)
	}
}

func TestBoardChurnCeiling(t *testing.T) {
	const cells = 200 * 60
	cases := []struct {
		minFill  time.Duration
		cooldown time.Duration
		wantRate float64
	}{
		{time.Hour, 15 * time.Second, 12000.0 / 3600},
		{30 * time.Minute, 15 * time.Second, 12000.0 / 1800},
		{0, 15 * time.Second, 0}, // disabled
	}
	for _, tc := range cases {
		rate, burst := boardChurnCeiling(cells, tc.minFill, tc.cooldown)
		if rate != tc.wantRate {
			t.Errorf("minFill %s: rate = %v, want %v", tc.minFill, rate, tc.wantRate)
		}
		if tc.wantRate > 0 && burst < 1 {
			t.Errorf("minFill %s: burst = %d, want at least 1", tc.minFill, burst)
		}
	}
	// A bigger canvas has to allow proportionally more churn for the same floor.
	small, _ := boardChurnCeiling(1000, time.Hour, 15*time.Second)
	big, _ := boardChurnCeiling(4000, time.Hour, 15*time.Second)
	if big <= small {
		t.Error("the ceiling does not scale with the canvas size")
	}
}

// The headline guarantee: whatever a client controls, it cannot repaint the whole
// board faster than -min-board-fill. This is the limit the per-key and
// per-network ones cannot provide, since keys and networks are both cheap.
func TestBoardCannotBeRepaintedFasterThanTheFloor(t *testing.T) {
	const (
		width, height = 200, 60
		cells         = width * height
		cooldown      = 15 * time.Second
		maxPerIP      = 5
		minFill       = time.Hour
	)
	rate, burst := boardChurnCeiling(cells, minFill, cooldown)

	a := &app.App{
		Canvas: canvas.New(width, height),
		Hub:    hub.New(0, 0), // every session cap off: only the limiter may refuse
		Limiter: ratelimit.New(cooldown, maxPerIP, derivedIPRefill(cooldown, maxPerIP),
			ratelimit.WithGlobalRate(rate, burst)),
		BlocksOnly: true,
	}

	// An attacker with a fresh key and a fresh network for every single placement,
	// hammering as fast as the clock allows. Both per-client limits are useless
	// against this by construction.
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	const window = 5 * time.Minute
	landed := 0
	for step := 0; step < 20000; step++ {
		now := start.Add(time.Duration(step) * (window / 20000))
		s, err := a.Hub.Add(fmt.Sprintf("2001:db8:%x::1", step), fmt.Sprintf("key-%d", step), true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Place(s, step%width, (step/width)%height, canvas.Block(1), now); err == nil {
			landed++
		}
		a.Hub.Remove(s)
	}

	// In five minutes of an unlimited-resource attack, the ceiling should permit
	// only about five minutes' worth of the hour-long floor.
	allowed := int(rate*window.Seconds()) + burst + 1
	t.Logf("%d cells painted in %s with a fresh key AND network each time (%.1f%% of the board)",
		landed, window, 100*float64(landed)/cells)
	if landed > allowed {
		t.Errorf("%d placements landed, want at most %d", landed, allowed)
	}
	if landed >= cells {
		t.Errorf("the whole board was repainted inside %s", window)
	}
}

func TestParseAdminKeys(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantBad []string
	}{
		{name: "empty", raw: ""},
		{name: "blank", raw: "   "},
		{name: "one", raw: "SHA256:abc", want: []string{"SHA256:abc"}},
		{
			name: "several with padding",
			raw:  " SHA256:abc , SHA256:def ,, ",
			want: []string{"SHA256:abc", "SHA256:def"},
		},
		{
			// A typo must be visible at boot. Silently granting nobody anything is
			// the failure you only discover when you need the exemption.
			name:    "wrong format is reported",
			raw:     "abc,SHA256:def",
			want:    []string{"SHA256:def"},
			wantBad: []string{"abc"},
		},
		{
			// The full public key is the easy mistake to make: it is what you have
			// in authorized_keys, and it is not a fingerprint.
			name:    "public key is not a fingerprint",
			raw:     "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 jenin@laptop",
			wantBad: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 jenin@laptop"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, bad := parseFingerprints(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("parsed %d keys (%v), want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for _, f := range tt.want {
				if !got[f] {
					t.Errorf("missing %q from %v", f, got)
				}
			}
			if len(bad) != len(tt.wantBad) {
				t.Fatalf("bad = %v, want %v", bad, tt.wantBad)
			}
			for i, f := range tt.wantBad {
				if bad[i] != f {
					t.Errorf("bad[%d] = %q, want %q", i, bad[i], f)
				}
			}
		})
	}
}

func TestSignoff(t *testing.T) {
	tests := []struct {
		name           string
		web, community string
		want           string
	}{
		{"both", "https://ssh.place", "r/sshplace", "Timelapses and stats: https://ssh.place · r/sshplace"},
		{"web only", "https://ssh.place", "", "Timelapses and stats: https://ssh.place"},
		{"community only", "", "r/sshplace", "r/sshplace"},
		// Neither configured must produce nothing at all, not a stray separator on
		// the way out of every session.
		{"neither", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := signoff(tt.web, tt.community); got != tt.want {
				t.Errorf("signoff = %q, want %q", got, tt.want)
			}
		})
	}
}
