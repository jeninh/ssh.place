package timelapse

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 3, 0, 17, 0, 0, time.UTC)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A name arrives from a URL path, so this is the boundary that stops a request
// reaching a file we never meant to serve.
func TestValidNameRejectsAnythingElse(t *testing.T) {
	good := []string{"all.gif", "day-2026-08-03.gif", "day-1999-12-31.gif"}
	for _, n := range good {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	bad := []string{
		"", "all", "all.gif.gif", "ALL.GIF",
		"../all.gif", "../../etc/passwd", "day-2026-08-03.gif/../../x",
		"/etc/passwd", "day-2026-8-3.gif", "day-.gif", "canvas.json",
		"all.gif\x00", "all.gif\n", "day-2026-08-03.png",
		"..%2fall.gif", "sub/all.gif", `..\all.gif`,
	}
	for _, n := range bad {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

func TestPathRefusesBadNames(t *testing.T) {
	s := &Store{Dir: "/data/timelapse"}
	if _, err := s.Path("../../etc/passwd"); err == nil {
		t.Fatal("Path accepted a traversal")
	}
	got, err := s.Path("all.gif")
	if err != nil {
		t.Fatalf("Path(all.gif) = %v", err)
	}
	if want := filepath.Join("/data/timelapse", "all.gif"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

// writeLog builds an event log spanning the given days, a handful of placements
// per day so every day has something to animate.
func writeLog(t *testing.T, days int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var b strings.Builder
	for d := 0; d < days; d++ {
		day := base.AddDate(0, 0, d)
		for i := 0; i < 40; i++ {
			at := day.Add(time.Duration(i) * time.Minute)
			b.WriteString(`{"t":"` + at.Format(time.RFC3339Nano) +
				`","id":"SHA256:x","x":` + itoa(i) + `,"y":` + itoa(d) +
				`,"r":" ","c":` + itoa(i%16) + `,"block":true}` + "\n")
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

func newStore(t *testing.T, log string, now time.Time) *Store {
	t.Helper()
	opt := DefaultOptions(20, 4)
	// Small and few: these tests are about the index, not the picture.
	opt.Scale, opt.Frames = 2, 4
	return &Store{
		Dir: filepath.Join(t.TempDir(), "timelapse"), LogPath: log,
		Width: 20, Height: 4, Opt: opt, Logger: quietLogger(),
		Now: func() time.Time { return now },
	}
}

func TestRefreshBackfillsCompleteDaysAndSkipsToday(t *testing.T) {
	// Three days of history, and "now" is partway through the third. The first two
	// are finished and should be rendered; today is not over and must be skipped.
	log := writeLog(t, 3)
	now := base.AddDate(0, 0, 2).Add(6 * time.Hour)
	s := newStore(t, log, now)

	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	entries := s.List()
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (all + two finished days): %+v", len(entries), entries)
	}
	// Whole history leads, then days newest first.
	if !entries[0].IsWhole {
		t.Errorf("first entry = %+v, want the whole history", entries[0])
	}
	if got, want := entries[1].Name, "day-2026-08-04.gif"; got != want {
		t.Errorf("second entry = %q, want %q", got, want)
	}
	if got, want := entries[2].Name, "day-2026-08-03.gif"; got != want {
		t.Errorf("third entry = %q, want %q", got, want)
	}
	for _, e := range entries {
		if e.Bytes <= 0 {
			t.Errorf("%s has no bytes", e.Name)
		}
		if _, err := os.Stat(filepath.Join(s.Dir, e.Name)); err != nil {
			t.Errorf("%s not on disk: %v", e.Name, err)
		}
	}
	if !s.Started().Equal(base) {
		t.Errorf("Started = %s, want the first placement at %s", s.Started(), base)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "day-2026-08-05.gif")); err == nil {
		t.Error("rendered a day that has not happened")
	}
}

func TestRefreshLeavesFinishedDaysAlone(t *testing.T) {
	// A finished day cannot change, so re-rendering it would burn CPU to produce
	// identical bytes.
	log := writeLog(t, 2)
	now := base.AddDate(0, 0, 1).Add(6 * time.Hour)
	s := newStore(t, log, now)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	day := filepath.Join(s.Dir, "day-2026-08-03.gif")
	st1, err := os.Stat(day)
	if err != nil {
		t.Fatal(err)
	}
	// Backdate it so an unchanged mtime is meaningful.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(day, old, old); err != nil {
		t.Fatal(err)
	}

	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(day)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ModTime().Equal(old) {
		t.Error("a finished day was re-rendered, want it left alone")
	}
	if st1.Size() != st2.Size() {
		t.Error("finished day changed size")
	}
}

func TestRefreshWithNoEventsIsNotAnError(t *testing.T) {
	// A brand new deployment has an empty log, and that must not stop the server.
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newStore(t, path, base)
	if err := s.Refresh(context.Background()); err != nil {
		t.Errorf("Refresh on an empty log = %v, want nil", err)
	}
	if len(s.List()) != 0 {
		t.Error("indexed something from an empty log")
	}
}

func TestRefreshHonoursContextCancellation(t *testing.T) {
	log := writeLog(t, 3)
	s := newStore(t, log, base.AddDate(0, 0, 2).Add(6*time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Refresh(ctx); err == nil {
		t.Error("Refresh ignored a cancelled context")
	}
}

// A day is seeded with the state it opened on, so its first frame already holds
// the previous days' work rather than starting from an empty board.
func TestDayIsSeededWithItsOpeningState(t *testing.T) {
	events, err := LoadEvents(writeLog(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	day2 := base.AddDate(0, 0, 1).Truncate(24 * time.Hour)
	before, within := Split(events, day2, day2.AddDate(0, 0, 1))
	if len(before) == 0 || len(within) == 0 {
		t.Fatalf("split gave %d before and %d within", len(before), len(within))
	}
	seed := Replay(20, 4, before)
	if drawnCount(seed) == 0 {
		t.Fatal("seed is empty, so day two would start from a blank canvas")
	}
	// Every event within the window must fall inside the day.
	for _, e := range within {
		if e.At.Before(day2) || !e.At.Before(day2.AddDate(0, 0, 1)) {
			t.Fatalf("event at %s is outside %s", e.At, day2)
		}
	}
}
