package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jeninh/ssh.place/internal/canvas"
	"github.com/jeninh/ssh.place/internal/eventlog"
	"github.com/jeninh/ssh.place/internal/hub"
	"github.com/jeninh/ssh.place/internal/ratelimit"
)

var base = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

const testCooldown = 15 * time.Second

type fixture struct {
	app    *App
	events string // path to the event log, "" when disabled
}

func newFixture(t *testing.T, opts ...func(*fixture)) *fixture {
	t.Helper()
	f := &fixture{
		app: &App{
			Canvas: canvas.New(40, 10),
			Hub:    hub.New(100, 10),
			// A large IP budget by default so tests exercise the cooldown
			// unless they opt into IP limiting.
			Limiter: ratelimit.New(testCooldown, 1_000_000, time.Nanosecond),
		},
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

func withEventLog(t *testing.T) func(*fixture) {
	t.Helper()
	return func(f *fixture) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		log, err := eventlog.Open(path)
		if err != nil {
			t.Fatalf("open event log: %v", err)
		}
		t.Cleanup(func() { _ = log.Close() })
		f.app.Log = log
		f.events = path
	}
}

func withIPBudget(burst int, refill time.Duration) func(*fixture) {
	return func(f *fixture) {
		f.app.Limiter = ratelimit.New(testCooldown, burst, refill)
	}
}

func (f *fixture) session(t *testing.T, ip, identity string) *hub.Session {
	t.Helper()
	s, err := f.app.Hub.Add(ip, identity, true)
	if err != nil {
		t.Fatalf("add session: %v", err)
	}
	t.Cleanup(func() { f.app.Hub.Remove(s) })
	return s
}

// drain reads every buffered update without blocking.
func drain(s *hub.Session) []hub.Update {
	var out []hub.Update
	for {
		select {
		case u, ok := <-s.Updates():
			if !ok {
				return out
			}
			out = append(out, u)
		default:
			return out
		}
	}
}

func TestPlaceWritesAndBroadcasts(t *testing.T) {
	f := newFixture(t)
	placer := f.session(t, "1.1.1.1", "key-a")
	watcher := f.session(t, "2.2.2.2", "key-b")

	if retry, err := f.app.Place(placer, 7, 3, canvas.Glyph('@', 9), base); err != nil {
		t.Fatalf("Place = %v (retry %s), want nil", err, retry)
	}

	got, ok := f.app.Canvas.At(7, 3)
	if !ok || got.Rune != '@' || got.Color != 9 {
		t.Errorf("canvas cell = %q/%d, want '@'/9", got.Rune, got.Color)
	}

	want := hub.Update{X: 7, Y: 3, Cell: canvas.Glyph('@', 9)}
	// The placer sees its own placement, which is what drives its fade marker.
	if updates := drain(placer); len(updates) != 1 || updates[0] != want {
		t.Errorf("placer updates = %+v, want exactly [%+v]", updates, want)
	}
	if updates := drain(watcher); len(updates) != 1 || updates[0] != want {
		t.Errorf("watcher updates = %+v, want exactly [%+v]", updates, want)
	}
}

func TestPlaceEnforcesCooldown(t *testing.T) {
	f := newFixture(t)
	s := f.session(t, "1.1.1.1", "key-a")

	if _, err := f.app.Place(s, 1, 1, canvas.Glyph('a', 1), base); err != nil {
		t.Fatalf("first Place: %v", err)
	}
	drain(s)

	retry, err := f.app.Place(s, 2, 2, canvas.Glyph('b', 1), base.Add(time.Second))
	if !errors.Is(err, ErrCooldown) {
		t.Fatalf("second Place = %v, want ErrCooldown", err)
	}
	if want := testCooldown - time.Second; retry != want {
		t.Errorf("retryAfter = %s, want %s", retry, want)
	}

	// The refused placement must leave no trace anywhere.
	if got, _ := f.app.Canvas.At(2, 2); got.Rune != canvas.Empty {
		t.Errorf("refused placement wrote %q to the canvas", got.Rune)
	}
	if updates := drain(s); len(updates) != 0 {
		t.Errorf("refused placement broadcast %+v, want nothing", updates)
	}

	// Once the cooldown has elapsed the placement lands.
	if _, err := f.app.Place(s, 2, 2, canvas.Glyph('b', 1), base.Add(testCooldown)); err != nil {
		t.Errorf("Place after cooldown = %v, want nil", err)
	}
	if got, _ := f.app.Canvas.At(2, 2); got.Rune != 'b' {
		t.Errorf("cell = %q, want 'b'", got.Rune)
	}
}

func TestCooldownLeftTracksLimiter(t *testing.T) {
	f := newFixture(t)
	s := f.session(t, "1.1.1.1", "key-a")

	if d := f.app.CooldownLeft(s, base); d != 0 {
		t.Errorf("CooldownLeft before placing = %s, want 0", d)
	}
	if _, err := f.app.Place(s, 1, 1, canvas.Glyph('a', 1), base); err != nil {
		t.Fatal(err)
	}
	if d := f.app.CooldownLeft(s, base.Add(5*time.Second)); d != 10*time.Second {
		t.Errorf("CooldownLeft = %s, want 10s", d)
	}
	if d := f.app.CooldownLeft(s, base.Add(testCooldown)); d != 0 {
		t.Errorf("CooldownLeft after the window = %s, want 0", d)
	}
}

// The cooldown follows the key, so reconnecting does not reset it. That is the
// property that makes the whole rate limit worth anything.
func TestCooldownSurvivesReconnect(t *testing.T) {
	f := newFixture(t)

	first := f.session(t, "1.1.1.1", "key-a")
	if _, err := f.app.Place(first, 1, 1, canvas.Glyph('a', 1), base); err != nil {
		t.Fatal(err)
	}
	f.app.Hub.Remove(first)

	// Same key, new connection, even from a different address.
	second := f.session(t, "9.9.9.9", "key-a")
	if _, err := f.app.Place(second, 2, 2, canvas.Glyph('b', 1), base.Add(time.Second)); !errors.Is(err, ErrCooldown) {
		t.Errorf("Place after reconnect = %v, want ErrCooldown", err)
	}
}

func TestPlaceRejectsInvalidInputWithoutSpendingCooldown(t *testing.T) {
	f := newFixture(t)
	s := f.session(t, "1.1.1.1", "key-a")

	cases := []struct {
		name    string
		x, y    int
		r       rune
		color   uint8
		wantErr error
	}{
		{"negative x", -1, 0, 'a', 1, canvas.ErrOutOfBounds},
		{"negative y", 0, -1, 'a', 1, canvas.ErrOutOfBounds},
		{"x past edge", 40, 0, 'a', 1, canvas.ErrOutOfBounds},
		{"y past edge", 0, 10, 'a', 1, canvas.ErrOutOfBounds},
		{"newline", 1, 1, '\n', 1, canvas.ErrBadRune},
		{"escape", 1, 1, 0x1b, 1, canvas.ErrBadRune},
		{"nul", 1, 1, 0, 1, canvas.ErrBadRune},
		{"delete", 1, 1, 0x7f, 1, canvas.ErrBadRune},
		{"non-ascii", 1, 1, '→', 1, canvas.ErrBadRune},
		{"bad color", 1, 1, 'a', canvas.PaletteSize, canvas.ErrBadColor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.app.Place(s, tc.x, tc.y, canvas.Glyph(tc.r, tc.color), base)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Place = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// None of those should have counted as a turn: a client aiming badly is not
	// spending its budget, so a legitimate placement still works.
	if _, err := f.app.Place(s, 1, 1, canvas.Glyph('a', 1), base); err != nil {
		t.Errorf("Place after rejected attempts = %v, want nil", err)
	}
	if got := f.app.Canvas.NonEmpty(); got != 1 {
		t.Errorf("canvas holds %d drawn cells, want 1", got)
	}
}

// Space is a legal stamp: it is how you erase.
func TestPlaceSpaceErases(t *testing.T) {
	f := newFixture(t)
	s := f.session(t, "1.1.1.1", "key-a")

	if _, err := f.app.Place(s, 3, 3, canvas.Glyph('X', 1), base); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Place(s, 3, 3, canvas.Glyph(' ', 1), base.Add(testCooldown)); err != nil {
		t.Fatalf("Place space = %v, want nil", err)
	}
	if got, _ := f.app.Canvas.At(3, 3); got.Rune != canvas.Empty {
		t.Errorf("cell = %q, want blank", got.Rune)
	}
}

// The per-IP bucket has to hold even when every placement uses a brand new key,
// because generating keys is free.
func TestPlaceLimitsKeyRotationFromOneIP(t *testing.T) {
	const burst = 3
	f := newFixture(t, withIPBudget(burst, time.Hour))

	allowed := 0
	for i := 0; i < 10; i++ {
		s := f.session(t, "6.6.6.6", fmt.Sprintf("rotating-key-%d", i))
		if _, err := f.app.Place(s, i, 0, canvas.Glyph('x', 1), base); err == nil {
			allowed++
		}
	}
	if allowed != burst {
		t.Errorf("%d placements landed from one IP with rotating keys, want %d", allowed, burst)
	}
	if got := f.app.Canvas.NonEmpty(); got != burst {
		t.Errorf("canvas holds %d cells, want %d", got, burst)
	}
}

func TestPlaceAppendsToEventLog(t *testing.T) {
	f := newFixture(t, withEventLog(t))
	s := f.session(t, "1.1.1.1", "SHA256:abc")

	if _, err := f.app.Place(s, 5, 6, canvas.Glyph('Q', 12), base); err != nil {
		t.Fatal(err)
	}
	// A refused placement must not be logged.
	if _, err := f.app.Place(s, 5, 7, canvas.Glyph('R', 12), base); !errors.Is(err, ErrCooldown) {
		t.Fatalf("second Place = %v, want ErrCooldown", err)
	}
	if err := f.app.Log.Flush(); err != nil {
		t.Fatal(err)
	}

	events := readEvents(t, f.events)
	if len(events) != 1 {
		t.Fatalf("logged %d events, want 1: %+v", len(events), events)
	}
	got := events[0]
	if got.X != 5 || got.Y != 6 || got.Rune != "Q" || got.Color != 12 {
		t.Errorf("event = %+v, want x=5 y=6 r=Q c=12", got)
	}
	if got.Identity != "SHA256:abc" {
		t.Errorf("event identity = %q, want the key fingerprint", got.Identity)
	}
	if !got.At.Equal(base) {
		t.Errorf("event time = %s, want %s", got.At, base)
	}
}

// A nil log is the "logging disabled" configuration and must be a no-op rather
// than a panic.
func TestPlaceWithoutEventLog(t *testing.T) {
	f := newFixture(t)
	if f.app.Log != nil {
		t.Fatal("fixture unexpectedly configured a log")
	}
	s := f.session(t, "1.1.1.1", "key-a")
	if _, err := f.app.Place(s, 0, 0, canvas.Glyph('a', 1), base); err != nil {
		t.Errorf("Place with no event log = %v, want nil", err)
	}
}

func TestCooldownAccessor(t *testing.T) {
	f := newFixture(t)
	if got := f.app.Cooldown(); got != testCooldown {
		t.Errorf("Cooldown = %s, want %s", got, testCooldown)
	}
}

// Many sessions placing at once must produce exactly one canvas write and one
// broadcast per accepted placement, with no lost or duplicated updates.
func TestConcurrentPlace(t *testing.T) {
	const (
		sessions = 12
		rounds   = 25
	)
	// No cooldown and a huge IP budget: every attempt should be accepted, so
	// the counts are exact and any mismatch is a real bug.
	f := newFixture(t)
	f.app.Limiter = ratelimit.New(0, 1_000_000, time.Nanosecond)

	watcher := f.session(t, "0.0.0.0", "watcher")

	// Drain the watcher in the background. It may still fall behind: the hub
	// deliberately drops updates rather than block a broadcaster, so the count
	// below is a ceiling, not an equality.
	var (
		received int
		recvMu   sync.Mutex
		done     = make(chan struct{})
	)
	go func() {
		for range watcher.Updates() {
			recvMu.Lock()
			received++
			recvMu.Unlock()
		}
		close(done)
	}()

	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		s := f.session(t, fmt.Sprintf("10.0.0.%d", i), fmt.Sprintf("key-%d", i))
		wg.Add(1)
		go func(s *hub.Session, id int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				// Every session writes its own column, so the final canvas is
				// fully determined.
				if _, err := f.app.Place(s, id, r%10, canvas.Glyph('a', uint8(id%canvas.PaletteSize)), base); err != nil {
					t.Errorf("session %d round %d: %v", id, r, err)
					return
				}
				f.app.CooldownLeft(s, base)
			}
		}(s, i)
	}
	wg.Wait()

	if got := f.app.Canvas.Version(); got != uint64(sessions*rounds) {
		t.Errorf("canvas version = %d, want %d", got, sessions*rounds)
	}

	// Closing the watcher ends the draining goroutine.
	f.app.Hub.Remove(watcher)
	<-done
	recvMu.Lock()
	defer recvMu.Unlock()

	total := sessions * rounds

	// The real contract is that nothing is lost silently. A session either gets
	// every update, or it gets told to resync. Asserting exact delivery instead
	// makes this test fail on a loaded machine, because falling behind is the
	// designed behaviour rather than a bug.
	if received > total {
		t.Errorf("watcher received %d updates, more than the %d placed", received, total)
	}
	if received < total && !watcher.TakeDirty() {
		t.Errorf("watcher received only %d of %d updates and was not flagged dirty, so the missing ones were lost silently",
			received, total)
	}
	// The undrained placer sessions guarantee the coalescing path ran at all.
	if f.app.Hub.Dropped() == 0 {
		t.Error("no updates were coalesced, so this test never exercised that path")
	}
}

func readEvents(t *testing.T, path string) []eventlog.Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer f.Close()

	var out []eventlog.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e eventlog.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("decode event %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan event log: %v", err)
	}
	return out
}

