package canvas

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSetAndAt(t *testing.T) {
	c := New(10, 4)

	if err := c.Set(3, 2, 'X', 9); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := c.At(3, 2)
	if !ok {
		t.Fatal("At(3,2) reported out of bounds")
	}
	if got.Rune != 'X' || got.Color != 9 {
		t.Errorf("At(3,2) = %q/%d, want 'X'/9", got.Rune, got.Color)
	}
}

func TestNewCanvasIsEmpty(t *testing.T) {
	c := New(5, 5)
	if n := c.NonEmpty(); n != 0 {
		t.Errorf("NonEmpty on fresh canvas = %d, want 0", n)
	}
	cell, ok := c.At(0, 0)
	if !ok || cell.Rune != Empty {
		t.Errorf("fresh cell = %q, want %q", cell.Rune, Empty)
	}
}

func TestSetBounds(t *testing.T) {
	c := New(10, 4)
	// Corners are inside; anything one step past them is not.
	for _, p := range [][2]int{{0, 0}, {9, 0}, {0, 3}, {9, 3}} {
		if err := c.Set(p[0], p[1], 'o', 1); err != nil {
			t.Errorf("Set(%d,%d) = %v, want nil", p[0], p[1], err)
		}
	}
	for _, p := range [][2]int{{-1, 0}, {0, -1}, {10, 0}, {0, 4}, {10, 4}, {-1, -1}} {
		if err := c.Set(p[0], p[1], 'o', 1); !errors.Is(err, ErrOutOfBounds) {
			t.Errorf("Set(%d,%d) = %v, want ErrOutOfBounds", p[0], p[1], err)
		}
		if _, ok := c.At(p[0], p[1]); ok {
			t.Errorf("At(%d,%d) reported in bounds", p[0], p[1])
		}
	}
}

func TestSetRejectsNonPrintable(t *testing.T) {
	c := New(4, 4)
	bad := []rune{0, '\n', '\r', '\t', 0x1b, 0x7f, 0x80, 'é', '→', '👋'}
	for _, r := range bad {
		if err := c.Set(1, 1, r, 1); !errors.Is(err, ErrBadRune) {
			t.Errorf("Set(%#U) = %v, want ErrBadRune", r, err)
		}
	}
	// The full printable ASCII range is accepted, space included: it erases.
	for r := rune(0x20); r <= 0x7e; r++ {
		if err := c.Set(1, 1, r, 1); err != nil {
			t.Fatalf("Set(%#U) = %v, want nil", r, err)
		}
	}
}

func TestSetRejectsBadColor(t *testing.T) {
	c := New(4, 4)
	if err := c.Set(1, 1, 'a', PaletteSize); !errors.Is(err, ErrBadColor) {
		t.Errorf("Set with color %d = %v, want ErrBadColor", PaletteSize, err)
	}
	for i := uint8(0); i < PaletteSize; i++ {
		if err := c.Set(1, 1, 'a', i); err != nil {
			t.Errorf("Set with color %d = %v, want nil", i, err)
		}
	}
}

func TestVersionAdvancesOnlyOnMutation(t *testing.T) {
	c := New(4, 4)
	v0 := c.Version()

	if _, ok := c.At(0, 0); !ok {
		t.Fatal("At failed")
	}
	if c.Version() != v0 {
		t.Error("reading changed the version")
	}

	if err := c.Set(0, 0, 'a', 1); err != nil {
		t.Fatal(err)
	}
	if c.Version() == v0 {
		t.Error("Set did not advance the version")
	}

	v1 := c.Version()
	if err := c.Set(99, 99, 'a', 1); err == nil {
		t.Fatal("expected out of bounds error")
	}
	if c.Version() != v1 {
		t.Error("a rejected Set advanced the version")
	}
}

func TestCopyRegionClipsToCanvas(t *testing.T) {
	c := New(4, 3)
	if err := c.Set(0, 0, 'A', 1); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(3, 2, 'Z', 2); err != nil {
		t.Fatal(err)
	}

	// Request a region that overhangs every edge; the overhang must come back
	// blank rather than panic or wrap.
	const w, h = 6, 5
	buf := make([]Cell, w*h)
	c.CopyRegion(-1, -1, w, h, buf)

	if got := buf[1*w+1]; got.Rune != 'A' {
		t.Errorf("cell (0,0) = %q, want 'A'", got.Rune)
	}
	if got := buf[3*w+4]; got.Rune != 'Z' {
		t.Errorf("cell (3,2) = %q, want 'Z'", got.Rune)
	}
	// Top row and left column are entirely outside the canvas.
	for x := 0; x < w; x++ {
		if buf[x].Rune != Empty {
			t.Errorf("overhang row cell %d = %q, want blank", x, buf[x].Rune)
		}
	}
	for y := 0; y < h; y++ {
		if buf[y*w].Rune != Empty {
			t.Errorf("overhang column cell %d = %q, want blank", y, buf[y*w].Rune)
		}
	}
}

