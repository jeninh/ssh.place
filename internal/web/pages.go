package web

import (
	"fmt"
	"html/template"
	"sort"

	"github.com/jeninh/ssh.place/internal/canvas"
)

// repoURL is where the source lives; it is linked from every page footer.
const repoURL = "https://github.com/jeninh/ssh.place"

// redditURL is the community, also linked from every footer.
const redditURL = "https://www.reddit.com/r/sshplace/"

// style is the shared stylesheet. It follows the same system as openotp.app —
// GitHub's typography, spacing and borders — but pinned to the dark palette
// rather than following the viewer's system preference, so the page always
// matches the terminal it is showing.
const style = `
  :root {
    --bg: #0d1117; --fg: #f0f6fc; --muted: #9198a1;
    --border: #3d444d; --code-bg: #151b23; --link: #4493f8;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0 auto; padding: 3rem 1.25rem 4rem; max-width: 680px;
    background: var(--bg); color: var(--fg);
    font: 16px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
  }
  header { text-align: center; margin-bottom: 2.5rem; }
  h1 { margin: 0.5rem 0 0.25rem; font-size: 2rem; }
  h1 .dot { color: var(--link); }
  .tagline { font-size: 1.1rem; margin: 0.25rem 0; }
  .sub { color: var(--muted); font-size: 0.9rem; margin: 0.25rem 0 0; }
  h2 { margin-top: 2.5rem; padding-bottom: 0.3rem; border-bottom: 1px solid var(--border); font-size: 1.4rem; }
  a { color: var(--link); text-decoration: none; }
  a:hover { text-decoration: underline; }
  code, pre {
    font: 0.85em/1.5 ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
    background: var(--code-bg); border-radius: 6px;
  }
  code { padding: 0.15em 0.4em; }
  pre { padding: 1rem; overflow-x: auto; border: 1px solid var(--border); }
  pre code { padding: 0; background: none; }
  .install {
    margin: 2rem 0; padding: 1.25rem; border: 1px solid var(--border); border-radius: 10px;
    background: var(--code-bg); text-align: center;
  }
  .install pre { margin: 0.5rem 0; text-align: center; font-size: 1.1em; }
  .install .alt { color: var(--muted); font-size: 0.85rem; margin: 0.5rem 0 0; }
  figure { margin: 2rem 0; }
  img.canvas {
    display: block; width: 100%; height: auto; border-radius: 10px;
    border: 1px solid var(--border); background: #0d1117;
    image-rendering: pixelated;
  }
  figcaption { color: var(--muted); font-size: 0.85rem; text-align: center; margin-top: 0.5rem; }
  ul { padding-left: 1.4rem; }
  li { margin: 0.35rem 0; }
  .note { color: var(--muted); font-size: 0.9rem; }
  table { width: 100%; border-collapse: collapse; margin: 1rem 0; }
  th, td { text-align: left; padding: 0.4rem 0.6rem; border-bottom: 1px solid var(--border); }
  th { color: var(--muted); font-weight: 600; font-size: 0.85rem; }
  td.num, th.num { text-align: right; font-variant-numeric: tabular-nums;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .figures { display: flex; flex-wrap: wrap; gap: 1rem; margin: 1.5rem 0; }
  .figures div {
    flex: 1 1 8rem; padding: 0.9rem 1rem; border: 1px solid var(--border);
    border-radius: 10px; background: var(--code-bg);
  }
  .figures dt { color: var(--muted); font-size: 0.8rem; margin: 0; }
  .figures dd {
    margin: 0.2rem 0 0; font-size: 1.5rem; font-variant-numeric: tabular-nums;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  }
  .swatch {
    display: inline-block; width: 0.85em; height: 0.85em; border-radius: 3px;
    margin-right: 0.5em; vertical-align: -0.05em; outline: 1px solid #3d444d;
  }
  .bar { background: var(--border); border-radius: 3px; height: 0.5rem; }
  .bar span { display: block; height: 100%; border-radius: 3px; }
  footer {
    margin-top: 3.5rem; padding-top: 1rem; border-top: 1px solid var(--border);
    color: var(--muted); font-size: 0.85rem; text-align: center;
  }
`

// layout wraps each page. Pages define a "title", "head" and "body" block.
const layout = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{template "title" .}}</title>
<meta name="description" content="{{template "description" .}}">
<style>` + style + `</style>
</head>
<body>
{{template "body" .}}
<footer>
  <a href="/">canvas</a> ·
  <a href="/stats">stats</a> ·
  <a href="/timelapse">timelapse</a> ·
  <a href="/canvas.png">png</a> ·
  <a href="` + repoURL + `">open source</a> ·
  <a href="` + redditURL + `">r/sshplace</a>
