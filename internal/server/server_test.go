package server_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/jeninh/ssh.place/internal/app"
	"github.com/jeninh/ssh.place/internal/canvas"
	"github.com/jeninh/ssh.place/internal/hub"
	"github.com/jeninh/ssh.place/internal/ratelimit"
	"github.com/jeninh/ssh.place/internal/server"
)

// These tests drive a real SSH server over a real TCP socket, so they wait on
// observable state rather than sleeping for a fixed period.
const (
	settleTimeout = 15 * time.Second
	pollInterval  = 5 * time.Millisecond
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// options tunes the fixture; the defaults are a small canvas with generous
// limits so each test only has to relax the one thing it is about.
type options struct {
	canvasW, canvasH int
	cooldown         time.Duration
	ipBurst          int
	ipRefill         time.Duration
	maxSessions      int
	maxPerIP         int
	idleTimeout      time.Duration
	dataDir          string
	blocksOnly       bool
}

type fixture struct {
	t    *testing.T
	srv  *server.Server
	app  *app.App
	addr string
	dir  string
}

func start(t *testing.T, mutate ...func(*options)) *fixture {
	t.Helper()

	o := options{
		canvasW: 60, canvasH: 20,
		cooldown:    15 * time.Second,
		ipBurst:     1000, // effectively off unless a test asks for it
		ipRefill:    time.Millisecond,
		maxSessions: 50,
		maxPerIP:    5,
		idleTimeout: time.Minute,
	}
	for _, m := range mutate {
		m(&o)
	}
	if o.dataDir == "" {
		o.dataDir = t.TempDir()
	}

	a := &app.App{
		Canvas:     canvas.New(o.canvasW, o.canvasH),
		Hub:        hub.New(o.maxSessions, o.maxPerIP),
		Limiter:    ratelimit.New(o.cooldown, o.ipBurst, o.ipRefill),
		Logger:     quiet(),
		BlocksOnly: o.blocksOnly,
	}

	srv, err := server.New(server.Config{
		Addr:        "127.0.0.1:0",
		HostKeyPath: filepath.Join(o.dataDir, "ssh_host_ed25519_key"),
		App:         a,
		IdleTimeout: o.idleTimeout,
		BlocksOnly:  o.blocksOnly,
		Logger:      quiet(),
	})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close server: %v", err)
		}
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(settleTimeout):
			t.Error("Serve did not return after Close")
		}
	})

	return &fixture{t: t, srv: srv, app: a, addr: srv.Addr().String(), dir: o.dataDir}
}

// waitFor polls until cond holds, and fails the test if it never does.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// lockedBuffer collects session output from the SSH read goroutine while the
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer
}

// dial connects with a freshly generated key, which is what a first-time
// visitor effectively does.
func (f *fixture) dial(t *testing.T, auth ...gossh.AuthMethod) *gossh.Client {
	t.Helper()
	if len(auth) == 0 {
		auth = []gossh.AuthMethod{gossh.PublicKeys(newSigner(t))}
	}
	client, err := gossh.Dial("tcp", f.addr, &gossh.ClientConfig{
		User:            "artist",
		Auth:            auth,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         settleTimeout,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// terminal is an interactive session with a PTY, the way a person connects.
type terminal struct {
	sess   *gossh.Session
	stdin  io.WriteCloser
	output *lockedBuffer
	waited chan error
}

// open starts an interactive session and waits until it has painted, which is
// the point a real person would start typing.
func (f *fixture) open(t *testing.T, client *gossh.Client) *terminal {
	t.Helper()
	term := f.openRaw(t, client)
	waitFor(t, "the first frame", func() bool {
		return strings.Contains(term.output.String(), "ssh.place")
	})
	return term
}

// openRaw starts a session without waiting for output. Sessions that are
// expected to be turned away never paint a frame, so they use this.
func (f *fixture) openRaw(t *testing.T, client *gossh.Client) *terminal {
	t.Helper()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	out := &lockedBuffer{}
	sess.Stdout = out
	sess.Stderr = out
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{
		gossh.ECHO: 0,
	}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("start shell: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- sess.Wait() }()

	term := &terminal{sess: sess, stdin: stdin, output: out, waited: waited}
	t.Cleanup(func() { _ = sess.Close() })
	return term
}

// typeKeys sends keystrokes one at a time so the server sees them as separate
// key presses rather than one pasted run.
func (term *terminal) typeKeys(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, err := term.stdin.Write([]byte(k)); err != nil {
			t.Fatalf("write %q: %v", k, err)
		}
		// A short pause between keys; the assertions that follow all poll.
		time.Sleep(20 * time.Millisecond)
	}
}

func (term *terminal) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-term.waited:
		return err
	case <-time.After(settleTimeout):
		t.Fatal("session did not end")
		return nil
	}
}

// --- the headline test: connect over SSH and place a cell ---

func TestPlaceCellOverSSH(t *testing.T) {
	f := start(t)
	term := f.open(t, f.dial(t))

	waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() == 1 })

	// The cursor starts at the centre of the canvas.
	w, h := f.app.Canvas.Size()
	x, y := w/2, h/2

	// Choose a stamp, choose a color, then place.
	term.typeKeys(t, "@", "5", "\r")

	waitFor(t, "the cell to be placed", func() bool {
		cell, _ := f.app.Canvas.At(x, y)
		return cell.Rune == '@'
	})

	cell, _ := f.app.Canvas.At(x, y)
	if cell.Color != 5 {
		t.Errorf("cell color = %d, want 5", cell.Color)
	}
	if got := f.app.Canvas.NonEmpty(); got != 1 {
		t.Errorf("canvas holds %d drawn cells, want 1", got)
	}

	// Quitting hangs up cleanly and the hub lets go of the session.
	term.typeKeys(t, "q")
	if err := term.wait(t); err != nil {
		t.Errorf("session exited with %v, want a clean exit", err)
	}
	waitFor(t, "the session to be released", func() bool { return f.app.Hub.Online() == 0 })

	if !strings.Contains(term.output.String(), "ssh.place") {
		t.Error("session output never mentioned ssh.place")
	}
}

