// Package server exposes the canvas over SSH.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	bm "github.com/charmbracelet/wish/bubbletea"
	gossh "golang.org/x/crypto/ssh"

	"github.com/jeninh/ssh.place/internal/app"
	"github.com/jeninh/ssh.place/internal/hub"
	"github.com/jeninh/ssh.place/internal/ratelimit"
	"github.com/jeninh/ssh.place/internal/tui"
)

// contextKey namespaces the values the admission middleware hands to the
// bubbletea handler.
type contextKey string

const (
	ctxKeySession   contextKey = "sshplace.session"
	ctxKeyExit      contextKey = "sshplace.exit"
	ctxKeyHandshake contextKey = "sshplace.handshake"
)

// Config configures a Server.
type Config struct {
	// Addr is the listen address, e.g. ":2222".
	Addr string
	// HostKeyPath is where the server's host key lives. It is generated on
	// first boot; keeping it on a persistent volume is what stops clients from
	// seeing a host key mismatch after a restart.
	HostKeyPath string
	// App is the shared canvas and its rules.
	App *app.App
	// IdleTimeout disconnects sessions that send no input.
	IdleTimeout time.Duration
	// WebURL is shown in the session help text, when set.
	WebURL string
	// Community is shown in the session help text after WebURL.
	Community string
	// Signoff is printed after the farewell, once the alternate screen is gone.
	Signoff string
	// BlocksOnly hides character mode from sessions. The App enforces it.
	BlocksOnly bool
	// MaxSessions is the hub's session cap. The listener uses it to bound how
	// many connections it will accept at once.
	MaxSessions int
	Logger      *slog.Logger
}

// Server is the SSH front end.
type Server struct {
	cfg Config
	log *slog.Logger
	ssh *ssh.Server
	ln  net.Listener
}

