package canvas

import (
	"errors"
	"strings"
	"testing"
)

func TestGlyphAndBlockConstructors(t *testing.T) {
	g := Glyph('A', 3)
	if g.IsBlock() {
		t.Error("a glyph reported itself as a block")
	}
	if g.Fill != NoFill {
		t.Errorf("glyph Fill = %d, want NoFill (%d)", g.Fill, NoFill)
	}
	if !g.Drawn() {
		t.Error("a glyph cell is not marked as drawn")
	}

	b := Block(3)
	if !b.IsBlock() {
		t.Error("a block did not report itself as a block")
	}
	if b.Fill != 3 {
		t.Errorf("block Fill = %d, want 3", b.Fill)
	}
	if b.Rune != Empty {
		t.Errorf("block Rune = %q, want a space: a block carries no character", b.Rune)
	}
	if !b.Drawn() {
		t.Error("a block is not marked as drawn")
	}

	// A blank cell is neither.
	blank := blank()
	if blank.IsBlock() || blank.Drawn() {
		t.Errorf("blank cell = %+v, want neither a block nor drawn", blank)
	}
}

// Block(0) is a legal black block and must not be confused with "no fill".
func TestBlackBlockIsAFill(t *testing.T) {
	b := Block(0)
	if !b.IsBlock() {
		t.Error("a black block reported no fill")
	}
	if !b.Drawn() {
		t.Error("a black block is not marked as drawn")
	}

	c := New(4, 4)
	if err := c.SetCell(1, 1, b); err != nil {
		t.Fatal(err)
	}
	got, _ := c.At(1, 1)
	if !got.IsBlock() || got.Fill != 0 {
		t.Errorf("stored cell = %+v, want a block with fill 0", got)
	}
	if n := c.NonEmpty(); n != 1 {
		t.Errorf("NonEmpty = %d, want 1", n)
	}
}

func TestFillWritesABlock(t *testing.T) {
	c := New(6, 3)
	if err := c.Fill(2, 1, 11); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	got, ok := c.At(2, 1)
	if !ok {
		t.Fatal("cell out of bounds")
	}
	if !got.IsBlock() || got.Fill != 11 {
		t.Errorf("cell = %+v, want a block filled with 11", got)
	}
}

func TestSetCellValidates(t *testing.T) {
	c := New(4, 4)

	if err := c.SetCell(9, 9, Block(1)); !errors.Is(err, ErrOutOfBounds) {
		t.Errorf("out of bounds SetCell = %v, want ErrOutOfBounds", err)
	}
	if err := c.SetCell(1, 1, Cell{Rune: '\n', Color: 1, Fill: NoFill}); !errors.Is(err, ErrBadRune) {
		t.Errorf("control rune = %v, want ErrBadRune", err)
	}
	if err := c.SetCell(1, 1, Cell{Rune: 'a', Color: PaletteSize, Fill: NoFill}); !errors.Is(err, ErrBadColor) {
		t.Errorf("bad color = %v, want ErrBadColor", err)
	}
	// A fill outside the palette that is not the NoFill sentinel is invalid.
	if err := c.SetCell(1, 1, Cell{Rune: ' ', Color: 1, Fill: PaletteSize}); !errors.Is(err, ErrBadColor) {
		t.Errorf("bad fill = %v, want ErrBadColor", err)
	}
	for i := uint8(0); i < PaletteSize; i++ {
		if err := c.SetCell(1, 1, Block(i)); err != nil {
			t.Errorf("Block(%d) = %v, want nil", i, err)
		}
	}
}

// Overwriting has to work in both directions, since space is also an eraser.
func TestBlocksAndGlyphsOverwriteEachOther(t *testing.T) {
	c := New(4, 4)

	if err := c.Fill(0, 0, 4); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(0, 0, 'x', 5); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.At(0, 0); got.IsBlock() {
		t.Errorf("cell = %+v, want a glyph after being overwritten", got)
	}

	if err := c.Fill(0, 0, 6); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.At(0, 0); !got.IsBlock() || got.Fill != 6 {
		t.Errorf("cell = %+v, want a block filled with 6", got)
	}

	// A space erases a block back to blank canvas.
	if err := c.Set(0, 0, ' ', 15); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.At(0, 0); got.Drawn() {
		t.Errorf("cell = %+v, want blank after being erased", got)
	}
	if n := c.NonEmpty(); n != 0 {
		t.Errorf("NonEmpty = %d, want 0", n)
	}
}

func TestBlocksSurviveSnapshotRoundTrip(t *testing.T) {
	c := New(12, 3)
	if err := c.Fill(0, 0, 0); err != nil { // black block, the tricky one
		t.Fatal(err)
	}
	if err := c.Fill(11, 2, 14); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(5, 1, 'K', 9); err != nil {
		t.Fatal(err)
	}

	data, err := c.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	restored := New(12, 3)
	if err := restored.UnmarshalSnapshot(data); err != nil {
		t.Fatalf("UnmarshalSnapshot: %v", err)
	}

	if got, _ := restored.At(0, 0); !got.IsBlock() || got.Fill != 0 {
		t.Errorf("black block restored as %+v", got)
	}
	if got, _ := restored.At(11, 2); !got.IsBlock() || got.Fill != 14 {
		t.Errorf("block restored as %+v, want fill 14", got)
	}
	if got, _ := restored.At(5, 1); got.IsBlock() || got.Rune != 'K' || got.Color != 9 {
		t.Errorf("glyph restored as %+v, want 'K'/9 with no fill", got)
	}
	if got, _ := restored.At(1, 0); got.Drawn() {
		t.Errorf("untouched cell restored as %+v, want blank", got)
	}
	if n := restored.NonEmpty(); n != 3 {
		t.Errorf("NonEmpty = %d, want 3", n)
	}
}

