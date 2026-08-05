package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// keylessTitle and keylessBody are the whole of what a keyless visitor is told.
//
// Written to be read by someone who did nothing wrong, because that is who most
// of them are: it explains the constraint, says plainly why it exists, and gives
// the one command that fixes it. No scolding, and no pretending the limit is for
// their benefit.
const keylessTitle = "You are watching"

// Hard wrapped to fit inside splashWidth minus its padding. Doing it by hand
// rather than letting lipgloss reflow keeps the two command lines intact, which
// is the part that has to survive.
var keylessBody = []string{
	"You connected without an SSH key, so the only thing I",
	"know about you is your network, and you might be",
	"sharing that with a whole building. A few people used",
	"exactly that to hammer the canvas, so keyless sessions",
	"can watch but not place.",
	"",
	"Fixing it takes one command:",
	"",
	"    ssh-keygen -t ed25519",
	"    ssh ssh.place",
	"",
	"press any key to carry on watching",
}

// keylessNarrow is the fallback for a terminal too small for the box. Losing the
// explanation is fine; losing the command that fixes it is not.
var keylessNarrow = []string{
	"no ssh key, so",
	"watching only.",
	"",
	"ssh-keygen -t ed25519",
	"then reconnect.",
	"",
	"any key",
}

// splashWidth is the widest the box will grow. Chosen so the box still fits an
// 80 column terminal with room to spare, which is what most people have.
const splashWidth = 66

// keylessSplash renders the full-screen notice, sized to the terminal.
func (m *Model) keylessSplash() string {
	if m.termW < splashWidth+4 || m.termH < len(keylessBody)+6 {
		return m.styles.help.Render(strings.Join(fitLines(keylessNarrow, m.termW), "\n"))
	}

	lines := make([]string, 0, len(keylessBody)+2)
	lines = append(lines, m.styles.splashTitle.Render(keylessTitle), "")
	for _, l := range keylessBody {
		if l == "press any key to carry on watching" {
			l = m.styles.help.Render(l)
		}
		lines = append(lines, l)
	}

	box := m.styles.splashBox.Width(splashWidth).Render(strings.Join(lines, "\n"))

	// Centre it in the terminal so it reads as a panel rather than as output that
	// happened to be printed.
	return lipgloss.PlaceHorizontal(m.termW, lipgloss.Center,
		lipgloss.PlaceVertical(m.termH, lipgloss.Center, box))
}

// fitLines truncates each line to width, so a narrow terminal wraps nothing.
func fitLines(in []string, width int) []string {
	out := make([]string, len(in))
	for i, l := range in {
		out[i] = fit(l, width)
	}
	return out
}
