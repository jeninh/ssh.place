package web

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/jeninh/ssh.place/internal/canvas"
)

// Glyph cell size. basicfont.Face7x13 is a fixed 7x13 bitmap font that ships
// with x/image, so the renderer needs no font files on disk and the PNG lines
// up exactly with the terminal grid.
const (
	cellW = 7
	cellH = 13
	// baseline is the font's ascent, measured from the top of the cell.
	baseline = 11
)

// background is a near-black that keeps black strokes faintly visible.
var background = color.RGBA{R: 0x0d, G: 0x0d, B: 0x0d, A: 0xff}

// RenderImage draws the canvas as an image.
func RenderImage(c *canvas.Canvas) *image.RGBA {
	w, h := c.Size()
	img := image.NewRGBA(image.Rect(0, 0, w*cellW, h*cellH))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: background}, image.Point{}, draw.Src)

	// One uniform source per palette entry, reused across every glyph, so a
	// full 200x60 render allocates 16 sources rather than 12000.
	sources := make([]*image.Uniform, canvas.PaletteSize)
	for i := range sources {
		sources[i] = image.NewUniform(canvas.Palette[i].RGB)
	}

	d := &font.Drawer{Dst: img, Face: basicfont.Face7x13}
	cells := c.Snapshot()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			cell := cells[y*w+x]

			// A block has no glyph: it is the whole cell painted in its color,
			// which is what the terminal shows as a background.
			if cell.IsBlock() {
				rect := image.Rect(x*cellW, y*cellH, (x+1)*cellW, (y+1)*cellH)
				draw.Draw(img, rect, sources[cell.Fill&0x0f], image.Point{}, draw.Src)
				continue
			}
			if cell.Rune == canvas.Empty || !canvas.ValidRune(cell.Rune) {
				continue
			}
			d.Src = sources[cell.Color&0x0f]
			d.Dot = fixed.P(x*cellW, y*cellH+baseline)
			d.DrawString(string(cell.Rune))
		}
	}
	return img
}

// RenderPNG encodes the canvas as a PNG.
func RenderPNG(c *canvas.Canvas) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, RenderImage(c)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pngCache serves the same bytes to every request until the canvas changes.
// Encoding a 1400x780 PNG is not free, and a browser left open on the web view
// polls steadily.
type pngCache struct {
	c *canvas.Canvas

	mu      sync.Mutex
	version uint64
	valid   bool
	data    []byte
}

func newPNGCache(c *canvas.Canvas) *pngCache { return &pngCache{c: c} }

// bytes returns the current PNG plus the canvas version it was rendered at,
// which doubles as a cheap ETag.
func (p *pngCache) bytes() ([]byte, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	v := p.c.Version()
	if p.valid && p.version == v {
		return p.data, v, nil
	}
	data, err := RenderPNG(p.c)
	if err != nil {
		return nil, 0, err
	}
	p.data, p.version, p.valid = data, v, true
	return data, v, nil
}

// countCache memoises the palette tally against the canvas version.
//
// ColorCounts walks all 12000 cells under the canvas lock. Serving that on every
// request would let anyone turn cheap HTTP requests into repeated full scans that
// contend with the sessions actually drawing, so the answer is computed once per
// change instead.
type countCache struct {
	c *canvas.Canvas

	mu      sync.Mutex
	version uint64
	valid   bool
	counts  [canvas.PaletteSize]int
	drawn   int
}

func newCountCache(c *canvas.Canvas) *countCache { return &countCache{c: c} }

func (p *countCache) get() ([canvas.PaletteSize]int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	v := p.c.Version()
	if p.valid && p.version == v {
		return p.counts, p.drawn
	}
	p.counts, p.drawn = p.c.ColorCounts()
	p.version, p.valid = v, true
	return p.counts, p.drawn
}
