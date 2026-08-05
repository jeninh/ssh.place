package timelapse

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// AllName is the file holding the whole history.
const AllName = "all.gif"

// dayLayout is the date form used in a day file's name, and shown on the page.
const dayLayout = "2006-01-02"

// nameRE is what the HTTP handler will serve. Anchored and narrow, so a request
// can never reach outside the timelapse directory whatever it contains.
var nameRE = regexp.MustCompile(`^(all|day-\d{4}-\d{2}-\d{2})\.gif$`)

// ValidName reports whether name is a file this package produces.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Entry describes one rendered GIF.
type Entry struct {
	Name string
	// Day is the UTC day a daily file covers, zero for the whole-history file.
	Day     time.Time
	Bytes   int64
	Frames  int
	Drawn   int
	Total   int
	Events  int
	Made    time.Time
	IsWhole bool
}

// Label is what the page calls this entry.
func (e Entry) Label() string {
	if e.IsWhole {
		return "Since the beginning"
	}
	return e.Day.Format("Monday 2 January 2006")
}

// Store renders the daily and whole-history GIFs and keeps an index of them.
//
// Rendering is deliberately not on the request path. A render holds every frame
// in memory at once, so serving one on demand would let anyone with a browser
// decide when the server allocates a hundred megabytes.
type Store struct {
	// Dir is where GIFs are written. Created if missing.
	Dir string
	// LogPath is the event log to replay.
	LogPath string
	// Canvas size in cells.
	Width, Height int
	// Opt is the render used for every file. Width and Height are overwritten
	// from the fields above.
	Opt    Options
	Logger *slog.Logger
	// Now defaults to time.Now. Tests override it.
	Now func() time.Time

	mu      sync.RWMutex
	index   []Entry
	started time.Time
}

// Started returns the timestamp of the very first placement in the log, or the
// zero time if nothing has been rendered yet.
func (s *Store) Started() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.started
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// List returns the index, whole history first, then days newest first.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, len(s.index))
	copy(out, s.index)
	return out
}

// Path returns the file path for a validated name.
func (s *Store) Path(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("timelapse: %q is not a timelapse", name)
	}
	return filepath.Join(s.Dir, name), nil
}

// Refresh renders anything missing or stale: every complete day that has no file
// yet, and the whole-history file.
//
// Days already on disk are left alone. A finished day cannot change, so
// re-rendering it would burn minutes of CPU to produce identical bytes.
func (s *Store) Refresh(ctx context.Context) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("timelapse dir: %w", err)
	}

	events, err := LoadEvents(s.LogPath)
	if err != nil {
		return fmt.Errorf("load events: %w", err)
	}
	if len(events) == 0 {
		s.log().Info("timelapse: no events yet")
		return nil
	}

	opt := s.Opt
	opt.Width, opt.Height = s.Width, s.Height

	first := events[0].At.UTC()
	last := events[len(events)-1].At.UTC()
	today := s.now().UTC().Truncate(24 * time.Hour)

	var index []Entry

	// One file per complete UTC day. Today is skipped: it is not over, and a
	// partial file would be replaced within hours anyway.
	for day := first.Truncate(24 * time.Hour); day.Before(today) && !day.After(last); day = day.AddDate(0, 0, 1) {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := "day-" + day.Format(dayLayout) + ".gif"
		path := filepath.Join(s.Dir, name)

		before, within := Split(events, day, day.AddDate(0, 0, 1))
		if len(within) == 0 {
			continue
		}

		if st, err := os.Stat(path); err == nil {
			// Already rendered, and a finished day cannot change, so keep the file.
			// The counts still have to be recovered from the log: they are not stored
			// anywhere, and leaving them zero made every historical day on the page
			// read "0 frames" after any restart, which is every deploy.
			index = append(index, Entry{
				Name: name, Day: day, Bytes: st.Size(),
				Frames: frameCount(len(within), opt.Frames),
				Events: len(within),
				Total:  s.Width * s.Height, Made: st.ModTime(),
			})
			continue
		}

		// Seed with the state the day opened on, so a day reads as that day's
		// changes in context rather than as a board that starts empty.
		seed := Replay(s.Width, s.Height, before)
		dayOpt := opt
		res, err := s.write(ctx, path, seed, within, dayOpt)
		if err != nil {
			return fmt.Errorf("render %s: %w", name, err)
		}
		s.log().Info("timelapse rendered", "file", name,
			"events", res.Applied, "frames", res.Frames,
			"mb", fmt.Sprintf("%.1f", float64(res.Bytes)/(1<<20)))
		index = append(index, Entry{
			Name: name, Day: day, Bytes: res.Bytes, Frames: res.Frames,
			Drawn: res.Drawn, Total: res.Total, Events: res.Applied, Made: s.now().UTC(),
		})
	}

	// The whole history is always re-rendered, because it grows every day.
	if err := ctx.Err(); err != nil {
		return err
	}
	allPath := filepath.Join(s.Dir, AllName)
	res, err := s.write(ctx, allPath, nil, events, opt)
	if err != nil {
		return fmt.Errorf("render %s: %w", AllName, err)
	}
	s.log().Info("timelapse rendered", "file", AllName,
		"events", res.Applied, "frames", res.Frames,
		"mb", fmt.Sprintf("%.1f", float64(res.Bytes)/(1<<20)))

	whole := Entry{
		Name: AllName, Bytes: res.Bytes, Frames: res.Frames, Drawn: res.Drawn,
		Total: res.Total, Events: res.Applied, Made: s.now().UTC(), IsWhole: true,
	}

	// Newest day first; the whole history leads.
	sort.Slice(index, func(i, j int) bool { return index[i].Day.After(index[j].Day) })
	index = append([]Entry{whole}, index...)

	s.mu.Lock()
	s.index = index
	s.started = first
	s.mu.Unlock()
	return nil
}

// write renders to a temporary file and renames it into place, so a reader never
// sees a half-written GIF and a crash mid-render leaves no corrupt file behind.
func (s *Store) write(ctx context.Context, path string, seed State, events []Event, opt Options) (Result, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".timelapse-*")
	if err != nil {
		return Result{}, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name()) // no-op once the rename has happened
	}()

	res, err := Render(ctx, tmp, seed, events, opt)
	if err != nil {
		return Result{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Result{}, err
	}
	if err := tmp.Close(); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return Result{}, err
	}
	return res, nil
}

// frameCount mirrors what Render does with a frame budget, so a file recovered
// from disk reports the same number it was rendered with.
func frameCount(events, want int) int {
	if want < 1 {
		want = 1
	}
	if want > events {
		return events
	}
	return want
}

// Run refreshes once at start, then once after every UTC midnight, until ctx is
// done.
//
// Aligned to the UTC day rather than to a 24 hour timer from boot, so "that day"
// means a real calendar day and a restart does not shift the boundary.
func (s *Store) Run(ctx context.Context) {
	if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
		s.log().Error("timelapse refresh", "err", err)
	}
	for {
		now := s.now().UTC()
		// A few minutes past midnight, so the day's last placements are certainly
		// in the log before it is read.
		next := now.Truncate(24*time.Hour).AddDate(0, 0, 1).Add(2 * time.Minute)
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
			s.log().Error("timelapse refresh", "err", err)
		}
	}
}
