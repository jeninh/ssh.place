package tui

import (
	"github.com/charmbracelet/lipgloss"
)

// styles holds the chrome styles for one session. They are built from the
// session's own renderer so a client without color support gets plain text.
type styles struct {
	logo        lipgloss.Style
	coords      lipgloss.Style
	ready       lipgloss.Style
	eraser      lipgloss.Style
	splashBox   lipgloss.Style
	splashTitle lipgloss.Style
	cooling     lipgloss.Style
	online      lipgloss.Style
	help        lipgloss.Style
	flash       lipgloss.Style
	literal     lipgloss.Style

	// base is the template for per-color swatches.
	base lipgloss.Style
}

func newStyles(r *lipgloss.Renderer) styles {
	newStyle := lipgloss.NewStyle
	if r != nil {
		newStyle = r.NewStyle
	}
	return styles{
		logo:   newStyle().Bold(true).Foreground(lipgloss.Color("13")),
		coords: newStyle().Foreground(lipgloss.Color("14")),
		ready:  newStyle().Foreground(lipgloss.Color("10")),
		// Reverse video so it reads as a mode rather than as one more colour to
		// choose from.
		eraser:  newStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("15")),
		cooling: newStyle().Foreground(lipgloss.Color("11")),
		online:  newStyle().Foreground(lipgloss.Color("8")),
		help:    newStyle().Foreground(lipgloss.Color("8")),
		flash:   newStyle().Foreground(lipgloss.Color("9")).Bold(true),
		literal: newStyle().Foreground(lipgloss.Color("11")).Bold(true),
		base:    newStyle(),
		splashBox: newStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("8")).
			Padding(1, 3),
		splashTitle: newStyle().Bold(true).Foreground(lipgloss.Color("15")),
	}
}

// swatch styles the stamp indicator in the currently selected color.
func (s styles) swatch(color uint8) lipgloss.Style {
	return s.base.Foreground(ansiColor(color))
}
