package tui

import (
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/jeninh/ssh.place/internal/app"
	"github.com/jeninh/ssh.place/internal/canvas"
	"github.com/jeninh/ssh.place/internal/hub"
	"github.com/jeninh/ssh.place/internal/ratelimit"
)

var base = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

const testCooldown = 15 * time.Second

// clock is a hand-cranked time source so nothing in these tests sleeps.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type harness struct {
	model *Model
	app   *app.App
	sess  *hub.Session
	clock *clock
	exit  *Exit
}

// newHarness builds a model on a small canvas with a plain-text renderer, so
// assertions can look at frame content directly.
func newHarness(t *testing.T, opts ...func(*Config)) *harness {
	t.Helper()
	return newHarnessProfile(t, termenv.Ascii, opts...)
}

// newRenderer builds a renderer pinned to profile. The profile must be set
// explicitly: lipgloss otherwise re-derives it from the environment, and a
// non-TTY writer always comes back as Ascii.
func newRenderer(profile termenv.Profile) *lipgloss.Renderer {
	r := lipgloss.NewRenderer(io.Discard, termenv.WithProfile(profile))
	r.SetColorProfile(profile)
	return r
}

func newHarnessProfile(t *testing.T, profile termenv.Profile, opts ...func(*Config)) *harness {
	t.Helper()

	a := &app.App{
		Canvas: canvas.New(40, 12),
		Hub:    hub.New(100, 10),
		// Generous IP budget: these tests are about the per-key cooldown.
		Limiter: ratelimit.New(testCooldown, 1_000_000, time.Nanosecond),
	}
	sess, err := a.Hub.Add("1.1.1.1", "key-a", true)
	if err != nil {
		t.Fatalf("add session: %v", err)
	}
	t.Cleanup(func() { a.Hub.Remove(sess) })

	clk := &clock{t: base}
	exit := &Exit{}
	cfg := Config{
		App:         a,
		Session:     sess,
		Renderer:    newRenderer(profile),
		IdleTimeout: 30 * time.Minute,
		Exit:        exit,
		Now:         clk.now,
	}
	for _, o := range opts {
		o(&cfg)
	}
	// The App is what actually refuses characters; keep it in step with the view
	// so the harness cannot test a combination the server would never produce.
	a.BlocksOnly = cfg.BlocksOnly

	m := New(cfg)
	// Give the model a definite size, as a real PTY would.
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return &harness{model: m, app: a, sess: sess, clock: clk, exit: exit}
}

// send delivers a message and returns the resulting command.
func (h *harness) send(msg tea.Msg) tea.Cmd {
	_, cmd := h.model.Update(msg)
	return cmd
}

// press sends a key and forces the frame to be current, so tests are not
// affected by the repaint throttle.
func (h *harness) press(msg tea.KeyMsg) {
	h.send(msg)
	h.flush()
}

// flush renders unconditionally, standing in for the deferred frame the
// throttle would have scheduled.
func (h *harness) flush() {
	h.model.render(h.clock.now())
}

func (h *harness) frame() string { return h.model.View() }

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// frameRows returns every rendered row above the chrome, border included.
func (h *harness) frameRows() []string {
	lines := strings.Split(stripANSI(h.frame()), "\n")
	if len(lines) < chromeLines {
		return nil
	}
	return lines[:len(lines)-chromeLines]
}

// grid returns just the canvas rows: the border ring is stripped so content
// assertions are about content, not chrome.
func (h *harness) grid() []string {
	rows := h.frameRows()
	var out []string
	for row, line := range rows {
		y := h.model.offY + row
		if y < 0 || y >= h.model.canvasH {
			continue // a horizontal border row
		}
		cells := []rune(line)
		var keep []rune
		for col, r := range cells {
			x := h.model.offX + col
			if x < 0 || x >= h.model.canvasW {
				continue // a vertical border column
			}
			keep = append(keep, r)
		}
		out = append(out, string(keep))
	}
	return out
}

func (h *harness) status() string {
	lines := strings.Split(stripANSI(h.frame()), "\n")
	return lines[len(lines)-2]
}

func (h *harness) help() string {
	lines := strings.Split(stripANSI(h.frame()), "\n")
	return lines[len(lines)-1]
}