// Moving the cursor before placing must land the cell where the cursor ended
// up, which is the check that panning and placement agree.
func TestPlaceAfterMovingCursor(t *testing.T) {
	f := start(t)
	term := f.open(t, f.dial(t))
	waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() == 1 })

	w, h := f.app.Canvas.Size()
	term.typeKeys(t, "+", "l", "l", "j", "\r")

	x, y := w/2+2, h/2+1
	waitFor(t, "the cell to be placed", func() bool {
		cell, _ := f.app.Canvas.At(x, y)
		return cell.Rune == '+'
	})
	if got := f.app.Canvas.NonEmpty(); got != 1 {
		t.Errorf("canvas holds %d cells, want exactly 1 at (%d,%d)", got, x, y)
	}
}

// The cooldown is enforced by the server, so a client that keeps pressing enter
// gets exactly one cell per window.
func TestCooldownIsEnforcedOverSSH(t *testing.T) {
	const cooldown = 400 * time.Millisecond
	f := start(t, func(o *options) { o.cooldown = cooldown })

	term := f.open(t, f.dial(t))
	waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() == 1 })

	term.typeKeys(t, "#", "\r")
	waitFor(t, "the first cell", func() bool { return f.app.Canvas.NonEmpty() == 1 })

	// Immediately try again somewhere else.
	term.typeKeys(t, "l", "\r", "\r", "\r")
	if got := f.app.Canvas.NonEmpty(); got != 1 {
		t.Fatalf("canvas holds %d cells during the cooldown, want 1", got)
	}
	if out := term.output.String(); !strings.Contains(out, "hold on") {
		t.Error("the session was never told it was cooling down")
	}

	// After the window, the next press lands.
	time.Sleep(cooldown)
	term.typeKeys(t, "\r")
	waitFor(t, "the second cell", func() bool { return f.app.Canvas.NonEmpty() == 2 })
}

// One session's placement has to show up on another session's screen without
// that session touching anything.
func TestPlacementBroadcastsToOtherSessions(t *testing.T) {
	f := start(t)

	watcher := f.open(t, f.dial(t))
	waitFor(t, "the watcher to register", func() bool { return f.app.Hub.Online() == 1 })

	artist := f.open(t, f.dial(t))
	waitFor(t, "the artist to register", func() bool { return f.app.Hub.Online() == 2 })

	// '%' appears nowhere in the chrome, so finding it in the watcher's output
	// means the canvas update really reached that terminal.
	term := "%"
	artist.typeKeys(t, term, "\r")
	waitFor(t, "the placement", func() bool { return f.app.Canvas.NonEmpty() == 1 })

	waitFor(t, "the watcher to see the placement", func() bool {
		return strings.Contains(watcher.output.String(), term)
	})
}

