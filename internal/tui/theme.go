package tui

import (
	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
)

// theme holds every style the TUI draws with. It follows the terminal's
// background so text stays legible on light and dark themes.
type theme struct {
	dark bool

	text, muted, faint, accent, success, danger, warn lipgloss.Style
	user, assistant, toolName, rule                   lipgloss.Style
	composer, card                                    lipgloss.Style
	diffAdd, diffDel, diffHunk, diffFile              lipgloss.Style
	selected                                          lipgloss.Style
	plan, god                                         lipgloss.Style
}

func newTheme(dark bool) theme {
	pick := lipgloss.LightDark(dark)
	text := pick(lipgloss.Color("#1F2328"), lipgloss.Color("#E6E9EF"))
	muted := pick(lipgloss.Color("#59636E"), lipgloss.Color("#8B949E"))
	faint := pick(lipgloss.Color("#8C959F"), lipgloss.Color("#5C6570"))
	accent := pick(lipgloss.Color("#0B7A69"), lipgloss.Color("#4FD1B5"))
	success := pick(lipgloss.Color("#1A7F37"), lipgloss.Color("#56D364"))
	danger := pick(lipgloss.Color("#CF222E"), lipgloss.Color("#FF7B72"))
	warn := pick(lipgloss.Color("#9A6700"), lipgloss.Color("#E3B341"))
	line := pick(lipgloss.Color("#D0D7DE"), lipgloss.Color("#30363D"))
	return theme{
		dark:      dark,
		text:      lipgloss.NewStyle().Foreground(text),
		muted:     lipgloss.NewStyle().Foreground(muted),
		faint:     lipgloss.NewStyle().Foreground(faint),
		accent:    lipgloss.NewStyle().Foreground(accent).Bold(true),
		success:   lipgloss.NewStyle().Foreground(success),
		danger:    lipgloss.NewStyle().Foreground(danger),
		warn:      lipgloss.NewStyle().Foreground(warn),
		user:      lipgloss.NewStyle().Foreground(accent).Bold(true),
		assistant: lipgloss.NewStyle().Foreground(text),
		toolName:  lipgloss.NewStyle().Foreground(text).Bold(true),
		rule:      lipgloss.NewStyle().Foreground(faint),
		composer:  lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(line).Padding(0, 1),
		card:      lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(warn).Padding(0, 1),
		diffAdd:   lipgloss.NewStyle().Foreground(success),
		diffDel:   lipgloss.NewStyle().Foreground(danger),
		diffHunk:  lipgloss.NewStyle().Foreground(accent),
		diffFile:  lipgloss.NewStyle().Foreground(text).Bold(true),
		selected:  lipgloss.NewStyle().Foreground(accent).Bold(true),
		plan:      lipgloss.NewStyle().Foreground(pick(lipgloss.Color("#8250DF"), lipgloss.Color("#BC8CFF"))),
		god:       lipgloss.NewStyle().Foreground(pick(lipgloss.Color("#CF222E"), lipgloss.Color("#FF5F5F"))),
	}
}

func (theme theme) glamourStyle() string {
	if theme.dark {
		return "dark"
	}
	return "light"
}

func (theme theme) composerStyles() textarea.Styles {
	styles := textarea.DefaultStyles(theme.dark)
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Focused.Placeholder = theme.faint
	styles.Blurred.Placeholder = theme.faint
	styles.Focused.Prompt = theme.accent
	return styles
}