func runes(rs ...rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: rs}
}

func special(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

// --- cursor movement ---

func TestCursorStartsAtCenter(t *testing.T) {
	h := newHarness(t)
	if h.model.curX != 20 || h.model.curY != 6 {
		t.Errorf("cursor at (%d,%d), want (20,6)", h.model.curX, h.model.curY)
	}
}

func TestArrowsAndVimKeysMoveCursor(t *testing.T) {
	cases := []struct {
		name   string
		key    tea.KeyMsg
		dx, dy int
	}{
		{"up arrow", special(tea.KeyUp), 0, -1},
		{"down arrow", special(tea.KeyDown), 0, 1},
		{"left arrow", special(tea.KeyLeft), -1, 0},
		{"right arrow", special(tea.KeyRight), 1, 0},
		{"k", runes('k'), 0, -1},
		{"j", runes('j'), 0, 1},
		{"h", runes('h'), -1, 0},
		{"l", runes('l'), 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			x0, y0 := h.model.curX, h.model.curY
			h.press(tc.key)
			if got, want := h.model.curX, x0+tc.dx; got != want {
				t.Errorf("curX = %d, want %d", got, want)
			}
			if got, want := h.model.curY, y0+tc.dy; got != want {
				t.Errorf("curY = %d, want %d", got, want)
			}
		})
	}
}

// Movement keys must not double as stamps, or you could never move without
// changing your brush.
func TestMovementKeysDoNotChangeStamp(t *testing.T) {
	h := newHarness(t)
	want := h.model.stamp
	for _, k := range []tea.KeyMsg{
		runes('h'), runes('j'), runes('k'), runes('l'),
		runes('w'), runes('a'), runes('s'), runes('d'),
	} {
		h.press(k)
	}
	if h.model.stamp != want {
		t.Errorf("stamp = %q after movement keys, want %q", h.model.stamp, want)
	}
}

func TestCursorClampsAtEdges(t *testing.T) {
	h := newHarness(t)
	w, hh := h.app.Canvas.Size()

	for i := 0; i < w+hh+10; i++ {
		h.send(special(tea.KeyLeft))
		h.send(special(tea.KeyUp))
	}
	if h.model.curX != 0 || h.model.curY != 0 {
		t.Errorf("cursor at (%d,%d) after moving up-left, want (0,0)", h.model.curX, h.model.curY)
	}

	for i := 0; i < w+hh+10; i++ {
		h.send(special(tea.KeyRight))
		h.send(special(tea.KeyDown))
	}
	if h.model.curX != w-1 || h.model.curY != hh-1 {
		t.Errorf("cursor at (%d,%d), want (%d,%d)", h.model.curX, h.model.curY, w-1, hh-1)
	}
}

func TestHomeAndEnd(t *testing.T) {
	h := newHarness(t)
	h.press(special(tea.KeyHome))
	if h.model.curX != 0 {
		t.Errorf("curX after home = %d, want 0", h.model.curX)
	}
	h.press(special(tea.KeyEnd))
	if want := h.app.Canvas.Width() - 1; h.model.curX != want {
		t.Errorf("curX after end = %d, want %d", h.model.curX, want)
	}
}

// Moving the cursor is never rate limited, even mid-cooldown.
func TestMovementIsNeverBlockedByCooldown(t *testing.T) {
	h := newHarness(t)
	h.press(special(tea.KeyEnter))
	if h.model.cooldownLeft(h.clock.now()) == 0 {
		t.Fatal("expected to be cooling down after placing")
	}
	x0 := h.model.curX
	h.press(special(tea.KeyRight))
	if h.model.curX != x0+1 {
		t.Errorf("cursor did not move during cooldown: %d, want %d", h.model.curX, x0+1)
	}
}

// --- stamp and color selection ---

func TestPrintableKeysSelectStamp(t *testing.T) {
	h := newHarness(t)
	for _, r := range []rune{'A', 'z', '#', '~', '0' + 10, '@'} {
		if r < 0x20 || r > 0x7e {
			continue
		}
		if strings.ContainsRune("hjklwasdq0123456789\\ ", r) {
			continue // these are commands; see TestLiteralPrefix
		}
		h.press(runes(r))
		if h.model.stamp != r {
			t.Errorf("stamp = %q after pressing %q, want %q", h.model.stamp, r, r)
		}
	}
}

