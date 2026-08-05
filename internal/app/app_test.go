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

// adminKey is the fingerprint the exemption tests grant. Real fingerprints are
// this shape, and only this shape is accepted.
const adminKey = "SHA256:aaaabbbbccccddddeeeeffff0000111122223333444"

func withAdmin(fingerprint string) func(*fixture) {
	return func(f *fixture) {
		f.app.Admins = map[string]bool{fingerprint: true}
	}
}

// withTightLimits makes every one of the three limits bite immediately, so a
// test that gets more than one placement through can only have bypassed all of
// them.
func withTightLimits(f *fixture) {
	f.app.Limiter = ratelimit.New(testCooldown, 1, time.Hour,
		ratelimit.WithGlobalRate(1, 1))
}

func TestAdminKeyHasNoCooldown(t *testing.T) {
	f := newFixture(t, withAdmin(adminKey), withTightLimits)
	admin := f.session(t, "1.1.1.1", adminKey)

	// Every placement at the same instant: no wall clock passes, so nothing can
	// have refilled. A limited identity would get exactly one.
	const want = 50
	for i := 0; i < want; i++ {
		if retry, err := f.app.Place(admin, i%40, i/40, canvas.Block(3), base); err != nil {
			t.Fatalf("Place %d = %v (retry %s), want nil", i, err, retry)
		}
	}

	for i := 0; i < want; i++ {
		if cell, ok := f.app.Canvas.At(i%40, i/40); !ok || cell.Color != 3 {
			t.Errorf("cell %d,%d = %+v, want color 3", i%40, i/40, cell)
		}
	}
	if left := f.app.CooldownLeft(admin, base); left != 0 {
		t.Errorf("CooldownLeft = %s, want 0 for an admin", left)
	}
}

func TestAdminBypassIsPerKeyNotServerWide(t *testing.T) {
	f := newFixture(t, withAdmin(adminKey), withTightLimits)
	admin := f.session(t, "1.1.1.1", adminKey)
	other := f.session(t, "2.2.2.2", "SHA256:someoneelse")

	if _, err := f.app.Place(admin, 0, 0, canvas.Block(1), base); err != nil {
		t.Fatalf("admin Place = %v, want nil", err)
	}
	// Granting an exemption to one key must not lift the limits for anyone else.
	if _, err := f.app.Place(other, 1, 0, canvas.Block(2), base); err != nil {
		t.Fatalf("first Place for a normal key = %v, want nil", err)
	}
	if _, err := f.app.Place(other, 2, 0, canvas.Block(2), base); err == nil {
		t.Fatal("second Place for a normal key succeeded, want it refused")
	}
}

func TestAdminPlacementsDoNotSpendOthersBudget(t *testing.T) {
	// The admin skips Reserve entirely rather than being refunded, so heavy
	// admin activity must not eat the global ceiling everyone shares.
	f := newFixture(t, withAdmin(adminKey),
		func(f *fixture) {
			f.app.Limiter = ratelimit.New(testCooldown, 1_000_000, time.Nanosecond,
				ratelimit.WithGlobalRate(1, 1))
		})
	admin := f.session(t, "1.1.1.1", adminKey)
	other := f.session(t, "2.2.2.2", "SHA256:someoneelse")

	for i := 0; i < 200; i++ {
		if _, err := f.app.Place(admin, i%40, i/40, canvas.Block(5), base); err != nil {
			t.Fatalf("admin Place %d = %v, want nil", i, err)
		}
	}
	// The global bucket still holds its single token.
	if _, err := f.app.Place(other, 0, 9, canvas.Block(6), base); err != nil {
		t.Errorf("Place after 200 admin writes = %v, want nil", err)
	}
}

