// Command sshplace runs ssh.place: a shared ASCII canvas that anyone can draw
// on over SSH.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jeninh/ssh.place/internal/app"
	"github.com/jeninh/ssh.place/internal/canvas"
	"github.com/jeninh/ssh.place/internal/eventlog"
	"github.com/jeninh/ssh.place/internal/hub"
	"github.com/jeninh/ssh.place/internal/ratelimit"
	"github.com/jeninh/ssh.place/internal/server"
	"github.com/jeninh/ssh.place/internal/timelapse"
	"github.com/jeninh/ssh.place/internal/web"
)

type config struct {
	sshAddr     string
	httpAddr    string
	dataDir     string
	width       int
	height      int
	cooldown    time.Duration
	ipBurst     int
	ipRefill    time.Duration
	maxSessions int
	maxPerIP    int
	idleTimeout time.Duration
	snapshotFor time.Duration
	eventLog    bool
	webURL      string
	community   string
	logLevel    string
	mode        string
	minFill     time.Duration
	adminKeys   string
	blockedKeys string
	requireKey  bool
	timelapse   bool
	lapseScale  int
	lapseFrames int
	slowKeys    string
	slowFactor  float64
	healthcheck string
}

// shutdownGrace is how long to let sessions finish before dropping them.
//
// Kept well inside the container runtime's own stop timeout, which defaults to
// ten seconds and then SIGKILLs. Interactive sessions never finish voluntarily,
// so a long wait here only delays the exit and risks the process being killed
// mid-cleanup.
const shutdownGrace = 2 * time.Second

func parseFlags() config {
	var c config
	flag.StringVar(&c.sshAddr, "ssh-addr", envOr("SSHPLACE_SSH_ADDR", ":2222"), "SSH listen address")
	flag.StringVar(&c.httpAddr, "http-addr", envOr("SSHPLACE_HTTP_ADDR", ":8080"), "HTTP listen address for the read-only web view (empty to disable)")
	flag.StringVar(&c.dataDir, "data", envOr("SSHPLACE_DATA", "./data"), "directory for the host key, canvas snapshot and event log")
	flag.IntVar(&c.width, "width", canvas.DefaultWidth, "canvas width in cells")
	flag.IntVar(&c.height, "height", canvas.DefaultHeight, "canvas height in cells")
	flag.DurationVar(&c.cooldown, "cooldown", 15*time.Second, "per-key wait between placements")
	flag.IntVar(&c.ipBurst, "ip-burst", 5, "placements a single IP may make back to back")
	flag.DurationVar(&c.ipRefill, "ip-refill", 0, "how long a network takes to earn one more placement (0 derives it from -cooldown and -max-per-ip)")
	flag.IntVar(&c.maxSessions, "max-sessions", 500, "maximum concurrent sessions")
	flag.IntVar(&c.maxPerIP, "max-per-ip", 5, "maximum concurrent sessions per IP")
	flag.DurationVar(&c.idleTimeout, "idle-timeout", 30*time.Minute, "disconnect sessions with no input for this long")
	flag.DurationVar(&c.snapshotFor, "snapshot-interval", 10*time.Second, "how often to write the canvas snapshot")
	flag.BoolVar(&c.eventLog, "event-log", true, "append every placement to events.jsonl")
	flag.StringVar(&c.webURL, "web-url", envOr("SSHPLACE_WEB_URL", "https://ssh.place"), "URL shown in the in-session help text")
	flag.StringVar(&c.community, "community", envOr("SSHPLACE_COMMUNITY", "r/sshplace"), "community name shown in the in-session help text (empty to hide)")
	flag.DurationVar(&c.minFill, "min-board-fill", time.Hour, "floor on how long repainting every cell of the canvas can take, whatever a client controls (0 disables the ceiling)")
	flag.StringVar(&c.mode, "mode", envOr("SSHPLACE_MODE", "blocks"), `"blocks" for solid color only, or "mixed" to also allow characters`)
	flag.StringVar(&c.adminKeys, "admin-keys", envOr("SSHPLACE_ADMIN_KEYS", ""), "comma separated SSH key fingerprints exempt from every rate limit, e.g. SHA256:abc...")
	flag.StringVar(&c.blockedKeys, "blocked-keys", envOr("SSHPLACE_BLOCKED_KEYS", ""), "comma separated SSH key fingerprints refused at placement time")
	flag.BoolVar(&c.timelapse, "timelapse", true, "render daily and whole-history timelapse GIFs from the event log and serve them at /timelapse")
	flag.IntVar(&c.lapseScale, "timelapse-scale", 5, "pixels per cell in rendered timelapses")
	flag.IntVar(&c.lapseFrames, "timelapse-frames", 300, "frames per rendered timelapse")
	flag.BoolVar(&c.requireKey, "require-key", true, "only let clients with an SSH public key place; keyless clients can still watch")
	flag.StringVar(&c.slowKeys, "slow-keys", envOr("SSHPLACE_SLOW_KEYS", ""), "comma separated SSH key fingerprints put on a longer cooldown instead of being blocked")
	flag.Float64Var(&c.slowFactor, "slow-factor", 4, "multiplier on -cooldown for -slow-keys (1 disables the slowing)")
	flag.StringVar(&c.logLevel, "log-level", envOr("SSHPLACE_LOG_LEVEL", "info"), "log level: debug, info, warn or error")
	flag.StringVar(&c.healthcheck, "healthcheck", "", "probe this URL, exit 0 if healthy, then stop; for container health checks")
	flag.Parse()
	return c
}