func TestCopyRegionMatchesAt(t *testing.T) {
	c := New(8, 6)
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			if err := c.Set(x, y, rune('a'+(x+y)%26), uint8((x*y)%PaletteSize)); err != nil {
				t.Fatal(err)
			}
		}
	}
	const w, h = 3, 2
	buf := make([]Cell, w*h)
	c.CopyRegion(2, 1, w, h, buf)
	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			want, _ := c.At(2+col, 1+row)
			if got := buf[row*w+col]; got != want {
				t.Errorf("region (%d,%d) = %v, want %v", col, row, got, want)
			}
		}
	}
}

// TestConcurrentPlacement is the -race workhorse: many writers on distinct
// cells, many writers contending for one cell, and readers copying regions
// throughout.
func TestConcurrentPlacement(t *testing.T) {
	const (
		w, h    = 40, 20
		writers = 16
		rounds  = 200
	)
	c := New(w, h)

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				x := (id*7 + r*3) % w
				y := (id*3 + r) % h
				if err := c.Set(x, y, rune('a'+id%26), uint8(id%PaletteSize)); err != nil {
					t.Errorf("Set: %v", err)
					return
				}
				// Contend on a single hot cell too.
				if err := c.Set(0, 0, rune('0'+id%10), uint8(id%PaletteSize)); err != nil {
					t.Errorf("hot Set: %v", err)
					return
				}
			}
		}(i)
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]Cell, w*h)
			for r := 0; r < rounds; r++ {
				c.CopyRegion(0, 0, w, h, buf)
				_ = c.Snapshot()
				_ = c.NonEmpty()
				_ = c.Version()
			}
		}()
	}
	wg.Wait()

	// Every write landed somewhere, and the version counter saw all of them.
	wantVersion := uint64(writers * rounds * 2)
	if got := c.Version(); got != wantVersion {
		t.Errorf("Version = %d, want %d (lost updates)", got, wantVersion)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	c := New(20, 5)
	want := []struct {
		x, y  int
		r     rune
		color uint8
	}{
		{0, 0, '#', 9},
		{19, 4, '~', 0},
		{7, 2, ' ', 15},
		{5, 1, '"', 3}, // JSON-sensitive characters must survive
		{6, 1, '\\', 4},
	}
	for _, p := range want {
		if err := c.Set(p.x, p.y, p.r, p.color); err != nil {
			t.Fatal(err)
		}
	}

	data, err := c.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot: %v", err)
	}

	restored := New(20, 5)
	if err := restored.UnmarshalSnapshot(data); err != nil {
		t.Fatalf("UnmarshalSnapshot: %v", err)
	}
	for _, p := range want {
		got, ok := restored.At(p.x, p.y)
		if !ok {
			t.Fatalf("At(%d,%d) out of bounds", p.x, p.y)
		}
		if got.Rune != p.r || got.Color != p.color {
			t.Errorf("At(%d,%d) = %q/%d, want %q/%d", p.x, p.y, got.Rune, got.Color, p.r, p.color)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "canvas.json")

	c := New(30, 8)
	if err := c.Set(11, 3, '@', 13); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path, 30, 8)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, _ := loaded.At(11, 3)
	if got.Rune != '@' || got.Color != 13 {
		t.Errorf("restored cell = %q/%d, want '@'/13", got.Rune, got.Color)
	}

	// Saving twice must not leave temp files behind.
	if err := c.Save(path); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("snapshot dir holds %v, want just canvas.json", names)
	}
}

func TestLoadMissingFileStartsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.json"), 12, 4)
	if err != nil {
		t.Fatalf("Load of missing file = %v, want nil", err)
	}
	if w, h := c.Size(); w != 12 || h != 4 {
		t.Errorf("size = %dx%d, want 12x4", w, h)
	}
	if n := c.NonEmpty(); n != 0 {
		t.Errorf("NonEmpty = %d, want 0", n)
	}
}

func TestLoadCorruptFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "canvas.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, 10, 10); err == nil {
		t.Error("Load of corrupt snapshot succeeded, want error")
	}
}