func TestAdminRequiresARealKey(t *testing.T) {
	f := newFixture(t, withAdmin(adminKey), withTightLimits)

	// A keyless client is identified by network. If that string could ever match
	// an admin entry, everyone sharing the network would inherit the exemption.
	keyless, err := f.app.Hub.Add("1.1.1.1", adminKey, false)
	if err != nil {
		t.Fatalf("add session: %v", err)
	}
	t.Cleanup(func() { f.app.Hub.Remove(keyless) })

	if f.app.IsAdmin(keyless) {
		t.Fatal("IsAdmin = true for a keyless session, want false")
	}
	if _, err := f.app.Place(keyless, 0, 0, canvas.Block(1), base); err != nil {
		t.Fatalf("first Place = %v, want nil", err)
	}
	if _, err := f.app.Place(keyless, 1, 0, canvas.Block(1), base); err == nil {
		t.Fatal("second Place succeeded, want the keyless session rate limited")
	}
}

func TestAdminStillValidated(t *testing.T) {
	// The exemption is about pacing only. Everything that protects the canvas
	// itself still applies, or an admin session becomes a way to write control
	// characters and escape sequences into cells other people render.
	f := newFixture(t, withAdmin(adminKey), blocksOnly)
	admin := f.session(t, "1.1.1.1", adminKey)

	if _, err := f.app.Place(admin, 0, 0, canvas.Glyph('A', 1), base); !errors.Is(err, ErrCharactersDisabled) {
		t.Errorf("Place character = %v, want ErrCharactersDisabled", err)
	}
	if _, err := f.app.Place(admin, 999, 999, canvas.Block(1), base); !errors.Is(err, canvas.ErrOutOfBounds) {
		t.Errorf("Place out of bounds = %v, want ErrOutOfBounds", err)
	}
	if _, err := f.app.Place(admin, 0, 0, canvas.Glyph('\x1b', 1), base); err == nil {
		t.Error("Place escape character succeeded, want it refused")
	}
}

func TestNoAdminsConfiguredMeansNoExemptions(t *testing.T) {
	f := newFixture(t, withTightLimits)
	s := f.session(t, "1.1.1.1", adminKey)

	if f.app.IsAdmin(s) {
		t.Fatal("IsAdmin = true with no Admins configured, want false")
	}
	if _, err := f.app.Place(s, 0, 0, canvas.Block(1), base); err != nil {
		t.Fatalf("first Place = %v, want nil", err)
	}
	if _, err := f.app.Place(s, 1, 0, canvas.Block(1), base); err == nil {
		t.Fatal("second Place succeeded, want it refused")
	}
}

const botKey = "SHA256:bbbbccccddddeeeeffff00001111222233334444555"

func withBlocked(fingerprints ...string) func(*fixture) {
	return func(f *fixture) {
		f.app.Blocked = make(map[string]bool, len(fingerprints))
		for _, fp := range fingerprints {
			f.app.Blocked[fp] = true
		}
	}
}

func TestBlockedKeyCannotPlace(t *testing.T) {
	f := newFixture(t, withBlocked(botKey))
	bot := f.session(t, "1.1.1.1", botKey)

	if !f.app.IsBlocked(bot) {
		t.Fatal("IsBlocked = false, want true")
	}
	retry, err := f.app.Place(bot, 3, 3, canvas.Block(4), base)
	if !errors.Is(err, ErrKeyBlocked) {
		t.Fatalf("Place = %v, want ErrKeyBlocked", err)
	}
	// Nothing to wait for, so offering a retry time would be a lie.
	if retry != 0 {
		t.Errorf("retryAfter = %s, want 0", retry)
	}
	if cell, ok := f.app.Canvas.At(3, 3); ok && cell.Drawn() {
		t.Errorf("cell was written despite the block: %+v", cell)
	}
	// Waiting must not help. This is the difference between a block and a
	// cooldown, and a bot will absolutely test it.
	if _, err := f.app.Place(bot, 3, 3, canvas.Block(4), base.Add(time.Hour)); !errors.Is(err, ErrKeyBlocked) {
		t.Errorf("Place an hour later = %v, want ErrKeyBlocked", err)
	}
}

