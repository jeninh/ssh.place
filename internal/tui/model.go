// Package tui is the per-session bubbletea program: one canvas viewport, one
// cursor, one status bar.
package tui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/jeninh/ssh.place/internal/app"
	"github.com/jeninh/ssh.place/internal/canvas"
	"github.com/jeninh/ssh.place/internal/hub"
)

const (
	// chromeLines is how many rows the status and help bars take.
	chromeLines = 2

	// minFrameInterval throttles repaints. A placement fans out to every
	// connected session, so without a floor here a busy canvas would have
	// hundreds of sessions rebuilding frames on every single placement.
	minFrameInterval = 50 * time.Millisecond

	// tickInterval drives the cooldown countdown and highlight decay.
	tickInterval = 250 * time.Millisecond

	// How long a freshly placed cell stays marked, and when the marker steps
	// down to its dimmer stage.
	highlightFresh = 350 * time.Millisecond
	highlightTotal = time.Second

	// flashDuration is how long a status message stays up.
	flashDuration = 2 * time.Second

	// scrollMargin keeps this many rows/columns visible past the cursor so the
	// viewport starts panning before the cursor hits the edge.
	scrollMargin = 3

	minTermW = 24
	minTermH = 5

	// Scroll pacing. A trackpad flick arrives as a burst of wheel events, so
	// moving a cell on every one of them flies across the canvas. These two
	// constants are the speed knob: cells per step, and events needed per step.
	mouseScrollStep     = 1
	scrollEventsPerCell = 2
)

// Messages internal to the model.
type (
	// canvasMsg is one placement observed from the hub.
	canvasMsg hub.Update
	// hubClosedMsg means the session was removed from the hub.
	hubClosedMsg struct{}
	// tickMsg drives time-based state: cooldown, fades, idle timeout.
	tickMsg time.Time
	// frameMsg is a deferred repaint scheduled by the frame throttle.
	frameMsg struct{}
)

// Config configures a Model.
type Config struct {
	App     *app.App
	Session *hub.Session
	// Renderer is bound to the SSH session so color support is detected
	// per-client rather than from the server's own environment.
	Renderer *lipgloss.Renderer
	// IdleTimeout disconnects a session that sends no input. Zero disables it.
	IdleTimeout time.Duration
	// WebURL, when set, is shown in the help text.
	WebURL string
	// BlocksOnly hides character mode entirely. It must match the App's own
	// setting, which is what actually enforces it.
	BlocksOnly bool
	// RequireKey mirrors the App setting so the view can explain the restriction.
	// The App is what enforces it.
	RequireKey bool
	// Exit receives the reason the session ended.
	Exit *Exit
	// Now defaults to time.Now; tests override it.
	Now func() time.Time
}

// Model is one session's view of the canvas.
type Model struct {
	app     *app.App
	sess    *hub.Session
	exit    *Exit
	now     func() time.Time
	idle    time.Duration
	webURL  string
	painter *painter
	styles  styles

	canvasW, canvasH int
	termW, termH     int

	curX, curY int
	offX, offY int

	stamp rune
	color uint8
	// block paints solid color instead of the stamp character.
	block bool
	// blocksOnly means there is no character mode to switch into.
	blocksOnly bool

	// scrollX and scrollY hold partial wheel notches, so scrolling can be paced
	// more slowly than the events arrive.
	scrollX, scrollY int

	// literalNext makes the next keypress select a stamp character even if it
	// would normally be a command. Without it the movement, color and quit keys
	// would be undrawable.
	literalNext bool

	// erase makes placements clear the cell instead of colouring it. Operators
	// only, toggled with backtick. Nothing here enforces that: the key handler
	// checks, and the server does not need to, because a blank cell is a legal
	// placement for anyone and always has been.
	erase bool

	// region holds the anchor corner of a rectangular selection, when one is in
	// progress. The cursor is the opposite corner. Operators only.
	region  bool
	regionX int
	regionY int
	// watching means this session cannot place, only look. Set for a keyless
	// client on a canvas that requires a key.
	watching bool
	// splash holds the one-time notice shown before the canvas. Any key clears it.
	splash bool

	cooldownUntil time.Time
	lastInput     time.Time

	flash      string
	flashUntil time.Time

	// recent maps a canvas index to when it was placed, for the fade marker.
	recent map[int]time.Time

	// Frame cache. View returns frame verbatim; rendering happens in Update so
	// it can be throttled.
	frame        string
	lastRender   time.Time
	dirty        bool
	framePending bool

	buf     []canvas.Cell
	scratch []byte

	quitting bool
}

