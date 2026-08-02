package canvas

import "testing"

// Escape sequences are the risk a shared text canvas carries that a pixel canvas
// does not: whatever gets stored is replayed into every other player's terminal.
// So the canvas has to refuse anything a terminal would interpret rather than
// print.
func TestCanvasRefusesTerminalEscapeSequences(t *testing.T) {
	c := New(20, 4)

	hostile := []struct {
		name string
		r    rune
	}{
		{"escape", 0x1b},
		{"bell", 0x07},
		{"backspace", 0x08},
		{"carriage return", '\r'},
		{"newline", '\n'},
		{"tab", '\t'},
		{"nul", 0x00},
		{"delete", 0x7f},
		{"shift out", 0x0e},
		{"8-bit CSI", 0x9b},
		{"8-bit OSC terminator", 0x9c},
		{"right-to-left override", 0x202e},
		{"zero width joiner", 0x200d},
		{"combining acute", 0x0301},
		{"full-width A", 0xff21},
		{"emoji", 0x1f4a3},
	}
	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Set(1, 1, tc.r, 1); err == nil {
				t.Errorf("Set(%#U) was accepted; it has to be refused", tc.r)
			}
			if err := c.SetCell(1, 1, Cell{Rune: tc.r, Color: 1, Fill: NoFill}); err == nil {
				t.Errorf("SetCell(%#U) was accepted; it has to be refused", tc.r)
			}
		})
	}

	if n := c.NonEmpty(); n != 0 {
		t.Errorf("%d hostile cells reached the canvas", n)
	}
}

// Exhaustive: everything the canvas will store is a single printable column, so
// no stored cell can widen a row or move somebody else's cursor.
func TestOnlyPrintableASCIIIsStorable(t *testing.T) {
	c := New(4, 1)
	for r := rune(0); r < 0x11000; r++ {
		err := c.Set(0, 0, r, 1)
		printable := r >= 0x20 && r <= 0x7e
		if printable && err != nil {
			t.Fatalf("printable %#U was refused: %v", r, err)
		}
		if !printable && err == nil {
			t.Fatalf("%#U was stored; only printable ASCII may be", r)
		}
	}
}

// A snapshot is the one path that fills cells without going through Set, so a
// hand-tampered file must not be able to smuggle an escape in either.
func TestTamperedSnapshotCannotInjectEscapes(t *testing.T) {
	// Decodes to ESC ] 0 ; x BEL a b, which is an OSC window-title sequence.
	data := []byte(`{"version":2,"width":8,"height":1,` +
		`"runes":["\u001b]0;x\u0007ab"],"colors":["11111111"],"fills":["--------"]}`)

	c := New(8, 1)
	if err := c.UnmarshalSnapshot(data); err != nil {
		t.Fatalf("UnmarshalSnapshot: %v", err)
	}
	for x := 0; x < 8; x++ {
		got, _ := c.At(x, 0)
		if !ValidRune(got.Rune) {
			t.Errorf("cell %d holds %#U, which is not printable ASCII", x, got.Rune)
		}
	}
	// The escape and the bell specifically have to be gone, not merely quoted.
	for _, row := range c.Text() {
		for _, r := range row {
			if r == 0x1b || r == 0x07 {
				t.Errorf("Text() emitted %#U", r)
			}
			if r != '█' && !ValidRune(r) {
				t.Errorf("Text() emitted %#U", r)
			}
		}
	}
}

// The plain-text view is served to browsers, so check the canvas cannot be used
// to plant markup that a sniffing client might run. The characters themselves are
// legal ASCII, so the defence is the Content-Type and nosniff header rather than
// validation; this pins that they at least stay inert text here.
func TestDrawnMarkupStaysLiteral(t *testing.T) {
	c := New(20, 1)
	for i, r := range "<script>x</script>" {
		if err := c.Set(i, 0, r, 1); err != nil {
			t.Fatalf("Set(%q): %v", r, err)
		}
	}
	row := c.Text()[0]
	if got := row[:18]; got != "<script>x</script>" {
		t.Errorf("row = %q, want the characters stored verbatim", got)
	}
}