func TestBlockedBeatsAdmin(t *testing.T) {
	// Contradictory config should fail closed, and main warns about it at boot.
	f := newFixture(t, withAdmin(botKey), withBlocked(botKey))
	s := f.session(t, "1.1.1.1", botKey)

	if _, err := f.app.Place(s, 0, 0, canvas.Block(1), base); !errors.Is(err, ErrKeyBlocked) {
		t.Errorf("Place = %v, want ErrKeyBlocked to win over the exemption", err)
	}
}

func TestBlockingOneKeyLeavesOthersAlone(t *testing.T) {
	f := newFixture(t, withBlocked(botKey))
	bot := f.session(t, "1.1.1.1", botKey)
	human := f.session(t, "2.2.2.2", "SHA256:areallyperson")

	for i := 0; i < 5; i++ {
		if _, err := f.app.Place(bot, i, 0, canvas.Block(1), base); !errors.Is(err, ErrKeyBlocked) {
			t.Fatalf("bot Place = %v, want ErrKeyBlocked", err)
		}
	}
	// A refused block consumes no budget, so it cannot be used to starve anyone.
	if _, err := f.app.Place(human, 0, 1, canvas.Block(2), base); err != nil {
		t.Errorf("human Place = %v, want nil", err)
	}
}

func TestNoBlockListMeansNobodyBlocked(t *testing.T) {
	f := newFixture(t)
	s := f.session(t, "1.1.1.1", botKey)

	if f.app.IsBlocked(s) {
		t.Fatal("IsBlocked = true with no block list, want false")
	}
	if _, err := f.app.Place(s, 0, 0, canvas.Block(1), base); err != nil {
		t.Errorf("Place = %v, want nil", err)
	}
}

func withSlowed(factor float64, fingerprints ...string) func(*fixture) {
	return func(f *fixture) {
		f.app.SlowFactor = factor
		f.app.Slowed = make(map[string]bool, len(fingerprints))
		for _, fp := range fingerprints {
			f.app.Slowed[fp] = true
		}
	}
}

func TestSlowedKeyWaitsLongerButStillPlays(t *testing.T) {
	f := newFixture(t, withSlowed(4, botKey))
	bot := f.session(t, "1.1.1.1", botKey)

	// The point of slowing rather than blocking: the first placement still lands.
	if _, err := f.app.Place(bot, 0, 0, canvas.Block(1), base); err != nil {
		t.Fatalf("first Place = %v, want nil", err)
	}

	// At the normal cooldown it is still waiting.
	retry, err := f.app.Place(bot, 1, 0, canvas.Block(1), base.Add(testCooldown))
	if !errors.Is(err, ErrCooldown) {
		t.Fatalf("Place at the normal cooldown = %v, want ErrCooldown", err)
	}
	if want := 3 * testCooldown; retry != want {
		t.Errorf("retryAfter = %s, want %s", retry, want)
	}

	// At four times the cooldown it goes through.
	if _, err := f.app.Place(bot, 1, 0, canvas.Block(1), base.Add(4*testCooldown)); err != nil {
		t.Errorf("Place at 4x cooldown = %v, want nil", err)
	}
	if cell, ok := f.app.Canvas.At(1, 0); !ok || !cell.Drawn() {
		t.Error("slowed key could not draw at all, want it slowed not blocked")
	}
}

func TestSlowedCountdownMatchesWhatTheServerAccepts(t *testing.T) {
	// A countdown that lies is worse than a slow one: the player would sit there
	// pressing a key that keeps being refused.
	f := newFixture(t, withSlowed(4, botKey))
	bot := f.session(t, "1.1.1.1", botKey)

	if _, err := f.app.Place(bot, 0, 0, canvas.Block(1), base); err != nil {
		t.Fatal(err)
	}
	if got, want := f.app.CooldownLeft(bot, base), 4*testCooldown; got != want {
		t.Errorf("CooldownLeft = %s, want %s", got, want)
	}
	if got := f.app.CooldownLeft(bot, base.Add(4*testCooldown)); got != 0 {
		t.Errorf("CooldownLeft after the slowed window = %s, want 0", got)
	}
}