// New returns a Model ready to hand to bubbletea.
func New(cfg Config) *Model {
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()

	w, h := cfg.App.Canvas.Size()
	colorful := cfg.Renderer == nil || cfg.Renderer.ColorProfile() != termenv.Ascii

	m := &Model{
		app:     cfg.App,
		sess:    cfg.Session,
		exit:    cfg.Exit,
		now:     nowFn,
		idle:    cfg.IdleTimeout,
		webURL:  cfg.WebURL,
		painter: newPainter(colorful),
		styles:  newStyles(cfg.Renderer),
		canvasW: w,
		canvasH: h,
		termW:   80,
		termH:   24,
		curX:    w / 2,
		curY:    h / 2,
		stamp:   '#',
		color:   canvas.DefaultColor,
		// Blocks are the default either way; blocks-only just removes the way
		// out of it.
		block:      true,
		blocksOnly: cfg.BlocksOnly,
		watching:   cfg.RequireKey && cfg.Session != nil && !cfg.Session.Keyed,
		lastInput:  now,
		recent:     make(map[int]time.Time),
	}
	// Explain up front rather than letting them press keys at a canvas that will
	// not respond and work it out themselves.
	m.splash = m.watching
	// An identity that placed just before reconnecting is still cooling down;
	// show that rather than a misleading "ready".
	if left := m.app.CooldownLeft(m.sess, now); left > 0 {
		m.cooldownUntil = now.Add(left)
	}
	m.render(now)
	return m
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(waitForUpdate(m.sess.Updates()), scheduleTick())
}

// waitForUpdate blocks on the session's update channel and turns the next
// update into a message. The hub closes the channel when the session goes
// away, which is what lets this goroutine finish instead of leaking.
func waitForUpdate(ch <-chan hub.Update) tea.Cmd {
	return func() tea.Msg {
		u, ok := <-ch
		if !ok {
			return hubClosedMsg{}
		}
		return canvasMsg(u)
	}
}

func scheduleTick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	now := m.now()

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termW, m.termH = msg.Width, msg.Height
		return m, m.repaint(now, true)

	case tea.KeyMsg:
		if m.splash {
			// Any key dismisses, and is swallowed: the first keystroke is an
			// acknowledgement, not a command.
			m.splash = false
			m.lastInput = now
			return m, m.repaint(now, true)
		}
		return m, m.handleKey(msg, now)

	case tea.MouseMsg:
		return m, m.handleMouse(msg, now)

	case canvasMsg:
		if msg.Resync {
			// A bulk change carries no coordinates, so there is no cell to mark as
			// recently placed. Redraw everything instead of trusting the fade map.
			return m, tea.Batch(m.repaint(now, true), waitForUpdate(m.sess.Updates()))
		}
		m.recent[m.idx(msg.X, msg.Y)] = now
		return m, tea.Batch(m.repaint(now, false), waitForUpdate(m.sess.Updates()))

	case hubClosedMsg:
		// The hub dropped us rather than the player leaving, which in practice
		// means the server is restarting. Say so: the default farewell reads as
		// though they chose to go, and does not tell them to come straight back.
		m.exit.Set("ssh.place restarted. Your canvas is safe, hop back on: ssh ssh.place")
		m.quitting = true
		return m, tea.Quit

	case tickMsg:
		return m, m.tick(now)

	case frameMsg:
		m.framePending = false
		if m.dirty {
			m.render(now)
		}
		return m, nil
	}

	return m, nil
}

// View implements tea.Model. Frames are built in Update so that repaints can
// be coalesced; View only hands back the latest one.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	return m.frame
}