// New builds the server and binds its listener, so Addr is available before
// Serve is called. Tests rely on that to listen on port zero.
func New(cfg Config) (*Server, error) {
	if cfg.App == nil {
		return nil, fmt.Errorf("server: App is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{cfg: cfg, log: logger}

	// The SSH-level idle timeout is a backstop for connections that have gone
	// away without a FIN. It is deliberately looser than the session timeout,
	// which the TUI enforces on actual keystrokes and reports with a friendly
	// message.
	sshIdle := cfg.IdleTimeout
	if sshIdle > 0 {
		sshIdle += 5 * time.Minute
	}

	srv, err := wish.NewServer(
		wish.WithAddress(cfg.Addr),
		wish.WithHostKeyPath(cfg.HostKeyPath),
		wish.WithIdleTimeout(sshIdle),
		// There is no signup: any key is welcome, and clients with no key at
		// all fall back to keyboard-interactive and are identified by IP.
		wish.WithPublicKeyAuth(func(ctx ssh.Context, _ ssh.PublicKey) bool {
			authenticated(ctx)
			return true
		}),
		wish.WithKeyboardInteractiveAuth(func(ctx ssh.Context, _ gossh.KeyboardInteractiveChallenge) bool {
			authenticated(ctx)
			return true
		}),
		// Middleware runs in reverse order: logging first, then the PTY check,
		// then admission, and bubbletea innermost.
		wish.WithMiddleware(
			bm.Middleware(s.teaHandler),
			s.admit,
			activeterm.Middleware(),
			s.logConnections,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build ssh server: %w", err)
	}
	// Kill connections that never authenticate. The library only wraps the
	// connection for idle timeouts after this hook runs, so a deadline set here
	// would be replaced on the first read; a timer that closes the connection is
	// not.
	srv.ConnCallback = func(ctx ssh.Context, conn net.Conn) net.Conn {
		t := time.AfterFunc(handshakeTimeout, func() { _ = conn.Close() })
		ctx.SetValue(ctxKeyHandshake, t)
		return conn
	}
	s.ssh = srv

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	// The accept loop spawns a goroutine per connection with nothing bounding
	// it, and the session limit only applies once a client has authenticated and
	// asked for a terminal. Cap accepted connections so a flood of half-open
	// ones cannot exhaust memory.
	if cfg.MaxSessions > 0 {
		ln = newLimitListener(ln, cfg.MaxSessions*maxConnsPerSession)
	}
	s.ln = ln
	return s, nil
}

// Addr returns the bound listen address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Serve accepts connections until the server is shut down. It returns nil on a
// clean shutdown.
func (s *Server) Serve() error {
	err := s.ssh.Serve(s.ln)
	if err == ssh.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown stops accepting connections and waits for open sessions to finish,
// or for ctx to be done.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.ssh.Shutdown(ctx)
}

// Close drops the listener and every open connection immediately.
func (s *Server) Close() error { return s.ssh.Close() }

// admit registers the session with the hub, enforcing the capacity limits, and
// tears it down again on the way out.
func (s *Server) admit(next ssh.Handler) ssh.Handler {
	return func(sess ssh.Session) {
		ip := remoteIP(sess)
		// Budgets and connection counts are held against the client's network,
		// not its exact address. See ratelimit.NetKey: one IPv6 customer owns
		// enough addresses to make per-address limiting meaningless.
		network := ratelimit.NetKey(ip)
		identity, keyed := identityOf(sess, ip)

		hs, err := s.cfg.App.Hub.Add(network, identity, keyed)
		if err != nil {
			// A refused client gets a plain sentence, not a stack trace.
			fmt.Fprintf(sess, "\r\n  %s\r\n\r\n", err.Error())
			s.log.Info("rejected session", "ip", ip, "reason", err)
			_ = sess.Exit(1)
			return
		}
		// Removing closes the session's update channel, which is what lets the
		// TUI's listener goroutine finish.
		defer s.cfg.App.Hub.Remove(hs)

		exit := &tui.Exit{}
		sess.Context().SetValue(ctxKeySession, hs)
		sess.Context().SetValue(ctxKeyExit, exit)

		next(sess)

		// bubbletea has left the alternate screen by now, so this lands on the
		// user's real terminal.
		msg := exit.Reason()
		if msg == "" {
			msg = "Thanks for drawing on ssh.place. Come back and add another one."
		}
		fmt.Fprintf(sess, "%s\r\n", msg)
		// The help bar cannot always carry these: its widest line is exactly 80
		// columns, so on the commonest terminal size there is no room left for a
		// link. Here there is room, this lands on the real terminal rather than
		// inside the alternate screen, and leaving is the moment someone might
		// actually go looking for the rest of it.
		if s.cfg.Signoff != "" {
			fmt.Fprintf(sess, "%s\r\n", s.cfg.Signoff)
		}
	}
}

func (s *Server) teaHandler(sess ssh.Session) (tea.Model, []tea.ProgramOption) {
	hs, _ := sess.Context().Value(ctxKeySession).(*hub.Session)
	if hs == nil {
		// admit rejects before we get here; this only guards against a
		// middleware misordering.
		return nil, nil
	}
	exit, _ := sess.Context().Value(ctxKeyExit).(*tui.Exit)

	m := tui.New(tui.Config{
		App:         s.cfg.App,
		Session:     hs,
		Renderer:    newRenderer(sess),
		IdleTimeout: s.cfg.IdleTimeout,
		WebURL:      s.cfg.WebURL,
		Community:   s.cfg.Community,
		BlocksOnly:  s.cfg.BlocksOnly,
		RequireKey:  s.cfg.App.RequireKey,
		Exit:        exit,
	})
	// Mouse reporting lets the wheel pan the canvas and a click move the cursor.
	// It costs the terminal's native text selection, which is a fair trade for a
	// drawing surface; hold alt (or shift) to select as usual.
	return m, []tea.ProgramOption{tea.WithAltScreen(), tea.WithMouseCellMotion()}
}

func (s *Server) logConnections(next ssh.Handler) ssh.Handler {
	return func(sess ssh.Session) {
		start := time.Now()
		ip := remoteIP(sess)
		// The fingerprint goes in the log because the event log records identities
		// with no address and this log recorded addresses with no identity, so
		// neither could answer "which network is that placer on".
		//
		// Derived from the session rather than read out of the context: this
		// middleware is the outermost one, so the hub session is not in the context
		// yet and looking for it there logged "-" for every connection.
		identity, keyed := identityOf(sess, ip)
		s.log.Info("session start", "ip", ip, "identity", identity, "keyed", keyed,
			"client", sess.Context().ClientVersion())
		next(sess)
		s.log.Info("session end", "ip", ip, "duration", time.Since(start).Round(time.Second))
	}
}

// identityOf derives the rate-limiting identity for a session. The SSH public
// key fingerprint is stable and free for the user to produce, which makes it a
// better key than the address. Clients with no key share their IP's budget.
func identityOf(sess ssh.Session, ip string) (identity string, keyed bool) {
	if pk := sess.PublicKey(); pk != nil {
		return gossh.FingerprintSHA256(pk), true
	}
	return "ip:" + ip, false
}

// handshakeTimeout is how long a connection may stay unauthenticated. Without
// it, the only bound on a pre-auth connection is the idle timeout, which is
// measured in tens of minutes.
const handshakeTimeout = 20 * time.Second

// maxConnsPerSession caps accepted connections as a multiple of the session
// limit, leaving room for connections that are still authenticating.
const maxConnsPerSession = 4

// authenticated cancels the handshake deadline once a client has proved who it
// is; from then on the idle timeout takes over.
func authenticated(ctx ssh.Context) {
	if t, ok := ctx.Value(ctxKeyHandshake).(*time.Timer); ok {
		t.Stop()
	}
}

func remoteIP(sess ssh.Session) string {
	addr := sess.RemoteAddr()
	if addr == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
