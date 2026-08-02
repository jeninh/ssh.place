package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"github.com/jeninh/ssh.place/internal/canvas"
)

// blocksOnly configures a canvas that refuses characters outright.
func blocksOnly(c *Config) { c.BlocksOnly = true }

// Everyone starts painting blocks, whether or not characters are available.
func TestSessionsStartInBlockMode(t *testing.T) {
	if h := newHarness(t); !h.model.block {
		t.Error("a mixed session did not start in block mode")
	}
	if h := newHarness(t, blocksOnly); !h.model.block {
		t.Error("a blocks-only session did not start in block mode")
	}
}

func TestCtrlBTogglesBlockMode(t *testing.T) {
	h := newHarness(t)
	if !h.model.block {
		t.Fatal("sessions should start in block mode")
	}

	h.press(tea.KeyMsg{Type: tea.KeyCtrlB})
	if h.model.block {
		t.Fatal("ctrl+b did not switch to characters")
	}
	h.press(tea.KeyMsg{Type: tea.KeyCtrlB})
	if !h.model.block {
		t.Error("ctrl+b did not switch back to blocks")
	}
}

func TestBlockModePlacesASolidBlock(t *testing.T) {
	h := newHarness(t)
	h.press(runes('7')) // olive
	x, y := h.model.curX, h.model.curY

	h.press(special(tea.KeyEnter))

	got, ok := h.app.Canvas.At(x, y)
	if !ok {
		t.Fatalf("cell (%d,%d) out of bounds", x, y)
	}
	if !got.IsBlock() {
		t.Fatalf("cell = %+v, want a block", got)
	}
	if got.Fill != 7 {
		t.Errorf("block fill = %d, want 7", got.Fill)
	}
	if got.Rune != canvas.Empty {
		t.Errorf("block carries the character %q, want a space", got.Rune)
	}
}

// In block mode the stamp character is irrelevant, so the block must not pick
// it up.
func TestBlockModeIgnoresTheStampCharacter(t *testing.T) {
	h := newHarness(t)
	h.press(runes('Z'))
	h.press(tea.KeyMsg{Type: tea.KeyCtrlB})
	h.press(special(tea.KeyEnter))

	got, _ := h.app.Canvas.At(h.model.curX, h.model.curY)
	if got.Rune != canvas.Empty {
		t.Errorf("cell = %q, want a space: block mode should ignore the stamp", got.Rune)
	}
	// The character is remembered for when they switch back.
	if h.model.stamp != 'Z' {
		t.Errorf("stamp = %q, want it remembered as 'Z'", h.model.stamp)
	}
}

// Reaching for a character is a request to draw it, so it should leave block
// mode rather than silently doing nothing.
func TestPickingACharacterLeavesBlockMode(t *testing.T) {
	h := newHarness(t)
	h.press(runes('%'))

	if h.model.block {
		t.Error("still in block mode after picking a character")
	}
	if h.model.stamp != '%' {
		t.Errorf("stamp = %q, want '%%'", h.model.stamp)
	}

	h.press(special(tea.KeyEnter))
	got, _ := h.app.Canvas.At(h.model.curX, h.model.curY)
	if got.IsBlock() || got.Rune != '%' {
		t.Errorf("cell = %+v, want the character '%%'", got)
	}
}

// The literal prefix is a character request too.
func TestLiteralPrefixLeavesBlockMode(t *testing.T) {
	h := newHarness(t)
	h.press(runes('\\'))
	h.press(runes('h'))

	if h.model.block {
		t.Error("still in block mode after a literal character")
	}
	if h.model.stamp != 'h' {
		t.Errorf("stamp = %q, want 'h'", h.model.stamp)
	}
}

// Colors and movement are shared between the modes.
func TestBlockModeKeepsColorAndMovementKeys(t *testing.T) {
	h := newHarness(t)

	h.press(runes('3'))
	if h.model.color != 3 {
		t.Errorf("color = %d, want 3", h.model.color)
	}
	if !h.model.block {
		t.Error("picking a color left block mode")
	}

	h.press(special(tea.KeyTab))
	if h.model.color != 4 {
		t.Errorf("color = %d after tab, want 4", h.model.color)
	}
	if !h.model.block {
		t.Error("cycling color left block mode")
	}

	x0 := h.model.curX
	h.press(runes('l'))
	if h.model.curX != x0+1 {
		t.Error("hjkl stopped moving the cursor in block mode")
	}
	if !h.model.block {
		t.Error("moving left block mode")
	}
}