func TestDigitKeysSelectColor(t *testing.T) {
	h := newHarness(t)
	for d := 0; d <= 9; d++ {
		h.press(runes(rune('0' + d)))
		if int(h.model.color) != d {
			t.Errorf("color = %d after pressing %d, want %d", h.model.color, d, d)
		}
		if h.model.stamp == rune('0'+d) {
			t.Errorf("digit %d was also taken as a stamp", d)
		}
	}
}

func TestTabCyclesColor(t *testing.T) {
	h := newHarness(t)
	h.press(runes('0'))

	for want := 1; want < canvas.PaletteSize; want++ {
		h.press(special(tea.KeyTab))
		if int(h.model.color) != want {
			t.Fatalf("color = %d, want %d", h.model.color, want)
		}
	}
	// Wraps around rather than running off the palette.
	h.press(special(tea.KeyTab))
	if h.model.color != 0 {
		t.Errorf("color = %d after wrapping, want 0", h.model.color)
	}
	h.press(special(tea.KeyShiftTab))
	if want := uint8(canvas.PaletteSize - 1); h.model.color != want {
		t.Errorf("color = %d after shift+tab, want %d", h.model.color, want)
	}
}

// The literal prefix is the escape hatch that makes the command keys drawable.
func TestLiteralPrefix(t *testing.T) {
	for _, r := range []rune{'h', 'j', 'k', 'l', 'w', 'a', 's', 'd', 'q', '0', '9', '\\'} {
		t.Run(string(r), func(t *testing.T) {
			h := newHarness(t)
			x0, y0, color0 := h.model.curX, h.model.curY, h.model.color

			h.press(runes('\\'))
			if !h.model.literalNext {
				t.Fatal("backslash did not arm the literal prefix")
			}
			h.press(runes(r))

			if h.model.stamp != r {
				t.Errorf("stamp = %q, want %q", h.model.stamp, r)
			}
			if h.model.literalNext {
				t.Error("literal prefix stayed armed")
			}
			// The key must not also have acted as a command.
			if h.model.curX != x0 || h.model.curY != y0 {
				t.Errorf("cursor moved to (%d,%d), want (%d,%d)", h.model.curX, h.model.curY, x0, y0)
			}
			if h.model.color != color0 {
				t.Errorf("color changed to %d, want %d", h.model.color, color0)
			}
		})
	}
}

func TestLiteralPrefixCancelsOnNonPrintable(t *testing.T) {
	h := newHarness(t)
	want := h.model.stamp
	h.press(runes('\\'))
	h.press(special(tea.KeyUp))
	if h.model.literalNext {
		t.Error("literal prefix stayed armed")
	}
	if h.model.stamp != want {
		t.Errorf("stamp = %q, want it unchanged at %q", h.model.stamp, want)
	}
	if !strings.Contains(h.help(), "cancelled") && !strings.Contains(h.status(), "cancelled") {
		t.Errorf("no cancellation feedback; status=%q help=%q", h.status(), h.help())
	}
}

func TestSpaceIsAPlaceKeyAndALiteralStamp(t *testing.T) {
	h := newHarness(t)

	// Bare space places rather than selecting a space stamp.
	h.press(runes('X'))
	h.press(runes(' '))
	if got, _ := h.app.Canvas.At(h.model.curX, h.model.curY); got.Rune != 'X' {
		t.Errorf("space did not place: cell = %q, want 'X'", got.Rune)
	}

	// Behind the literal prefix, space selects the eraser.
	h.press(runes('\\'))
	h.press(runes(' '))
	if h.model.stamp != ' ' {
		t.Errorf("stamp = %q, want a space", h.model.stamp)
	}
}

