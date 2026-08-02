package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// base is a fixed instant; every test advances from it explicitly so nothing
// depends on wall-clock timing.
var base = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// generous IP settings for tests that only care about the identity cooldown.
func cooldownOnly(cooldown time.Duration) *Limiter {
	return New(cooldown, 1_000_000, time.Nanosecond)
}

func TestCooldownBlocksSecondPlacement(t *testing.T) {
	const cooldown = 15 * time.Second
	l := cooldownOnly(cooldown)

	if ok, _, _ := l.Reserve("key-a", "1.1.1.1", base); !ok {
		t.Fatal("first placement refused")
	}

	// Immediately after, and at every point strictly inside the window.
	for _, elapsed := range []time.Duration{0, time.Millisecond, 7 * time.Second, cooldown - time.Nanosecond} {
		ok, retry, _ := l.Reserve("key-a", "1.1.1.1", base.Add(elapsed))
		if ok {
			t.Errorf("placement at +%s allowed, want refused", elapsed)
		}
		if want := cooldown - elapsed; retry != want {
			t.Errorf("retryAfter at +%s = %s, want %s", elapsed, retry, want)
		}
	}

	// Exactly at the boundary the cooldown has elapsed.
	if ok, retry, _ := l.Reserve("key-a", "1.1.1.1", base.Add(cooldown)); !ok {
		t.Errorf("placement at +%s refused (retry %s), want allowed", cooldown, retry)
	}
}

func TestCooldownIsPerIdentity(t *testing.T) {
	l := cooldownOnly(15 * time.Second)

	if ok, _, _ := l.Reserve("key-a", "1.1.1.1", base); !ok {
		t.Fatal("key-a refused")
	}
	// A different key from the same address is unaffected by key-a's cooldown.
	if ok, _, _ := l.Reserve("key-b", "1.1.1.1", base); !ok {
		t.Error("key-b refused, want allowed: the cooldown is per identity")
	}
	if ok, _, _ := l.Reserve("key-a", "2.2.2.2", base); ok {
		t.Error("key-a allowed from a new address, want refused: the cooldown follows the key")
	}
}

// A refused attempt must not extend the wait, or a client polling the key would
// lock itself out forever.
func TestRefusalDoesNotExtendCooldown(t *testing.T) {
	const cooldown = 15 * time.Second
	l := cooldownOnly(cooldown)

	if ok, _, _ := l.Reserve("key", "1.1.1.1", base); !ok {
		t.Fatal("first placement refused")
	}
	for i := 0; i < 50; i++ {
		l.Reserve("key", "1.1.1.1", base.Add(time.Duration(i)*100*time.Millisecond))
	}
	if ok, retry, _ := l.Reserve("key", "1.1.1.1", base.Add(cooldown)); !ok {
		t.Errorf("placement after the full cooldown refused (retry %s)", retry)
	}
}

func TestRemainingDoesNotConsume(t *testing.T) {
	const cooldown = 10 * time.Second
	l := cooldownOnly(cooldown)

	if d := l.Remaining("key", base); d != 0 {
		t.Errorf("Remaining for an unseen key = %s, want 0", d)
	}
	if ok, _, _ := l.Reserve("key", "1.1.1.1", base); !ok {
		t.Fatal("refused")
	}
	if d := l.Remaining("key", base.Add(4*time.Second)); d != 6*time.Second {
		t.Errorf("Remaining = %s, want 6s", d)
	}
	// Asking many times changes nothing.
	for i := 0; i < 10; i++ {
		l.Remaining("key", base.Add(4*time.Second))
	}
	if ok, _, _ := l.Reserve("key", "1.1.1.1", base.Add(cooldown)); !ok {
		t.Error("Remaining consumed the placement budget")
	}
}

// The per-IP bucket is what blunts someone generating a fresh key per
// placement, so it has to hold even when every identity is brand new.
func TestIPBucketLimitsKeyRotation(t *testing.T) {
	const (
		burst  = 5
		refill = 15 * time.Second
	)
	l := New(15*time.Second, burst, refill)

	for i := 0; i < burst; i++ {
		if ok, retry, _ := l.Reserve(fmt.Sprintf("fresh-key-%d", i), "9.9.9.9", base); !ok {
			t.Fatalf("placement %d refused (retry %s), want allowed within the burst", i, retry)
		}
	}
	ok, retry, _ := l.Reserve("fresh-key-99", "9.9.9.9", base)
	if ok {
		t.Fatal("placement past the burst allowed, want refused")
	}
	if retry <= 0 || retry > refill {
		t.Errorf("retryAfter = %s, want in (0, %s]", retry, refill)
	}

	// One refill period buys exactly one more placement.
	if ok, _, _ := l.Reserve("fresh-key-100", "9.9.9.9", base.Add(refill)); !ok {
		t.Error("placement after one refill period refused")
	}
	if ok, _, _ := l.Reserve("fresh-key-101", "9.9.9.9", base.Add(refill)); ok {
		t.Error("second placement in the same refill period allowed")
	}
}