// A canvas resized between restarts should keep whatever still fits rather
// than dropping the artwork entirely.
func TestSnapshotSurvivesResize(t *testing.T) {
	big := New(20, 6)
	if err := big.Set(2, 1, 'K', 5); err != nil {
		t.Fatal(err)
	}
	if err := big.Set(18, 5, 'L', 6); err != nil {
		t.Fatal(err)
	}
	data, err := big.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	small := New(10, 3)
	if err := small.UnmarshalSnapshot(data); err != nil {
		t.Fatalf("UnmarshalSnapshot into smaller canvas: %v", err)
	}
	if got, _ := small.At(2, 1); got.Rune != 'K' {
		t.Errorf("in-range cell = %q, want 'K'", got.Rune)
	}
	if n := small.NonEmpty(); n != 1 {
		t.Errorf("NonEmpty = %d, want 1 (the out-of-range cell should be dropped)", n)
	}

	larger := New(40, 12)
	if err := larger.UnmarshalSnapshot(data); err != nil {
		t.Fatalf("UnmarshalSnapshot into larger canvas: %v", err)
	}
	if got, _ := larger.At(18, 5); got.Rune != 'L' {
		t.Errorf("cell = %q, want 'L'", got.Rune)
	}
	if got, _ := larger.At(30, 10); got.Rune != Empty {
		t.Errorf("new area cell = %q, want blank", got.Rune)
	}
}

func TestUnmarshalSnapshotSanitizesInput(t *testing.T) {
	// A hand-edited or corrupted snapshot must not smuggle control characters
	// onto the canvas, since every session would then render them.
	data := []byte(`{"version":1,"width":4,"height":1,"runes":["a\u001bb\n"],"colors":["0z2"]}`)
	c := New(4, 1)
	if err := c.UnmarshalSnapshot(data); err != nil {
		t.Fatalf("UnmarshalSnapshot: %v", err)
	}
	for x, want := range []rune{'a', Empty, 'b', Empty} {
		got, _ := c.At(x, 0)
		if got.Rune != want {
			t.Errorf("cell %d = %#U, want %#U", x, got.Rune, want)
		}
	}
	// 'z' is not a hex digit, so that cell falls back to the default color.
	if got, _ := c.At(1, 0); got.Color != DefaultColor {
		t.Errorf("color for invalid nibble = %d, want %d", got.Color, DefaultColor)
	}
}

func TestUnmarshalSnapshotRejectsBadHeaders(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"wrong version", `{"version":99,"width":2,"height":1,"runes":["ab"],"colors":["00"]}`},
		{"zero width", `{"version":1,"width":0,"height":1,"runes":["ab"],"colors":["00"]}`},
		{"missing rows", `{"version":1,"width":2,"height":4,"runes":["ab"],"colors":["00"]}`},
		{"not json", `nope`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := New(2, 1).UnmarshalSnapshot([]byte(tc.body)); err == nil {
				t.Error("UnmarshalSnapshot succeeded, want error")
			}
		})
	}
}

func TestText(t *testing.T) {
	c := New(5, 2)
	if err := c.Set(1, 0, 'h', 1); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(2, 0, 'i', 1); err != nil {
		t.Fatal(err)
	}
	rows := c.Text()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0] != " hi  " {
		t.Errorf("row 0 = %q, want %q", rows[0], " hi  ")
	}
	if rows[1] != "     " {
		t.Errorf("row 1 = %q, want all spaces", rows[1])
	}
}

func TestNewPanicsOnBadSize(t *testing.T) {
	for _, p := range [][2]int{{0, 5}, {5, 0}, {-1, 5}} {
		t.Run(fmt.Sprintf("%dx%d", p[0], p[1]), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("New did not panic")
				}
			}()
			New(p[0], p[1])
		})
	}
}

func TestPaletteIsComplete(t *testing.T) {
	if len(Palette) != PaletteSize {
		t.Fatalf("palette has %d entries, want %d", len(Palette), PaletteSize)
	}
	seen := map[uint8]bool{}
	for i, e := range Palette {
		if e.Name == "" {
			t.Errorf("palette entry %d has no name", i)
		}
		if e.ANSI > 15 {
			t.Errorf("palette entry %d uses ANSI %d, outside the 16-color range", i, e.ANSI)
		}
		if seen[e.ANSI] {
			t.Errorf("palette entry %d reuses ANSI color %d", i, e.ANSI)
		}
		seen[e.ANSI] = true
		if !ValidColor(uint8(i)) {
			t.Errorf("ValidColor(%d) = false", i)
		}
	}
	if ValidColor(PaletteSize) {
		t.Errorf("ValidColor(%d) = true, want false", PaletteSize)
	}
}
