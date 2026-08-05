package tui

import (
	"strconv"
	"strings"
	"sync"

	"github.com/jeninh/ssh.place/internal/canvas"
)

// A cell has exactly one background, so a block's fill and the recently-placed
// marker compete for the same slot.
//
// Palette indices are offset by one so that zero means "no background": that
// way the zero value of a styleKey is a plain cell rather than a black one.
const (
	bgNone        = 0
	bgPaletteBase = 1
	bgHlFresh     = bgPaletteBase + canvas.PaletteSize // just placed
	bgHlFade      = bgHlFresh + 1                      // fading out
	bgStates      = bgHlFade + 1

	decorNone = 0
	// decorReverse is the cursor on a cell whose foreground and background
	// differ, where swapping them is both visible and keeps the content legible.
	decorReverse = 1
	// decorBold is the cursor on a solid block, where reverse video would swap a
	// color with itself. The marker supplies its own contrast, so it only needs
	// emphasis.
	decorBold  = 2
	decorCount = 3
)

// bgOfFill returns the background slot for a block filled with the given
// palette color.
func bgOfFill(fill uint8) uint8 { return bgPaletteBase + fill&0x0f }

// cursorGlyph marks where the cursor is. Deliberately not one of the frame
// characters: a cursor that looks like a border corner is confusing when it sits
// near one.
//
// The cursor used to be reverse video, which is invisible on a solid block: a
// block sets its foreground and its fill to the same color, so swapping them
// changes nothing. A marker in a deliberately contrasting color shows up on
// every color in the palette, and keeps the cell's own color visible underneath
// rather than blanking it.
const cursorGlyph = 'x'

// anchorGlyph marks the fixed corner of a rectangular selection. Distinct from
// the cursor so it is obvious which end moves.
const anchorGlyph = 'o'

// Contrast colors for the cursor marker: white on dark cells, black on light
// ones.
const (
	cursorOnDark  = 15 // white
	cursorOnLight = 0  // black
)

// paletteIsLight reports whether a palette entry is bright enough that a black
// marker reads better on it than a white one. Uses the standard luminance
// weighting rather than a plain average, because the eye is far more sensitive
// to green than to blue.
func paletteIsLight(i uint8) bool {
	c := canvas.Palette[i&0x0f].RGB
	lum := 0.2126*float64(c.R) + 0.7152*float64(c.G) + 0.0722*float64(c.B)
	return lum > 140
}

// cursorFg picks the marker color for a cursor sitting on the given background.
func cursorFg(bg uint8) uint8 {
	if bg >= bgPaletteBase && bg < bgPaletteBase+canvas.PaletteSize {
		if paletteIsLight(bg - bgPaletteBase) {
			return cursorOnLight
		}
	}
	// An unfilled cell shows the terminal's own background, which we cannot
	// measure. Assume dark: overwhelmingly the common case for a terminal.
	return cursorOnDark
}

// styleKey packs everything that affects a cell's appearance into one
// comparable value, so the renderer emits one escape sequence per run of
// identical cells rather than one per cell.
type styleKey struct {
	fg    uint8
	bg    uint8
	decor uint8
}

// painter turns styleKeys into escape sequences.
//
// Sequences are precomputed, and the whole table is shared process-wide: it
// depends only on whether the client supports color, and a busy canvas repaints
// several times a second for every connected session.
type painter struct {
	color bool
	seqs  [canvas.PaletteSize][bgStates][decorCount]string
}

const reset = "\x1b[0m"

var (
	colorPainter = sync.OnceValue(func() *painter { return buildPainter(true) })
	plainPainter = sync.OnceValue(func() *painter { return buildPainter(false) })
)

// newPainter returns the shared painter for a client with or without color.
func newPainter(color bool) *painter {
	if color {
		return colorPainter()
	}
	return plainPainter()
}

func buildPainter(color bool) *painter {
	p := &painter{color: color}
	if !color {
		return p
	}
	for fg := 0; fg < canvas.PaletteSize; fg++ {
		for bg := 0; bg < bgStates; bg++ {
			for d := 0; d < decorCount; d++ {
				p.seqs[fg][bg][d] = buildSeq(uint8(fg), uint8(bg), uint8(d))
			}
		}
	}
	return p
}

// buildSeq assembles an SGR sequence. Foregrounds use the 30-37 / 90-97 ranges
// and backgrounds 40-47 / 100-107, which every color-capable terminal
// understands without needing 256-color or truecolor support.
func buildSeq(fg, bg, decor uint8) string {
	parts := []string{fgCode(canvas.Palette[fg&0x0f].ANSI)}

	switch bg {
	case bgNone:
		// Leave the terminal's own background showing.
	case bgHlFresh:
		parts = append(parts, bgCode(7)) // silver
	case bgHlFade:
		parts = append(parts, bgCode(8)) // bright black
	default:
		parts = append(parts, bgCode(canvas.Palette[(bg-bgPaletteBase)&0x0f].ANSI))
	}

	switch decor {
	case decorReverse:
		parts = append(parts, "7")
	case decorBold:
		parts = append(parts, "1")
	}

	return "\x1b[" + strings.Join(parts, ";") + "m"
}

func fgCode(ansi uint8) string {
	if ansi < 8 {
		return strconv.Itoa(30 + int(ansi))
	}
	return strconv.Itoa(90 + int(ansi-8))
}

func bgCode(ansi uint8) string {
	if ansi < 8 {
		return strconv.Itoa(40 + int(ansi))
	}
	return strconv.Itoa(100 + int(ansi-8))
}

// paint writes one run of identical cells to b.
func (p *painter) paint(b *strings.Builder, k styleKey, run string) {
	if !p.color {
		b.WriteString(run)
		return
	}
	seq := p.seqs[k.fg&0x0f][k.bg%bgStates][k.decor%decorCount]
	if seq == "" {
		b.WriteString(run)
		return
	}
	b.WriteString(seq)
	b.WriteString(run)
	b.WriteString(reset)
}

// blockFallback is what a solid block looks like to a client with no color
// support, where a background is invisible. Without it, blocks would be
// indistinguishable from blank canvas.
const blockFallback = '#'