// --- blocks-only: the policy is enforced here, not in the UI ---

func blocksOnly(f *fixture) { f.app.BlocksOnly = true }

func TestBlocksOnlyRefusesCharacters(t *testing.T) {
	f := newFixture(t, blocksOnly)
	s := f.session(t, "1.1.1.1", "key-a")

	for _, r := range []rune{'a', 'Z', '#', '@', '1'} {
		_, err := f.app.Place(s, 1, 1, canvas.Glyph(r, 1), base)
		if !errors.Is(err, ErrCharactersDisabled) {
			t.Errorf("Place(%q) = %v, want ErrCharactersDisabled", r, err)
		}
	}
	if got := f.app.Canvas.NonEmpty(); got != 0 {
		t.Errorf("canvas holds %d cells, want 0", got)
	}
	if got := f.app.Placements(); got != 0 {
		t.Errorf("Placements = %d, want 0", got)
	}
}

func TestBlocksOnlyAllowsBlocks(t *testing.T) {
	f := newFixture(t, blocksOnly)
	s := f.session(t, "1.1.1.1", "key-a")

	if _, err := f.app.Place(s, 2, 2, canvas.Block(9), base); err != nil {
		t.Fatalf("Place block = %v, want nil", err)
	}
	got, _ := f.app.Canvas.At(2, 2)
	if !got.IsBlock() || got.Fill != 9 {
		t.Errorf("cell = %+v, want a block filled with 9", got)
	}
}