func TestSlowingOnlyTouchesListedKeys(t *testing.T) {
	f := newFixture(t, withSlowed(4, botKey))
	bot := f.session(t, "1.1.1.1", botKey)
	human := f.session(t, "2.2.2.2", "SHA256:areallyperson")

	for _, s := range []*hub.Session{bot, human} {
		if _, err := f.app.Place(s, 0, 0, canvas.Block(1), base); err != nil {
			t.Fatalf("first Place = %v, want nil", err)
		}
	}
	// One cooldown later the human is ready and the bot is not.
	at := base.Add(testCooldown)
	if _, err := f.app.Place(human, 1, 1, canvas.Block(2), at); err != nil {
		t.Errorf("human Place = %v, want nil", err)
	}
	if _, err := f.app.Place(bot, 2, 2, canvas.Block(2), at); !errors.Is(err, ErrCooldown) {
		t.Errorf("bot Place = %v, want ErrCooldown", err)
	}
}

func TestSlowFactorOneOrZeroDoesNothing(t *testing.T) {
	// An unset factor must be harmless, or forgetting it silently changes the
	// pacing for whoever is on the list.
	for _, factor := range []float64{0, 1} {
		f := newFixture(t, withSlowed(factor, botKey))
		bot := f.session(t, "1.1.1.1", botKey)
		if f.app.IsSlowed(bot) {
			t.Errorf("factor %v: IsSlowed = true, want false", factor)
		}
		if _, err := f.app.Place(bot, 0, 0, canvas.Block(1), base); err != nil {
			t.Fatalf("factor %v: Place = %v", factor, err)
		}
		if _, err := f.app.Place(bot, 1, 0, canvas.Block(1), base.Add(testCooldown)); err != nil {
			t.Errorf("factor %v: Place at normal cooldown = %v, want nil", factor, err)
		}
	}
}

func TestAdminBeatsSlowing(t *testing.T) {
	f := newFixture(t, withAdmin(adminKey), withSlowed(4, adminKey))
	s := f.session(t, "1.1.1.1", adminKey)

	for i := 0; i < 5; i++ {
		if _, err := f.app.Place(s, i, 0, canvas.Block(1), base); err != nil {
			t.Fatalf("Place %d = %v, want the exemption to win", i, err)
		}
	}
}

func TestKeylessSessionsCannotPlaceWhenAKeyIsRequired(t *testing.T) {
	f := newFixture(t, func(f *fixture) { f.app.RequireKey = true })
	keyless, err := f.app.Hub.Add("1.1.1.1", "ip:1.1.1.1", false)
	if err != nil {
		t.Fatalf("add session: %v", err)
	}
	t.Cleanup(func() { f.app.Hub.Remove(keyless) })

	retry, err := f.app.Place(keyless, 1, 1, canvas.Block(3), base)
	if !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("Place = %v, want ErrKeyRequired", err)
	}
	// Not a cooldown, so offering a wait would be misleading.
	if retry != 0 {
		t.Errorf("retryAfter = %s, want 0", retry)
	}
	if cell, ok := f.app.Canvas.At(1, 1); ok && cell.Drawn() {
		t.Error("keyless placement reached the canvas")
	}
	// It also must not burn the network budget, or a keyless client could still
	// starve the keyed sessions sharing its address.
	keyed := f.session(t, "1.1.1.1", "SHA256:realkey")
	if _, err := f.app.Place(keyed, 2, 2, canvas.Block(3), base); err != nil {
		t.Errorf("keyed Place from the same network = %v, want nil", err)
	}
}