func (m *Model) tick(now time.Time) tea.Cmd {
	if m.idle > 0 && now.Sub(m.lastInput) >= m.idle {
		m.exit.Set(fmt.Sprintf("You went idle for %s, so we let the connection go. Come back any time: ssh ssh.place",
			roundDur(m.idle)))
		m.quitting = true
		return tea.Quit
	}

	changed := m.sess.TakeDirty()

	// While anything is fading, keep repainting; expired markers are dropped so
	// they actually clear instead of lingering.
	if len(m.recent) > 0 {
		changed = true
		for idx, at := range m.recent {
			if now.Sub(at) >= highlightTotal {
				delete(m.recent, idx)
			}
		}
	}
	if !m.flashUntil.IsZero() && now.After(m.flashUntil) {
		m.flash = ""
		m.flashUntil = time.Time{}
		changed = true
	}
	if !m.cooldownUntil.IsZero() {
		changed = true
		if now.After(m.cooldownUntil) {
			m.cooldownUntil = time.Time{}
		}
	}

	cmds := []tea.Cmd{scheduleTick()}
	if changed {
		if c := m.repaint(now, false); c != nil {
			cmds = append(cmds, c)
		}
	}
	return tea.Batch(cmds...)
}

// repaint renders now if the frame budget allows, otherwise schedules a
// deferred render. force skips the throttle, for events like a resize where a
// stale frame is visibly wrong.
func (m *Model) repaint(now time.Time, force bool) tea.Cmd {
	m.dirty = true
	if force || now.Sub(m.lastRender) >= minFrameInterval {
		m.render(now)
		return nil
	}
	if m.framePending {
		return nil
	}
	m.framePending = true
	wait := minFrameInterval - now.Sub(m.lastRender)
	return tea.Tick(wait, func(time.Time) tea.Msg { return frameMsg{} })
}

// handleMouse pans on a wheel and warps the cursor on a click.
//
// The viewport is derived from the cursor, so there is nothing to scroll
// independently: moving the cursor is how the view moves. A click deliberately
// only moves the cursor rather than placing, because a stray click would
// otherwise cost the player their whole cooldown.
// scrollBy accumulates wheel notches and moves the cursor once enough have
// arrived, which is what keeps a trackpad's momentum burst from throwing the
// view across the canvas. It reports whether the cursor actually moved, so
// swallowed events do not cost a repaint.
func (m *Model) scrollBy(dx, dy int) bool {
	m.scrollX += dx
	m.scrollY += dy

	moved := false
	// Integer division truncates toward zero, so this works in both directions.
	if n := m.scrollX / scrollEventsPerCell; n != 0 {
		m.moveCursor(n*mouseScrollStep, 0)
		m.scrollX -= n * scrollEventsPerCell
		moved = true
	}
	if n := m.scrollY / scrollEventsPerCell; n != 0 {
		m.moveCursor(0, n*mouseScrollStep)
		m.scrollY -= n * scrollEventsPerCell
		moved = true
	}
	return moved
}

func (m *Model) handleMouse(msg tea.MouseMsg, now time.Time) tea.Cmd {
	m.lastInput = now

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if !m.scrollBy(0, -1) {
			return nil
		}
	case tea.MouseButtonWheelDown:
		if !m.scrollBy(0, 1) {
			return nil
		}
	case tea.MouseButtonWheelLeft:
		if !m.scrollBy(-1, 0) {
			return nil
		}
	case tea.MouseButtonWheelRight:
		if !m.scrollBy(1, 0) {
			return nil
		}

	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return nil
		}
		// Ignore the status and help bars, which sit below the grid.
		if msg.X < 0 || msg.Y < 0 || msg.X >= m.gridWidth() || msg.Y >= m.gridHeight() {
			return nil
		}
		x, y := m.offX+msg.X, m.offY+msg.Y
		if !m.app.Canvas.InBounds(x, y) {
			// The border ring, not a cell.
			return nil
		}
		m.curX, m.curY = x, y

	default:
		return nil
	}
	return m.repaint(now, true)
}

func (m *Model) handleKey(msg tea.KeyMsg, now time.Time) tea.Cmd {
	m.lastInput = now

	// Keys typed faster than the terminal is read — and anything pasted —
	// arrive as one message carrying several runes. Handle them one at a time:
	// treating the burst as a single unrecognised key would silently swallow
	// everything a fast typist did. Placements stay rate limited server-side, so
	// a pasted wall of text still buys at most one cell.
	if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 {
		for _, r := range msg.Runes {
			cmd := m.handleOneKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, now)
			if m.quitting {
				return cmd
			}
		}
		return m.repaint(now, true)
	}
	return m.handleOneKey(msg, now)
}