func TestKeySpaceTypeAlsoPlaces(t *testing.T) {
	// Different clients report space as either a rune or the dedicated key
	// type; both have to work.
	if got := (tea.KeyMsg{Type: tea.KeySpace}).String(); got != " " {
		t.Skipf("this bubbletea renders KeySpace as %q; the rune form covers it", got)
	}
	h := newHarness(t)
	h.press(runes('Y'))
	h.press(tea.KeyMsg{Type: tea.KeySpace})
	if got, _ := h.app.Canvas.At(h.model.curX, h.model.curY); got.Rune != 'Y' {
		t.Errorf("cell = %q, want 'Y'", got.Rune)
	}
}

// --- placing ---

func TestEnterPlacesSelectedStampAndColor(t *testing.T) {
	h := newHarness(t)
	h.press(runes('&'))
	h.press(runes('4'))
	h.press(special(tea.KeyLeft))
	x, y := h.model.curX, h.model.curY

	h.press(special(tea.KeyEnter))

	got, ok := h.app.Canvas.At(x, y)
	if !ok {
		t.Fatalf("cell (%d,%d) out of bounds", x, y)
	}
	if got.Rune != '&' || got.Color != 4 {
		t.Errorf("cell = %q/%d, want '&'/4", got.Rune, got.Color)
	}
}

func TestPlaceStartsCooldownAndCountsDown(t *testing.T) {
	h := newHarness(t)
	h.press(special(tea.KeyEnter))

	if got := h.model.cooldownLeft(h.clock.now()); got != testCooldown {
		t.Errorf("cooldown = %s, want %s", got, testCooldown)
	}
	if !strings.Contains(h.status(), "15s") {
		t.Errorf("status = %q, want it to show the 15s countdown", h.status())
	}

	h.clock.advance(5 * time.Second)
	h.flush()
	if got := h.model.cooldownLeft(h.clock.now()); got != 10*time.Second {
		t.Errorf("cooldown = %s, want 10s", got)
	}
	if !strings.Contains(h.status(), "10s") {
		t.Errorf("status = %q, want it to show 10s", h.status())
	}

	// Once elapsed, the bar goes back to "ready".
	h.clock.advance(testCooldown)
	h.send(tickMsg(h.clock.now()))
	h.flush()
	if !strings.Contains(h.status(), "ready") {
		t.Errorf("status = %q, want %q", h.status(), "ready")
	}
}

// The client never decides whether a placement is allowed; it only reports what
// the server said.
func TestSecondPlaceInCooldownIsRefused(t *testing.T) {
	h := newHarness(t)
	h.press(special(tea.KeyEnter))
	h.press(special(tea.KeyRight))
	x, y := h.model.curX, h.model.curY

	h.press(special(tea.KeyEnter))

	if got, _ := h.app.Canvas.At(x, y); got.Drawn() {
		t.Errorf("cell = %+v, want blank: the placement should have been refused", got)
	}
	if !strings.Contains(h.status(), "hold on") {
		t.Errorf("status = %q, want the cooldown message", h.status())
	}

	h.clock.advance(testCooldown)
	h.send(tickMsg(h.clock.now()))
	h.press(special(tea.KeyEnter))
	if got, _ := h.app.Canvas.At(x, y); !got.Drawn() {
		t.Error("placement after the cooldown did not land")
	}
}

// A session that reconnects mid-cooldown must show the remaining time rather
// than claiming to be ready.
func TestNewModelShowsInheritedCooldown(t *testing.T) {
	h := newHarness(t)
	h.press(special(tea.KeyEnter))

	reconnected := New(Config{
		App:      h.app,
		Session:  h.sess,
		Renderer: newRenderer(termenv.Ascii),
		Exit:     &Exit{},
		Now:      h.clock.now,
	})
	if got := reconnected.cooldownLeft(h.clock.now()); got != testCooldown {
		t.Errorf("cooldown on a fresh model = %s, want %s", got, testCooldown)
	}
}

// --- rendering ---

func TestFrameShowsCanvasContents(t *testing.T) {
	h := newHarness(t)
	if err := h.app.Canvas.Set(0, 0, 'Q', 1); err != nil {
		t.Fatal(err)
	}
	h.flush()

	rows := h.grid()
	if len(rows) == 0 {
		t.Fatal("no grid rows rendered")
	}
	if !strings.HasPrefix(rows[0], "Q") {
		t.Errorf("first row = %q, want it to start with 'Q'", rows[0])
	}
}

