package tui

import "charm.land/lipgloss/v2"

// permissionMode is what the next turn may do. Shift+Tab cycles through the
// modes in order.
type permissionMode int

const (
	// modeAuto edits the workspace and asks before each command.
	modeAuto permissionMode = iota
	// modePlan is read-only and ends with a plan.
	modePlan
	// modeGod runs commands without asking.
	modeGod
)

func (mode permissionMode) next() permissionMode { return (mode + 1) % 3 }

func (mode permissionMode) label() string {
	switch mode {
	case modePlan:
		return "plan mode"
	case modeGod:
		return "god mode"
	default:
		return "auto"
	}
}

func (mode permissionMode) description() string {
	switch mode {
	case modePlan:
		return "plan mode: read-only, erka investigates and proposes a plan"
	case modeGod:
		return "god mode: full access, commands run without asking"
	default:
		return "auto mode: edits the workspace and asks before commands"
	}
}

// badge renders the mode for the header and status line.
func (theme theme) modeBadge(mode permissionMode) string {
	switch mode {
	case modePlan:
		return theme.plan.Bold(true).Render("⏸ plan mode")
	case modeGod:
		return theme.god.Bold(true).Render("⚡ god mode")
	default:
		return theme.muted.Render("auto")
	}
}

func (theme theme) composerFor(mode permissionMode) lipgloss.Style {
	switch mode {
	case modePlan:
		return theme.composer.BorderForeground(theme.plan.GetForeground())
	case modeGod:
		return theme.composer.BorderForeground(theme.god.GetForeground())
	default:
		return theme.composer
	}
}

// setMode applies a mode to the session. Changes take effect from the next
// turn; a running turn keeps the permissions it started with.
func (model *Model) setMode(mode permissionMode) {
	model.mode = mode
	model.full = mode == modeGod
	if model.controller != nil {
		model.controller.SetFullExecution(model.full)
		model.controller.SetPlanMode(mode == modePlan)
	}
	model.status = mode.description()
	if model.active {
		model.status += " (from the next turn)"
	}
}