</footer>
</body>
</html>
`

// page builds a template from the shared layout plus the page's own blocks.
func page(name, body string) *template.Template {
	return template.Must(template.Must(template.New(name).Parse(layout)).Parse(body))
}

var indexTmpl = page("index", `
{{define "title"}}ssh.place: r/place over SSH{{end}}
{{define "description"}}A shared canvas you draw on over SSH. No account, no install. Just run ssh ssh.place{{end}}
{{define "body"}}
<header>
  <h1>ssh<span class="dot">.</span>place</h1>
  <p class="tagline"><b>One canvas. Everyone draws on it over SSH.</b></p>
  <p class="sub">No account, no install · {{.Width}}&times;{{.Height}} cells · one placement every {{.Cooldown}}s</p>
</header>

<div class="install">
  <b>Draw on it:</b>
  <pre><code>ssh ssh.place</code></pre>
  <p class="alt">Any SSH key works. There is nothing to sign up for.</p>
</div>

<figure>
  <a href="/canvas.png"><img class="canvas" id="canvas" src="/canvas.png"
     alt="The current ssh.place canvas" width="{{.PixelW}}" height="{{.PixelH}}"></a>
  <figcaption id="caption">{{.Online}} online · {{.Drawn}} of {{.Cells}} cells drawn</figcaption>
</figure>

<h2>How it works</h2>
<ul>
  <li>Move the cursor with the <b>arrow keys</b>, <code>wasd</code> or <code>hjkl</code></li>
  <li>The canvas is wider than your terminal, so <b>scroll</b> to pan around. <kbd>shift</kbd>+<kbd>←</kbd>/<kbd>→</kbd> jumps a whole screen.</li>
  <li>Pick a color with <code>0</code> to <code>9</code>. <kbd>tab</kbd> cycles through all 16.</li>
  <li><kbd>space</kbd> puts down a solid block of that color</li>
{{- if not .BlocksOnly}}
  <li><code>^B</code> switches to characters. Then any key you press becomes your stamp.</li>
{{- end}}
  <li>Now wait {{.Cooldown}} seconds, same as everyone else</li>
</ul>
{{- if .BlocksOnly}}
<p class="note">This canvas is color only. The server turns down anything with a character in it, so you cannot write text here. Draw something instead.</p>
{{- end}}
<p class="note">Your cooldown is tied to your SSH key, so reconnecting will not reset it. This page only reads the canvas. It changes over SSH and nowhere else.</p>
<script>
  const img = document.getElementById('canvas');
  const caption = document.getElementById('caption');
  let timer = null;

  async function refresh() {
    img.src = '/canvas.png?t=' + Date.now();
    try {
      const s = await (await fetch('/stats.json', { cache: 'no-store' })).json();
      caption.textContent = s.online + ' online · ' + s.cells_drawn +
        ' of ' + s.cells_total + ' cells drawn';
    } catch (e) { /* transient: the next tick retries */ }
  }

  // Polling a background tab wastes everyone's bandwidth.
  function start() { if (!timer) timer = setInterval(refresh, 5000); }
  function stop() { clearInterval(timer); timer = null; }
  document.addEventListener('visibilitychange', () => document.hidden ? stop() : (refresh(), start()));
  start();
</script>
{{end}}
`)

var statsTmpl = page("stats", `
{{define "title"}}ssh.place: stats{{end}}
{{define "description"}}Live statistics for the ssh.place shared canvas.{{end}}
{{define "body"}}
<header>
  <h1>ssh<span class="dot">.</span>place</h1>
  <p class="tagline"><b>Stats</b></p>
  <p class="sub">Live. Refreshes every five seconds.</p>
</header>

<dl class="figures">
  <div><dt>Online now</dt><dd id="online">{{.Online}}</dd></div>
  <div><dt>Cells drawn</dt><dd id="drawn">{{.Drawn}}</dd></div>
  <div><dt>Filled</dt><dd id="filled">{{.FilledPct}}%</dd></div>
  <div><dt>Placements</dt><dd id="placements">{{.Placements}}</dd></div>
</dl>

<h2>Canvas</h2>
<table>
  <tr><td>Size</td><td class="num">{{.Width}} &times; {{.Height}}</td></tr>
  <tr><td>Total cells</td><td class="num">{{.Cells}}</td></tr>
  <tr><td>Drawn</td><td class="num">{{.Drawn}}</td></tr>
  <tr><td>Untouched</td><td class="num">{{.Untouched}}</td></tr>
  <tr><td>Cooldown per key</td><td class="num">{{.Cooldown}}s</td></tr>
  <tr><td>Placements since restart</td><td class="num">{{.Placements}}</td></tr>
</table>
<p class="note">Placements only counts what has landed since the server last started. The canvas itself survives restarts.</p>