func TestFrameGeometryMatchesTerminal(t *testing.T) {
	h := newHarness(t)
	// An 80x24 terminal against a 40x12 canvas: everything fits, so the frame
	// is visible on all four sides.
	if got, want := len(h.frameRows()), h.app.Canvas.Height()+2; got != want {
		t.Errorf("rendered %d rows, want %d (canvas plus a border ring)", got, want)
	}
	if got, want := len(h.grid()), h.app.Canvas.Height(); got != want {
		t.Errorf("%d canvas rows inside the frame, want %d", got, want)
	}
	for i, row := range h.grid() {
		if got, want := len([]rune(row)), h.app.Canvas.Width(); got != want {
			t.Errorf("canvas row %d is %d columns, want %d", i, got, want)
		}
	}
}

func TestStatusBarContents(t *testing.T) {
	h := newHarness(t)
	h.press(runes('*'))
	h.press(runes('9'))
	h.flush()

	status := h.status()
	for _, want := range []string{"ssh.place", "20,6", "*", canvas.Palette[9].Name, "1 online"} {
		if !strings.Contains(status, want) {
			t.Errorf("status = %q, want it to contain %q", status, want)
		}
	}
}

func TestStatusBarTracksOnlineCount(t *testing.T) {
	h := newHarness(t)
	other, err := h.app.Hub.Add("3.3.3.3", "key-c", true)
	if err != nil {
		t.Fatal(err)
	}
	h.flush()
	if !strings.Contains(h.status(), "2 online") {
		t.Errorf("status = %q, want %q", h.status(), "2 online")
	}

	h.app.Hub.Remove(other)
	h.flush()
	if !strings.Contains(h.status(), "1 online") {
		t.Errorf("status = %q, want %q", h.status(), "1 online")
	}
}

func TestHelpBarListsControls(t *testing.T) {
	h := newHarness(t)
	help := h.help()
	for _, want := range []string{"wasd", "0-9", "place", "quit"} {
		if !strings.Contains(help, want) {
			t.Errorf("help = %q, want it to mention %q", help, want)
		}
	}
}

func TestChromeFitsNarrowTerminal(t *testing.T) {
	h := newHarness(t)
	h.send(tea.WindowSizeMsg{Width: 30, Height: 10})
	h.flush()

	for _, line := range strings.Split(stripANSI(h.frame()), "\n") {
		if got := lipgloss.Width(line); got > 30 {
			t.Errorf("line %q is %d columns wide, want at most 30", line, got)
		}
	}
}

func TestTinyTerminalGetsAMessage(t *testing.T) {
	h := newHarness(t)
	h.send(tea.WindowSizeMsg{Width: 10, Height: 3})
	if !strings.Contains(h.frame(), "needs at least") {
		t.Errorf("frame = %q, want a size warning", h.frame())
	}
	// It must recover when the terminal grows again.
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	if strings.Contains(h.frame(), "needs at least") {
		t.Error("size warning persisted after the terminal grew")
	}
}

// Resizing repeatedly, including to degenerate sizes, must never panic.
func TestResizeStorm(t *testing.T) {
	h := newHarness(t)
	sizes := [][2]int{{80, 24}, {1, 1}, {0, 0}, {200, 60}, {24, 5}, {300, 100}, {40, 12}, {-1, -1}, {80, 24}}
	for i := 0; i < 3; i++ {
		for _, s := range sizes {
			h.send(tea.WindowSizeMsg{Width: s[0], Height: s[1]})
			h.send(special(tea.KeyRight))
			h.send(special(tea.KeyDown))
		}
	}
}

// --- viewport panning ---

