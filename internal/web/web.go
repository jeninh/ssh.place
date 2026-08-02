// Package web serves a read-only view of the canvas over HTTP: a PNG for
// screenshots and timelapses, a live stats page, and a small landing page so
// ssh.place works in a browser too.
package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/jeninh/ssh.place/internal/app"
)

// Handler returns the HTTP mux for the web view. Nothing it serves can change
// the canvas; the canvas only moves over SSH.
func Handler(a *app.App) http.Handler {
	h := &handler{app: a, png: newPNGCache(a.Canvas), counts: newCountCache(a.Canvas)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.index)
	mux.HandleFunc("GET /stats", h.stats)
	mux.HandleFunc("GET /stats.json", h.statsJSON)
	mux.HandleFunc("GET /canvas.png", h.canvasPNG)
	mux.HandleFunc("GET /canvas.txt", h.canvasTXT)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	return noSniff(mux)
}

// noSniff stops a browser second-guessing the Content-Type. It matters most for
// /canvas.txt: in mixed mode the canvas can legitimately hold the characters
// "<script>", and a sniffing browser deciding a text/plain body is really HTML
// would turn drawn text into script on our own origin.
func noSniff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

type handler struct {
	app    *app.App
	png    *pngCache
	counts *countCache
}

// data gathers everything the pages display in one pass.
func (h *handler) data() pageData {
	w, hh := h.app.Canvas.Size()
	total := w * hh
	counts, drawn := h.counts.get()

	return pageData{
		Width:      w,
		Height:     hh,
		PixelW:     w * cellW,
		PixelH:     hh * cellH,
		Cells:      total,
		Drawn:      drawn,
		Untouched:  total - drawn,
		FilledPct:  pct(drawn, total),
		Online:     h.app.Hub.Online(),
		Placements: h.app.Placements(),
		Cooldown:   int(h.app.Cooldown().Seconds()),
		BlocksOnly: h.app.BlocksOnly,
		Colors:     colorStats(counts, drawn),
	}
}

func (h *handler) index(w http.ResponseWriter, _ *http.Request) {
	h.renderPage(w, indexTmpl)
}

func (h *handler) stats(w http.ResponseWriter, _ *http.Request) {
	h.renderPage(w, statsTmpl)
}

// renderPage buffers the page before writing it, so a template error can still
// become a 500 instead of a half-sent page.
func (h *handler) renderPage(w http.ResponseWriter, t *template.Template) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, h.data()); err != nil {
		http.Error(w, "could not render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = buf.WriteTo(w)
}

func (h *handler) statsJSON(w http.ResponseWriter, _ *http.Request) {
	d := h.data()
	body := struct {
		Width      int     `json:"width"`
		Height     int     `json:"height"`
		Online     int     `json:"online"`
		Drawn      int     `json:"cells_drawn"`
		Cells      int     `json:"cells_total"`
		Filled     string  `json:"filled_percent"`
		Placements uint64  `json:"placements_since_restart"`
		Version    uint64  `json:"version"`
		Cooldown   float64 `json:"cooldown_seconds"`
		Colors     []struct {
			Name  string `json:"name"`
			Hex   string `json:"hex"`
			Count int    `json:"count"`
		} `json:"colors"`
	}{
		Width:      d.Width,
		Height:     d.Height,
		Online:     d.Online,
		Drawn:      d.Drawn,
		Cells:      d.Cells,
		Filled:     d.FilledPct,
		Placements: d.Placements,
		Version:    h.app.Canvas.Version(),
		Cooldown:   h.app.Cooldown().Seconds(),
	}
	for _, c := range d.Colors {
		body.Colors = append(body.Colors, struct {
			Name  string `json:"name"`
			Hex   string `json:"hex"`
			Count int    `json:"count"`
		}{c.Name, c.Hex, c.Count})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func (h *handler) canvasPNG(w http.ResponseWriter, r *http.Request) {
	data, version, err := h.png.bytes()
	if err != nil {
		http.Error(w, "could not render canvas", http.StatusInternalServerError)
		return
	}
	etag := `"v` + strconv.FormatUint(version, 10) + `"`
	w.Header().Set("ETag", etag)
	// The canvas changes constantly, so the page revalidates rather than
	// caching; the ETag keeps the common case a 304.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "image/png")
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (h *handler) canvasTXT(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	rows := h.app.Canvas.Text()
	// Trailing spaces carry no information and triple the payload.
	for i, row := range rows {
		rows[i] = strings.TrimRight(row, " ")
	}
	_, _ = w.Write([]byte(strings.Join(rows, "\n") + "\n"))
}
