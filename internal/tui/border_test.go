package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"
)

func TestBorderAt(t *testing.T) {
	const w, h = 10, 4
	cases := []struct {
		name string
		x, y int
		want byte // 0 means "not on the border"
	}{
		{"top-left corner", -1, -1, borderCorner},
		{"top-right corner", w, -1, borderCorner},
		{"bottom-left corner", -1, h, borderCorner},
		{"bottom-right corner", w, h, borderCorner},
		{"top edge", 3, -1, borderHorz},
		{"bottom edge", 3, h, borderHorz},
		{"left edge", -1, 2, borderVert},
		{"right edge", w, 2, borderVert},
		{"inside top-left", 0, 0, 0},
		{"inside bottom-right", w - 1, h - 1, 0},
		{"middle", 5, 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := borderAt(tc.x, tc.y, w, h)
			if tc.want == 0 {
				if ok {
					t.Errorf("borderAt(%d,%d) = %q, want no border", tc.x, tc.y, got)
				}
				return
			}
			if !ok {
				t.Fatalf("borderAt(%d,%d) reported no border, want %q", tc.x, tc.y, tc.want)
			}
			if got != tc.want {
				t.Errorf("borderAt(%d,%d) = %q, want %q", tc.x, tc.y, got, tc.want)
			}
		})
	}
}

// The whole point is being able to see where the canvas ends, so on a terminal
// big enough to show it, the frame has to be complete on all four sides.
func TestFrameSurroundsTheCanvasWhenItFits(t *testing.T) {
	h := newHarness(t) // 80x24 terminal, 40x12 canvas
	rows := h.frameRows()

	cw := h.app.Canvas.Width()
	if got, want := len(rows), h.app.Canvas.Height()+2; got != want {
		t.Fatalf("got %d rows, want %d", got, want)
	}

	top, bottom := rows[0], rows[len(rows)-1]
	want := string(rune(borderCorner)) + strings.Repeat(string(rune(borderHorz)), cw) + string(rune(borderCorner))
	if top != want {
		t.Errorf("top row    = %q\nwant         %q", top, want)
	}
	if bottom != want {
		t.Errorf("bottom row = %q\nwant         %q", bottom, want)
	}

	// Every row between them is walled on both sides.
	for i, row := range rows[1 : len(rows)-1] {
		cells := []rune(row)
		if len(cells) != cw+2 {
			t.Fatalf("row %d is %d columns, want %d", i+1, len(cells), cw+2)
		}
		if cells[0] != borderVert || cells[len(cells)-1] != borderVert {
			t.Errorf("row %d = %q, want %q at both ends", i+1, row, rune(borderVert))
		}
	}
}

// A frame character must appear at exactly the border positions and nowhere
// else, whatever the terminal size or where the viewport has been panned to.
// The canvas is left empty so any '+', '|' or '-' on screen has to be frame.
func TestFrameCharactersAppearOnlyOnTheBorder(t *testing.T) {
	h := newHarness(t)

	for _, size := range [][2]int{{80, 24}, {30, 10}, {42, 14}, {24, 8}, {200, 60}} {
		h.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})

		for _, pos := range [][2]int{{0, 0}, {20, 6}, {39, 11}, {10, 3}, {0, 11}, {39, 0}} {
			h.model.curX, h.model.curY = pos[0], pos[1]
			h.flush()

			for row, line := range h.frameRows() {
				y := h.model.offY + row
				for col, r := range []rune(line) {
					x := h.model.offX + col
					_, onBorder := borderAt(x, y, h.model.canvasW, h.model.canvasH)
					isFrameChar := r == borderCorner || r == borderVert || r == borderHorz
					if onBorder != isFrameChar {
						t.Fatalf("term %dx%d cursor (%d,%d): cell (%d,%d) drew %q, onBorder=%v",
							size[0], size[1], pos[0], pos[1], x, y, r, onBorder)
					}
				}
			}
		}
	}
}