func TestViewportFollowsCursorAtEdges(t *testing.T) {
	h := newHarness(t)
	// Narrower than the canvas, so panning is required, but still wide enough
	// for the TUI to draw at all.
	h.send(tea.WindowSizeMsg{Width: 30, Height: 10})
	h.press(special(tea.KeyHome))
	h.flush()
	// -1 rather than 0: at the left edge the frame's own column is on screen.
	if h.model.offX != -1 {
		t.Fatalf("offX = %d at the left edge, want -1", h.model.offX)
	}

	// Walk right to the far edge; the viewport has to come along.
	for i := 0; i < h.app.Canvas.Width(); i++ {
		h.send(special(tea.KeyRight))
	}
	h.flush()

	viewW := h.model.gridWidth()
	// The far edge scrolls one further than the canvas so the right-hand frame
	// is visible once the cursor reaches the last column.
	if want := h.app.Canvas.Width() + 1 - viewW; h.model.offX != want {
		t.Errorf("offX = %d at the right edge, want %d", h.model.offX, want)
	}
	// The cursor must still be inside the viewport.
	if h.model.curX < h.model.offX || h.model.curX >= h.model.offX+viewW {
		t.Errorf("cursor %d outside viewport [%d,%d)", h.model.curX, h.model.offX, h.model.offX+viewW)
	}
}

func TestViewportStaysAtZeroWhenCanvasFits(t *testing.T) {
	h := newHarness(t)
	// 80x24 terminal against a 40x12 canvas: everything fits, frame included, so
	// the viewport parks on the frame's top-left corner and stays there.
	for i := 0; i < 50; i++ {
		h.send(special(tea.KeyRight))
		h.send(special(tea.KeyDown))
	}
	h.flush()
	if h.model.offX != -1 || h.model.offY != -1 {
		t.Errorf("viewport at (%d,%d), want (-1,-1) when the canvas and its frame fit",
			h.model.offX, h.model.offY)
	}
}

func TestScrollTo(t *testing.T) {
	cases := []struct {
		name                  string
		off, cur, view, total int
		want                  int
	}{
		{"canvas fits", 0, 30, 100, 50, 0},
		{"already centred", 10, 20, 20, 100, 10},
		{"scroll left to margin", 10, 11, 20, 100, 8},
		{"scroll right to margin", 10, 28, 20, 100, 12},
		{"clamped at left edge", 0, 0, 20, 100, 0},
		{"clamped at right edge", 80, 99, 20, 100, 80},
		{"tiny viewport", 0, 50, 1, 100, 50},
		{"two-wide viewport", 0, 50, 2, 100, 49},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrollTo(tc.off, tc.cur, tc.view, tc.total)
			if got != tc.want {
				t.Errorf("scrollTo(%d,%d,%d,%d) = %d, want %d",
					tc.off, tc.cur, tc.view, tc.total, got, tc.want)
			}
			// Whatever it returns, the cursor must be visible and the offset in
			// range.
			if got < 0 || (tc.total > tc.view && got > tc.total-tc.view) {
				t.Errorf("offset %d out of range for view %d of %d", got, tc.view, tc.total)
			}
			if tc.cur < got || tc.cur >= got+tc.view {
				t.Errorf("cursor %d not visible in [%d,%d)", tc.cur, got, got+tc.view)
			}
		})
	}
}

// --- live updates ---