func (m *Model) handleOneKey(msg tea.KeyMsg, now time.Time) tea.Cmd {
	key := msg.String()

	if m.literalNext {
		m.literalNext = false
		if r, ok := stampRune(msg); ok {
			m.stamp, m.block = r, false
			m.setFlash(fmt.Sprintf("now drawing %q", r), now)
		} else {
			m.setFlash("never mind, cancelled", now)
		}
		return m.repaint(now, true)
	}

	switch key {
	case "ctrl+c", "ctrl+d", "q":
		m.quitting = true
		return tea.Quit

	case "up", "k", "w":
		m.moveCursor(0, -1)
	case "down", "j", "s":
		m.moveCursor(0, 1)
	case "left", "h", "a":
		m.moveCursor(-1, 0)
	case "right", "l", "d":
		m.moveCursor(1, 0)

	// Page movement on both axes. The viewport is far wider than it is tall, so
	// without a horizontal page key crossing a 200-column canvas one cell at a
	// time is a long walk, and it looks like panning is broken.
	case "pgup":
		m.moveCursor(0, -m.gridHeight())
	case "pgdown":
		m.moveCursor(0, m.gridHeight())
	case "shift+up", "ctrl+up":
		m.moveCursor(0, -m.gridHeight())
	case "shift+down", "ctrl+down":
		m.moveCursor(0, m.gridHeight())
	case "shift+left", "ctrl+left":
		m.moveCursor(-m.gridWidth(), 0)
	case "shift+right", "ctrl+right":
		m.moveCursor(m.gridWidth(), 0)
	case "home":
		m.curX = 0
	case "end":
		m.curX = m.canvasW - 1

	case "enter", " ":
		m.place(now)

	case "tab":
		m.color = (m.color + 1) % canvas.PaletteSize
	case "shift+tab":
		m.color = (m.color + canvas.PaletteSize - 1) % canvas.PaletteSize

	// Backtick is the eraser, and it is only offered to an operator. Everyone
	// else placing blanks would just be a faster way to wreck other people's
	// work, whereas clearing a region is the one moderation move that cannot be
	// done with a colour.
	// v for a visual selection, which is what vim calls it and where an operator
	// will reach first.
	case "v":
		if !m.app.IsAdmin(m.sess) {
			m.setFlash("that key does nothing here", now)
			break
		}
		if m.region {
			m.region = false
			m.setFlash("selection cancelled", now)
			break
		}
		m.region, m.regionX, m.regionY = true, m.curX, m.curY
		m.setFlash("corner set, move and press enter", now)

	case "`":
		if !m.app.IsAdmin(m.sess) {
			m.setFlash("that key does nothing here", now)
			break
		}
		m.erase = !m.erase
		if m.erase {
			m.setFlash("eraser on, placing clears the cell", now)
		} else {
			m.setFlash("eraser off", now)
		}

	case `\`:
		if m.blocksOnly {
			m.setFlash(blocksOnlyMsg, now)
			break
		}
		m.literalNext = true

	// ctrl+b rather than a letter: every printable key is a stamp, so a plain
	// letter could not carry a command.
	case "ctrl+b":
		if m.blocksOnly {
			m.setFlash(blocksOnlyMsg, now)
			break
		}
		m.block = !m.block
		if m.block {
			m.setFlash("back to solid blocks", now)
		} else {
			m.setFlash(fmt.Sprintf("now drawing %q", m.stamp), now)
		}

	case "esc":
		if m.region {
			m.region = false
			m.setFlash("selection cancelled", now)
			break
		}
		// Nothing to cancel; clear any lingering message.
		m.flash, m.flashUntil = "", time.Time{}

	default:
		if key >= "0" && key <= "9" && len(key) == 1 {
			m.color = uint8(key[0] - '0')
			break
		}
		if r, ok := stampRune(msg); ok {
			if m.blocksOnly {
				// Say why, rather than letting the key appear to do nothing.
				m.setFlash(blocksOnlyMsg, now)
				break
			}
			// Picking a character is a request to draw it, so stop painting.
			m.stamp, m.block = r, false
			break
		}
		// Unknown key: nothing to redraw.
		return nil
	}

	return m.repaint(now, true)
}

// stampRune extracts a single printable stamp character from a key press.
func stampRune(msg tea.KeyMsg) (rune, bool) {
	if msg.String() == " " {
		return ' ', true
	}
	if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && canvas.ValidRune(msg.Runes[0]) {
		return msg.Runes[0], true
	}
	return 0, false
}

// commitRegion applies the current stamp across the selected rectangle.
//
// Whether that clears or paints is just the eraser toggle, so the one selection
// mechanism covers both wiping a region and blocking one out in a colour.
func (m *Model) commitRegion(now time.Time) {
	n, err := m.app.PlaceRegion(m.sess, m.regionX, m.regionY, m.curX, m.curY, m.cell(), now)
	m.region = false
	if err != nil {
		m.setFlash(err.Error(), now)
		return
	}
	what := "filled"
	if m.erase {
		what = "cleared"
	}
	m.setFlash(fmt.Sprintf("%s %d cells", what, n), now)
}

// regionSize is the rectangle the selection currently covers.
func (m *Model) regionSize() (w, h int) {
	return abs(m.curX-m.regionX) + 1, abs(m.curY-m.regionY) + 1
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// cell is what this session would place right now.
func (m *Model) cell() canvas.Cell {
	if m.erase {
		// A blank cell carries no character and no fill, which is exactly what an
		// undrawn cell looks like, so this genuinely resets rather than painting
		// something that merely looks like the background.
		return canvas.Glyph(canvas.Empty, m.color)
	}
	if m.block || m.blocksOnly {
		return canvas.Block(m.color)
	}
	return canvas.Glyph(m.stamp, m.color)
}

func (m *Model) place(now time.Time) {
	if m.region {
		m.commitRegion(now)
		return
	}
	retryAfter, err := m.app.Place(m.sess, m.curX, m.curY, m.cell(), now)
	switch {
	case err == nil:
		// An exempt key must not arm the local countdown either. The status bar
		// hides it, but anything else reading this state would throttle a session
		// the server is happy to accept.
		if !m.app.IsAdmin(m.sess) {
			m.cooldownUntil = now.Add(m.app.Cooldown())
		}
		m.flash, m.flashUntil = "", time.Time{}
	case errors.Is(err, app.ErrCooldown):
		m.cooldownUntil = now.Add(retryAfter)
		m.setFlash(fmt.Sprintf("hold on, %s to go", roundDur(retryAfter)), now)
	case errors.Is(err, app.ErrNetworkBusy), errors.Is(err, app.ErrCanvasBusy):
		// Not this player's own cooldown, so say which limit it was rather than
		// letting them think they miscounted. The countdown still runs, because
		// the wait is real either way.
		m.cooldownUntil = now.Add(retryAfter)
		m.setFlash(err.Error(), now)
	default:
		m.setFlash(err.Error(), now)
	}
}

func (m *Model) setFlash(msg string, now time.Time) {
	m.flash = msg
	m.flashUntil = now.Add(flashDuration)
}

func (m *Model) moveCursor(dx, dy int) {
	m.curX = clamp(m.curX+dx, 0, m.canvasW-1)
	m.curY = clamp(m.curY+dy, 0, m.canvasH-1)
}

func (m *Model) idx(x, y int) int { return y*m.canvasW + x }

// gridWidth and gridHeight are the viewport dimensions in cells, counting the
// one-cell border ring drawn around the canvas.
func (m *Model) gridWidth() int  { return min(m.termW, m.canvasW+2) }
func (m *Model) gridHeight() int { return min(m.termH-chromeLines, m.canvasH+2) }

func (m *Model) render(now time.Time) {
	m.lastRender = now
	m.dirty = false

	if m.termW < minTermW || m.termH < minTermH {
		m.frame = fmt.Sprintf("ssh.place needs at least %dx%d. Your terminal is %dx%d.",
			minTermW, minTermH, m.termW, m.termH)
		return
	}

	if m.splash {
		m.frame = m.keylessSplash()
		return
	}

	viewW, viewH := m.gridWidth(), m.gridHeight()
	// Scroll in framed coordinates, where the border ring occupies -1 and the
	// canvas width. Shifting by one lets the same scroll logic bring the frame
	// into view instead of treating it as out of bounds.
	m.offX = scrollTo(m.offX+1, m.curX+1, viewW, m.canvasW+2) - 1
	m.offY = scrollTo(m.offY+1, m.curY+1, viewH, m.canvasH+2) - 1

	if need := viewW * viewH; len(m.buf) < need {
		m.buf = make([]canvas.Cell, need)
	}
	m.app.Canvas.CopyRegion(m.offX, m.offY, viewW, viewH, m.buf)

	var b strings.Builder
	b.Grow(viewW*viewH*2 + viewH*16 + 512)

	scratch := m.scratch
	hasRecent := len(m.recent) > 0
	for row := 0; row < viewH; row++ {
		cells := m.buf[row*viewW : (row+1)*viewW]
		y := m.offY + row
		scratch = scratch[:0]
		var cur styleKey
		started := false
		for col, cell := range cells {
			k, g := m.styleFor(m.offX+col, y, cell, now, hasRecent)
			if started && k != cur {
				m.painter.paint(&b, cur, string(scratch))
				scratch = scratch[:0]
			}
			cur, started = k, true
			scratch = append(scratch, g)
		}
		if started {
			m.painter.paint(&b, cur, string(scratch))
		}
		b.WriteByte('\n')
	}
	m.scratch = scratch

	// The bars are chrome, not canvas: they span the terminal even when the
	// canvas is narrower than it.
	b.WriteString(m.statusBar(now, m.termW))
	b.WriteByte('\n')
	b.WriteString(m.helpBar(m.termW))

	m.frame = b.String()
}

// borderColor is the palette slot the frame is drawn in: grey, so it reads as
// chrome rather than as somebody's artwork.
const borderColor = 8

// Frame glyphs. Plain ASCII, because the frame has to look right on a terminal
// whose font has no box-drawing characters.
const (
	borderCorner = '+'
	borderVert   = '|'
	borderHorz   = '-'
)

// borderAt returns the frame glyph at (x, y), if that position is part of the
// one-cell ring drawn just outside the canvas.
func borderAt(x, y, w, h int) (byte, bool) {
	onSide := x == -1 || x == w
	onEnd := y == -1 || y == h
	switch {
	case onSide && onEnd:
		return borderCorner, true
	case onSide:
		return borderVert, true
	case onEnd:
		return borderHorz, true
	}
	return 0, false
}

// styleFor returns how to draw one viewport position: the frame if it sits on
// the border ring, otherwise the canvas cell underneath.
func (m *Model) styleFor(x, y int, cell canvas.Cell, now time.Time, hasRecent bool) (styleKey, byte) {
	if g, ok := borderAt(x, y, m.canvasW, m.canvasH); ok {
		return styleKey{fg: borderColor, bg: bgNone}, g
	}

	// The anchor corner gets the same treatment as the cursor, so an operator can
	// see both ends of the rectangle they are about to write over.
	if m.region && x == m.regionX && y == m.regionY && !(x == m.curX && y == m.curY) {
		k := m.keyFor(x, y, cell, now, hasRecent)
		if cell.IsBlock() {
			k.fg = cursorFg(k.bg)
			k.decor = decorBold
			return k, anchorGlyph
		}
		k.decor = decorReverse
		return k, m.glyphFor(cell)
	}

	k := m.keyFor(x, y, cell, now, hasRecent)
	if k.decor != decorNone {
		// On a solid block the foreground and fill are the same color, so reverse
		// video would swap a color with itself and the cursor would vanish. Keep
		// the block's color and stamp a contrasting marker on it.
		if cell.IsBlock() {
			k.fg = cursorFg(k.bg)
			k.decor = decorBold
			return k, cursorGlyph
		}
		// Anywhere else reverse video is both visible and non-destructive: the
		// character under the cursor stays readable instead of being covered up.
		k.decor = decorReverse
	}
	return k, m.glyphFor(cell)
}

// glyphFor returns the byte to draw for a cell. A block carries no character; it
// is the background that colors it, except on a client with no color at all,
// where a background is invisible and a stand-in is the only option.
func (m *Model) glyphFor(cell canvas.Cell) byte {
	if cell.IsBlock() {
		if m.painter.color {
			return canvas.Empty
		}
		return blockFallback
	}
	if !canvas.ValidRune(cell.Rune) {
		return canvas.Empty
	}
	return byte(cell.Rune)
}

func (m *Model) keyFor(x, y int, cell canvas.Cell, now time.Time, hasRecent bool) styleKey {
	k := styleKey{fg: cell.Color, bg: bgNone}
	if cell.IsBlock() {
		k.bg = bgOfFill(cell.Fill)
	}
	if x == m.curX && y == m.curY {
		// Refined in styleFor, which knows whether reverse video will work here.
		k.decor = decorReverse
	}
	// The marker and the fill share the one background slot, so a freshly
	// placed block briefly shows the marker instead of its own color.
	if hasRecent {
		if at, ok := m.recent[m.idx(x, y)]; ok {
			switch age := now.Sub(at); {
			case age < highlightFresh:
				k.bg = bgHlFresh
			case age < highlightTotal:
				k.bg = bgHlFade
			}
		}
	}
	return k
}

func (m *Model) statusBar(now time.Time, width int) string {
	segs := []segment{
		{text: "ssh.place", style: m.styles.logo},
		{text: fmt.Sprintf("%d,%d", m.curX, m.curY), style: m.styles.coords},
		{text: m.stampLabel(), style: m.stampStyle()},
	}

	if m.region {
		w, h := m.regionSize()
		segs = append(segs, segment{
			text:  fmt.Sprintf("select %dx%d", w, h),
			style: m.styles.eraser,
		})
	}

	switch {
	case m.watching:
		// Not "ready", and not a countdown either: neither would be true.
		segs = append(segs, segment{text: "watching", style: m.styles.cooling})
	case m.app.IsAdmin(m.sess):
		// Worth saying out loud rather than showing a permanent "ready": if the
		// exemption ever stops applying, you find out from the status bar instead
		// of from the canvas refusing you mid-cleanup.
		segs = append(segs, segment{text: "no cooldown", style: m.styles.ready})
	case m.cooldownLeft(now) > 0:
		segs = append(segs, segment{text: roundDur(m.cooldownLeft(now)).String(), style: m.styles.cooling})
	default:
		segs = append(segs, segment{text: "ready", style: m.styles.ready})
	}

	// The message comes before the online count: if only one of them fits, the
	// thing that just happened matters more than the population.
	if m.flash != "" {
		segs = append(segs, segment{text: m.flash, style: m.styles.flash})
	}

	segs = append(segs, segment{
		text:  fmt.Sprintf("%d online", m.app.Hub.Online()),
		style: m.styles.online,
	})

	return joinFit(segs, "  ", width)
}

// blocksOnlyMsg explains why a character key did nothing. Kept short so it
// still fits in the status bar of an 80-column terminal.
const blocksOnlyMsg = "blocks only, no characters"

// stampStyle colours the stamp segment. The eraser gets its own look rather
// than a swatch, because a swatch showing a colour it will not place is worse
// than no swatch at all.
func (m *Model) stampStyle() lipgloss.Style {
	if m.erase {
		return m.styles.eraser
	}
	return m.styles.swatch(m.color)
}

// stampLabel describes what the next placement will put down.
func (m *Model) stampLabel() string {
	if m.erase {
		return "erase"
	}
	if m.block || m.blocksOnly {
		return fmt.Sprintf("███ %s", canvas.Palette[m.color].Name)
	}
	return fmt.Sprintf("%c █ %s", m.stamp, canvas.Palette[m.color].Name)
}

// cooldownLeft prefers the value the server last told us, so the countdown
// always reflects the authoritative limiter.
func (m *Model) cooldownLeft(now time.Time) time.Duration {
	if m.cooldownUntil.IsZero() {
		return 0
	}
	if left := m.cooldownUntil.Sub(now); left > 0 {
		return left
	}
	return 0
}

// helpBar renders the most detailed hint line that fits. Narrow terminals are
// common over SSH, and a truncated line that loses "q quit" is worse than a
// terser one that keeps it.
func (m *Model) helpBar(width int) string {
	if m.literalNext {
		return m.styles.literal.Render(fit(`\ pressed: the next key becomes your stamp`, width))
	}
	if m.region {
		w, h := m.regionSize()
		verb := "fill"
		if m.erase {
			verb = "clear"
		}
		return m.styles.literal.Render(fit(fmt.Sprintf(
			"selecting %dx%d · move to size it · enter to %s · esc to cancel", w, h, verb), width))
	}
	if m.erase {
		return m.styles.literal.Render(fit("eraser on · place to clear a cell · ` to go back to colour", width))
	}
	// Each mode advertises only what is useful in it, which is what keeps the
	// line inside 80 columns without dropping a binding.
	ladder := charHelp
	switch {
	case m.blocksOnly:
		ladder = blocksOnlyHelp
	case m.block:
		ladder = blockHelp
	}
	candidates := make([]string, 0, len(ladder)+1)
	if m.webURL != "" {
		candidates = append(candidates, ladder[0]+" · "+m.webURL)
	}
	candidates = append(candidates, ladder...)
	for _, c := range candidates {
		if lipgloss.Width(c) <= width {
			return m.styles.help.Render(c)
		}
	}
	return m.styles.help.Render(fit(ladder[len(ladder)-1], width))
}

