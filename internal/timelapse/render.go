// Package timelapse turns the event log into animated GIFs of the canvas
// filling up.
//
// GIF because the canvas is already sixteen palette colours, which is exactly
// what GIF stores natively: no colour quantisation, no encoder to shell out to,
// and nothing outside the standard library plus the bitmap font already vendored
// for the PNG endpoint.
package timelapse

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"io"
	"os"
	"sort"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/jeninh/ssh.place/internal/canvas"
)

// Event is one placement, as the event log records it. Only the fields the
// animation needs are decoded.
type Event struct {
	At    time.Time `json:"t"`
	X     int       `json:"x"`
	Y     int       `json:"y"`
	Color uint8     `json:"c"`
	Block bool      `json:"block"`
}

// Options controls one render.
type Options struct {
	// Width and Height are the canvas size in cells.
	Width, Height int
	// Scale is pixels per cell.
	Scale int
	// Frames is how many frames the animation holds. Frames are spaced evenly
	// over events rather than over wall-clock time: spacing by time would spend
	// most of the animation on whichever hours were quiet.
	Frames int
	// Delay between frames, and extra delay on the last one, both in hundredths
	// of a second as GIF measures them.
	Delay, Hold int
	// Caption is drawn along the bottom. Empty draws no caption row.
	Caption string
	// Bar draws a progress strip above the caption.
	Bar bool
}

// DefaultOptions is a reasonable render for a 200x60 canvas.
func DefaultOptions(w, h int) Options {
	return Options{
		Width: w, Height: h,
		Scale: 5, Frames: 300, Delay: 6, Hold: 300,
		Caption: "ssh.place - jeninh/ssh.place",
		Bar:     true,
	}
}

// Result reports what a render produced.
type Result struct {
	Frames  int
	Width   int
	Height  int
	Bytes   int64
	Drawn   int
	Total   int
	From    time.Time
	To      time.Time
	Applied int
}

const (
	// barHeight and captionHeight are in pixels. The caption row has to clear the
	// 13 pixel bitmap font with a little air above and below.
	captionHeight = 18
	captionPadX   = 4
)

// State is the canvas as the animation tracks it: palette index plus one, so
// zero means an undrawn cell and the background index can be zero too.
type State = []uint8

// Replay applies every event to a fresh state and returns it. Used both to seed
// a single day's animation with the state it opened on, and to check a replay
// against the live canvas.
func Replay(w, h int, events []Event) State {
	cells := make(State, w*h)
	apply(cells, w, h, events)
	return cells
}

func apply(c State, w, h int, events []Event) {
	for _, e := range events {
		if e.X < 0 || e.X >= w || e.Y < 0 || e.Y >= h {
			continue
		}
		if e.Block {
			c[e.Y*w+e.X] = e.Color + 1
		} else {
			// A space is how you erase, and characters never reach the canvas while
			// it runs blocks-only. Anything that is not a block clears the cell.
			c[e.Y*w+e.X] = 0
		}
	}
}

func drawnCount(c State) int {
	n := 0
	for _, v := range c {
		if v != 0 {
			n++
		}
	}
	return n
}