func TestFrameComesIntoViewAtEachEdge(t *testing.T) {
	h := newHarness(t)
	// Smaller than the canvas in both directions, so each edge has to be
	// reached by panning.
	h.send(tea.WindowSizeMsg{Width: 30, Height: 10})

	// Left and top.
	h.press(special(tea.KeyHome))
	for i := 0; i < h.app.Canvas.Height(); i++ {
		h.send(special(tea.KeyUp))
	}
	h.flush()
	rows := h.frameRows()
	if !strings.HasPrefix(rows[0], string(rune(borderCorner))) {
		t.Errorf("top-left corner missing: first row = %q", rows[0])
	}
	for i, row := range rows[1:] {
		if !strings.HasPrefix(row, string(rune(borderVert))) {
			t.Errorf("row %d does not start with the left wall: %q", i+1, row)
			break
		}
	}

	// Right and bottom.
	for i := 0; i < h.app.Canvas.Width(); i++ {
		h.send(special(tea.KeyRight))
	}
	for i := 0; i < h.app.Canvas.Height(); i++ {
		h.send(special(tea.KeyDown))
	}
	h.flush()
	rows = h.frameRows()
	last := rows[len(rows)-1]
	if !strings.HasSuffix(last, string(rune(borderCorner))) {
		t.Errorf("bottom-right corner missing: last row = %q", last)
	}
	for i, row := range rows[:len(rows)-1] {
		if !strings.HasSuffix(row, string(rune(borderVert))) {
			t.Errorf("row %d does not end with the right wall: %q", i, row)
			break
		}
	}
}

// The frame is chrome, so it must not be mistaken for canvas content or steal
// the cursor.
func TestFrameIsDrawnAsChrome(t *testing.T) {
	h := newHarnessProfile(t, termenv.ANSI)
	h.flush()

	want := buildSeq(borderColor, bgNone, decorNone)
	if !strings.Contains(h.frame(), want) {
		t.Errorf("frame is not drawn in the border color %q", strings.TrimPrefix(want, "\x1b"))
	}
}

// The cursor is clamped to the canvas, so it can never sit on the frame.
func TestCursorNeverReachesTheFrame(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < h.app.Canvas.Width()+10; i++ {
		h.send(special(tea.KeyLeft))
		h.send(special(tea.KeyUp))
	}
	if h.model.curX < 0 || h.model.curY < 0 {
		t.Errorf("cursor at (%d,%d), want it inside the canvas", h.model.curX, h.model.curY)
	}
	for i := 0; i < h.app.Canvas.Width()+10; i++ {
		h.send(special(tea.KeyRight))
		h.send(special(tea.KeyDown))
	}
	if h.model.curX >= h.app.Canvas.Width() || h.model.curY >= h.app.Canvas.Height() {
		t.Errorf("cursor at (%d,%d), want it inside the canvas", h.model.curX, h.model.curY)
	}
}

// --- wasd ---

func TestWASDMovesCursor(t *testing.T) {
	cases := []struct {
		key    rune
		dx, dy int
	}{
		{'w', 0, -1},
		{'s', 0, 1},
		{'a', -1, 0},
		{'d', 1, 0},
	}
	for _, tc := range cases {
		t.Run(string(tc.key), func(t *testing.T) {
			h := newHarness(t)
			x0, y0 := h.model.curX, h.model.curY
			h.press(runes(tc.key))
			if got, want := h.model.curX, x0+tc.dx; got != want {
				t.Errorf("curX = %d, want %d", got, want)
			}
			if got, want := h.model.curY, y0+tc.dy; got != want {
				t.Errorf("curY = %d, want %d", got, want)
			}
		})
	}
}

// wasd must not double as stamps, the same way hjkl does not.
func TestWASDDoesNotChangeStamp(t *testing.T) {
	h := newHarness(t)
	want := h.model.stamp
	for _, r := range []rune{'w', 'a', 's', 'd'} {
		h.press(runes(r))
		if h.model.stamp != want {
			t.Errorf("%q became the stamp", r)
		}
		if !h.model.block {
			t.Errorf("%q left block mode", r)
		}
	}
}

