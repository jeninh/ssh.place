<h1 align="center">ssh.place</h1>

<p align="center">
  <b>One canvas. Everyone draws on it over SSH.</b><br>
  <sub>No account, no install, nothing to sign up for</sub>
</p>

<p align="center">
  <code>ssh ssh.place</code>
</p>

<p align="center">
  <a href="https://ssh.place">Live canvas</a> ·
  <a href="https://ssh.place/stats">Stats</a> ·
  <a href="#running-your-own">Run your own</a> <br>
</p>

<p align="center">
  <a href="https://ssh.place">
    <img src="https://ssh.place/canvas.png" alt="The ssh.place canvas, right now" width="900">
  </a>
</p>

<p align="center">
  <sub>That is the canvas as it stands right now. go change it :)</sub>
</p>

## What it is

You already have an SSH client. That's the whole idea.

It's r/place on one 200x60 canvas, shared by everyone who's connected right now.
Run `ssh ssh.place` and you're in. Your first cell goes down instantly, then you
wait 15 seconds for the next one, same as everybody else.

No signup, no user table, no web app to load. Whatever SSH key you connect with,
its fingerprint is who you are. That's the entire account system.

Heads up: I built this because the idea was funny, not because anyone needed it. It
works, but expect rough edges :)

## Getting started

```sh
ssh ssh.place
```

