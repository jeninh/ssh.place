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

	decorNone  = 0
	decorCsr   = 1
	decorCount = 2
)

// bgOfFill returns the background slot for a block filled with the given
// palette color.
func bgOfFill(fill uint8) uint8 { return bgPaletteBase + fill&0x0f }

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

	if decor == decorCsr {
		parts = append(parts, "7") // reverse video
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
