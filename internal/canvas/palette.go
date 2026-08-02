package canvas

import "image/color"

// PaletteSize is the number of colors available on the canvas. Indices are
// stored in a single byte per cell and map 1:1 onto the 16 standard ANSI
// colors, so the canvas renders identically on any terminal with color
// support and needs no truecolor negotiation.
const PaletteSize = 16

// PaletteEntry describes one slot of the fixed canvas palette.
type PaletteEntry struct {
	// Name is the human readable label shown in the status bar.
	Name string
	// ANSI is the standard ANSI color index (0-15) used when rendering to a
	// terminal.
	ANSI uint8
	// RGB is the xterm default value for ANSI, used by the PNG renderer.
	RGB color.RGBA
}

func rgb(r, g, b uint8) color.RGBA { return color.RGBA{R: r, G: g, B: b, A: 0xff} }

// Palette is the fixed canvas palette. Index 15 (white) is the default stamp
// color. Indices are part of the on-disk snapshot format, so entries may be
// renamed but must never be reordered.
var Palette = [PaletteSize]PaletteEntry{
	{Name: "black", ANSI: 0, RGB: rgb(0x00, 0x00, 0x00)},
	{Name: "maroon", ANSI: 1, RGB: rgb(0x80, 0x00, 0x00)},
	{Name: "green", ANSI: 2, RGB: rgb(0x00, 0x80, 0x00)},
	{Name: "olive", ANSI: 3, RGB: rgb(0x80, 0x80, 0x00)},
	{Name: "navy", ANSI: 4, RGB: rgb(0x00, 0x00, 0x80)},
	{Name: "purple", ANSI: 5, RGB: rgb(0x80, 0x00, 0x80)},
	{Name: "teal", ANSI: 6, RGB: rgb(0x00, 0x80, 0x80)},
	{Name: "silver", ANSI: 7, RGB: rgb(0xc0, 0xc0, 0xc0)},
	{Name: "gray", ANSI: 8, RGB: rgb(0x80, 0x80, 0x80)},
	{Name: "red", ANSI: 9, RGB: rgb(0xff, 0x00, 0x00)},
	{Name: "lime", ANSI: 10, RGB: rgb(0x00, 0xff, 0x00)},
	{Name: "yellow", ANSI: 11, RGB: rgb(0xff, 0xff, 0x00)},
	{Name: "blue", ANSI: 12, RGB: rgb(0x00, 0x00, 0xff)},
	{Name: "fuchsia", ANSI: 13, RGB: rgb(0xff, 0x00, 0xff)},
	{Name: "aqua", ANSI: 14, RGB: rgb(0x00, 0xff, 0xff)},
	{Name: "white", ANSI: 15, RGB: rgb(0xff, 0xff, 0xff)},
}

// DefaultColor is the palette index new sessions start with.
const DefaultColor uint8 = 15

// NoFill is the Fill value of a cell with no background of its own, which shows
// the terminal's own background through.
const NoFill uint8 = 0xff

// ValidColor reports whether i addresses a palette slot.
func ValidColor(i uint8) bool { return int(i) < PaletteSize }

// ValidFill reports whether i is a palette slot or NoFill.
func ValidFill(i uint8) bool { return i == NoFill || ValidColor(i) }