// They still have to be drawable on a canvas that allows characters.
func TestWASDCanStillBeStampedLiterally(t *testing.T) {
	for _, r := range []rune{'w', 'a', 's', 'd'} {
		t.Run(string(r), func(t *testing.T) {
			h := newHarness(t)
			x0, y0 := h.model.curX, h.model.curY
			h.press(runes('\\'))
			h.press(runes(r))
			if h.model.stamp != r {
				t.Errorf("stamp = %q, want %q", h.model.stamp, r)
			}
			if h.model.curX != x0 || h.model.curY != y0 {
				t.Error("the literal key also moved the cursor")
			}
		})
	}
}

func TestWASDWorksOnABlocksOnlyCanvas(t *testing.T) {
	h := newHarness(t, blocksOnly)
	x0 := h.model.curX
	h.press(runes('d'))
	if h.model.curX != x0+1 {
		t.Errorf("curX = %d, want %d: wasd must still move", h.model.curX, x0+1)
	}
	if strings.Contains(h.status(), "blocks only") {
		t.Error("a movement key was reported as a rejected character")
	}
}

// --- page movement ---

// Crossing a wide canvas one cell at a time is impractical, so both axes need a
// page key. Horizontal is the one that matters: the viewport is far wider than
// it is tall.
func TestPageKeysMoveAViewportAtATime(t *testing.T) {
	cases := []struct {
		key    tea.KeyMsg
		dx, dy int
	}{
		{special(tea.KeyShiftRight), 1, 0},
		{special(tea.KeyShiftLeft), -1, 0},
		{special(tea.KeyShiftDown), 0, 1},
		{special(tea.KeyShiftUp), 0, -1},
		{special(tea.KeyCtrlRight), 1, 0},
		{special(tea.KeyCtrlLeft), -1, 0},
		{special(tea.KeyCtrlDown), 0, 1},
		{special(tea.KeyCtrlUp), 0, -1},
		{special(tea.KeyPgDown), 0, 1},
		{special(tea.KeyPgUp), 0, -1},
	}
	for _, tc := range cases {
		t.Run(tc.key.String(), func(t *testing.T) {
			h := newHarness(t)
			// A canvas big enough that a page in any direction stays in bounds.
			h.model.canvasW, h.model.canvasH = 200, 60
			h.model.curX, h.model.curY = 100, 30
			h.flush()

			x0, y0 := h.model.curX, h.model.curY
			wantX := x0 + tc.dx*h.model.gridWidth()
			wantY := y0 + tc.dy*h.model.gridHeight()

			h.press(tc.key)
			if h.model.curX != wantX || h.model.curY != wantY {
				t.Errorf("%s moved to (%d,%d), want (%d,%d)",
					tc.key.String(), h.model.curX, h.model.curY, wantX, wantY)
			}
		})
	}
}

// A page key near an edge must clamp rather than run off the canvas.
func TestPageKeysClampAtEdges(t *testing.T) {
	h := newHarness(t)
	h.press(special(tea.KeyShiftLeft))
	h.press(special(tea.KeyShiftUp))
	if h.model.curX != 0 || h.model.curY != 0 {
		t.Errorf("cursor at (%d,%d), want (0,0)", h.model.curX, h.model.curY)
	}
	for i := 0; i < 10; i++ {
		h.press(special(tea.KeyShiftRight))
		h.press(special(tea.KeyShiftDown))
	}
	w, hh := h.app.Canvas.Size()
	if h.model.curX != w-1 || h.model.curY != hh-1 {
		t.Errorf("cursor at (%d,%d), want (%d,%d)", h.model.curX, h.model.curY, w-1, hh-1)
	}
}

