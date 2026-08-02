// Package ratelimit enforces how often a player may place a cell.
//
// Three limits stack, in widening scope:
//
//   - The per-identity cooldown is the game rule: after placing, that SSH key
//     waits out the cooldown.
//   - The per-network token bucket blunts key rotation, since keys are free.
//   - The global ceiling bounds how fast the canvas as a whole can turn over.
//
// The third exists because the first two both scale with whatever the caller
// controls. Keys cost nothing, and one hosting allocation can be tens of
// thousands of distinct networks, so per-network limiting alone still lets a
// determined client repaint the entire board in minutes.
//
// Every method takes the current time explicitly. That keeps the limiter a
// pure function of its inputs, which is what makes the cooldown tests
// deterministic instead of sleep-based.
package ratelimit

import (
	"sync"
	"time"
)

// Refusal says which limit turned a placement down, so the caller can explain
// itself rather than blaming the player's own cooldown for everything.
type Refusal int

const (
	// Allowed means the placement may proceed.
	Allowed Refusal = iota
	// RefusedCooldown is the player's own wait between placements.
	RefusedCooldown
	// RefusedNetwork is the budget shared by everyone on their network.
	RefusedNetwork
	// RefusedGlobal is the whole canvas being at its churn limit.
	RefusedGlobal
)

// Limiter holds the three limits a placement has to satisfy: the player's own
// cooldown, their network's shared budget, and the canvas-wide churn ceiling.
//
// The first two both scale with an attacker's resources: keys are free, and a
// single hosting allocation can be tens of thousands of distinct networks. Only
// the global ceiling bounds the worst case, which is what stops the board being
// repainted end to end in minutes.
type Limiter struct {
	cooldown time.Duration
	ipBurst  float64
	ipRefill time.Duration

	// globalRefill is the time to earn one placement server-wide. Zero disables
	// the ceiling.
	globalRefill time.Duration
	globalBurst  float64

	mu     sync.Mutex
	ids    map[string]time.Time // identity -> time of last accepted placement
	ips    map[string]*bucket
	global *bucket
}

// Option adjusts a Limiter at construction.
type Option func(*Limiter)

// WithGlobalRate caps placements across the whole server at perSecond, allowing
// burst of them back to back.
//
// This is the only limit that holds no matter how many keys or networks the
// caller controls, so it is what actually bounds how fast the canvas can turn
// over. Pass a non-positive rate to leave it off.
func WithGlobalRate(perSecond float64, burst int) Option {
	return func(l *Limiter) {
		if perSecond <= 0 || burst < 1 {
			return
		}
		l.globalRefill = time.Duration(float64(time.Second) / perSecond)
		if l.globalRefill <= 0 {
			l.globalRefill = time.Nanosecond
		}
		l.globalBurst = float64(burst)
	}
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a Limiter.
//
// cooldown is the per-identity wait between placements. ipBurst is how many
// placements a single IP may make back-to-back and ipRefill is how long it
// takes to earn one more, so an IP settles at ipBurst placements immediately
// and one per ipRefill thereafter.
func New(cooldown time.Duration, ipBurst int, ipRefill time.Duration, opts ...Option) *Limiter {
	if ipBurst < 1 {
		ipBurst = 1
	}
	if ipRefill <= 0 {
		ipRefill = time.Second
	}
	l := &Limiter{
		cooldown: cooldown,
		ipBurst:  float64(ipBurst),
		ipRefill: ipRefill,
		ids:      make(map[string]time.Time),
		ips:      make(map[string]*bucket),
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Cooldown returns the configured per-identity cooldown.
func (l *Limiter) Cooldown() time.Duration { return l.cooldown }

// Reserve consumes one placement for identity and ip. It reports whether the
// placement is allowed and, when it is not, how long the caller must wait.
//
// Nothing is consumed on refusal, so a client that hammers the key does not
// push its own deadline further out.
func (l *Limiter) Reserve(identity, ip string, now time.Time) (ok bool, retryAfter time.Duration, why Refusal) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if wait := l.identityWaitLocked(identity, now); wait > 0 {
		return false, wait, RefusedCooldown
	}

	net := l.ips[ip]
	if net == nil {
		net = &bucket{tokens: l.ipBurst, last: now}
		l.ips[ip] = net
	} else {
		l.refill(net, now, l.ipRefill, l.ipBurst)
	}
	if net.tokens < 1 {
		return false, l.waitFor(net, l.ipRefill), RefusedNetwork
	}

	// The global ceiling is checked last and consumed with the rest, so a
	// placement the network budget was going to refuse anyway does not eat into
	// everyone else's allowance.
	if l.globalRefill > 0 {
		if l.global == nil {
			l.global = &bucket{tokens: l.globalBurst, last: now}
		} else {
			l.refill(l.global, now, l.globalRefill, l.globalBurst)
		}
		if l.global.tokens < 1 {
			return false, l.waitFor(l.global, l.globalRefill), RefusedGlobal
		}
		l.global.tokens--
	}

	net.tokens--
	l.ids[identity] = now
	return true, 0, Allowed
}

// waitFor is how long until b holds a whole token again.
func (l *Limiter) waitFor(b *bucket, refill time.Duration) time.Duration {
	wait := time.Duration((1 - b.tokens) * float64(refill))
	if wait <= 0 {
		wait = refill
	}
	return wait
}

// Remaining reports how long identity must wait before its next placement,
// ignoring the per-IP budget. The TUI calls this to draw its countdown.
func (l *Limiter) Remaining(identity string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.identityWaitLocked(identity, now)
}

func (l *Limiter) identityWaitLocked(identity string, now time.Time) time.Duration {
	last, seen := l.ids[identity]
	if !seen {
		return 0
	}
	// A clock that jumps backwards would otherwise strand the identity for the
	// length of the jump; treat "in the future" as ready.
	if now.Before(last) {
		return 0
	}
	if wait := l.cooldown - now.Sub(last); wait > 0 {
		return wait
	}
	return 0
}

func (l *Limiter) refill(b *bucket, now time.Time, per time.Duration, cap float64) {
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		// Clock went backwards; hold the bucket where it is.
		b.last = now
		return
	}
	b.tokens += float64(elapsed) / float64(per)
	if b.tokens > cap {
		b.tokens = cap
	}
	b.last = now
}

// Prune drops entries that can no longer affect a decision, and returns how
// many it removed. Without it both maps would grow with every distinct key and
// address the server ever sees.
func (l *Limiter) Prune(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	removed := 0
	for id, last := range l.ids {
		if now.Sub(last) > l.cooldown {
			delete(l.ids, id)
			removed++
		}
	}
	// A bucket that has refilled to capacity is indistinguishable from a fresh
	// one, so it is safe to forget.
	full := time.Duration(l.ipBurst * float64(l.ipRefill))
	for ip, b := range l.ips {
		if now.Sub(b.last) > full {
			delete(l.ips, ip)
			removed++
		}
	}
	return removed
}

// Len reports how many identities and IPs are currently tracked.
func (l *Limiter) Len() (identities, ips int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ids), len(l.ips)
}