func TestKeylessStillWorksWhenNoKeyIsRequired(t *testing.T) {
	// The default for anyone running their own copy must stay permissive unless
	// they opt in, so this is the switch being off.
	f := newFixture(t)
	keyless, err := f.app.Hub.Add("1.1.1.1", "ip:1.1.1.1", false)
	if err != nil {
		t.Fatalf("add session: %v", err)
	}
	t.Cleanup(func() { f.app.Hub.Remove(keyless) })

	if _, err := f.app.Place(keyless, 1, 1, canvas.Block(3), base); err != nil {
		t.Errorf("Place = %v, want nil", err)
	}
}

func TestPlaceRegionIsOperatorOnly(t *testing.T) {
	f := newFixture(t, withAdmin(adminKey), withEventLog(t))
	stranger := f.session(t, "2.2.2.2", "SHA256:notyou")

	if _, err := f.app.PlaceRegion(stranger, 0, 0, 9, 5, canvas.Block(4), base); !errors.Is(err, ErrNotOperator) {
		t.Fatalf("PlaceRegion = %v, want ErrNotOperator", err)
	}
	if f.app.Canvas.NonEmpty() != 0 {
		t.Error("a refused region reached the canvas")
	}
}

func TestPlaceRegionWritesLogsAndBroadcasts(t *testing.T) {
	f := newFixture(t, withAdmin(adminKey), withEventLog(t))
	admin := f.session(t, "1.1.1.1", adminKey)
	watcher := f.session(t, "2.2.2.2", "SHA256:watcher")

	n, err := f.app.PlaceRegion(admin, 2, 1, 5, 3, canvas.Block(6), base)
	if err != nil {
		t.Fatalf("PlaceRegion = %v", err)
	}
	if want := 4 * 3; n != want {
		t.Fatalf("wrote %d, want %d", n, want)
	}
	if got := f.app.Canvas.NonEmpty(); got != n {
		t.Errorf("canvas has %d drawn, want %d", got, n)
	}

	// Every cell must reach the event log, or the timelapse replay stops matching
	// the canvas from this point on.
	if err := f.app.Log.Flush(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, f.events)
	if len(events) != n {
		t.Errorf("logged %d events, want %d", len(events), n)
	}
	for _, e := range events {
		if e.Identity != adminKey {
			t.Errorf("event identity = %q, want the operator's fingerprint", e.Identity)
		}
	}

	// The watcher sees it, though the hub is free to drop and flag a resync.
	got := len(drain(watcher))
	if got == 0 && !watcher.TakeDirty() {
		t.Error("watcher got neither updates nor a dirty flag")
	}
}

func TestPlaceRegionClearsWithoutLeavingAFill(t *testing.T) {
	f := newFixture(t, withAdmin(adminKey), withEventLog(t))
	admin := f.session(t, "1.1.1.1", adminKey)

	if _, err := f.app.PlaceRegion(admin, 0, 0, 39, 9, canvas.Block(9), base); err != nil {
		t.Fatal(err)
	}
	// Now wipe a rectangle out of the middle of it.
	if _, err := f.app.PlaceRegion(admin, 10, 3, 19, 6, canvas.Glyph(canvas.Empty, 0), base); err != nil {
		t.Fatalf("clearing region = %v", err)
	}
	for y := 3; y <= 6; y++ {
		for x := 10; x <= 19; x++ {
			cell, _ := f.app.Canvas.At(x, y)
			if cell.Drawn() || cell.IsBlock() {
				t.Fatalf("cell %d,%d = %+v, want truly blank", x, y, cell)
			}
		}
	}
	// And the surroundings are untouched.
	if cell, _ := f.app.Canvas.At(9, 3); cell.Color != 9 {
		t.Errorf("cell just outside the wipe = %+v, want colour 9", cell)
	}
}

func TestPlaceRegionStillRefusesCharactersInBlocksOnly(t *testing.T) {
	f := newFixture(t, withAdmin(adminKey), withEventLog(t), blocksOnly)
	admin := f.session(t, "1.1.1.1", adminKey)

	if _, err := f.app.PlaceRegion(admin, 0, 0, 5, 5, canvas.Glyph('A', 1), base); !errors.Is(err, ErrCharactersDisabled) {
		t.Errorf("PlaceRegion with a character = %v, want ErrCharactersDisabled", err)
	}
	// An erase carries no character, so it stays legal.
	if _, err := f.app.PlaceRegion(admin, 0, 0, 5, 5, canvas.Glyph(canvas.Empty, 0), base); err != nil {
		t.Errorf("clearing region in blocks-only = %v, want nil", err)
	}
}