// Version 1 snapshots predate blocks. They must still load, with every cell
// coming back as a character.
func TestVersion1SnapshotStillLoads(t *testing.T) {
	v1 := []byte(`{"version":1,"width":4,"height":2,` +
		`"runes":["ab  ","  cd"],"colors":["1234","5678"]}`)

	c := New(4, 2)
	if err := c.UnmarshalSnapshot(v1); err != nil {
		t.Fatalf("UnmarshalSnapshot of a version 1 file: %v", err)
	}
	got, _ := c.At(0, 0)
	if got.Rune != 'a' || got.Color != 1 {
		t.Errorf("cell = %q/%d, want 'a'/1", got.Rune, got.Color)
	}
	if got.IsBlock() {
		t.Error("a version 1 cell came back as a block; it has no fill data")
	}
	if got, _ := c.At(3, 1); got.Rune != 'd' {
		t.Errorf("cell = %q, want 'd'", got.Rune)
	}
	if n := c.NonEmpty(); n != 4 {
		t.Errorf("NonEmpty = %d, want 4", n)
	}
}

func TestSnapshotVersionsOutsideTheSupportedRangeAreRejected(t *testing.T) {
	for _, v := range []string{"0", "3", "99"} {
		body := `{"version":` + v + `,"width":2,"height":1,"runes":["ab"],"colors":["00"]}`
		if err := New(2, 1).UnmarshalSnapshot([]byte(body)); err == nil {
			t.Errorf("version %s was accepted, want an error", v)
		}
	}
}

// A hand-edited fills row must not smuggle a bogus fill onto the canvas.
func TestUnmarshalSanitizesFills(t *testing.T) {
	data := []byte(`{"version":2,"width":4,"height":1,` +
		`"runes":["    "],"colors":["1111"],"fills":["-a?z"]}`)
	c := New(4, 1)
	if err := c.UnmarshalSnapshot(data); err != nil {
		t.Fatal(err)
	}
	// '-' is the no-fill marker, 'a' is 10, and '?' and 'z' are not hex so they
	// fall back to no fill.
	for x, want := range []uint8{NoFill, 10, NoFill, NoFill} {
		got, _ := c.At(x, 0)
		if got.Fill != want {
			t.Errorf("cell %d fill = %d, want %d", x, got.Fill, want)
		}
	}
}

func TestTextRendersBlocksAsFullBlock(t *testing.T) {
	c := New(4, 1)
	if err := c.Fill(0, 0, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(1, 0, 'x', 1); err != nil {
		t.Fatal(err)
	}

	rows := c.Text()
	if got, want := rows[0], "█x  "; got != want {
		t.Errorf("row = %q, want %q", got, want)
	}
	// Text is measured in runes, so a full block still occupies one cell.
	if got, want := len([]rune(rows[0])), 4; got != want {
		t.Errorf("row is %d runes, want %d", got, want)
	}
}

func TestColorCounts(t *testing.T) {
	c := New(10, 2)
	// Three red blocks, two lime glyphs, and a space that counts as nothing.
	for x := 0; x < 3; x++ {
		if err := c.Fill(x, 0, 9); err != nil {
			t.Fatal(err)
		}
	}
	for x := 0; x < 2; x++ {
		if err := c.Set(x, 1, 'o', 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Set(9, 1, ' ', 4); err != nil {
		t.Fatal(err)
	}

	counts, drawn := c.ColorCounts()
	if drawn != 5 {
		t.Errorf("drawn = %d, want 5", drawn)
	}
	if counts[9] != 3 {
		t.Errorf("red count = %d, want 3 (blocks count under their fill)", counts[9])
	}
	if counts[10] != 2 {
		t.Errorf("lime count = %d, want 2", counts[10])
	}
	if counts[4] != 0 {
		t.Errorf("a placed space was counted under color 4: %d", counts[4])
	}

	total := 0
	for _, n := range counts {
		total += n
	}
	if total != drawn {
		t.Errorf("counts sum to %d but drawn = %d", total, drawn)
	}
}

func TestSnapshotStaysCompact(t *testing.T) {
	// Fills add a third row array, so check the file is still a reasonable size
	// for something written every ten seconds.
	c := New(DefaultWidth, DefaultHeight)
	for x := 0; x < DefaultWidth; x++ {
		if err := c.Fill(x, 0, uint8(x%PaletteSize)); err != nil {
			t.Fatal(err)
		}
	}
	data, err := c.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 64<<10 {
		t.Errorf("snapshot is %d bytes, want it comfortably under 64 KiB", len(data))
	}
	if !strings.Contains(string(data), `"fills"`) {
		t.Error("snapshot has no fills array")
	}
}