// A session joining later must see what is already on the canvas.
func TestCanvasStateIsVisibleToNewSessions(t *testing.T) {
	f := start(t)
	if err := f.app.Canvas.Set(0, 0, '&', 9); err != nil {
		t.Fatal(err)
	}

	term := f.open(t, f.dial(t))
	waitFor(t, "the canvas to be drawn", func() bool {
		return strings.Contains(term.output.String(), "&")
	})
}

// --- admission control ---

func TestPerIPConnectionLimit(t *testing.T) {
	f := start(t, func(o *options) { o.maxPerIP = 1 })

	f.open(t, f.dial(t))
	waitFor(t, "the first session", func() bool { return f.app.Hub.Online() == 1 })

	// Everything in this test comes from 127.0.0.1, so the second connection is
	// over the per-IP limit even with a different key.
	second := f.openRaw(t, f.dial(t))
	err := second.wait(t)
	if err == nil {
		t.Fatal("the second session from one IP was accepted, want a rejection")
	}
	var exitErr *gossh.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("session error = %T (%v), want an ExitError", err, err)
	}
	if !strings.Contains(second.output.String(), "too many connections") {
		t.Errorf("rejection message = %q, want it to explain the limit", second.output.String())
	}
	if got := f.app.Hub.Online(); got != 1 {
		t.Errorf("Online = %d, want 1: the rejected session must not be counted", got)
	}
}

func TestServerCapacityLimit(t *testing.T) {
	f := start(t, func(o *options) {
		o.maxSessions = 1
		o.maxPerIP = 10 // isolate the global cap from the per-IP one
	})

	f.open(t, f.dial(t))
	waitFor(t, "the first session", func() bool { return f.app.Hub.Online() == 1 })

	second := f.openRaw(t, f.dial(t))
	if err := second.wait(t); err == nil {
		t.Fatal("a session past capacity was accepted, want a rejection")
	}
	if out := second.output.String(); !strings.Contains(out, "capacity") {
		t.Errorf("rejection message = %q, want it to mention capacity", out)
	}
}

// A freed slot has to be reusable, or the server would decay to unusable.
func TestCapacityRecoversAfterDisconnect(t *testing.T) {
	f := start(t, func(o *options) {
		o.maxSessions = 1
		o.maxPerIP = 10
	})

	first := f.open(t, f.dial(t))
	waitFor(t, "the first session", func() bool { return f.app.Hub.Online() == 1 })
	first.typeKeys(t, "q")
	if err := first.wait(t); err != nil {
		t.Fatalf("clean quit returned %v", err)
	}
	waitFor(t, "the slot to be freed", func() bool { return f.app.Hub.Online() == 0 })

	replacement := f.open(t, f.dial(t))
	waitFor(t, "the replacement session", func() bool { return f.app.Hub.Online() == 1 })
	replacement.typeKeys(t, "*", "\r")
	waitFor(t, "the replacement to place a cell", func() bool { return f.app.Canvas.NonEmpty() == 1 })
}

// --- authentication ---

// There is no signup: any key at all is accepted.
func TestAnyPublicKeyIsAccepted(t *testing.T) {
	f := start(t, func(o *options) { o.maxPerIP = 10 })
	for i := 0; i < 3; i++ {
		term := f.open(t, f.dial(t))
		waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() >= 1 })
		term.typeKeys(t, "q")
		if err := term.wait(t); err != nil {
			t.Errorf("session %d with a fresh key exited %v", i, err)
		}
	}
}

// Clients with no key must still get in, identified by address.
func TestKeyboardInteractiveClientCanConnectAndPlace(t *testing.T) {
	f := start(t)
	client := f.dial(t, gossh.KeyboardInteractive(
		func(name, instruction string, questions []string, echos []bool) ([]string, error) {
			return make([]string, len(questions)), nil
		},
	))
	term := f.open(t, client)
	waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() == 1 })

	term.typeKeys(t, "?", "\r")
	waitFor(t, "the placement", func() bool { return f.app.Canvas.NonEmpty() == 1 })
}

// A non-interactive connection has no terminal to draw on, so it is turned away
// rather than left hanging.
func TestSessionWithoutPTYIsRejected(t *testing.T) {
	f := start(t)
	client := f.dial(t)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	out, err := sess.CombinedOutput("")
	if err == nil {
		t.Fatal("a session without a PTY was accepted, want a rejection")
	}
	if len(out) == 0 {
		t.Error("the client was disconnected without an explanation")
	}
	waitFor(t, "the hub to stay empty", func() bool { return f.app.Hub.Online() == 0 })
}

// --- host key persistence ---

