package web

import (
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeninh/ssh.place/internal/app"
	"github.com/jeninh/ssh.place/internal/canvas"
	"github.com/jeninh/ssh.place/internal/hub"
	"github.com/jeninh/ssh.place/internal/ratelimit"
)

func newApp(t *testing.T) *app.App {
	t.Helper()
	return &app.App{
		Canvas:  canvas.New(40, 10),
		Hub:     hub.New(10, 5),
		Limiter: ratelimit.New(15*time.Second, 5, 15*time.Second),
	}
}

func get(t *testing.T, h http.Handler, path string, headers ...[2]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, kv := range headers {
		req.Header.Set(kv[0], kv[1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIndexServesThePage(t *testing.T) {
	a := newApp(t)
	rec := get(t, Handler(a, nil), "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"ssh.place", "ssh ssh.place", "/canvas.png"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestUnknownPathIs404(t *testing.T) {
	if rec := get(t, Handler(newApp(t), nil), "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestCanvasPNGDecodesAtTheRightSize(t *testing.T) {
	a := newApp(t)
	if err := a.Canvas.Set(3, 4, 'W', 9); err != nil {
		t.Fatal(err)
	}

	rec := get(t, Handler(a, nil), "/canvas.png")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}

	img, err := png.Decode(rec.Body)
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	w, h := a.Canvas.Size()
	want := image.Rect(0, 0, w*cellW, h*cellH)
	if got := img.Bounds(); got != want {
		t.Errorf("bounds = %v, want %v", got, want)
	}
}

// The glyph has to actually be drawn in its palette color, or the PNG would be
// a blank rectangle that still passed every structural check.
func TestPNGDrawsGlyphsInPaletteColors(t *testing.T) {
	a := newApp(t)
	const colorIdx = 9 // red
	if err := a.Canvas.Set(0, 0, 'X', colorIdx); err != nil {
		t.Fatal(err)
	}

	img := RenderImage(a.Canvas)
	want := canvas.Palette[colorIdx].RGB

	found := false
	for y := 0; y < cellH && !found; y++ {
		for x := 0; x < cellW; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if uint8(r>>8) == want.R && uint8(g>>8) == want.G && uint8(b>>8) == want.B {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("no pixel of the glyph was drawn in palette color %d (%s)", colorIdx, canvas.Palette[colorIdx].Name)
	}

	// A cell that was never drawn on stays background.
	r, g, b, _ := img.At(20*cellW+3, 5*cellH+6).RGBA()
	if uint8(r>>8) != background.R || uint8(g>>8) != background.G || uint8(b>>8) != background.B {
		t.Errorf("empty cell = (%d,%d,%d), want the background color", r>>8, g>>8, b>>8)
	}
}

// The PNG is cached against the canvas version, so an unchanged canvas must
// return the identical bytes and a changed one must not.
func TestPNGCacheTracksCanvasVersion(t *testing.T) {
	a := newApp(t)
	c := newPNGCache(a.Canvas)

	first, v1, err := c.bytes()
	if err != nil {
		t.Fatal(err)
	}
	second, v2, err := c.bytes()
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Errorf("version changed without a canvas write: %d then %d", v1, v2)
	}
	if &first[0] != &second[0] {
		t.Error("cache re-encoded an unchanged canvas")
	}

	if err := a.Canvas.Set(1, 1, 'z', 3); err != nil {
		t.Fatal(err)
	}
	third, v3, err := c.bytes()
	if err != nil {
		t.Fatal(err)
	}
	if v3 == v1 {
		t.Error("version did not advance after a canvas write")
	}
	if len(third) == len(first) && &third[0] == &first[0] {
		t.Error("cache served stale bytes after a canvas write")
	}
}

func TestCanvasPNGETagRevalidation(t *testing.T) {
	a := newApp(t)
	h := Handler(a, nil)

	first := get(t, h, "/canvas.png")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the PNG response")
	}

	// A matching ETag saves re-sending the image.
	notModified := get(t, h, "/canvas.png", [2]string{"If-None-Match", etag})
	if notModified.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", notModified.Code)
	}
	if notModified.Body.Len() != 0 {
		t.Errorf("304 carried %d bytes of body, want none", notModified.Body.Len())
	}

	// Drawing changes the ETag, so the browser fetches the new frame.
	if err := a.Canvas.Set(2, 2, 'q', 1); err != nil {
		t.Fatal(err)
	}
	after := get(t, h, "/canvas.png", [2]string{"If-None-Match", etag})
	if after.Code != http.StatusOK {
		t.Errorf("status = %d after the canvas changed, want 200", after.Code)
	}
	if got := after.Header().Get("ETag"); got == etag {
		t.Error("ETag did not change with the canvas")
	}
}

func TestCanvasTXT(t *testing.T) {
	a := newApp(t)
	for i, r := range "hey" {
		if err := a.Canvas.Set(i+1, 0, r, 1); err != nil {
			t.Fatal(err)
		}
	}

	rec := get(t, Handler(a, nil), "/canvas.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	lines := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n")
	if got, want := len(lines), a.Canvas.Height(); got != want {
		t.Fatalf("got %d lines, want %d", got, want)
	}
	if lines[0] != " hey" {
		t.Errorf("first line = %q, want %q (trailing blanks trimmed)", lines[0], " hey")
	}
	if lines[1] != "" {
		t.Errorf("blank line = %q, want empty", lines[1])
	}
}

// canvas.txt must not hand back the canvas's own row strings, or trimming them
// for the response would corrupt the live canvas.
func TestCanvasTXTDoesNotMutateCanvas(t *testing.T) {
	a := newApp(t)
	if err := a.Canvas.Set(0, 0, 'a', 1); err != nil {
		t.Fatal(err)
	}
	h := Handler(a, nil)
	get(t, h, "/canvas.txt")

	rows := a.Canvas.Text()
	if got, want := len(rows[0]), a.Canvas.Width(); got != want {
		t.Errorf("canvas row is %d wide after serving text, want %d", got, want)
	}
}

func TestStatsJSON(t *testing.T) {
	a := newApp(t)
	s, err := a.Hub.Add("1.1.1.1", "k", true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Hub.Remove(s)
	if err := a.Canvas.Set(0, 0, 'a', 1); err != nil {
		t.Fatal(err)
	}
	if err := a.Canvas.Set(1, 0, 'b', 1); err != nil {
		t.Fatal(err)
	}

	rec := get(t, Handler(a, nil), "/stats.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got struct {
		Width    int     `json:"width"`
		Height   int     `json:"height"`
		Online   int     `json:"online"`
		Placed   int     `json:"cells_drawn"`
		Cooldown float64 `json:"cooldown_seconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if got.Width != 40 || got.Height != 10 {
		t.Errorf("size = %dx%d, want 40x10", got.Width, got.Height)
	}
	if got.Online != 1 {
		t.Errorf("online = %d, want 1", got.Online)
	}
	if got.Placed != 2 {
		t.Errorf("cells_drawn = %d, want 2", got.Placed)
	}
	if got.Cooldown != 15 {
		t.Errorf("cooldown_seconds = %v, want 15", got.Cooldown)
	}
}

func TestHealthz(t *testing.T) {
	rec := get(t, Handler(newApp(t), nil), "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

// The web view is read-only: nothing here may accept a write.
func TestWritesAreRejected(t *testing.T) {
	h := Handler(newApp(t), nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		for _, path := range []string{"/", "/canvas.png", "/stats.json"} {
			req := httptest.NewRequest(method, path, strings.NewReader("x=1"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				t.Errorf("%s %s returned 200, want a rejection", method, path)
			}
		}
	}
}

func TestFullCanvasRenders(t *testing.T) {
	// A default-sized canvas with every cell drawn: the shape the real server
	// serves once the canvas fills up.
	c := canvas.New(canvas.DefaultWidth, canvas.DefaultHeight)
	for y := 0; y < canvas.DefaultHeight; y++ {
		for x := 0; x < canvas.DefaultWidth; x++ {
			if err := c.Set(x, y, rune('!'+(x+y)%94), uint8((x+y)%canvas.PaletteSize)); err != nil {
				t.Fatal(err)
			}
		}
	}
	data, err := RenderPNG(c)
	if err != nil {
		t.Fatalf("RenderPNG: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("RenderPNG returned no bytes")
	}
	if _, err := png.Decode(strings.NewReader(string(data))); err != nil {
		t.Errorf("decode: %v", err)
	}
}

func BenchmarkRenderPNG(b *testing.B) {
	c := canvas.New(canvas.DefaultWidth, canvas.DefaultHeight)
	for y := 0; y < canvas.DefaultHeight; y++ {
		for x := 0; x < canvas.DefaultWidth; x++ {
			_ = c.Set(x, y, rune('!'+(x+y)%94), uint8((x+y)%canvas.PaletteSize))
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := RenderPNG(c); err != nil {
			b.Fatal(err)
		}
	}
}