// Render animates events on top of seed and writes a GIF to out.
//
// seed is the canvas state the animation opens on, so a single day can be shown
// in the context it started from rather than from an empty board. Pass nil to
// start blank.
func Render(ctx context.Context, out io.Writer, seed State, events []Event, opt Options) (Result, error) {
	if opt.Width <= 0 || opt.Height <= 0 {
		return Result{}, fmt.Errorf("timelapse: canvas size %dx%d", opt.Width, opt.Height)
	}
	if len(events) == 0 {
		return Result{}, fmt.Errorf("timelapse: no events to animate")
	}
	if opt.Scale < 1 {
		opt.Scale = 1
	}
	nFrames := opt.Frames
	if nFrames < 1 {
		nFrames = 1
	}
	if nFrames > len(events) {
		nFrames = len(events)
	}

	// Always a fresh copy: callers reuse a seed across renders, and animating in
	// place would leave the second render starting from the first one's ending.
	cells := make(State, opt.Width*opt.Height)
	copy(cells, seed)

	pal := paletteWithBackdrop()
	barH := 0
	if opt.Bar {
		barH = opt.Scale
	}
	capH := 0
	if opt.Caption != "" {
		capH = captionHeight
	}
	pxW := opt.Width * opt.Scale
	pxH := opt.Height*opt.Scale + barH + capH

	anim := &gif.GIF{LoopCount: 0}
	per := len(events) / nFrames
	next := 0

	for f := 0; f < nFrames; f++ {
		// Checked per frame, not just per file. A single render of a busy day takes
		// seconds, and without this a shutdown waits for all of it.
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		upto := (f + 1) * per
		if f == nFrames-1 {
			upto = len(events)
		}
		apply(cells, opt.Width, opt.Height, events[next:upto])
		next = upto

		img := image.NewPaletted(image.Rect(0, 0, pxW, pxH), pal)
		for y := 0; y < opt.Height; y++ {
			for x := 0; x < opt.Width; x++ {
				if ci := cells[y*opt.Width+x]; ci != 0 {
					fillRect(img, x*opt.Scale, y*opt.Scale, opt.Scale, opt.Scale, ci)
				}
			}
		}
		if opt.Bar {
			// Two tones, so the bar still reads as a bar against a full canvas.
			y0 := opt.Height * opt.Scale
			fillRect(img, 0, y0, pxW, barH, idxDark)
			fillRect(img, 0, y0, pxW*next/len(events), barH, idxLight)
		}
		if capH > 0 {
			drawCaption(img, opt.Caption, opt.Height*opt.Scale+barH, capH)
		}

		d := opt.Delay
		if f == nFrames-1 {
			d += opt.Hold
		}
		anim.Image = append(anim.Image, img)
		anim.Delay = append(anim.Delay, d)
	}

	counter := &countWriter{w: out}
	bw := bufio.NewWriterSize(counter, 1<<16)
	if err := gif.EncodeAll(bw, anim); err != nil {
		return Result{}, err
	}
	if err := bw.Flush(); err != nil {
		return Result{}, err
	}

	return Result{
		Frames: len(anim.Image), Width: pxW, Height: pxH,
		Bytes: counter.n, Drawn: drawnCount(cells), Total: opt.Width * opt.Height,
		From: events[0].At, To: events[len(events)-1].At, Applied: len(events),
	}, nil
}

// Palette indices. Index 0 is the backdrop for undrawn cells; the sixteen canvas
// colours follow, offset by one.
const (
	idxBackdrop = 0
	idxDark     = 1 + 0  // canvas black
	idxLight    = 1 + 15 // canvas white
)

func paletteWithBackdrop() color.Palette {
	pal := make(color.Palette, 0, canvas.PaletteSize+1)
	pal = append(pal, color.RGBA{0x0d, 0x11, 0x17, 0xff}) // matches the web page background
	for i := 0; i < canvas.PaletteSize; i++ {
		c := canvas.Palette[i].RGB
		pal = append(pal, color.RGBA{c.R, c.G, c.B, 0xff})
	}
	return pal
}

// drawCaption writes the caption into a strip at y0, using the same bitmap font
// as the PNG endpoint so there are still no font files to ship.
func drawCaption(img *image.Paletted, text string, y0, h int) {
	fillRect(img, 0, y0, img.Bounds().Dx(), h, idxBackdrop)
	face := basicfont.Face7x13
	// Baseline sits so the 13 pixel font is vertically centred in the strip.
	baseline := y0 + (h+face.Ascent-face.Descent)/2
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(img.Palette[idxLight]),
		Face: face,
		Dot:  fixed.P(captionPadX, baseline),
	}
	d.DrawString(text)
}

func fillRect(img *image.Paletted, x0, y0, w, h int, ci uint8) {
	b := img.Bounds()
	for y := y0; y < y0+h && y < b.Max.Y; y++ {
		if y < b.Min.Y {
			continue
		}
		row := img.Pix[y*img.Stride:]
		for x := x0; x < x0+w && x < b.Max.X; x++ {
			row[x] = ci
		}
	}
}

// countWriter records how many bytes reached the underlying writer, so a render
// can report its own size without stat-ing a file it may not own.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// interface check: the caption relies on Paletted being a draw.Image.
var _ draw.Image = (*image.Paletted)(nil)

// LoadEvents reads and parses an event log, oldest first.
func LoadEvents(path string) ([]Event, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	var out []Event
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			// The log is append-only and its last line can be torn mid-write.
			// Skipping beats refusing to run.
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// The log is written in order, but a restart could interleave, and replaying
	// out of order would show cells changing backwards.
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Split returns the events before from, and those within [from, to).
//
// The first group is what the day opened on; the second is what to animate.
//
// events must be sorted by time, which LoadEvents guarantees. The results are
// subslices, not copies: Refresh calls this once per day, and copying the whole
// history each time turned a cheap loop into one that allocated the entire log
// again for every day rendered.
func Split(events []Event, from, to time.Time) (before, within []Event) {
	lo := sort.Search(len(events), func(i int) bool { return !events[i].At.Before(from) })
	hi := sort.Search(len(events), func(i int) bool { return !events[i].At.Before(to) })
	return events[:lo], events[lo:hi]
}
