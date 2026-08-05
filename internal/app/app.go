// Package app is the authoritative placement path. Every rule that matters —
// bounds, printability, cooldown, per-IP budget — is enforced here, on the
// server, so a hand-rolled SSH client that skips the TUI gains nothing.
package app

import (
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jeninh/ssh.place/internal/canvas"
	"github.com/jeninh/ssh.place/internal/eventlog"
	"github.com/jeninh/ssh.place/internal/hub"
	"github.com/jeninh/ssh.place/internal/netset"
	"github.com/jeninh/ssh.place/internal/ratelimit"
)

// ErrCooldown is returned when the placement was refused for pacing reasons.
// Place also returns how long to wait.
var ErrCooldown = errors.New("still cooling down")

// ErrCharactersDisabled is returned when a character was placed on a canvas
// configured for solid blocks only.
var ErrCharactersDisabled = errors.New("blocks only, no characters")

// ErrNetworkBusy is returned when the budget shared by everyone on the player's
// network is spent, rather than the player's own cooldown. Telling them apart
// matters: one means wait your turn, the other means somebody nearby is hogging.
var ErrNetworkBusy = errors.New("your network has used its share, hold on")

// ErrKeyRequired is returned when a session with no public key tries to place
// on a canvas that requires one.
//
// A keyless client is identified by network, which is the weakest identity here:
// it is shared by everyone behind the same address, so a cooldown against it is
// really a cooldown against a whole building. Every abusive session measured on
// the live canvas was keyless for exactly that reason.
var ErrKeyRequired = errors.New("no key, so watching only")

// ErrKeyBlocked is returned when the key has been blocked by the operator. It
// carries no retryAfter because there is nothing to wait for.
var ErrKeyBlocked = errors.New("this key is blocked from placing on ssh.place")

// ErrNetworkBlocked is returned when the client's network has been blocked.
//
// Separate from ErrKeyBlocked because it is a different fact about a different
// thing, and because the operator wants to see which one fired in the logs.
var ErrNetworkBlocked = errors.New("this network is blocked from placing on ssh.place")

// ErrCanvasBusy is returned when the canvas as a whole has hit its churn
// ceiling, which is the limit that stops it being repainted end to end.
var ErrCanvasBusy = errors.New("the canvas is busy right now, try again shortly")

// App wires the canvas, the session hub, the limiter and the event log
// together.
type App struct {
	Canvas  *canvas.Canvas
	Hub     *hub.Hub
	Limiter *ratelimit.Limiter
	Log     *eventlog.Log
	Logger  *slog.Logger

	// BlocksOnly refuses characters outright, so the canvas can only ever hold
	// solid color. Enforced here rather than in the TUI: a hand-rolled client
	// must not be able to write words either.
	BlocksOnly bool

	// Admins holds SSH key fingerprints that bypass every placement limit. The
	// operator needs to be able to paint over something offensive at speed
	// rather than one cell every fifteen seconds. Read only from server config,
	// never from anything the client sends.
	Admins map[string]bool

	// RequireKey makes a public key mandatory for placing. Keyless clients can
	// still connect and watch.
	RequireKey bool

	// Blocked holds SSH key fingerprints refused at placement time. Read only
	// from server config. Blocked keys can still connect and watch: the canvas is
	// public either way, and taking the drawing away is the whole point.
	//
	// This is for vandalism, not for bots. Bots get Slowed.
	Blocked map[string]bool

	// Slowed holds SSH key fingerprints that keep playing on a longer cooldown.
	//
	// Bots are welcome on a canvas like this; a bot that outpaces every human on
	// it is not. Slowing beats blocking because it leaves them in the game and
	// gives them nothing to route around: there is no error to detect and retry,
	// just a longer wait. Blocking teaches an operator to rotate keys, and keys
	// are free.
	Slowed map[string]bool

	// BlockedNets refuses whole networks. This is the only lever that survives
	// key rotation: 6,173 placements were measured coming from 2,642 distinct
	// keys, about two each, so a fingerprint list is chasing something that is
	// regenerated per placement. A subnet costs money and cannot be reissued on a
	// whim.
	//
	// It is blunt on purpose. Everyone behind a blocked network loses the ability
	// to place, so it belongs on hosting ranges running fleets, not on a range
	// that might be somebody's home.
	BlockedNets *netset.Set

	// SlowedNets puts whole networks on the longer cooldown instead of refusing
	// them, for a range that is probably mixed.
	SlowedNets *netset.Set

	// SlowFactor multiplies the cooldown for a slowed key. Zero or one means no
	// slowing at all, which makes an unset factor safe rather than silently
	// punitive.
	SlowFactor float64

	// placements counts accepted writes since the process started. The canvas
	// outlives restarts but this counter does not, and the stats page says so.
	placements atomic.Uint64
}