| Key | What it does |
| --- | --- |
| `←↑↓→`, `wasd` or `hjkl` | move the cursor |
| `0` to `9` | pick a color |
| `tab` / `shift+tab` | cycle all 16 colors |
| `space` or `enter` | place a block |
| scroll wheel | pan around |
| click | move the cursor there (it won't place) |
| `shift`/`ctrl` + arrow | jump a whole screen |
| `home` / `end` | jump to the left or right edge |
| `q` or `ctrl+c` | quit |

The canvas is 200 columns wide and your terminal isn't, so the view pans along as
you get near an edge. Crossing the whole thing one cell at a time is a slog. Scroll
instead, or hit `shift`+`←`/`→` to jump a screen.

There's a grey `+--+` frame around the canvas so you can tell where it stops. I
added it because without one you just drift into blank space and can't tell whether
you've hit the edge.

Moving is always free. Only placing waits.

## There are no letters

You paint solid blocks of color. That's the only thing you can do.

I tried characters first, which seemed obviously better. It wasn't. On a canvas
anyone on the internet can reach, letters get used to write *at* people far more
than to draw. So they're gone, and not just hidden from the UI: the server refuses
anything with a character in it, so writing your own client won't get you further.

A block is a background color, not a `█` glyph. Same reasoning. `█` depends on the
client's font having it, and I'd rather the canvas look identical to everyone than
gamble on that. Backgrounds also work on a terminal with no Unicode at all.

Want characters on your own copy? Run it with `-mode mixed`. You still start on
blocks, `ctrl+b` switches over, and any printable key becomes your stamp. Put `\`
in front of a key to stamp it literally, which is how `wasd`, `hjkl`, `q` and the
digits stay drawable.

## How it works

The canvas is a slice in memory behind a `sync.RWMutex`, saved to
`data/canvas.json` every 10 seconds and again on the way out. Each cell holds one
printable-ASCII rune plus two palette indices, a foreground and a background. There
are 16 colors and nothing outside that palette can be stored, so the whole 200x60
board comes to about 35 KB of JSON you can actually read.

Every connection gets its own [bubbletea](https://github.com/charmbracelet/bubbletea)
program, served over [wish](https://github.com/charmbracelet/wish). Place a cell and
it fans out to every session through that session's own buffered channel. If someone
falls behind, their updates get dropped and a resync flag set instead. That way one
stuck client can't hold up everyone else. Sessions redraw from the canvas itself, so
a dropped update just means a slightly later frame.

### The rules live on the server

The TUI is only a view. `internal/app.Place` is the one path that changes a cell and
it re-checks everything, so writing your own client to skip the UI gets you nowhere:

- **Bounds** - anything off the canvas is refused.
- **Characters** - refused outright in the default `blocks` mode. Under
  `-mode mixed` only printable ASCII gets through, `0x20` to `0x7e`. That one check
  rules out escape sequences and keeps every cell exactly one column wide. A space
  is always fine, that's how you erase.
- **Colors** - the color and the fill are both palette indices, so you can't ask for
  a color that doesn't exist.
- **Cooldown** - 15 seconds, tracked against your key's fingerprint. It follows the
  key, so reconnecting won't reset it.
- **Budgets** - see below, that part got interesting.

Getting refused doesn't cost you your turn. Aiming off the canvas shouldn't burn 15
seconds.

### Identity

Any public key works and there's nothing to register. Its SHA256 fingerprint is who
you are. Connect with no key at all and you fall back to keyboard-interactive, which
means you get identified by network instead and share that budget with anyone else
on it.

### Limits

| Limit | Default | Flag |
| --- | --- | --- |
| Characters allowed | no | `-mode blocks` / `-mode mixed` |
| Cooldown per key | 15s | `-cooldown` |
| Per-network burst / refill | 5 / 3s | `-ip-burst`, `-ip-refill` |
| Minimum time to repaint the board | 1 hour | `-min-board-fill` |
| Concurrent sessions | 500 | `-max-sessions` |
| Sessions per network | 5 | `-max-per-ip` |
| Idle disconnect | 30 min | `-idle-timeout` |
| Unauthenticated connection | 20s | fixed |
| Accepted connections | 4 x `-max-sessions` | fixed |

Get turned away and you get one plain sentence saying why, not a stack trace.

### Why the limits look like that

**Keys are free, so a cooldown alone does nothing.** Generate a new one per
placement and you've beaten it. Hence a token bucket per network on top.

**Networks are nearly free too.** I keyed that bucket on the exact address at
first, which is fine for IPv4. For IPv6 it's useless: one customer gets handed 2^64
addresses and every one of them looks like a fresh client. I tested it, and 200
placements landed from a single /64 against a budget of 5. IPv6 now gets grouped by
/64.

**Then I starved everyone on shared wifi.** The per-network bucket refilled at one
per 15s, which is one player's worth of throughput for a whole network. Five people
behind one NAT, all waiting their turn properly, and whoever asked first each round
took the lot. The other four got one cell each in four minutes. So `-ip-refill` now
defaults to `-cooldown` divided by `-max-per-ip`, the rate the most sessions I'll
accept from one network can legitimately produce.

**And none of that bounds the total.** Both limits scale with whatever you can buy,
and providers hand out IPv6 /48s, which is 65,536 distinct /64s from one account. So
500 sessions at one-per-15s came to 33 placements a second, and the entire
12,000-cell board could be repainted in **six minutes** by one person with a script.

That's what `-min-board-fill` is for. It says repainting every cell can't take less
than an hour, and turns that into a placements-per-second ceiling worked out from
the canvas size. It's the only limit that doesn't care how much anybody controls.
With it on, an attacker using a fresh key *and* a fresh network for every single
placement gets about 9% of the board in five minutes instead of all of it.

The burst is one cooldown's worth, so a crowd who've all been waiting can fire
together without getting refused for it. Roughly 50 people drawing flat out fit
under the default. Watchers cost nothing.

There's a real tradeoff here. Past that point the canvas is genuinely at its limit
and starts saying no. It tells you which limit it was rather than blaming your own
cooldown, but it's still a no. 

## The web view


| Path | What it serves |
| --- | --- |
| `/` | landing page with a live view |
| `/stats` | who's on, how full the board is, which colors are winning |
| `/canvas.png` | 1400x780 PNG, for screenshots and timelapses |
| `/stats.json` | the same numbers but json |
| `/canvas.txt` | plain text, blocks as `█` |
| `/healthz` | `ok` |

The PNG uses the bitmap font out of `x/image`, so there are no font files to ship.
It's cached against a canvas version counter and sent with an ETag, so leaving the
page open mostly gets you 304s back.

## Timelapses

Every placement that lands gets appended to `data/events.jsonl`, which is enough to
replay the board from empty:

```json
{"t":"2026-07-31T00:48:24.236Z","id":"SHA256:DVG3kW…","x":100,"y":30,"r":"Q","c":15}
{"t":"2026-07-31T00:48:41.882Z","id":"SHA256:DVG3kW…","x":101,"y":30,"r":" ","c":9,"block":true}
```

`block` means it was a solid block and `c` is its color. Turn the log off with
`-event-log=false`.

## Running your own

```sh
git clone https://github.com/jeninh/ssh.place
cd ssh.place
make run            # SSH on :2222, web on :8080
ssh -p 2222 localhost
```

Or Docker, which is what runs the real thing:

```sh
docker compose up -d --build
```

That brings up two containers. `sshplace` publishes port 22, and `caddy` takes 80
and 443 and reverse-proxies the web view, fetching a Let's Encrypt certificate on
first boot. The app's HTTP port is deliberately not published, so the only way to
reach the web view is through TLS.

**Point DNS at the box before you start it.** Caddy validates over HTTP, so if
`ssh.place` does not already resolve to that machine the certificate request fails
and it sits there retrying. Change the domain and the contact address at the top of
the `Caddyfile` if you are running your own.

Compose maps host port 22 to the container's SSH port, which is what lets
`ssh ssh.place` work with no `-p`. If sshd is already on 22, move it first, and
reconnect on the new port to prove it works before you bring this up. The app image
is distroless with no shell in it, so its health check runs the binary's own
`-healthcheck` flag instead of curl.

**Don't skip the `/data` volume.** The host key lives there, generated on first
boot. Lose it and every returning visitor gets a host key mismatch warning and
refuses to connect. I did this to myself while testing. The canvas snapshot and the
event log are in there too.

Most flags have an env var (`SSHPLACE_SSH_ADDR`, `SSHPLACE_HTTP_ADDR`,
`SSHPLACE_DATA`, `SSHPLACE_WEB_URL`, `SSHPLACE_MODE`, `SSHPLACE_LOG_LEVEL`).
`./sshplace -h` has the rest.

You can resize the board between restarts with `-width` and `-height`. If the saved
snapshot doesn't match any more, whatever still fits gets loaded instead of the
whole thing being thrown out.

## Development

```sh
make test     # go test -race ./...
make check    # fmt, vet, test
make build
make docker
```

```
main.go                    flags, wiring, snapshot loop, signals
internal/canvas            the grid, the palette, snapshot save/load
internal/app               the one authoritative placement path
internal/hub               session registry and update fan-out
internal/ratelimit         cooldown, per-network bucket, board ceiling
internal/tui               the per-session bubbletea program
internal/server            the SSH front end
internal/web               PNG renderer, landing page, stats page
internal/eventlog          append-only placement log
```

Tests cover the canvas (bounds, validation, concurrent writes, snapshot
round-trips including the older format), the limiter (cooldown, key rotation,
address rotation, clock jumps), the hub (capacity, fan-out, coalescing) and the TUI
(keys, typing bursts, panning, the border, the cooldown, the fade, the idle
timeout). The TUI tests run on a clock I crank by hand, so none of them sleep.

`internal/server` stands up a real SSH server on a real socket and drives it with an
SSH client. It places a cell end to end, checks the cooldown holds, proves no
sequence of keys writes a character on a blocks-only canvas, watches one session's
placement appear on another's screen, pushes the connection limits, confirms the
host key survives a restart, and checks sessions don't leak goroutines.

## Security

Whatever you draw gets replayed into everyone else's terminal, so that's the part I
was careful about.

Only printable ASCII can be stored, checked in `Cell.Validate` and again when a
snapshot loads. Escape sequences, bidi overrides, combining marks and wide
characters can't reach a cell at all. `internal/canvas/escape_test.go` walks the
first 70,000 code points to keep it that way.

Two other things worth knowing:

- The event log is 0600 and pairs key fingerprints with networks. It grows forever,
  so rotate it if you leave this running.
- The host key is the only secret on disk. Keep `/data` off shared boxes and out of
  your images.

For dependency advisories:

```sh
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Found something sharp? Open an issue.

## Credits

Built with [wish](https://github.com/charmbracelet/wish),
[bubbletea](https://github.com/charmbracelet/bubbletea) and
[lipgloss](https://github.com/charmbracelet/lipgloss) from [Charm](https://charm.sh).

Built with help from [Claude Code](https://claude.com/claude-code)

## License

[MIT](LICENSE) © jeninh