func TestStatusBarShowsTheMode(t *testing.T) {
	h := newHarness(t)
	h.press(runes('9'))
	if got := h.status(); !strings.Contains(got, "███") {
		t.Errorf("status = %q, want a solid swatch in block mode", got)
	}
	if got := h.status(); !strings.Contains(got, canvas.Palette[9].Name) {
		t.Errorf("status = %q, want the color name", got)
	}

	h.press(tea.KeyMsg{Type: tea.KeyCtrlB})
	if got := h.status(); strings.Contains(got, "███") {
		t.Errorf("status = %q, want the character stamp shown instead", got)
	}
}

func TestHelpBarIsModeAware(t *testing.T) {
	h := newHarness(t)
	if got := h.help(); !strings.Contains(got, "^B") {
		t.Errorf("block-mode help = %q, want it to mention ^B", got)
	}
	if got := h.help(); !strings.Contains(got, "letters") {
		t.Errorf("block-mode help = %q, want it to offer letters", got)
	}

	h.press(tea.KeyMsg{Type: tea.KeyCtrlB})
	if got := h.help(); !strings.Contains(got, "block") {
		t.Errorf("character-mode help = %q, want it to offer blocks", got)
	}

	// Whatever the mode or the width, the hint has to fit and keep "q quit".
	for _, width := range []int{24, 40, 60, 80, 100, 200} {
		for _, mode := range []struct {
			name              string
			block, blocksOnly bool
		}{
			{"block", true, false},
			{"character", false, false},
			{"blocks-only", true, true},
		} {
			h.model.block, h.model.blocksOnly = mode.block, mode.blocksOnly
			got := h.model.helpBar(width)
			if w := len([]rune(stripANSI(got))); w > width {
				t.Errorf("%s help at width %d is %d columns: %q", mode.name, width, w, got)
			}
			if !strings.Contains(stripANSI(got), "quit") {
				t.Errorf("%s help at width %d lost the quit hint: %q", mode.name, width, got)
			}
		}
	}
}

// A blocks-only canvas must not advertise a character mode that does not exist.
func TestBlocksOnlyHelpNeverMentionsCharacters(t *testing.T) {
	h := newHarness(t, blocksOnly)
	for _, width := range []int{24, 40, 80, 120, 200} {
		got := stripANSI(h.model.helpBar(width))
		for _, unwanted := range []string{"^B", "letters", "stamp", "literal"} {
			if strings.Contains(got, unwanted) {
				t.Errorf("blocks-only help at width %d offers %q: %q", width, unwanted, got)
			}
		}
		if !strings.Contains(got, "color") && width >= 40 {
			t.Errorf("blocks-only help at width %d does not mention colors: %q", width, got)
		}
	}
}

// A block is a background color, so it has to appear as one in the frame.
func TestBlockRendersAsABackground(t *testing.T) {
	h := newHarnessProfile(t, termenv.ANSI)
	if err := h.app.Canvas.Fill(0, 0, 9); err != nil { // red
		t.Fatal(err)
	}
	h.flush()

	want := buildSeq(9, bgOfFill(9), decorNone)
	if !strings.Contains(h.frame(), want) {
		t.Errorf("frame is missing the block background sequence %q", strings.TrimPrefix(want, "\x1b"))
	}
	// The cell itself is blank; the color does the drawing.
	if rows := h.grid(); !strings.HasPrefix(rows[0], " ") {
		t.Errorf("first row = %q, want it to start with a space", rows[0])
	}
}

// Without color support a background is invisible, so a block needs a visible
// stand-in or the canvas would look empty.
func TestBlockFallsBackToACharacterWithoutColor(t *testing.T) {
	h := newHarnessProfile(t, termenv.Ascii)
	if err := h.app.Canvas.Fill(0, 0, 9); err != nil {
		t.Fatal(err)
	}
	h.flush()

	rows := h.grid()
	if !strings.HasPrefix(rows[0], string(rune(blockFallback))) {
		t.Errorf("first row = %q, want it to start with %q", rows[0], rune(blockFallback))
	}
	if strings.Contains(h.frame(), "\x1b") {
		t.Error("frame contains escape sequences for a client without color support")
	}
}