// Hint lines, widest first. Block mode has less to explain, so it gets its own
// ladder; the last rung is the bare minimum that still lets someone quit.
var (
	// blocksOnlyHelp never mentions characters, because there are none.
	blocksOnlyHelp = []string{
		`←↑↓→/wasd/hjkl move · scroll or shift+←→ pans · 0-9 color · space place · q quit`,
		`wasd/hjkl move · scroll pans · 0-9 color · tab · space place · q quit`,
		`wasd move · 0-9 color · space place · q quit`,
		`wasd move · 0-9 color · space · q quit`,
		`wasd · space · q quit`,
	}

	blockHelp = []string{
		`←↑↓→/wasd/hjkl move · shift+←→ page · 0-9/tab color · ^B letters · space place · q quit`,
		`wasd/hjkl move · shift+←→ page · 0-9 color · ^B letters · space place · q quit`,
		`wasd move · 0-9 color · ^B letters · space place · q quit`,
		`wasd move · 0-9 color · space · q quit`,
		`wasd · space · q quit`,
	}

	charHelp = []string{
		`←↑↓→/wasd/hjkl move · shift+←→ page · keys stamp · 0-9/tab color · \ literal · ^B blocks · space place · q quit`,
		`wasd/hjkl move · keys stamp · 0-9/tab color · \ literal · ^B block · space place · q quit`,
		`wasd move · keys stamp · 0-9/tab color · ^B block · space place · q quit`,
		`wasd move · 0-9 color · space · q quit`,
		`wasd · space · q quit`,
	}
)

