// Package canvas holds the shared ASCII grid that every ssh.place session
// draws on. A Canvas is safe for concurrent use by any number of goroutines.
package canvas

import (
	"errors"
	"sync"
	"sync/atomic"
)

// Default canvas dimensions.
const (
	DefaultWidth  = 200
	DefaultHeight = 60
)

// Empty is the rune stored in a cell that has never been drawn on. It is also
// a legal stamp, which makes space double as an eraser.
const Empty = ' '

// Errors returned by Set. They are deliberately user-presentable: the TUI
// shows them in the status bar.
var (
	ErrOutOfBounds = errors.New("that spot is off the canvas")
	ErrBadRune     = errors.New("that character is not allowed here")
	ErrBadColor    = errors.New("that is not one of the 16 colors")
)

// Cell is a single position on the canvas.
//
// A cell is either a character in a palette color, or a solid block of color.
// Blocks are drawn as a background rather than as a block glyph: every terminal
// can paint a background, whereas U+2588 depends on the client's font, and the
// canvas has to look the same to everyone.
type Cell struct {
	Rune  rune
	Color uint8
	// Fill is the background palette index, or NoFill.
	Fill uint8
}

// Glyph returns a cell holding a character.
func Glyph(r rune, color uint8) Cell {
	return Cell{Rune: r, Color: color, Fill: NoFill}
}

// Block returns a solid cell of color. Color is carried alongside Fill so a
// renderer with no way to paint a background still has something to fall back
// on.
func Block(color uint8) Cell {
	return Cell{Rune: Empty, Color: color, Fill: color}
}

// blank is the value of a cell nobody has drawn on.
func blank() Cell { return Glyph(Empty, DefaultColor) }

// IsBlock reports whether the cell is a solid block of color.
func (c Cell) IsBlock() bool { return c.Fill != NoFill }

// Drawn reports whether anything was ever placed here.
func (c Cell) Drawn() bool { return c.Rune != Empty || c.IsBlock() }

// Validate checks the cell against everything the canvas will accept.
func (c Cell) Validate() error {
	if !ValidRune(c.Rune) {
		return ErrBadRune
	}
	if !ValidColor(c.Color) {
		return ErrBadColor
	}
	if !ValidFill(c.Fill) {
		return ErrBadColor
	}
	return nil
}

// ValidRune reports whether r may be stamped onto the canvas. Only printable
// ASCII is accepted: it guarantees every cell is exactly one terminal column
// wide, which keeps the grid aligned on every client, and it rules out control
// characters, escape sequences and bidi tricks in one check.
func ValidRune(r rune) bool { return r >= 0x20 && r <= 0x7e }

// Canvas is a fixed-size grid of Cells.
type Canvas struct {
	mu    sync.RWMutex
	w, h  int
	cells []Cell

	// version increments on every mutation. Readers use it to skip redundant
	// re-renders. It is atomic so it can be read without taking mu.
	version atomic.Uint64
}

// New returns an empty w by h canvas. It panics on non-positive dimensions,
// which can only be a programming error.
func New(w, h int) *Canvas {
	if w <= 0 || h <= 0 {
		panic("canvas: dimensions must be positive")
	}
	c := &Canvas{w: w, h: h, cells: make([]Cell, w*h)}
	for i := range c.cells {
		c.cells[i] = blank()
	}
	return c
}

// Size returns the canvas dimensions, which never change after New.
func (c *Canvas) Size() (w, h int) { return c.w, c.h }

// Width returns the canvas width.
func (c *Canvas) Width() int { return c.w }

// Height returns the canvas height.
func (c *Canvas) Height() int { return c.h }

// Version returns a counter that increments on every mutation.
func (c *Canvas) Version() uint64 { return c.version.Load() }

// InBounds reports whether (x, y) addresses a cell.
func (c *Canvas) InBounds(x, y int) bool {
	return x >= 0 && y >= 0 && x < c.w && y < c.h
}

// At returns the cell at (x, y). ok is false if the position is out of bounds.
func (c *Canvas) At(x, y int) (cell Cell, ok bool) {
	if !c.InBounds(x, y) {
		return Cell{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cells[y*c.w+x], true
}

// SetCell writes a cell, validating position and contents. It performs no rate
// limiting; see app.App.Place for the player-facing entry point.
func (c *Canvas) SetCell(x, y int, cell Cell) error {
	if !c.InBounds(x, y) {
		return ErrOutOfBounds
	}
	if err := cell.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	c.cells[y*c.w+x] = cell
	c.mu.Unlock()
	c.version.Add(1)
	return nil
}

// Set writes a character cell. It is shorthand for SetCell with a Glyph.
func (c *Canvas) Set(x, y int, r rune, col uint8) error {
	return c.SetCell(x, y, Glyph(r, col))
}

// Fill writes a solid block of color.
func (c *Canvas) Fill(x, y int, col uint8) error {
	return c.SetCell(x, y, Block(col))
}

// CopyRegion copies the w by h region whose top-left corner is (x0, y0) into
// dst in row-major order. Positions outside the canvas are filled with an
// empty cell, so callers may request a region larger than the canvas. dst must
// have room for w*h cells.
//
// Sessions render from a private copy so that no session holds the canvas lock
// while building its frame.
func (c *Canvas) CopyRegion(x0, y0, w, h int, dst []Cell) {
	blank := blank()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for row := 0; row < h; row++ {
		y := y0 + row
		out := dst[row*w : row*w+w]
		if y < 0 || y >= c.h {
			for i := range out {
				out[i] = blank
			}
			continue
		}
		for col := 0; col < w; col++ {
			x := x0 + col
			if x < 0 || x >= c.w {
				out[col] = blank
				continue
			}
			out[col] = c.cells[y*c.w+x]
		}
	}
}

// Snapshot returns a copy of every cell in row-major order.
func (c *Canvas) Snapshot() []Cell {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Cell, len(c.cells))
	copy(out, c.cells)
	return out
}

// ColorCounts tallies how many drawn cells use each palette color, and how many
// are drawn in total. A block counts under its fill; a character under its
// foreground.
func (c *Canvas) ColorCounts() (counts [PaletteSize]int, drawn int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, cell := range c.cells {
		if !cell.Drawn() {
			continue
		}
		drawn++
		if cell.IsBlock() {
			counts[cell.Fill&0x0f]++
		} else {
			counts[cell.Color&0x0f]++
		}
	}
	return counts, drawn
}

// NonEmpty counts cells that have been drawn on, blocks included.
func (c *Canvas) NonEmpty() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, cell := range c.cells {
		if cell.Drawn() {
			n++
		}
	}
	return n
}