// The zero value of a styleKey has to mean "no background", not black.
func TestZeroStyleKeyHasNoBackground(t *testing.T) {
	if bgNone != 0 {
		t.Fatalf("bgNone = %d, want 0 so the zero styleKey is a plain cell", bgNone)
	}
	var k styleKey
	if k.bg != bgNone {
		t.Errorf("zero styleKey bg = %d, want bgNone", k.bg)
	}
	// A black block must be distinguishable from no background at all.
	if bgOfFill(0) == bgNone {
		t.Error("a black block is indistinguishable from an unfilled cell")
	}
}

func TestPainterCoversEveryBackgroundState(t *testing.T) {
	p := newPainter(true)
	for fg := 0; fg < canvas.PaletteSize; fg++ {
		for bg := 0; bg < bgStates; bg++ {
			for d := 0; d < decorCount; d++ {
				if p.seqs[fg][bg][d] == "" {
					t.Fatalf("no sequence for fg=%d bg=%d decor=%d", fg, bg, d)
				}
			}
		}
	}
	// Every fill produces a distinct background, so colors stay tellable apart.
	seen := map[string]int{}
	for i := 0; i < canvas.PaletteSize; i++ {
		seq := p.seqs[canvas.DefaultColor][bgOfFill(uint8(i))][decorNone]
		if prev, dup := seen[seq]; dup {
			t.Errorf("fills %d and %d share the sequence %q", prev, i, seq)
		}
		seen[seq] = i
	}
}

// The painter table is shared process-wide, so a session must not be able to
// get a table configured for a different client.
func TestPainterIsSharedPerColorSupport(t *testing.T) {
	if newPainter(true) != newPainter(true) {
		t.Error("color painters are not shared")
	}
	if newPainter(false) != newPainter(false) {
		t.Error("plain painters are not shared")
	}
	if newPainter(true) == newPainter(false) {
		t.Error("color and plain clients share one painter")
	}
}

// Fast typing and pastes arrive as one KeyRunes message carrying several runes.
// Dropping the burst would silently swallow everything the user just typed.
func TestMultiRuneBurstIsHandledKeyByKey(t *testing.T) {
	h := newHarness(t)

	// "3A" is a color pick followed by a stamp pick, delivered as one message.
	h.press(runes('3', 'A'))
	if h.model.color != 3 {
		t.Errorf("color = %d, want 3", h.model.color)
	}
	if h.model.stamp != 'A' {
		t.Errorf("stamp = %q, want 'A'", h.model.stamp)
	}
}

func TestMultiRuneBurstMovesAndPlaces(t *testing.T) {
	h := newHarness(t)
	x0, y0 := h.model.curX, h.model.curY

	// Two rights and a down, all in one burst.
	h.press(runes('l', 'l', 'j'))
	if h.model.curX != x0+2 || h.model.curY != y0+1 {
		t.Errorf("cursor at (%d,%d), want (%d,%d)", h.model.curX, h.model.curY, x0+2, y0+1)
	}

	// A burst containing a space still places, once.
	h.press(runes('7', ' '))
	got, _ := h.app.Canvas.At(h.model.curX, h.model.curY)
	if !got.IsBlock() || got.Fill != 7 {
		t.Errorf("cell = %+v, want a block filled with 7", got)
	}
}

// A pasted wall of text must not turn into a wall of cells: the server-side
// cooldown still applies to every placement in the burst.
func TestPastedBurstCannotBeatTheCooldown(t *testing.T) {
	h := newHarness(t)

	paste := make([]rune, 0, 40)
	for i := 0; i < 20; i++ {
		paste = append(paste, 'l', ' ') // move right, place
	}
	h.press(runes(paste...))

	if got := h.app.Canvas.NonEmpty(); got != 1 {
		t.Errorf("a pasted burst placed %d cells, want 1", got)
	}
}