// Placements returns the number of accepted placements since start.
func (a *App) Placements() uint64 { return a.placements.Load() }

// IsAdmin reports whether s belongs to an operator.
//
// Only a real public key can be an admin. Clients with no key are identified by
// network, which is not something to grant privileges against: anyone sharing
// that network would inherit them.
func (a *App) IsAdmin(s *hub.Session) bool {
	if s == nil || !s.Keyed || len(a.Admins) == 0 {
		return false
	}
	return a.Admins[s.Identity]
}

// IsBlocked reports whether s has been blocked from placing.
func (a *App) IsBlocked(s *hub.Session) bool {
	if s == nil || len(a.Blocked) == 0 {
		return false
	}
	return a.Blocked[s.Identity]
}

// IsNetBlocked reports whether s connected from a blocked network.
func (a *App) IsNetBlocked(s *hub.Session) bool {
	return s != nil && a.BlockedNets.Contains(s.IP)
}

// IsSlowed reports whether s has been put on a longer cooldown, by key or by
// network.
func (a *App) IsSlowed(s *hub.Session) bool {
	if s == nil || a.SlowFactor <= 1 {
		return false
	}
	if a.SlowedNets.Contains(s.IP) {
		return true
	}
	return len(a.Slowed) > 0 && a.Slowed[s.Identity]
}

// cooldownFor returns the cooldown that applies to s.
func (a *App) cooldownFor(s *hub.Session) time.Duration {
	base := a.Limiter.Cooldown()
	if a.IsSlowed(s) {
		return time.Duration(float64(base) * a.SlowFactor)
	}
	return base
}

// Cooldown returns the per-identity cooldown, for display purposes.
func (a *App) Cooldown() time.Duration { return a.Limiter.Cooldown() }

// Place validates and applies one placement on behalf of s.
//
// On refusal it returns ErrCooldown (with retryAfter set) or one of the canvas
// validation errors. On success the change is persisted in memory, appended to
// the event log and broadcast to every session including s.
func (a *App) Place(s *hub.Session, x, y int, cell canvas.Cell, now time.Time) (retryAfter time.Duration, err error) {
	// Validate before consuming budget: a client that aims off-canvas or sends
	// a control character should not burn its turn.
	if !a.Canvas.InBounds(x, y) {
		return 0, canvas.ErrOutOfBounds
	}
	if err := cell.Validate(); err != nil {
		return 0, err
	}
	// A block and an erase both carry no character, so both stay legal.
	if a.BlocksOnly && cell.Rune != canvas.Empty {
		return 0, ErrCharactersDisabled
	}

	// Before the limiter, because this is about who they are rather than how fast
	// they are going, and a keyless client should not be told to wait for a turn
	// it is never going to get.
	if a.RequireKey && !s.Keyed {
		return 0, ErrKeyRequired
	}

	// Checked before the exemption, so a fingerprint appearing in both lists is
	// blocked rather than exempt. Config that contradicts itself should fail
	// closed.
	if a.IsBlocked(s) {
		return 0, ErrKeyBlocked
	}
	// After the key check so a blocked key still reports as a blocked key, and
	// before the exemption so a network ban is not something an exempt key can
	// sit inside.
	if a.IsNetBlocked(s) {
		return 0, ErrNetworkBlocked
	}

	// Admins skip all three limits. Every placement still goes through the same
	// validation above and still lands in the event log, so the audit trail is
	// unchanged: their fingerprint is on every cell they place.
	if !a.IsAdmin(s) {
		ok, wait, why := a.Limiter.ReserveFor(s.Identity, s.IP, a.cooldownFor(s), now)
		if !ok {
			switch why {
			case ratelimit.RefusedNetwork:
				return wait, ErrNetworkBusy
			case ratelimit.RefusedGlobal:
				return wait, ErrCanvasBusy
			}
			return wait, ErrCooldown
		}
	}

	if err := a.Canvas.SetCell(x, y, cell); err != nil {
		return 0, err
	}

	if err := a.Log.Append(eventlog.Event{
		At:       now.UTC(),
		Identity: s.Identity,
		X:        x,
		Y:        y,
		Rune:     string(cell.Rune),
		Color:    cell.Color,
		Block:    cell.IsBlock(),
	}); err != nil && a.Logger != nil {
		// A failing event log must not cost the player their placement: the
		// canvas is the source of truth and it has already been updated.
		a.Logger.Error("append event log", "err", err)
	}

	a.placements.Add(1)
	a.Hub.Broadcast(hub.Update{X: x, Y: y, Cell: cell})
	return 0, nil
}