func TestUpdateFromAnotherSessionAppears(t *testing.T) {
	h := newHarness(t)
	other, err := h.app.Hub.Add("4.4.4.4", "key-d", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.Place(other, 0, 0, canvas.Glyph('Z', 2), h.clock.now()); err != nil {
		t.Fatal(err)
	}

	// Deliver the broadcast the way bubbletea would.
	u := <-h.sess.Updates()
	h.send(canvasMsg(u))
	h.flush()

	if rows := h.grid(); !strings.HasPrefix(rows[0], "Z") {
		t.Errorf("first row = %q, want it to start with 'Z'", rows[0])
	}
}

func TestRecentHighlightFades(t *testing.T) {
	// Highlighting is a color effect, so this one needs a color profile.
	h := newHarnessProfile(t, termenv.ANSI)
	h.send(canvasMsg(hub.Update{X: 0, Y: 0, Cell: canvas.Cell{Rune: 'A', Color: 1}}))
	if err := h.app.Canvas.Set(0, 0, 'A', 1); err != nil {
		t.Fatal(err)
	}
	h.flush()

	fresh := h.frame()
	if !strings.Contains(fresh, "\x1b[") {
		t.Fatal("no escape sequences in a color frame")
	}
	freshSeq := buildSeq(1, bgHlFresh, decorNone)
	if !strings.Contains(fresh, freshSeq) {
		t.Errorf("fresh frame is missing the highlight sequence %q", strings.TrimPrefix(freshSeq, "\x1b"))
	}

	// Midway through, the marker steps down to its dimmer stage.
	h.clock.advance(highlightFresh + 10*time.Millisecond)
	h.send(tickMsg(h.clock.now()))
	h.flush()
	fadeSeq := buildSeq(1, bgHlFade, decorNone)
	if !strings.Contains(h.frame(), fadeSeq) {
		t.Error("faded frame is missing the dimmer highlight stage")
	}

	// And after a second it is gone, along with its bookkeeping.
	h.clock.advance(highlightTotal)
	h.send(tickMsg(h.clock.now()))
	h.flush()
	if strings.Contains(h.frame(), freshSeq) || strings.Contains(h.frame(), fadeSeq) {
		t.Error("highlight still present after it should have faded out")
	}
	if len(h.model.recent) != 0 {
		t.Errorf("recent map holds %d entries, want 0", len(h.model.recent))
	}
}

func TestDroppedUpdatesTriggerFullRedraw(t *testing.T) {
	h := newHarness(t)
	// Simulate the hub coalescing: the canvas moved on but no update arrived.
	if err := h.app.Canvas.Set(0, 0, 'D', 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < updateBufferOverflow; i++ {
		h.app.Hub.Broadcast(hub.Update{X: 0, Y: 0})
	}
	if !h.sess.TakeDirty() {
		t.Skip("hub did not report a drop; buffer is larger than expected")
	}
	// Put the flag back and let the tick notice it.
	h.app.Hub.Broadcast(hub.Update{X: 0, Y: 0})

	h.send(tickMsg(h.clock.now()))
	h.flush()
	if rows := h.grid(); !strings.HasPrefix(rows[0], "D") {
		t.Errorf("first row = %q, want the redraw to pick up 'D'", rows[0])
	}
}

// updateBufferOverflow is comfortably more than the hub's channel buffer.
const updateBufferOverflow = 512

func TestHubCloseQuitsSession(t *testing.T) {
	h := newHarness(t)
	cmd := h.send(hubClosedMsg{})
	if cmd == nil {
		t.Fatal("hubClosedMsg produced no command, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("hubClosedMsg did not quit the program")
	}
	if h.model.View() != "" {
		t.Error("a quitting model still rendered a frame")
	}
}

// --- quitting and idling ---

func TestQuitKeys(t *testing.T) {
	for _, k := range []tea.KeyMsg{runes('q'), special(tea.KeyCtrlC), special(tea.KeyCtrlD)} {
		t.Run(k.String(), func(t *testing.T) {
			h := newHarness(t)
			cmd := h.send(k)
			if cmd == nil {
				t.Fatal("no command returned, want tea.Quit")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Errorf("%q did not quit", k.String())
			}
		})
	}
}

func TestIdleTimeoutDisconnects(t *testing.T) {
	h := newHarness(t)

	// Just short of the timeout: still connected.
	h.clock.advance(30*time.Minute - time.Second)
	cmd := h.send(tickMsg(h.clock.now()))
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatal("quit before the idle timeout elapsed")
		}
	}

	h.clock.advance(2 * time.Second)
	cmd = h.send(tickMsg(h.clock.now()))
	if cmd == nil {
		t.Fatal("no command at the idle timeout, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("idle timeout did not quit")
	}
	if reason := h.exit.Reason(); !strings.Contains(reason, "idle") {
		t.Errorf("exit reason = %q, want it to mention idling", reason)
	}
}

func TestAnyKeyResetsIdleTimer(t *testing.T) {
	h := newHarness(t)
	h.clock.advance(29 * time.Minute)
	h.press(special(tea.KeyRight))

	h.clock.advance(29 * time.Minute)
	cmd := h.send(tickMsg(h.clock.now()))
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Error("session timed out despite recent input")
		}
	}
}

func TestIdleTimeoutDisabled(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.IdleTimeout = 0 })
	h.clock.advance(100 * time.Hour)
	cmd := h.send(tickMsg(h.clock.now()))
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Error("session timed out with the idle timeout disabled")
		}
	}
}