// Paging horizontally has to actually pan the viewport, which is the whole point.
func TestHorizontalPagingPansTheViewport(t *testing.T) {
	h := newHarness(t)
	h.model.canvasW, h.model.canvasH = 200, 60
	h.model.curX, h.model.curY = 0, 0
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	h.flush()

	start := h.model.offX
	h.press(special(tea.KeyShiftRight))
	if h.model.offX <= start {
		t.Errorf("offX stayed at %d after paging right, want it to advance from %d",
			h.model.offX, start)
	}

	// And paging back returns to the left frame.
	for i := 0; i < 5; i++ {
		h.press(special(tea.KeyShiftLeft))
	}
	if h.model.offX != -1 {
		t.Errorf("offX = %d after paging back, want -1", h.model.offX)
	}
}

// --- mouse ---

func wheel(b tea.MouseButton) tea.MouseMsg {
	return tea.MouseMsg{Action: tea.MouseActionPress, Button: b}
}

func TestWheelPansTheCanvas(t *testing.T) {
	cases := []struct {
		name   string
		button tea.MouseButton
		dx, dy int
	}{
		{"up", tea.MouseButtonWheelUp, 0, -mouseScrollStep},
		{"down", tea.MouseButtonWheelDown, 0, mouseScrollStep},
		{"left", tea.MouseButtonWheelLeft, -mouseScrollStep, 0},
		{"right", tea.MouseButtonWheelRight, mouseScrollStep, 0},
		// listed once per direction; the loop sends a full step's worth of events
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.model.canvasW, h.model.canvasH = 200, 60
			h.model.curX, h.model.curY = 100, 30
			h.flush()

			x0, y0 := h.model.curX, h.model.curY
			// One notch is deliberately not enough to move.
			h.send(wheel(tc.button))
			if h.model.curX != x0 || h.model.curY != y0 {
				t.Errorf("a single wheel notch moved the cursor; scrolling should be paced")
			}
			for i := 1; i < scrollEventsPerCell; i++ {
				h.send(wheel(tc.button))
			}
			h.flush()

			if got, want := h.model.curX, x0+tc.dx; got != want {
				t.Errorf("curX = %d, want %d", got, want)
			}
			if got, want := h.model.curY, y0+tc.dy; got != want {
				t.Errorf("curY = %d, want %d", got, want)
			}
		})
	}
}

// Scrolling past an edge must clamp, not run off the canvas.
func TestWheelClampsAtEdges(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 40*scrollEventsPerCell; i++ {
		h.send(wheel(tea.MouseButtonWheelUp))
		h.send(wheel(tea.MouseButtonWheelLeft))
	}
	if h.model.curX != 0 || h.model.curY != 0 {
		t.Errorf("cursor at (%d,%d), want (0,0)", h.model.curX, h.model.curY)
	}
	for i := 0; i < 200*scrollEventsPerCell; i++ {
		h.send(wheel(tea.MouseButtonWheelDown))
		h.send(wheel(tea.MouseButtonWheelRight))
	}
	w, hh := h.app.Canvas.Size()
	if h.model.curX != w-1 || h.model.curY != hh-1 {
		t.Errorf("cursor at (%d,%d), want (%d,%d)", h.model.curX, h.model.curY, w-1, hh-1)
	}
}

// Wheeling far enough has to actually pan the viewport, not just nudge the
// cursor inside it.
func TestWheelEventuallyPansTheViewport(t *testing.T) {
	h := newHarness(t)
	h.model.canvasW, h.model.canvasH = 200, 60
	h.model.curX, h.model.curY = 0, 0
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	h.flush()

	start := h.model.offY
	for i := 0; i < 40*scrollEventsPerCell; i++ {
		h.send(wheel(tea.MouseButtonWheelDown))
	}
	h.flush()
	if h.model.offY <= start {
		t.Errorf("offY = %d after scrolling down, want it past %d", h.model.offY, start)
	}
}

func TestClickMovesTheCursor(t *testing.T) {
	h := newHarness(t)
	h.flush()

	// The grid starts with the border ring, so screen (5,3) is canvas (4,2).
	h.send(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 5, Y: 3})
	h.flush()

	wantX, wantY := h.model.offX+5, h.model.offY+3
	if h.model.curX != wantX || h.model.curY != wantY {
		t.Errorf("cursor at (%d,%d), want (%d,%d)", h.model.curX, h.model.curY, wantX, wantY)
	}
}