// Reusing the host key across restarts is what keeps clients from seeing a
// man-in-the-middle warning after a redeploy.
func TestHostKeyPersistsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()

	fingerprint := func(t *testing.T) string {
		t.Helper()
		f := start(t, func(o *options) { o.dataDir = dir })

		var captured gossh.PublicKey
		client, err := gossh.Dial("tcp", f.addr, &gossh.ClientConfig{
			User: "artist",
			Auth: []gossh.AuthMethod{gossh.PublicKeys(newSigner(t))},
			HostKeyCallback: func(_ string, _ net.Addr, key gossh.PublicKey) error {
				captured = key
				return nil
			},
			Timeout: settleTimeout,
		})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer client.Close()

		if captured == nil {
			t.Fatal("the client never saw a host key")
		}
		return gossh.FingerprintSHA256(captured)
	}

	var first, second string
	t.Run("first boot", func(t *testing.T) { first = fingerprint(t) })
	t.Run("after restart", func(t *testing.T) { second = fingerprint(t) })

	if first == "" || second == "" {
		t.Fatal("failed to capture a host key")
	}
	if first != second {
		t.Errorf("host key changed across restarts:\n  before %s\n  after  %s", first, second)
	}
	if _, err := filepath.Glob(filepath.Join(dir, "ssh_host_ed25519_key")); err != nil {
		t.Errorf("host key was not written to the data dir: %v", err)
	}
}

// --- resilience ---

// Dropping the TCP connection with no SSH teardown is what a closed laptop lid
// looks like; the hub has to notice.
func TestAbruptDisconnectReleasesSession(t *testing.T) {
	f := start(t)
	client := f.dial(t)
	f.open(t, client)
	waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() == 1 })

	if err := client.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("close client: %v", err)
	}
	waitFor(t, "the session to be released", func() bool { return f.app.Hub.Online() == 0 })
	if got := f.app.Hub.ConnectionsFrom("127.0.0.1"); got != 0 {
		t.Errorf("per-IP count = %d after an abrupt disconnect, want 0", got)
	}
}

// A resize mid-session must not disturb the canvas or the connection.
func TestWindowResizeDuringSession(t *testing.T) {
	f := start(t)
	client := f.dial(t)
	term := f.open(t, client)
	waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() == 1 })

	for _, size := range [][2]int{{40, 12}, {200, 60}, {24, 5}, {10, 3}, {120, 40}} {
		if err := term.sess.WindowChange(size[1], size[0]); err != nil {
			t.Fatalf("resize to %dx%d: %v", size[0], size[1], err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Still alive and still able to draw.
	term.typeKeys(t, "=", "\r")
	waitFor(t, "a placement after resizing", func() bool { return f.app.Canvas.NonEmpty() == 1 })
	if got := f.app.Hub.Online(); got != 1 {
		t.Errorf("Online = %d after resizing, want 1", got)
	}
}

// Sessions connecting and disconnecting concurrently must leave no goroutines
// behind, or a long-lived server would bleed memory one visitor at a time.
func TestNoGoroutineLeakAcrossManySessions(t *testing.T) {
	f := start(t, func(o *options) { o.maxPerIP = 20 })

	// One warm-up connection so lazily started machinery is not mistaken for a
	// leak, torn all the way down again before the baseline is taken.
	warmClient := f.dial(t)
	warm := f.open(t, warmClient)
	warm.typeKeys(t, "q")
	_ = warm.wait(t)
	_ = warmClient.Close()
	waitFor(t, "the warm-up session to be released", func() bool { return f.app.Hub.Online() == 0 })

	settle(t)
	baseline := runtime.NumGoroutine()

	const rounds = 12
	for i := 0; i < rounds; i++ {
		client := f.dial(t)
		term := f.open(t, client)

		// Alternate between quitting cleanly and hanging up mid-session: the
		// two take different teardown paths through the middleware.
		if i%2 == 0 {
			term.typeKeys(t, "q")
			_ = term.wait(t)
		}
		// Close the client too, so each round is a complete connect/disconnect
		// cycle. Leaving it open would pile up the test's own SSH plumbing and
		// read as a server leak.
		_ = client.Close()
		waitFor(t, "the session to be released", func() bool { return f.app.Hub.Online() == 0 })
	}

	settle(t)
	// Allow a little slack for runtime bookkeeping, but nothing like one
	// goroutine per session.
	const slack = 8
	if got := runtime.NumGoroutine(); got > baseline+slack {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Errorf("goroutines grew from %d to %d over %d sessions (slack %d)\n%s",
			baseline, got, rounds, slack, buf[:n])
	}
}

// settle waits for the goroutine count to stop moving, so leak checks are not
// racing teardown.
func settle(t *testing.T) {
	t.Helper()
	prev, stable := -1, 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == prev {
			// Two consecutive identical readings: teardown has caught up.
			if stable++; stable >= 2 {
				return
			}
			continue
		}
		prev, stable = n, 0
	}
}

// Shutdown must return promptly even with a session mid-draw.
func TestShutdownWithLiveSession(t *testing.T) {
	f := start(t)
	f.open(t, f.dial(t))
	waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() == 1 })

	// Close drops live connections; the fixture's cleanup asserts Serve returns.
	if err := f.srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, "sessions to be dropped", func() bool { return f.app.Hub.Online() == 0 })
}