// A space carries no character, so erasing stays possible.
func TestBlocksOnlyAllowsErasing(t *testing.T) {
	f := newFixture(t, blocksOnly)
	s := f.session(t, "1.1.1.1", "key-a")

	if _, err := f.app.Place(s, 3, 3, canvas.Block(4), base); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Place(s, 3, 3, canvas.Glyph(' ', 4), base.Add(testCooldown)); err != nil {
		t.Fatalf("erase = %v, want nil", err)
	}
	if got, _ := f.app.Canvas.At(3, 3); got.Drawn() {
		t.Errorf("cell = %+v, want blank", got)
	}
}

// Refusing a character must not consume the player's turn.
func TestBlocksOnlyRefusalDoesNotSpendCooldown(t *testing.T) {
	f := newFixture(t, blocksOnly)
	s := f.session(t, "1.1.1.1", "key-a")

	for i := 0; i < 5; i++ {
		if _, err := f.app.Place(s, 1, 1, canvas.Glyph('x', 1), base); err == nil {
			t.Fatal("a character was accepted")
		}
	}
	if _, err := f.app.Place(s, 1, 1, canvas.Block(1), base); err != nil {
		t.Errorf("Place block after refusals = %v, want nil", err)
	}
}

// Characters are the default for a plain App; only the flag turns them off.
func TestCharactersAllowedByDefault(t *testing.T) {
	f := newFixture(t)
	s := f.session(t, "1.1.1.1", "key-a")
	if _, err := f.app.Place(s, 1, 1, canvas.Glyph('a', 1), base); err != nil {
		t.Errorf("Place = %v, want nil on a mixed canvas", err)
	}
}

