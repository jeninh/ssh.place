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

	// placements counts accepted writes since the process started. The canvas
	// outlives restarts but this counter does not, and the stats page says so.
	placements atomic.Uint64
}

// Placements returns the number of accepted placements since start.
func (a *App) Placements() uint64 { return a.placements.Load() }

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

	ok, wait, why := a.Limiter.Reserve(s.Identity, s.IP, now)
	if !ok {
		switch why {
		case ratelimit.RefusedNetwork:
			return wait, ErrNetworkBusy
		case ratelimit.RefusedGlobal:
			return wait, ErrCanvasBusy
		}
		return wait, ErrCooldown
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
	return a.Limiter.Remaining(s.Identity, now)
}