func TestPlaceRegionIsNotRateLimited(t *testing.T) {
	// The point of the exemption: an operator cleaning up cannot be doing it one
	// cell per cooldown.
	f := newFixture(t, withAdmin(adminKey), withEventLog(t), withTightLimits)
	admin := f.session(t, "1.1.1.1", adminKey)

	for i := 0; i < 4; i++ {
		if _, err := f.app.PlaceRegion(admin, 0, i, 39, i, canvas.Block(uint8(i+1)), base); err != nil {
			t.Fatalf("region %d = %v, want nil", i, err)
		}
	}
}

func TestPlaceRegionRespectsTheBlockList(t *testing.T) {
	// The bulk path must not be a way around a block. Precedence is the same as
	// Place: a fingerprint in both lists is blocked, not exempt.
	f := newFixture(t, withAdmin(adminKey), withBlocked(adminKey), withEventLog(t))
	s := f.session(t, "1.1.1.1", adminKey)

	if _, err := f.app.PlaceRegion(s, 0, 0, 9, 5, canvas.Block(4), base); !errors.Is(err, ErrKeyBlocked) {
		t.Fatalf("PlaceRegion = %v, want ErrKeyBlocked", err)
	}
	if f.app.Canvas.NonEmpty() != 0 {
		t.Error("a blocked operator still wrote a region")
	}
}

func TestPlaceRegionLogsExactlyWhatItWrote(t *testing.T) {
	// A selection dragged off the edge is clipped, and the log has to describe the
	// clipped rectangle. If it claimed the requested one, replaying the log would
	// stop reproducing the canvas.
	f := newFixture(t, withAdmin(adminKey), withEventLog(t))
	admin := f.session(t, "1.1.1.1", adminKey)

	n, err := f.app.PlaceRegion(admin, -10, -10, 3, 2, canvas.Block(7), base)
	if err != nil {
		t.Fatalf("PlaceRegion = %v", err)
	}
	if want := 4 * 3; n != want {
		t.Fatalf("wrote %d, want %d", n, want)
	}
	if err := f.app.Log.Flush(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, f.events)
	if len(events) != n {
		t.Fatalf("logged %d events, want %d", len(events), n)
	}
	for _, e := range events {
		if e.X < 0 || e.Y < 0 || e.X > 3 || e.Y > 2 {
			t.Errorf("logged a cell outside the clipped rectangle: %d,%d", e.X, e.Y)
		}
	}
}

func TestPlaceRegionMarksSessionsForResync(t *testing.T) {
	// One flag beats n updates for a bulk change: nothing to drop, and every
	// session converges on the canvas as it now is.
	f := newFixture(t, withAdmin(adminKey), withEventLog(t))
	admin := f.session(t, "1.1.1.1", adminKey)
	watcher := f.session(t, "2.2.2.2", "SHA256:watcher")

	if _, err := f.app.PlaceRegion(admin, 0, 0, 39, 9, canvas.Block(2), base); err != nil {
		t.Fatal(err)
	}
	if !watcher.TakeDirty() {
		t.Error("watcher was not flagged to resync")
	}
	// It also gets a nudge, so its listener wakes up rather than waiting for the
	// next placement or tick.
	updates := drain(watcher)
	if len(updates) == 0 {
		t.Fatal("watcher got no wake-up at all")
	}
	if !updates[0].Resync {
		t.Errorf("first update = %+v, want a resync marker", updates[0])
	}
	// A full-canvas wipe must not have queued a message per cell.
	if len(updates) > 4 {
		t.Errorf("got %d updates for one region, want a handful", len(updates))
	}
}
