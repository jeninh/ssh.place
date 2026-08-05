package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeninh/ssh.place/internal/timelapse"
)

var lapseBase = time.Date(2026, 8, 3, 0, 17, 0, 0, time.UTC)

// newLapses builds a store with two days of real rendered GIFs behind it.
func newLapses(t *testing.T) *timelapse.Store {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "events.jsonl")

	var b strings.Builder
	for d := 0; d < 2; d++ {
		day := lapseBase.AddDate(0, 0, d)
		for i := 0; i < 30; i++ {
			at := day.Add(time.Duration(i) * time.Minute)
			b.WriteString(`{"t":"` + at.Format(time.RFC3339Nano) +
				`","id":"SHA256:x","x":` + itoa(i) + `,"y":` + itoa(d) +
				`,"r":" ","c":` + itoa(i%16) + `,"block":true}` + "\n")
		}
	}
	if err := os.WriteFile(log, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	opt := timelapse.DefaultOptions(40, 10)
	opt.Scale, opt.Frames = 2, 3
	s := &timelapse.Store{
		Dir: filepath.Join(dir, "timelapse"), LogPath: log,
		Width: 40, Height: 10, Opt: opt,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return lapseBase.AddDate(0, 0, 1).Add(6 * time.Hour) },
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return s
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

func TestTimelapsePageListsRenders(t *testing.T) {
	h := Handler(newApp(t), newLapses(t))
	rec := get(t, h, "/timelapse")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Since the beginning",
		"/timelapse/all.gif",
		"/timelapse/day-2026-08-03.gif",
		// The first pixel's timestamp, which is the whole reason it is on the page.
		"3 August 2026",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// A day recovered from disk must not report zero frames, which is what the
	// page showed for every historical day after any restart.
	if strings.Contains(body, "0 frames") {
		t.Error("page reports 0 frames for a rendered day")
	}
}

func TestTimelapsePageWithoutAStore(t *testing.T) {
	// Timelapses are off when the event log is off, and the page still has to work.
	h := Handler(newApp(t), nil)
	rec := get(t, h, "/timelapse")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No timelapses yet") {
		t.Error("page did not explain that there is nothing to show")
	}
}

func TestTimelapseFileServesAGIF(t *testing.T) {
	h := Handler(newApp(t), newLapses(t))
	rec := get(t, h, "/timelapse/all.gif")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/gif" {
		t.Errorf("Content-Type = %q, want image/gif", got)
	}
	body := rec.Body.Bytes()
	if len(body) < 6 || string(body[:6]) != "GIF89a" {
		t.Errorf("body does not start with a GIF header: %q", body[:min(6, len(body))])
	}
}

// The name comes straight off the URL, so this is the boundary that has to hold.
func TestTimelapseFileRejectsTraversal(t *testing.T) {
	lapses := newLapses(t)
	// A file that exists but is not ours, one directory up from the GIFs.
	secret := filepath.Join(filepath.Dir(lapses.Dir), "events.jsonl")
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	h := Handler(newApp(t), lapses)
	for _, path := range []string{
		"/timelapse/../events.jsonl",
		"/timelapse/%2e%2e%2fevents.jsonl",
		"/timelapse/..%2Fevents.jsonl",
		"/timelapse/....//events.jsonl",
		"/timelapse/canvas.json",
		"/timelapse/all.gif.bak",
		"/timelapse/day-2026-8-3.gif",
		"/timelapse/",
	} {
		rec := get(t, h, path)
		if rec.Code == http.StatusOK {
			t.Errorf("%s returned 200, want it refused. body starts: %q",
				path, rec.Body.String()[:min(60, rec.Body.Len())])
		}
		if strings.Contains(rec.Body.String(), "SHA256:") {
			t.Errorf("%s leaked event log contents", path)
		}
	}
}

func TestTimelapseIsLinkedFromEveryPage(t *testing.T) {
	h := Handler(newApp(t), newLapses(t))
	for _, path := range []string{"/", "/stats", "/timelapse"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `href="/timelapse"`) {
			t.Errorf("%s does not link to the timelapse page", path)
		}
	}
}