// Rotating source addresses inside one IPv6 /64 must not buy extra placements.
// Without grouping by network this is a complete bypass of the per-address
// budget, since one customer is routinely handed 2^64 addresses.
func TestKeyAndAddressRotationFromOneIPv6BlockIsContained(t *testing.T) {
	const burst = 5
	f := newFixture(t, withIPBudget(burst, time.Hour))

	landed := 0
	for i := 0; i < 300; i++ {
		// A fresh key and a fresh address every time, all inside one /64.
		network := ratelimit.NetKey(fmt.Sprintf("2001:db8:dead:beef::%x", i))
		s, err := f.app.Hub.Add(network, fmt.Sprintf("throwaway-key-%d", i), true)
		if err != nil {
			t.Fatalf("add session: %v", err)
		}
		if _, err := f.app.Place(s, i%40, i/40%10, canvas.Block(1), base); err == nil {
			landed++
		}
		f.app.Hub.Remove(s)
	}
	if landed != burst {
		t.Errorf("%d placements landed from one /64 using 300 keys, want %d", landed, burst)
	}
}

// The same rotation from separate /64s is a different set of clients and should
// each get their own budget, so the grouping must not be broader than a /64.
func TestSeparateIPv6BlocksKeepSeparateBudgets(t *testing.T) {
	f := newFixture(t, withIPBudget(1, time.Hour))

	landed := 0
	for i := 0; i < 10; i++ {
		network := ratelimit.NetKey(fmt.Sprintf("2001:db8:dead:%x::1", i))
		s, err := f.app.Hub.Add(network, fmt.Sprintf("key-%d", i), true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.Place(s, i, 0, canvas.Block(1), base); err == nil {
			landed++
		}
		f.app.Hub.Remove(s)
	}
	if landed != 10 {
		t.Errorf("%d of 10 distinct /64s could place, want all of them", landed)
	}
}
