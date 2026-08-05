// Package hub tracks connected sessions and fans placement updates out to
// them.
package hub

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/jeninh/ssh.place/internal/canvas"
)

// updateBuffer is how many pending updates a session may queue before the hub
// starts coalescing. A session only needs the buffer to survive a burst; it
// re-reads the canvas when it redraws, so dropping updates costs nothing but a
// slightly later repaint.
const updateBuffer = 64

// Errors returned by Add. Their text is shown to the rejected client.
var (
	ErrServerFull    = errors.New("ssh.place is at capacity right now. Try again in a minute.")
	ErrTooManyFromIP = errors.New("you already have too many connections open from here")
)

// Update announces that one cell changed.
type Update struct {
	X, Y int
	Cell canvas.Cell
	// Resync means "many cells changed, redraw from the canvas". X, Y and Cell
	// carry nothing in that case, so a receiver must check this first.
	Resync bool
}

// Session is one connected client's handle on the hub.
type Session struct {
	// ID is unique for the lifetime of the process.
	ID int64
	// IP is the client's remote address, without a port.
	IP string
	// Identity is the SSH public key fingerprint, or an IP-derived stand-in
	// for clients that connected without a key.
	Identity string
	// Keyed reports whether Identity came from a real public key.
	Keyed bool

	ch    chan Update
	dirty atomic.Bool
}

// Updates returns the session's update stream. It is closed when the session
// is removed from the hub, which is what unblocks any reader.
func (s *Session) Updates() <-chan Update { return s.ch }

// TakeDirty reports whether updates were dropped since the last call, and
// clears the flag. A session that sees true should redraw from the canvas
// instead of trusting the updates it received.
func (s *Session) TakeDirty() bool { return s.dirty.Swap(false) }

// Hub is the set of live sessions.
type Hub struct {
	maxSessions int
	maxPerIP    int

	mu       sync.RWMutex
	sessions map[int64]*Session
	perIP    map[string]int
	nextID   int64

	dropped atomic.Uint64
}

// New returns a Hub. Non-positive limits are treated as unlimited.
func New(maxSessions, maxPerIP int) *Hub {
	return &Hub{
		maxSessions: maxSessions,
		maxPerIP:    maxPerIP,
		sessions:    make(map[int64]*Session),
		perIP:       make(map[string]int),
	}
}

// Add registers a session, or returns ErrServerFull / ErrTooManyFromIP.
func (h *Hub) Add(ip, identity string, keyed bool) (*Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.maxSessions > 0 && len(h.sessions) >= h.maxSessions {
		return nil, ErrServerFull
	}
	if h.maxPerIP > 0 && h.perIP[ip] >= h.maxPerIP {
		return nil, ErrTooManyFromIP
	}

	h.nextID++
	s := &Session{
		ID:       h.nextID,
		IP:       ip,
		Identity: identity,
		Keyed:    keyed,
		ch:       make(chan Update, updateBuffer),
	}
	h.sessions[s.ID] = s
	h.perIP[ip]++
	return s, nil
}

// Remove deregisters a session and closes its update channel. It is safe to
// call more than once, which matters because it runs from a defer on a path
// that may also remove explicitly.
//
// Closing under the write lock is what makes the non-blocking sends in
// Broadcast safe: a broadcaster holds the read lock, so no send can be in
// flight while the channel is being closed.
func (h *Hub) Remove(s *Session) {
	if s == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.sessions[s.ID]; !ok {
		return
	}
	delete(h.sessions, s.ID)
	if n := h.perIP[s.IP]; n <= 1 {
		delete(h.perIP, s.IP)
	} else {
		h.perIP[s.IP] = n - 1
	}
	close(s.ch)
}

// Broadcast delivers u to every session. Delivery never blocks: a session
// whose buffer is full is flagged dirty and skipped, so one wedged client
// cannot stall the placement path for everyone else.
func (h *Hub) Broadcast(u Update) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.sessions {
		select {
		case s.ch <- u:
		default:
			s.dirty.Store(true)
			h.dropped.Add(1)
		}
	}
}

// MarkAllDirty flags every session to redraw from the canvas instead of from
// updates.
//
// For a change that touches many cells at once this is both cheaper and more
// truthful than a burst of individual updates: one pass under the lock, no
// messages to drop, and every session converges on the canvas as it now is.
func (h *Hub) MarkAllDirty() {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.sessions {
		s.dirty.Store(true)
		// Nudge the session awake. Its listener is parked on the channel, so
		// without something to receive it would not notice the flag until the next
		// placement or tick.
		select {
		case s.ch <- Update{Resync: true}:
		default:
		}
	}
}

// Online returns the number of connected sessions.
func (h *Hub) Online() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessions)
}

// ConnectionsFrom returns how many sessions ip currently holds.
func (h *Hub) ConnectionsFrom(ip string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.perIP[ip]
}

// Dropped returns the total number of coalesced updates since start, which is
// the signal that clients are falling behind.
func (h *Hub) Dropped() uint64 { return h.dropped.Load() }