// healthcheck probes url and reports whether it answered with a 2xx. The
// container image is distroless and has no shell or curl, so the binary has to
// be able to check itself.
func healthcheck(url string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return nil
}

// derivedIPRefill sizes the per-network placement budget to match the number of
// sessions a network is allowed to hold.
//
// The budget exists to stop one client gaining anything by rotating keys, but a
// network is also a whole building full of people behind one address, or one
// household sharing a /64. Refilling it at the same rate as a single player's
// cooldown gives the entire network one player's throughput, so the first person
// to ask each round takes it and everyone else is refused despite having waited
// out their own cooldown properly.
//
// Sizing it at maxPerIP placements per cooldown is the rate that the most
// sessions we will accept from one network can legitimately produce. Rotating
// keys still buys nothing beyond that, because holding the sessions is what is
// actually capped.
func derivedIPRefill(cooldown time.Duration, maxPerIP int) time.Duration {
	if cooldown <= 0 {
		return time.Second
	}
	if maxPerIP < 1 {
		return cooldown
	}
	return cooldown / time.Duration(maxPerIP)
}

// boardChurnCeiling converts "repainting the whole board must take at least
// this long" into a placements-per-second ceiling.
//
// Expressing it that way round is the point: it is the property anyone actually
// cares about, it scales with the canvas automatically, and unlike the per-key
// and per-network limits it cannot be widened by acquiring more keys or more
// addresses.
//
// The burst is one cooldown's worth, so a crowd that has all been waiting
// politely can fire together without anyone being turned away for it.
func boardChurnCeiling(cells int, minFill, cooldown time.Duration) (perSecond float64, burst int) {
	if minFill <= 0 || cells <= 0 {
		return 0, 0
	}
	perSecond = float64(cells) / minFill.Seconds()
	burst = int(perSecond * cooldown.Seconds())
	if burst < 1 {
		burst = 1
	}
	return perSecond, burst
}

// parseMode turns the -mode flag into the canvas policy.
//
// Blocks-only is the default. On a canvas anyone can reach, characters get used
// to write at each other far more than to draw, so they are off unless someone
// deliberately turns them on.
func parseMode(v string) (blocksOnly bool, err error) {
	switch v {
	case "blocks", "block":
		return true, nil
	case "mixed", "chars", "characters":
		return false, nil
	}
	return false, fmt.Errorf("-mode must be \"blocks\" or \"mixed\", got %q", v)
}

// signoff is the line printed as a session ends.
//
// It exists because the in-session help bar is width limited and its fullest
// line already fills an 80 column terminal, so on the commonest size there is
// nowhere to put a link. This lands on the real terminal after the TUI has gone.
func signoff(webURL, community string) string {
	var parts []string
	if webURL != "" {
		parts = append(parts, "Timelapses and stats: "+webURL)
	}
	if community != "" {
		parts = append(parts, community)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// parseFingerprints turns a comma separated flag into a lookup set.
//
// Fingerprints are not secrets, so there is nothing to protect here beyond
// being strict about the format: a typo would otherwise silently grant nobody
// anything, and you would only find out when you needed it.
func parseFingerprints(raw string) (map[string]bool, []string) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	set := make(map[string]bool)
	var bad []string
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		// This is the exact form `ssh-keygen -lf` prints and the form the SSH
		// server computes at auth time. Anything else can never match, so say so
		// at boot rather than failing silently under pressure.
		if !strings.HasPrefix(f, "SHA256:") {
			bad = append(bad, f)
			continue
		}
		set[f] = true
	}
	if len(set) == 0 {
		return nil, bad
	}
	return set, bad
}