func TestIPBucketIsPerAddress(t *testing.T) {
	l := New(time.Second, 1, time.Minute)

	if ok, _, _ := l.Reserve("a", "1.1.1.1", base); !ok {
		t.Fatal("refused")
	}
	if ok, _, _ := l.Reserve("b", "1.1.1.1", base); ok {
		t.Error("second placement from the same IP allowed, want refused")
	}
	if ok, _, _ := l.Reserve("c", "2.2.2.2", base); !ok {
		t.Error("placement from a different IP refused")
	}
}

func TestIPBucketCapsAtBurst(t *testing.T) {
	const burst = 3
	// No identity cooldown, so only the bucket can refuse anything here.
	l := New(0, burst, time.Second)

	// Idle for a long time: the bucket must not accumulate beyond the burst.
	later := base.Add(time.Hour)
	for i := 0; i < burst; i++ {
		if ok, _, _ := l.Reserve("k", "1.1.1.1", later); !ok {
			t.Fatalf("placement %d refused", i)
		}
	}
	if ok, _, _ := l.Reserve("k", "1.1.1.1", later); ok {
		t.Error("bucket accumulated past its burst while idle")
	}
}

// A backwards clock jump (NTP correction, VM restore) must not strand a player.
func TestClockGoingBackwards(t *testing.T) {
	l := cooldownOnly(15 * time.Second)
	if ok, _, _ := l.Reserve("k", "1.1.1.1", base); !ok {
		t.Fatal("refused")
	}
	if d := l.Remaining("k", base.Add(-time.Hour)); d != 0 {
		t.Errorf("Remaining with a rewound clock = %s, want 0", d)
	}
	if ok, _, _ := l.Reserve("k", "1.1.1.1", base.Add(-time.Hour)); !ok {
		t.Error("Reserve with a rewound clock refused")
	}
}

func TestPruneDropsExpiredEntries(t *testing.T) {
	const cooldown = 15 * time.Second
	l := New(cooldown, 5, cooldown)

	for i := 0; i < 10; i++ {
		l.Reserve(fmt.Sprintf("key-%d", i), fmt.Sprintf("10.0.0.%d", i), base)
	}
	ids, ips := l.Len()
	if ids != 10 || ips != 10 {
		t.Fatalf("tracked %d identities and %d ips, want 10 and 10", ids, ips)
	}

	// Nothing has expired yet.
	if n := l.Prune(base.Add(time.Second)); n != 0 {
		t.Errorf("Prune removed %d entries too early", n)
	}

	// Well past both the cooldown and a full bucket refill.
	l.Prune(base.Add(time.Hour))
	ids, ips = l.Len()
	if ids != 0 || ips != 0 {
		t.Errorf("after Prune: %d identities, %d ips, want 0 and 0", ids, ips)
	}

	// Pruning must not hand out free placements: a pruned identity is one whose
	// cooldown had already elapsed.
	if ok, _, _ := l.Reserve("key-0", "10.0.0.0", base.Add(time.Hour)); !ok {
		t.Error("placement after prune refused")
	}
}

func TestConcurrentReserve(t *testing.T) {
	const (
		workers = 16
		rounds  = 200
	)
	// A cooldown of zero and a huge bucket: every call should be allowed, so
	// any refusal points at a lost update rather than a real limit.
	l := New(0, 1_000_000, time.Nanosecond)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			identity := fmt.Sprintf("key-%d", id)
			for r := 0; r < rounds; r++ {
				now := base.Add(time.Duration(r) * time.Millisecond)
				if ok, _, _ := l.Reserve(identity, "1.1.1.1", now); !ok {
					t.Errorf("worker %d round %d refused unexpectedly", id, r)
					return
				}
				l.Remaining(identity, now)
				l.Len()
			}
		}(w)
	}
	// Prune concurrently to exercise the same maps from another angle.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := 0; r < rounds; r++ {
			l.Prune(base.Add(time.Duration(r) * time.Millisecond))
		}
	}()
	wg.Wait()
}

// Two clients sharing one address must each be limited, and the shared bucket
// must be enforced without either being able to see the other's identity.
func TestConcurrentSharedIPRespectsBurst(t *testing.T) {
	const burst = 4
	l := New(0, burst, time.Hour) // no refill within the test

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if ok, _, _ := l.Reserve(fmt.Sprintf("key-%d", id), "5.5.5.5", base); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if allowed != burst {
		t.Errorf("%d placements allowed, want exactly %d", allowed, burst)
	}
}

func TestCooldownAccessor(t *testing.T) {
	if got := New(7*time.Second, 1, time.Second).Cooldown(); got != 7*time.Second {
		t.Errorf("Cooldown = %s, want 7s", got)
	}
}

func TestNewNormalizesBadArguments(t *testing.T) {
	// A zero burst would refuse every placement; a zero refill would divide by
	// zero. Both are clamped instead.
	l := New(time.Second, 0, 0)
	if ok, _, _ := l.Reserve("k", "1.1.1.1", base); !ok {
		t.Error("Reserve with a zero burst refused, want the burst clamped to 1")
	}
	if ok, _, _ := l.Reserve("j", "1.1.1.1", base); ok {
		t.Error("second placement allowed, want the burst clamped to exactly 1")
	}
}