func TestMultiRuneBurstCanQuit(t *testing.T) {
	h := newHarness(t)
	cmd := h.send(runes('a', 'q', 'b'))
	if cmd == nil {
		t.Fatal("no command returned, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("a burst containing q did not quit")
	}
	// The stamp is whatever was picked before the quit, not after it.
	if h.model.stamp == 'b' {
		t.Error("keys after q in the burst were still processed")
	}
}

// The literal prefix consumes exactly the next rune of a burst.
func TestLiteralPrefixInsideABurst(t *testing.T) {
	h := newHarness(t)
	h.press(runes('\\', 'q', '5'))
	if h.model.stamp != 'q' {
		t.Errorf("stamp = %q, want 'q' taken literally", h.model.stamp)
	}
	if h.model.color != 5 {
		t.Errorf("color = %d, want 5 from the rune after the literal", h.model.color)
	}
	if h.model.quitting {
		t.Error("the literal q quit the session")
	}
}

// --- blocks-only: characters are refused, not merely hidden ---

func TestBlocksOnlyIgnoresCtrlB(t *testing.T) {
	h := newHarness(t, blocksOnly)

	h.press(tea.KeyMsg{Type: tea.KeyCtrlB})
	if !h.model.block {
		t.Error("ctrl+b left block mode on a blocks-only canvas")
	}
	if !strings.Contains(h.status(), "blocks only") {
		t.Errorf("status = %q, want it to explain that characters are unavailable", h.status())
	}
}

func TestBlocksOnlyIgnoresPrintableKeys(t *testing.T) {
	h := newHarness(t, blocksOnly)

	// Compare against the stamp the session started with, since one of these
	// characters happens to be that default.
	want := h.model.stamp
	for _, r := range []rune{'A', 'z', '#', '@', '~'} {
		h.press(runes(r))
		if h.model.stamp != want {
			t.Errorf("%q changed the stamp to %q on a blocks-only canvas", r, h.model.stamp)
		}
		if !h.model.block {
			t.Errorf("%q left block mode", r)
		}
	}
	if !strings.Contains(h.status(), "blocks only") {
		t.Errorf("status = %q, want an explanation", h.status())
	}
}

func TestBlocksOnlyIgnoresTheLiteralPrefix(t *testing.T) {
	h := newHarness(t, blocksOnly)

	h.press(runes('\\'))
	if h.model.literalNext {
		t.Fatal("the literal prefix armed on a blocks-only canvas")
	}
	// The next key must still behave normally rather than being swallowed.
	h.press(runes('4'))
	if h.model.color != 4 {
		t.Errorf("color = %d, want 4", h.model.color)
	}
}

// Colors and movement are untouched; only characters go away.
func TestBlocksOnlyKeepsColorsAndMovement(t *testing.T) {
	h := newHarness(t, blocksOnly)

	h.press(runes('6'))
	if h.model.color != 6 {
		t.Errorf("color = %d, want 6", h.model.color)
	}
	h.press(special(tea.KeyTab))
	if h.model.color != 7 {
		t.Errorf("color = %d after tab, want 7", h.model.color)
	}
	x0 := h.model.curX
	h.press(runes('l'))
	if h.model.curX != x0+1 {
		t.Error("hjkl stopped moving the cursor")
	}
}

func TestBlocksOnlyPlacesOnlyBlocks(t *testing.T) {
	h := newHarness(t, blocksOnly)

	// Try hard to get a character down: pick one, arm the literal prefix, then
	// place. Every route has to come out as a block.
	h.press(runes('X'))
	h.press(runes('\\'))
	h.press(runes('X'))
	h.press(tea.KeyMsg{Type: tea.KeyCtrlB})
	h.press(runes('5'))
	h.press(special(tea.KeyEnter))

	got, _ := h.app.Canvas.At(h.model.curX, h.model.curY)
	if !got.IsBlock() {
		t.Fatalf("cell = %+v, want a block", got)
	}
	if got.Rune != canvas.Empty {
		t.Errorf("cell holds the character %q", got.Rune)
	}
	if got.Fill != 5 {
		t.Errorf("fill = %d, want 5", got.Fill)
	}
}

// The model must never even ask the server for a character on such a canvas.
func TestBlocksOnlyCellIsAlwaysABlock(t *testing.T) {
	h := newHarness(t, blocksOnly)
	// Force the internal state a buggy key handler might leave behind.
	h.model.block = false
	h.model.stamp = 'Q'

	if got := h.model.cell(); !got.IsBlock() || got.Rune != canvas.Empty {
		t.Errorf("cell() = %+v, want a block even with block mode off", got)
	}
}

func TestBlocksOnlyStatusShowsNoCharacter(t *testing.T) {
	h := newHarness(t, blocksOnly)
	h.press(runes('2'))
	status := h.status()
	if !strings.Contains(status, "███") {
		t.Errorf("status = %q, want a solid swatch", status)
	}
	if strings.Contains(status, "#") {
		t.Errorf("status = %q, want no stamp character", status)
	}
}