// --- misc ---

func TestUnknownMessagesAreIgnored(t *testing.T) {
	h := newHarness(t)
	before := h.frame()
	if cmd := h.send(struct{ nonsense int }{}); cmd != nil {
		t.Error("an unknown message produced a command")
	}
	if h.frame() != before {
		t.Error("an unknown message changed the frame")
	}
}

func TestUnknownKeysAreIgnored(t *testing.T) {
	h := newHarness(t)
	before := h.frame()
	stamp, color := h.model.stamp, h.model.color
	for _, k := range []tea.KeyMsg{special(tea.KeyF1), special(tea.KeyCtrlA), special(tea.KeyInsert)} {
		if cmd := h.send(k); cmd != nil {
			t.Errorf("%q produced a command", k.String())
		}
	}
	if h.model.stamp != stamp || h.model.color != color {
		t.Error("an unhandled key changed the selection")
	}
	if h.frame() != before {
		t.Error("an unhandled key changed the frame")
	}
}

func TestNoColorProfileEmitsNoEscapes(t *testing.T) {
	h := newHarnessProfile(t, termenv.Ascii)
	if err := h.app.Canvas.Set(1, 1, 'x', 9); err != nil {
		t.Fatal(err)
	}
	h.flush()
	if strings.Contains(h.frame(), "\x1b") {
		t.Error("frame contains escape sequences for a client without color support")
	}
}

func TestFit(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 1, "h"},
		{"hello", 0, ""},
		{"hello", -1, ""},
		{"", 5, ""},
	}
	for _, tc := range cases {
		if got := fit(tc.in, tc.width); got != tc.want {
			t.Errorf("fit(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
	}
}

func TestRoundDur(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, 0},
		{-time.Second, 0},
		{1234 * time.Millisecond, 1200 * time.Millisecond},
		{14*time.Second + 800*time.Millisecond, 15 * time.Second},
		{30 * time.Minute, 30 * time.Minute},
	}
	for _, tc := range cases {
		if got := roundDur(tc.in); got != tc.want {
			t.Errorf("roundDur(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestClamp(t *testing.T) {
	cases := []struct{ v, lo, hi, want int }{
		{5, 0, 10, 5},
		{-1, 0, 10, 0},
		{11, 0, 10, 10},
		{5, 10, 0, 10}, // inverted range collapses to lo
	}
	for _, tc := range cases {
		if got := clamp(tc.v, tc.lo, tc.hi); got != tc.want {
			t.Errorf("clamp(%d,%d,%d) = %d, want %d", tc.v, tc.lo, tc.hi, got, tc.want)
		}
	}
}

func TestExitIsNilSafe(t *testing.T) {
	var e *Exit
	e.Set("ignored")
	if got := e.Reason(); got != "" {
		t.Errorf("Reason on a nil Exit = %q, want empty", got)
	}
}

func TestPainterWithoutColorPassesTextThrough(t *testing.T) {
	p := newPainter(false)
	var b strings.Builder
	p.paint(&b, styleKey{fg: 9, bg: bgHlFresh, decor: decorCsr}, "abc")
	if got := b.String(); got != "abc" {
		t.Errorf("paint = %q, want %q", got, "abc")
	}
}

func TestPainterEmitsCursorAndHighlight(t *testing.T) {
	p := newPainter(true)
	var b strings.Builder
	p.paint(&b, styleKey{fg: 15, decor: decorCsr}, "x")
	out := b.String()
	if !strings.Contains(out, "7") || !strings.HasSuffix(out, reset) {
		t.Errorf("cursor cell = %q, want reverse video and a reset", out)
	}
}

// Every palette entry must produce a distinct sequence, or colors would be
// indistinguishable on the canvas.
func TestPainterSequencesAreDistinct(t *testing.T) {
	p := newPainter(true)
	seen := map[string]int{}
	for i := 0; i < canvas.PaletteSize; i++ {
		seq := p.seqs[i][bgNone][decorNone]
		if prev, dup := seen[seq]; dup {
			t.Errorf("palette entries %d and %d share the sequence %q", prev, i, seq)
		}
		seen[seq] = i
	}
}
