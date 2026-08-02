package hub

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jeninh/ssh.place/internal/canvas"
)

func TestAddAndRemoveTracksOnline(t *testing.T) {
	h := New(10, 10)
	if got := h.Online(); got != 0 {
		t.Fatalf("Online = %d, want 0", got)
	}

	a, err := h.Add("1.1.1.1", "key-a", true)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := h.Add("2.2.2.2", "key-b", true)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := h.Online(); got != 2 {
		t.Errorf("Online = %d, want 2", got)
	}
	if a.ID == b.ID {
		t.Error("sessions share an ID")
	}
	if a.Identity != "key-a" || !a.Keyed || a.IP != "1.1.1.1" {
		t.Errorf("session fields not carried through: %+v", a)
	}

	h.Remove(a)
	if got := h.Online(); got != 1 {
		t.Errorf("Online after Remove = %d, want 1", got)
	}
	h.Remove(b)
	if got := h.Online(); got != 0 {
		t.Errorf("Online after both removed = %d, want 0", got)
	}
}

func TestMaxSessions(t *testing.T) {
	h := New(2, 10)

	s1, err := h.Add("1.1.1.1", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Add("2.2.2.2", "b", true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Add("3.3.3.3", "c", true); !errors.Is(err, ErrServerFull) {
		t.Errorf("third Add = %v, want ErrServerFull", err)
	}

	// Freeing a slot lets the next client in.
	h.Remove(s1)
	if _, err := h.Add("3.3.3.3", "c", true); err != nil {
		t.Errorf("Add after a slot freed = %v, want nil", err)
	}
}

func TestMaxPerIP(t *testing.T) {
	h := New(100, 2)

	s1, err := h.Add("1.1.1.1", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Add("1.1.1.1", "b", true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Add("1.1.1.1", "c", true); !errors.Is(err, ErrTooManyFromIP) {
		t.Errorf("third Add from one IP = %v, want ErrTooManyFromIP", err)
	}
	// A different address is unaffected.
	if _, err := h.Add("9.9.9.9", "d", true); err != nil {
		t.Errorf("Add from another IP = %v, want nil", err)
	}
	if got := h.ConnectionsFrom("1.1.1.1"); got != 2 {
		t.Errorf("ConnectionsFrom = %d, want 2", got)
	}

	h.Remove(s1)
	if got := h.ConnectionsFrom("1.1.1.1"); got != 1 {
		t.Errorf("ConnectionsFrom after Remove = %d, want 1", got)
	}
	if _, err := h.Add("1.1.1.1", "e", true); err != nil {
		t.Errorf("Add after a slot freed = %v, want nil", err)
	}
}

// Per-IP counts must not leak: a full connect/disconnect cycle has to leave the
// map empty, or the IP would eventually be locked out permanently.
func TestPerIPCountDoesNotLeak(t *testing.T) {
	h := New(100, 3)
	for i := 0; i < 50; i++ {
		s, err := h.Add("7.7.7.7", fmt.Sprintf("key-%d", i), true)
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		h.Remove(s)
	}
	if got := h.ConnectionsFrom("7.7.7.7"); got != 0 {
		t.Errorf("ConnectionsFrom after 50 cycles = %d, want 0", got)
	}
	h.mu.RLock()
	n := len(h.perIP)
	h.mu.RUnlock()
	if n != 0 {
		t.Errorf("perIP map holds %d entries, want 0", n)
	}
}

func TestZeroLimitsMeanUnlimited(t *testing.T) {
	h := New(0, 0)
	for i := 0; i < 20; i++ {
		if _, err := h.Add("1.1.1.1", fmt.Sprintf("k%d", i), true); err != nil {
			t.Fatalf("Add %d with limits disabled = %v", i, err)
		}
	}
}

func TestBroadcastReachesEverySession(t *testing.T) {
	h := New(10, 10)
	var sessions []*Session
	for i := 0; i < 3; i++ {
		s, err := h.Add(fmt.Sprintf("10.0.0.%d", i), fmt.Sprintf("k%d", i), true)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}

	want := Update{X: 4, Y: 5, Cell: canvas.Cell{Rune: '@', Color: 3}}
	h.Broadcast(want)

	for i, s := range sessions {
		select {
		case got := <-s.Updates():
			if got != want {
				t.Errorf("session %d got %+v, want %+v", i, got, want)
			}
		default:
			t.Errorf("session %d received nothing", i)
		}
	}
}

func TestRemovedSessionStopsReceiving(t *testing.T) {
	h := New(10, 10)
	s, err := h.Add("1.1.1.1", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	h.Remove(s)

	// The channel is closed, which is what unblocks the TUI's reader.
	if _, open := <-s.Updates(); open {
		t.Error("update channel still open after Remove")
	}
	// Broadcasting to an empty hub must not panic on the closed channel.
	h.Broadcast(Update{X: 1, Y: 1})
}

func TestRemoveIsIdempotent(t *testing.T) {
	h := New(10, 10)
	s, err := h.Add("1.1.1.1", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	h.Remove(s)
	// A second Remove must not double-close the channel or corrupt the counts.
	h.Remove(s)
	h.Remove(nil)
	if got := h.Online(); got != 0 {
		t.Errorf("Online = %d, want 0", got)
	}
	if got := h.ConnectionsFrom("1.1.1.1"); got != 0 {
		t.Errorf("ConnectionsFrom = %d, want 0", got)
	}
}

// A session that never drains its channel must not block the broadcaster; it
// gets flagged dirty instead so it can resynchronise from the canvas.
func TestBroadcastCoalescesForSlowSession(t *testing.T) {
	h := New(10, 10)
	slow, err := h.Add("1.1.1.1", "slow", true)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := h.Add("2.2.2.2", "fast", true)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < updateBuffer*3; i++ {
		// Drain the fast session so only the slow one falls behind.
		select {
		case <-fast.Updates():
		default:
		}
		h.Broadcast(Update{X: i, Y: 0})
	}

	if !slow.TakeDirty() {
		t.Error("slow session was not flagged dirty")
	}
	// TakeDirty clears the flag so the session redraws once, not forever.
	if slow.TakeDirty() {
		t.Error("dirty flag was not cleared by TakeDirty")
	}
	if h.Dropped() == 0 {
		t.Error("Dropped = 0, want a non-zero count of coalesced updates")
	}
	if got := len(slow.Updates()); got != updateBuffer {
		t.Errorf("slow session buffered %d updates, want the cap of %d", got, updateBuffer)
	}
}

func TestFreshSessionIsNotDirty(t *testing.T) {
	h := New(10, 10)
	s, err := h.Add("1.1.1.1", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	if s.TakeDirty() {
		t.Error("a new session reported dirty")
	}
}

func TestUnkeyedSession(t *testing.T) {
	h := New(10, 10)
	s, err := h.Add("1.1.1.1", "ip:1.1.1.1", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.Keyed {
		t.Error("Keyed = true for a session added without a key")
	}
}

// The whole point of the non-blocking fan-out is that Broadcast keeps working
// while sessions come and go, so hammer all three paths at once under -race.
func TestConcurrentAddBroadcastRemove(t *testing.T) {
	const (
		workers = 12
		rounds  = 300
	)
	h := New(0, 0)

	var wg sync.WaitGroup

	// Churn: sessions connecting, reading a little, and disconnecting.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for r := 0; r < rounds/10; r++ {
				s, err := h.Add(fmt.Sprintf("10.0.%d.%d", id, r), fmt.Sprintf("k-%d-%d", id, r), true)
				if err != nil {
					t.Errorf("Add: %v", err)
					return
				}
				for i := 0; i < 3; i++ {
					select {
					case <-s.Updates():
					default:
					}
				}
				s.TakeDirty()
				h.Remove(s)
			}
		}(w)
	}

	// Broadcasters running against that churn.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				h.Broadcast(Update{X: r % 20, Y: r % 10, Cell: canvas.Cell{Rune: 'x', Color: 1}})
				_ = h.Online()
				_ = h.Dropped()
			}
		}()
	}

	wg.Wait()

	if got := h.Online(); got != 0 {
		t.Errorf("Online after all sessions removed = %d, want 0", got)
	}
}
