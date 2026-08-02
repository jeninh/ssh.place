package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jeninh/ssh.place/internal/canvas"
)

func TestIndexUsesTheDarkPaletteOnly(t *testing.T) {
	body := get(t, Handler(newApp(t)), "/").Body.String()

	if !strings.Contains(body, "--bg: #0d1117") {
		t.Error("page is missing the dark background")
	}
	// The page is deliberately not theme-aware: it always matches the terminal.
	if strings.Contains(body, "prefers-color-scheme") {
		t.Error("page follows the system theme; it should always be dark")
	}
}

func TestFooterLinksOnEveryPage(t *testing.T) {
	h := Handler(newApp(t))
	for _, path := range []string{"/", "/stats"} {
		body := get(t, h, path).Body.String()

		if !strings.Contains(body, "https://github.com/jeninh/ssh.place") {
			t.Errorf("%s has no open-source link", path)
		}
		if !strings.Contains(body, ">open source<") {
			t.Errorf("%s does not label the repo link %q", path, "open source")
		}
		if !strings.Contains(body, `href="/stats"`) {
			t.Errorf("%s does not link to the stats page", path)
		}
		// The plain-text view is no longer offered on the pages.
		if strings.Contains(body, "canvas.txt") {
			t.Errorf("%s still offers the plain text view", path)
		}
		if strings.Contains(body, "plain text") {
			t.Errorf("%s still mentions %q", path, "plain text")
		}
	}
}

func TestStatsPageRendersFigures(t *testing.T) {
	a := newApp(t)
	s, err := a.Hub.Add("1.1.1.1", "k", true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Hub.Remove(s)

	// Four red blocks and one lime character.
	for x := 0; x < 4; x++ {
		if err := a.Canvas.Fill(x, 0, 9); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Canvas.Set(0, 1, 'x', 10); err != nil {
		t.Fatal(err)
	}

	rec := get(t, Handler(a), "/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"ssh.place",
		"Online now",
		"Cells drawn",
		"Placements",
		"Palette",
		canvas.Palette[9].Name,  // red appears in the breakdown
		canvas.Palette[10].Name, // and so does lime
		"#ff0000",               // its swatch color
		"ssh ssh.place",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stats page is missing %q", want)
		}
	}

	// 400 cells on a 40x10 canvas, 5 drawn.
	if !strings.Contains(body, ">400<") {
		t.Error("stats page does not report the total cell count")
	}
	if !strings.Contains(body, ">5<") {
		t.Error("stats page does not report the drawn count")
	}
}

func TestStatsPageWithAnEmptyCanvas(t *testing.T) {
	body := get(t, Handler(newApp(t)), "/stats").Body.String()
	if !strings.Contains(body, "Nothing has been drawn yet") {
		t.Error("an empty canvas should say so rather than show an empty table")
	}
	if strings.Contains(body, "<th>Color</th>") {
		t.Error("empty canvas rendered a palette table with no rows")
	}
}

func TestStatsJSONReportsBlocksAndColors(t *testing.T) {
	a := newApp(t)
	for x := 0; x < 3; x++ {
		if err := a.Canvas.Fill(x, 0, 12); err != nil {
			t.Fatal(err)
		}
	}

	rec := get(t, Handler(a), "/stats.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got struct {
		Drawn  int `json:"cells_drawn"`
		Cells  int `json:"cells_total"`
		Colors []struct {
			Name  string `json:"name"`
			Hex   string `json:"hex"`
			Count int    `json:"count"`
		} `json:"colors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Drawn != 3 {
		t.Errorf("cells_drawn = %d, want 3", got.Drawn)
	}
	if got.Cells != 400 {
		t.Errorf("cells_total = %d, want 400", got.Cells)
	}
	if len(got.Colors) != 1 {
		t.Fatalf("colors = %+v, want exactly one entry", got.Colors)
	}
	if got.Colors[0].Name != canvas.Palette[12].Name || got.Colors[0].Count != 3 {
		t.Errorf("colors[0] = %+v, want %s x3", got.Colors[0], canvas.Palette[12].Name)
	}
	if got.Colors[0].Hex != "#0000ff" {
		t.Errorf("hex = %q, want #0000ff", got.Colors[0].Hex)
	}
}

// Blocks are what most of the canvas will be, so the PNG has to paint the whole
// cell rather than draw a glyph.
func TestPNGFillsTheWholeCellForABlock(t *testing.T) {
	a := newApp(t)
	const colorIdx = 10 // lime
	if err := a.Canvas.Fill(0, 0, colorIdx); err != nil {
		t.Fatal(err)
	}

	img := RenderImage(a.Canvas)
	want := canvas.Palette[colorIdx].RGB

	for y := 0; y < cellH; y++ {
		for x := 0; x < cellW; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if uint8(r>>8) != want.R || uint8(g>>8) != want.G || uint8(b>>8) != want.B {
				t.Fatalf("pixel (%d,%d) = (%d,%d,%d), want the whole cell filled with (%d,%d,%d)",
					x, y, r>>8, g>>8, b>>8, want.R, want.G, want.B)
			}
		}
	}

	// The neighbouring cell is untouched, so blocks do not bleed.
	r, g, b, _ := img.At(cellW+1, 1).RGBA()
	if uint8(r>>8) != background.R || uint8(g>>8) != background.G || uint8(b>>8) != background.B {
		t.Errorf("neighbouring cell = (%d,%d,%d), want the background", r>>8, g>>8, b>>8)
	}
}

// A black block must be painted, not skipped as if it were empty.
func TestPNGPaintsBlackBlocks(t *testing.T) {
	a := newApp(t)
	if err := a.Canvas.Fill(0, 0, 0); err != nil {
		t.Fatal(err)
	}
	img := RenderImage(a.Canvas)
	r, g, b, _ := img.At(3, 6).RGBA()
	if uint8(r>>8) != 0 || uint8(g>>8) != 0 || uint8(b>>8) != 0 {
		t.Errorf("black block pixel = (%d,%d,%d), want (0,0,0)", r>>8, g>>8, b>>8)
	}
	// And it has to differ from untouched canvas, or it would be invisible.
	if background.R == 0 && background.G == 0 && background.B == 0 {
		t.Error("the background is pure black, so a black block cannot be seen")
	}
}

func TestCanvasTXTStillServesBlocks(t *testing.T) {
	// The endpoint is no longer linked from the pages, but it still works.
	a := newApp(t)
	if err := a.Canvas.Fill(0, 0, 5); err != nil {
		t.Fatal(err)
	}
	rec := get(t, Handler(a), "/canvas.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.HasPrefix(rec.Body.String(), "█") {
		t.Errorf("body starts %q, want a full block", rec.Body.String()[:6])
	}
}

// Only the exact root serves the landing page; anything else is a 404 rather
// than a copy of the index.
func TestOnlyExactRootServesTheIndex(t *testing.T) {
	h := Handler(newApp(t))
	for _, path := range []string{"/nope", "/stats/extra", "/canvas"} {
		if rec := get(t, h, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}
