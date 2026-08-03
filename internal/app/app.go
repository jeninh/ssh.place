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

// ErrKeyBlocked is returned when the key has been blocked by the operator. It
// carries no retryAfter because there is nothing to wait for.
var ErrKeyBlocked = errors.New("this key is blocked from placing on ssh.place")

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

// IsSlowed reports whether s has been put on a longer cooldown.
func (a *App) IsSlowed(s *hub.Session) bool {
	if s == nil || len(a.Slowed) == 0 || a.SlowFactor <= 1 {
		return false
	}
	return a.Slowed[s.Identity]
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

	// Checked before the exemption, so a fingerprint appearing in both lists is
	// blocked rather than exempt. Config that contradicts itself should fail
	// closed.
	if a.IsBlocked(s) {
		return 0, ErrKeyBlocked
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

// CooldownLeft reports how long s must wait before its next placement.
func (a *App) CooldownLeft(s *hub.Session, now time.Time) time.Duration {
	if a.IsAdmin(s) {
		return 0
	}
	return a.Limiter.RemainingFor(s.Identity, a.cooldownFor(s), now)
}