// --- small helpers ---

type segment struct {
	text  string
	style lipgloss.Style
}

// joinFit renders segments separated by sep, dropping trailing segments that
// would not fit. Measuring the plain text first avoids trying to truncate a
// string that already contains escape sequences.
func joinFit(segs []segment, sep string, width int) string {
	var b strings.Builder
	used := 0
	for i, s := range segs {
		add := lipgloss.Width(s.text)
		if i > 0 {
			add += len(sep)
		}
		if used+add > width {
			break
		}
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s.style.Render(s.text))
		used += add
	}
	return b.String()
}

// fit truncates s to width columns, trimming whole runes so a multi-byte
// character is never cut in half.
func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	rs := []rune(s)
	if width == 1 {
		return string(rs[0])
	}
	// Leave a column for the ellipsis.
	for len(rs) > 1 && lipgloss.Width(string(rs))+1 > width {
		rs = rs[:len(rs)-1]
	}
	return string(rs) + "…"
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}

// scrollTo returns the viewport offset that keeps cur visible with a margin,
// moving as little as possible from off.
func scrollTo(off, cur, view, total int) int {
	if view >= total {
		return 0
	}
	margin := scrollMargin
	if maxMargin := (view - 1) / 2; margin > maxMargin {
		margin = maxMargin
	}
	if cur-margin < off {
		off = cur - margin
	}
	if cur+margin > off+view-1 {
		off = cur + margin - view + 1
	}
	return clamp(off, 0, total-view)
}

// roundDur trims a duration to something readable in a status bar.
func roundDur(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d < 10*time.Second {
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Second)
}

// ansiColor converts a palette index into a lipgloss color.
func ansiColor(i uint8) lipgloss.Color {
	return lipgloss.Color(strconv.Itoa(int(canvas.Palette[i&0x0f].ANSI)))
}