func main() {
	cfg := parseFlags()

	if cfg.healthcheck != "" {
		if err := healthcheck(cfg.healthcheck); err != nil {
			fmt.Fprintf(os.Stderr, "ssh.place: unhealthy: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ssh.place: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	logger := newLogger(cfg.logLevel)

	if cfg.width <= 0 || cfg.height <= 0 {
		return errors.New("canvas width and height must be positive")
	}
	blocksOnly, err := parseMode(cfg.mode)
	if err != nil {
		return err
	}
	ipRefill := cfg.ipRefill
	if ipRefill <= 0 {
		ipRefill = derivedIPRefill(cfg.cooldown, cfg.maxPerIP)
	}
	if err := os.MkdirAll(cfg.dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	var (
		snapshotPath = filepath.Join(cfg.dataDir, "canvas.json")
		hostKeyPath  = filepath.Join(cfg.dataDir, "ssh_host_ed25519_key")
		eventLogPath = filepath.Join(cfg.dataDir, "events.jsonl")
		lapseDir     = filepath.Join(cfg.dataDir, "timelapse")
	)

	board, err := canvas.Load(snapshotPath, cfg.width, cfg.height)
	if err != nil {
		return err
	}

	var events *eventlog.Log
	if cfg.eventLog {
		events, err = eventlog.Open(eventLogPath)
		if err != nil {
			return err
		}
		defer func() {
			if err := events.Close(); err != nil {
				logger.Error("close event log", "err", err)
			}
		}()
	}

	// The ceiling that holds regardless of how many keys or networks a client
	// has. Derived from the board size so it keeps meaning the same thing if the
	// canvas is resized.
	globalRate, globalBurst := boardChurnCeiling(cfg.width*cfg.height, cfg.minFill, cfg.cooldown)
	limiter := ratelimit.New(cfg.cooldown, cfg.ipBurst, ipRefill,
		ratelimit.WithGlobalRate(globalRate, globalBurst))

	admins, badAdmins := parseFingerprints(cfg.adminKeys)
	for _, f := range badAdmins {
		// Not fatal: a bad entry should not stop the canvas from coming up.
		logger.Warn("ignoring admin key, not a SHA256 fingerprint", "value", f)
	}
	slowed, badSlowed := parseFingerprints(cfg.slowKeys)
	for _, f := range badSlowed {
		logger.Warn("ignoring slow key, not a SHA256 fingerprint", "value", f)
	}
	blocked, badBlocked := parseFingerprints(cfg.blockedKeys)
	for _, f := range badBlocked {
		logger.Warn("ignoring blocked key, not a SHA256 fingerprint", "value", f)
	}
	// A key that is both exempt and blocked is a config mistake worth shouting
	// about, because Place resolves it as blocked and the operator probably meant
	// the opposite.
	for f := range admins {
		if blocked[f] {
			logger.Warn("key is both admin and blocked, treating as blocked", "key", f)
		}
	}

	logger.Info("canvas ready",
		"width", cfg.width, "height", cfg.height,
		"drawn", board.NonEmpty(), "mode", cfg.mode,
		"cooldown", cfg.cooldown, "ip_refill", ipRefill,
		"max_placements_per_sec", fmt.Sprintf("%.2f", globalRate),
		"min_board_fill", cfg.minFill, "snapshot", snapshotPath,
		"admin_keys", len(admins), "blocked_keys", len(blocked),
		"require_key", cfg.requireKey, "slow_keys", len(slowed), "slow_cooldown", time.Duration(float64(cfg.cooldown)*cfg.slowFactor))
	a := &app.App{
		Canvas:     board,
		Hub:        hub.New(cfg.maxSessions, cfg.maxPerIP),
		Limiter:    limiter,
		Log:        events,
		Logger:     logger,
		BlocksOnly: blocksOnly,
		Admins:     admins,
		Blocked:    blocked,
		RequireKey: cfg.requireKey,
		Slowed:     slowed,
		SlowFactor: cfg.slowFactor,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sshSrv, err := server.New(server.Config{
		Addr:        cfg.sshAddr,
		HostKeyPath: hostKeyPath,
		App:         a,
		IdleTimeout: cfg.idleTimeout,
		WebURL:      cfg.webURL,
		Community:   cfg.community,
		Signoff:     signoff(cfg.webURL, cfg.community),
		BlocksOnly:  blocksOnly,
		MaxSessions: cfg.maxSessions,
		Logger:      logger,
	})
	if err != nil {
		return err
	}

	// Timelapses are rendered from the event log, so they are only available when
	// it is being written.
	var lapses *timelapse.Store
	if cfg.eventLog && cfg.timelapse {
		opt := timelapse.DefaultOptions(cfg.width, cfg.height)
		opt.Scale = cfg.lapseScale
		opt.Frames = cfg.lapseFrames
		lapses = &timelapse.Store{
			Dir: lapseDir, LogPath: eventLogPath,
			Width: cfg.width, Height: cfg.height,
			Opt: opt, Logger: logger,
		}
	}

	var httpSrv *http.Server
	if cfg.httpAddr != "" {
		httpSrv = &http.Server{
			Addr:    cfg.httpAddr,
			Handler: web.Handler(a, lapses),
			// Every phase gets a bound, so a client that stalls mid-request or
			// reads a PNG one byte at a time cannot hold a connection open.
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 << 10,
		}
	}

	// background holds the goroutines that must finish before we save and exit.
	var background sync.WaitGroup

	background.Add(1)
	go func() {
		defer background.Done()
		snapshotLoop(ctx, logger, board, snapshotPath, events, cfg.snapshotFor)
	}()

	background.Add(1)
	go func() {
		defer background.Done()
		pruneLoop(ctx, logger, limiter)
	}()

	if lapses != nil {
		background.Add(1)
		go func() {
			defer background.Done()
			// Renders on boot to fill in any day that has no file yet, which is what
			// makes the history retroactive, then once after every UTC midnight.
			lapses.Run(ctx)
		}()
	}

	serveErr := make(chan error, 2)

	background.Add(1)
	go func() {
		defer background.Done()
		logger.Info("ssh listening", "addr", sshSrv.Addr().String())
		if err := sshSrv.Serve(); err != nil {
			serveErr <- fmt.Errorf("ssh server: %w", err)
		}
	}()

	if httpSrv != nil {
		background.Add(1)
		go func() {
			defer background.Done()
			logger.Info("http listening", "addr", httpSrv.Addr)
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("http server: %w", err)
			}
		}()
	}

	// Wait for a signal or for a listener to fail.
	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case runErr = <-serveErr:
		logger.Error("listener failed", "err", runErr)
	}
	stop() // unblock the background loops even if we got here via serveErr

	// Save first. Everyone drawing is sitting in a full-screen TUI that will
	// never close on its own, so waiting for sessions before saving burns the
	// whole shutdown budget on a wait that cannot succeed, and the container
	// runtime SIGKILLs us before the canvas reaches disk.
	saveCanvas := func(stage string) {
		if err := board.Save(snapshotPath); err != nil {
			logger.Error("snapshot", "stage", stage, "err", err)
			if runErr == nil {
				runErr = err
			}
			return
		}
		logger.Info("snapshot saved", "stage", stage, "drawn", board.NonEmpty())
	}
	saveCanvas("shutdown")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if httpSrv != nil {
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("http shutdown", "err", err)
		}
	}
	if err := sshSrv.Shutdown(shutdownCtx); err != nil {
		// Expected, not exceptional: interactive sessions do not end themselves.
		logger.Info("dropping live sessions", "after", shutdownGrace)
		if err := sshSrv.Close(); err != nil {
			logger.Error("ssh close", "err", err)
		}
	}

	background.Wait()

	// Again at the very end, in case anything landed while sessions were being
	// closed. Writing a 36 KB file twice costs nothing.
	saveCanvas("final")
	return runErr
}

// snapshotLoop persists the canvas periodically so a crash costs at most one
// interval of drawing.
func snapshotLoop(ctx context.Context, logger *slog.Logger, board *canvas.Canvas, path string, events *eventlog.Log, every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()

	var lastVersion uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := events.Flush(); err != nil {
				logger.Error("flush event log", "err", err)
			}
			// Skip the write when nothing changed; an idle canvas should not
			// rewrite the same file every interval.
			v := board.Version()
			if v == lastVersion {
				continue
			}
			if err := board.Save(path); err != nil {
				logger.Error("snapshot", "err", err)
				continue
			}
			lastVersion = v
		}
	}
}

// pruneLoop keeps the limiter's bookkeeping from growing without bound as keys
// and addresses come and go.
func pruneLoop(ctx context.Context, logger *slog.Logger, limiter *ratelimit.Limiter) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n := limiter.Prune(now); n > 0 {
				ids, ips := limiter.Len()
				logger.Debug("pruned limiter", "removed", n, "identities", ids, "ips", ips)
			}
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