// ErrNotOperator is returned when a session asks for something only an operator
// may do.
var ErrNotOperator = errors.New("that is an operator action")

// PlaceRegion writes cell across a whole rectangle on behalf of s.
//
// Operators only, and checked here rather than in the TUI: this is a bulk write,
// so it is the one place in this file where a missing check would let one
// keystroke flatten the canvas.
//
// It is exempt from the limiters for the same reason the per-key exemption
// exists. Moderating at one cell every fifteen seconds is not moderating, and
// clearing a region is the thing you need when somebody has drawn something
// vile at three in the morning.
func (a *App) PlaceRegion(s *hub.Session, x0, y0, x1, y1 int, cell canvas.Cell, now time.Time) (int, error) {
	if !a.IsAdmin(s) {
		return 0, ErrNotOperator
	}
	// Same precedence as Place: a fingerprint in both lists is blocked, not
	// exempt. Without this the bulk path would be a way around the block list,
	// which is the one thing an exemption must never buy.
	if a.IsBlocked(s) {
		return 0, ErrKeyBlocked
	}
	if a.IsNetBlocked(s) {
		return 0, ErrNetworkBlocked
	}
	if err := cell.Validate(); err != nil {
		return 0, err
	}
	// A block and an erase both carry no character, so both stay legal.
	if a.BlocksOnly && cell.Rune != canvas.Empty {
		return 0, ErrCharactersDisabled
	}

	// SetRegion reports the rectangle it actually wrote, so the log and the
	// broadcast describe the change rather than the request.
	rect, err := a.Canvas.SetRegion(x0, y0, x1, y1, cell)
	if err != nil {
		return 0, err
	}
	n := rect.Cells()

	for y := rect.Y0; y <= rect.Y1; y++ {
		for x := rect.X0; x <= rect.X1; x++ {
			// Every cell is logged individually. The event log is the only record of
			// how the canvas got to where it is, and the timelapse replays it to
			// reproduce the canvas exactly; a bulk change that skipped the log would
			// break that and every later frame would be wrong.
			if err := a.Log.Append(eventlog.Event{
				At:       now.UTC(),
				Identity: s.Identity,
				X:        x,
				Y:        y,
				Rune:     string(cell.Rune),
				Color:    cell.Color,
				Block:    cell.IsBlock(),
			}); err != nil && a.Logger != nil {
				a.Logger.Error("append event log", "err", err)
			}
		}
	}

	// One flag rather than n updates. Sending a cell at a time would take the hub
	// lock once per cell and push twelve thousand messages at every session for a
	// full-canvas wipe, which stalls the operator's own session doing it. Sessions
	// redraw from the canvas when they see this, which is the same path a client
	// that fell behind already takes.
	a.Hub.MarkAllDirty()
	a.placements.Add(uint64(n))

	if a.Logger != nil {
		a.Logger.Info("region written", "by", s.Identity,
			"x0", rect.X0, "y0", rect.Y0, "x1", rect.X1, "y1", rect.Y1,
			"cells", n, "cleared", !cell.Drawn())
	}
	return n, nil
}

// CooldownLeft reports how long s must wait before its next placement.
func (a *App) CooldownLeft(s *hub.Session, now time.Time) time.Duration {
	if a.IsAdmin(s) {
		return 0
	}
	return a.Limiter.RemainingFor(s.Identity, a.cooldownFor(s), now)
}
