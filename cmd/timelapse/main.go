// Command timelapse renders the event log to an animated GIF by hand.
//
// The server does this on its own once a day and serves the results at
// /timelapse. This is for one-offs: a different size, a narrower window, or a
// render from a log copied off the box.
//
//	go run ./cmd/timelapse -in data/events.jsonl -out timelapse.gif
//	go run ./cmd/timelapse -in events.jsonl -day 2026-08-03 -out day.gif
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jeninh/ssh.place/internal/canvas"
	"github.com/jeninh/ssh.place/internal/timelapse"
)

func main() {
	in := flag.String("in", "data/events.jsonl", "event log to replay")
	out := flag.String("out", "timelapse.gif", "GIF to write")
	width := flag.Int("width", canvas.DefaultWidth, "canvas width in cells")
	height := flag.Int("height", canvas.DefaultHeight, "canvas height in cells")
	day := flag.String("day", "", "render only this UTC day (YYYY-MM-DD), seeded with the state it opened on")
	opt := timelapse.DefaultOptions(canvas.DefaultWidth, canvas.DefaultHeight)
	flag.IntVar(&opt.Scale, "scale", opt.Scale, "pixels per cell")
	flag.IntVar(&opt.Frames, "frames", opt.Frames, "number of frames")
	flag.IntVar(&opt.Delay, "delay", opt.Delay, "delay between frames, hundredths of a second")
	flag.IntVar(&opt.Hold, "hold", opt.Hold, "extra delay on the final frame")
	flag.StringVar(&opt.Caption, "caption", opt.Caption, "text along the bottom (empty for none)")
	flag.BoolVar(&opt.Bar, "bar", opt.Bar, "draw a progress bar")
	flag.Parse()

	opt.Width, opt.Height = *width, *height

	if err := run(*in, *out, *day, opt); err != nil {
		fmt.Fprintln(os.Stderr, "timelapse:", err)
		os.Exit(1)
	}
}

func run(in, out, day string, opt timelapse.Options) error {
	events, err := timelapse.LoadEvents(in)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("no events in %s", in)
	}

	var seed []uint8
	if day != "" {
		from, err := time.Parse("2006-01-02", day)
		if err != nil {
			return fmt.Errorf("-day must look like 2026-08-03: %w", err)
		}
		from = from.UTC()
		before, within := timelapse.Split(events, from, from.AddDate(0, 0, 1))
		if len(within) == 0 {
			return fmt.Errorf("no events on %s", day)
		}
		// Seeded with the state the day opened on, so it shows that day's changes
		// in context rather than starting from an empty board.
		seed = timelapse.Replay(opt.Width, opt.Height, before)
		events = within
	}

	fh, err := os.Create(out)
	if err != nil {
		return err
	}
	defer fh.Close()

	res, err := timelapse.Render(context.Background(), fh, seed, events, opt)
	if err != nil {
		return err
	}
	fmt.Printf("%d events, %s to %s (%s)\n", res.Applied,
		res.From.UTC().Format("2006-01-02 15:04"), res.To.UTC().Format("2006-01-02 15:04"),
		res.To.Sub(res.From).Round(time.Minute))
	fmt.Printf("%d frames, %dx%d, %.1f MB\n", res.Frames, res.Width, res.Height,
		float64(res.Bytes)/(1<<20))
	fmt.Printf("final frame: %d/%d cells drawn (%.1f%%)\n", res.Drawn, res.Total,
		100*float64(res.Drawn)/float64(res.Total))
	return nil
}