<h2>Palette</h2>
{{if .Colors}}
<table>
  <tr><th>Color</th><th class="num">Cells</th><th class="num">Share</th><th></th></tr>
  {{range .Colors}}
  <tr>
    <td><span class="swatch" style="background:{{.Hex}}"></span>{{.Name}}</td>
    <td class="num">{{.Count}}</td>
    <td class="num">{{.Share}}%</td>
    <td style="width:35%"><div class="bar"><span style="width:{{.BarPct}}%;background:{{.Hex}}"></span></div></td>
  </tr>
  {{end}}
</table>
{{else}}
<p class="note">Nothing has been drawn yet. <a href="/">Be the first.</a></p>
{{end}}

<h2>Draw on it</h2>
<pre><code>ssh ssh.place</code></pre>
<script>
  let timer = null;

  async function refresh() {
    try {
      const s = await (await fetch('/stats.json', { cache: 'no-store' })).json();
      const set = (id, v) => { const el = document.getElementById(id); if (el) el.textContent = v; };
      set('online', s.online);
      set('drawn', s.cells_drawn);
      set('filled', s.filled_percent + '%');
      set('placements', s.placements_since_restart);
    } catch (e) { /* transient: the next tick retries */ }
  }

  function start() { if (!timer) timer = setInterval(refresh, 5000); }
  function stop() { clearInterval(timer); timer = null; }
  document.addEventListener('visibilitychange', () => document.hidden ? stop() : (refresh(), start()));
  start();
</script>
{{end}}
`)

// colorStat is one palette row on the stats page.
type colorStat struct {
	Name  string
	Hex   string
	Count int
	Share string
	// BarPct scales the bar against the most used color, so the chart still
	// reads when one color dominates.
	BarPct int
}

// pageData is everything both templates can reference.
type pageData struct {
	Width, Height  int
	PixelW, PixelH int
	Cells          int
	Drawn          int
	Untouched      int
	FilledPct      string
	Online         int
	Placements     uint64
	Cooldown       int
	BlocksOnly     bool
	Colors         []colorStat

	// Timelapse page only.
	Entries []lapseView
	Started string
}

// lapseView is one timelapse as the page needs it, with sizes already formatted
// so the template stays free of arithmetic.
type lapseView struct {
	Name   string
	Label  string
	Frames int
	Events int
	MB     string
}

func pct(n, total int) string {
	if total == 0 {
		return "0"
	}
	return fmt.Sprintf("%.1f", 100*float64(n)/float64(total))
}

// colorStats summarises which palette colors are actually on the canvas,
// busiest first.
func colorStats(counts [canvas.PaletteSize]int, drawn int) []colorStat {
	out := make([]colorStat, 0, canvas.PaletteSize)
	most := 0
	for _, n := range counts {
		if n > most {
			most = n
		}
	}
	for i, n := range counts {
		if n == 0 {
			continue
		}
		e := canvas.Palette[i]
		bar := 0
		if most > 0 {
			bar = 100 * n / most
		}
		out = append(out, colorStat{
			Name:   e.Name,
			Hex:    fmt.Sprintf("#%02x%02x%02x", e.RGB.R, e.RGB.G, e.RGB.B),
			Count:  n,
			Share:  pct(n, drawn),
			BarPct: bar,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

var timelapseTmpl = page("timelapse", `
{{define "title"}}ssh.place timelapses{{end}}
{{define "description"}}Every day of the ssh.place canvas, as an animated GIF{{end}}
{{define "body"}}
<header>
  <h1>ssh<span class="dot">.</span>place</h1>
  <p class="tagline"><b>Timelapses</b></p>
  <p class="sub">
    {{if .Started}}First pixel {{.Started}} · {{end}}rebuilt after every UTC midnight
  </p>
</header>

{{if .Entries}}
{{range .Entries}}
<h2>{{.Label}}</h2>
<figure>
  <a href="/timelapse/{{.Name}}"><img class="canvas" src="/timelapse/{{.Name}}"
     alt="Timelapse of the ssh.place canvas: {{.Label}}" loading="lazy"></a>
  <figcaption>
    {{if .Events}}{{.Events}} placements · {{end}}{{.Frames}} frames · {{.MB}} MB ·
    <a href="/timelapse/{{.Name}}">download</a>
  </figcaption>
</figure>
{{end}}
{{else}}
<p class="alt">
  No timelapses yet. The first one is written a couple of minutes after the next
  UTC midnight, once there is a full day to show.
</p>
{{end}}

<h2>How they are made</h2>
<p>
  Every placement since the first one is in an append-only log, so a timelapse is
  just that log replayed onto a blank canvas. Frames are spaced evenly over
  placements rather than over the clock, because spacing them by time would spend
  most of the animation on whichever hours everyone was asleep.
</p>
<p>
  A single day starts from the state the canvas opened on that morning rather than
  from empty, so it shows that day's changes in context. GIF is a natural fit
  here: the canvas is already sixteen palette colours, which is exactly what the
  format stores, so nothing is quantised and there is no video encoder involved.
</p>
{{end}}
`)