func TestNewRequiresApp(t *testing.T) {
	if _, err := server.New(server.Config{Addr: "127.0.0.1:0"}); err == nil {
		t.Error("New with no App succeeded, want an error")
	}
}

func TestNewFailsOnUnavailablePort(t *testing.T) {
	f := start(t)
	_, err := server.New(server.Config{
		Addr:        f.addr, // already bound
		HostKeyPath: filepath.Join(t.TempDir(), "hostkey"),
		App:         f.app,
		Logger:      quiet(),
	})
	if err == nil {
		t.Error("New on a bound port succeeded, want an error")
	}
}

// --- blocks-only over a real connection ---

// The headline promise: on a blocks-only canvas there is no keystroke sequence
// that writes a character, and the block still lands.
func TestBlocksOnlyOverSSH(t *testing.T) {
	f := start(t, func(o *options) {
		o.blocksOnly = true
		o.cooldown = 0
	})
	term := f.open(t, f.dial(t))
	waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() == 1 })

	w, h := f.app.Canvas.Size()
	x, y := w/2, h/2

	// Try every route to a character: pick one, use the literal prefix, and ask
	// for character mode. Then place.
	term.typeKeys(t, "X", "\\", "X", "\x02", "3", "\r")

	waitFor(t, "the placement", func() bool { return f.app.Canvas.NonEmpty() == 1 })

	cell, _ := f.app.Canvas.At(x, y)
	if !cell.IsBlock() {
		t.Errorf("cell = %+v, want a block", cell)
	}
	if cell.Rune != canvas.Empty {
		t.Errorf("cell holds the character %q; characters should be impossible here", cell.Rune)
	}
	if cell.Fill != 3 {
		t.Errorf("fill = %d, want 3", cell.Fill)
	}

	// Nothing anywhere on the canvas carries a character.
	for _, row := range f.app.Canvas.Text() {
		for _, r := range row {
			if r != canvas.Empty && r != '\u2588' {
				t.Fatalf("canvas contains the character %q", r)
			}
		}
	}
	if !strings.Contains(term.output.String(), "blocks only") {
		t.Error("the session was never told why its character keys did nothing")
	}
}

// wasd has to move the cursor over a real connection, not just in unit tests.
func TestWASDMovesOverSSH(t *testing.T) {
	f := start(t, func(o *options) { o.cooldown = 0 })
	term := f.open(t, f.dial(t))
	waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() == 1 })

	w, h := f.app.Canvas.Size()
	// Right twice and down once, then place.
	term.typeKeys(t, "d", "d", "s", "\r")

	x, y := w/2+2, h/2+1
	waitFor(t, "the placement", func() bool {
		cell, _ := f.app.Canvas.At(x, y)
		return cell.Drawn()
	})
	if got := f.app.Canvas.NonEmpty(); got != 1 {
		t.Errorf("canvas holds %d cells, want exactly 1 at (%d,%d)", got, x, y)
	}
}

// The frame has to be on screen, since it is the only thing telling a player
// where the canvas ends.
func TestFrameIsVisibleOverSSH(t *testing.T) {
	f := start(t)
	term := f.open(t, f.dial(t))
	waitFor(t, "the session to register", func() bool { return f.app.Hub.Online() == 1 })

	// Walk to the top-left corner so both walls are in view.
	keys := []string{"\x1b[H"}
	for i := 0; i < 25; i++ {
		keys = append(keys, "w")
	}
	term.typeKeys(t, keys...)

	waitFor(t, "the frame to be drawn", func() bool {
		out := term.output.String()
		return strings.Contains(out, "+---") && strings.Contains(out, "|")
	})
}
