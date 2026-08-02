package server

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/ssh"
	"github.com/muesli/termenv"
)

// newRenderer builds a lipgloss renderer for sess without ever asking the
// client's terminal a question.
//
// wish's own MakeRenderer, and termenv's color cache, detect the terminal's
// background by writing an OSC 11 query to the session and reading the reply
// back off it. Over SSH that is a bad trade three times over: it delays the
// first frame by up to a second on every connection, it consumes anything the
// user typed while it waits (their first keystroke is simply eaten), and a
// client that never answers leaves the session stuck behind the read.
//
// ssh.place draws from a fixed 16-color palette and uses no adaptive colors, so
// the answer would be discarded even if it arrived. Derive the color profile
// from TERM instead, and pin every value the renderer might otherwise go
// looking for later.
func newRenderer(sess ssh.Session) *lipgloss.Renderer {
	pty, _, ok := sess.Pty()
	if !ok || pty.Term == "" || pty.Term == "dumb" {
		return pinned(lipgloss.NewRenderer(sess, termenv.WithProfile(termenv.Ascii)), termenv.Ascii)
	}

	env := sessionEnviron(append(sess.Environ(), "TERM="+pty.Term))
	r := lipgloss.NewRenderer(sess,
		termenv.WithEnvironment(env),
		// The session carries a terminal even though it is not an os.File.
		// Without this, termenv's tty check fails and everything degrades to
		// no color at all.
		termenv.WithUnsafe(),
	)
	// Reads TERM and COLORTERM out of env; no I/O.
	return pinned(r, r.ColorProfile())
}

// pinned fixes the renderer's profile and background so no later call can
// trigger a terminal query. The background value is arbitrary because nothing
// in ssh.place is adaptive; what matters is that it is already answered.
func pinned(r *lipgloss.Renderer, profile termenv.Profile) *lipgloss.Renderer {
	r.SetColorProfile(profile)
	r.SetHasDarkBackground(true)
	return r
}

// sessionEnviron adapts an SSH session's environment to termenv.Environ.
type sessionEnviron []string

var _ termenv.Environ = sessionEnviron(nil)

func (e sessionEnviron) Environ() []string { return e }

func (e sessionEnviron) Getenv(key string) string {
	prefix := key + "="
	for _, kv := range e {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}