// A click must not place: a stray one would cost the player a full cooldown.
func TestClickDoesNotPlace(t *testing.T) {
	h := newHarness(t)
	h.send(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 5, Y: 3})
	h.flush()
	if got := h.app.Canvas.NonEmpty(); got != 0 {
		t.Errorf("a click placed %d cells, want 0", got)
	}
	if h.model.cooldownLeft(h.clock.now()) != 0 {
		t.Error("a click started the cooldown")
	}
}

// Clicks on the chrome and on the border ring are not canvas positions.
func TestClickOutsideTheCanvasIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.flush()
	x0, y0 := h.model.curX, h.model.curY

	for _, p := range [][2]int{
		{0, 0},   // the border corner
		{5, 0},   // the top wall
		{0, 3},   // the left wall
		{5, 22},  // the status bar
		{5, 23},  // the help bar
		{-1, -1}, // nonsense
	} {
		h.send(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: p[0], Y: p[1]})
		if h.model.curX != x0 || h.model.curY != y0 {
			t.Errorf("click at (%d,%d) moved the cursor to (%d,%d)", p[0], p[1], h.model.curX, h.model.curY)
			h.model.curX, h.model.curY = x0, y0
		}
	}
}

// Releases and drags must not warp the cursor; only a press does.
func TestNonPressMouseEventsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.flush()
	x0, y0 := h.model.curX, h.model.curY

	for _, action := range []tea.MouseAction{tea.MouseActionRelease, tea.MouseActionMotion} {
		h.send(tea.MouseMsg{Action: action, Button: tea.MouseButtonLeft, X: 5, Y: 3})
		if h.model.curX != x0 || h.model.curY != y0 {
			t.Errorf("mouse action %v moved the cursor", action)
		}
	}
	// A right click is not a command either.
	h.send(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonRight, X: 5, Y: 3})
	if h.model.curX != x0 || h.model.curY != y0 {
		t.Error("a right click moved the cursor")
	}
}

// Scrolling counts as activity, or a player panning around would be timed out.
func TestMouseResetsIdleTimer(t *testing.T) {
	h := newHarness(t)
	h.clock.advance(29 * time.Minute)
	// Even a swallowed notch counts as activity.
	h.send(wheel(tea.MouseButtonWheelDown))

	h.clock.advance(29 * time.Minute)
	if cmd := h.send(tickMsg(h.clock.now())); cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Error("session timed out despite recent scrolling")
		}
	}
}

// Pin the scroll ratio, so a change to the pacing constants is a deliberate act
// rather than a surprise.
func TestScrollPacingRatio(t *testing.T) {
	h := newHarness(t)
	h.model.canvasW, h.model.canvasH = 200, 60
	h.model.curX, h.model.curY = 100, 30
	h.flush()

	const notches = 20
	y0 := h.model.curY
	for i := 0; i < notches; i++ {
		h.send(wheel(tea.MouseButtonWheelDown))
	}

	want := notches / scrollEventsPerCell * mouseScrollStep
	if got := h.model.curY - y0; got != want {
		t.Errorf("%d wheel notches moved %d cells, want %d", notches, got, want)
	}
	// A flick must not cross a large fraction of the canvas.
	if got := h.model.curY - y0; got > h.model.canvasH/4 {
		t.Errorf("%d notches travelled %d cells, more than a quarter of the canvas height", notches, got)
	}
}

// Partial notches must not accumulate into a jump when direction reverses.
func TestScrollReversalDoesNotJump(t *testing.T) {
	h := newHarness(t)
	h.model.curX, h.model.curY = 20, 6
	h.flush()
	y0 := h.model.curY

	// One notch down (swallowed), then one up: net zero, not a lurch.
	h.send(wheel(tea.MouseButtonWheelDown))
	h.send(wheel(tea.MouseButtonWheelUp))
	if h.model.curY != y0 {
		t.Errorf("cursor at %d after down-then-up, want %d", h.model.curY, y0)
	}
}
