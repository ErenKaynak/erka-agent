package tui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const (
	maxSummaryWidth    = 72
	expandedOutputRows = 14
	toolNameWidth      = 12
)

// markdownCache keeps rendered assistant messages, because re-rendering the
// whole transcript through glamour on every streamed token is slow.
type markdownCache struct {
	width    int
	dark     bool
	renderer *glamour.TermRenderer
	entries  map[string]string
}

func (cache *markdownCache) render(key, source string, width int, dark bool) string {
	if cache.renderer == nil || cache.width != width || cache.dark != dark {
		style := "light"
		if dark {
			style = "dark"
		}
		renderer, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width))
		if err != nil {
			return source
		}
		cache.renderer, cache.width, cache.dark, cache.entries = renderer, width, dark, make(map[string]string)
	}
	if value, ok := cache.entries[key]; ok {
		return value
	}
	value, err := cache.renderer.Render(source)
	if err != nil {
		value = source
	}
	value = trimBlankLines(value)
	cache.entries[key] = value
	return value
}

// renderTranscript draws the conversation as one column: prompts, inline tool
// steps with status, assistant replies, and stage separators.
func (model *Model) renderTranscript(width int) string {
	theme := model.theme
	if width < 20 {
		width = 20
	}
	results := make(map[erkaruntime.ToolCallID]*erkaruntime.ToolResult)
	for _, item := range model.conversation.Items {
		if item.Kind == erkaruntime.ItemToolResult {
			results[item.ToolResult.CallID] = item.ToolResult
		}
	}
	runs := make(map[erkaruntime.RunID]erkaruntime.Run, len(model.conversation.Runs))
	for _, run := range model.conversation.Runs {
		runs[run.ID] = run
	}
	pipeline := model.conversation.Task.Pipeline != ""

	blocks := make([]string, 0, len(model.conversation.Items)+2)
	tools := make([]string, 0)
	flushTools := func() {
		if len(tools) > 0 {
			blocks = append(blocks, strings.Join(tools, "\n"))
			tools = tools[:0]
		}
	}
	lastRun := erkaruntime.RunID("")
	for _, item := range model.conversation.Items {
		if pipeline && item.RunID != lastRun && item.RunID != "" {
			if run, ok := runs[item.RunID]; ok && item.Kind != erkaruntime.ItemUserMessage {
				flushTools()
				label := run.Stage
				if run.Model != "" {
					label += " · " + strings.TrimPrefix(run.Connection+"/"+run.Model, "/")
				}
				blocks = append(blocks, stageRule(theme, Sanitize(label), width))
				lastRun = item.RunID
			}
		}
		switch item.Kind {
		case erkaruntime.ItemUserMessage:
			flushTools()
			blocks = append(blocks, hang(theme.user.Render("›")+" ", theme.text.Bold(true), Sanitize(item.Message.Content), width))
		case erkaruntime.ItemAssistantMessage:
			flushTools()
			rendered := model.markdown.render(string(item.ID), Sanitize(item.Message.Content), width-2, theme.dark)
			blocks = append(blocks, bullet(theme.accent.Render("●"), rendered))
		case erkaruntime.ItemToolCall:
			tools = append(tools, model.toolLine(*item.ToolCall, results[item.ToolCall.CallID], width))
		case erkaruntime.ItemError:
			flushTools()
			blocks = append(blocks, hang(theme.danger.Render("✗")+" ", theme.danger, Sanitize(item.Error.Message), width))
		}
	}
	flushTools()
	if model.liveText != "" {
		blocks = append(blocks, hang(theme.accent.Render("●")+" ", theme.assistant, Sanitize(model.liveText)+"▌", width))
	}
	if len(blocks) == 0 {
		return model.welcome(width)
	}
	return strings.Join(blocks, "\n\n")
}

func (model *Model) welcome(width int) string {
	theme := model.theme
	lines := []string{
		theme.accent.Render("erka") + theme.muted.Render(" is ready in ") + theme.text.Render(Sanitize(model.projectName())),
		"",
		theme.muted.Render("Describe a change, ask about the code, or type / for commands."),
		theme.muted.Render("Ctrl+O conversations · Ctrl+D diff · ? help"),
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}

// toolLine renders one tool call as a status line, optionally followed by its
// output when details are expanded.
func (model *Model) toolLine(call erkaruntime.ToolCall, result *erkaruntime.ToolResult, width int) string {
	theme := model.theme
	var status string
	switch {
	case result == nil && model.active:
		status = theme.accent.Render(model.spinner.View())
	case result == nil:
		status = theme.faint.Render("·")
	case result.IsError:
		status = theme.danger.Render("✗")
	default:
		status = theme.success.Render("✓")
	}
	name := Sanitize(call.Tool)
	summary := Sanitize(toolSummary(call))
	outcome := ""
	if result != nil {
		outcome = Sanitize(resultSummary(call, *result))
	} else if !model.active {
		outcome = "not finished"
	}
	if padding := toolNameWidth - ansi.StringWidth(name); padding > 0 {
		name += strings.Repeat(" ", padding)
	}
	line := "  " + status + " " + theme.toolName.Render(name)
	if summary != "" {
		line += "  " + theme.text.Render(summary)
	}
	if outcome != "" {
		style := theme.muted
		if result != nil && result.IsError {
			style = theme.danger
		}
		line += "  " + style.Render(outcome)
	}
	line = ansi.Truncate(line, width, "…")
	if model.expanded && result != nil && strings.TrimSpace(result.Output) != "" {
		output := strings.Split(strings.TrimRight(Sanitize(result.Output), "\n"), "\n")
		hidden := 0
		if len(output) > expandedOutputRows {
			hidden = len(output) - expandedOutputRows
			output = output[:expandedOutputRows]
		}
		for _, row := range output {
			line += "\n" + ansi.Truncate(theme.faint.Render("    │ ")+theme.muted.Render(row), width, "…")
		}
		if hidden > 0 {
			line += "\n" + theme.faint.Render(fmt.Sprintf("    │ … %d more lines", hidden))
		}
	}
	return line
}

func toolArguments(call erkaruntime.ToolCall) map[string]any {
	values := map[string]any{}
	_ = json.Unmarshal(call.Arguments, &values)
	return values
}

// toolSummary describes what a tool call targets in a few words.
func toolSummary(call erkaruntime.ToolCall) string {
	arguments := toolArguments(call)
	text := func(key string) string {
		value, _ := arguments[key].(string)
		return value
	}
	summary := ""
	switch call.Tool {
	case "run_command":
		if parts, ok := arguments["command"].([]any); ok {
			words := make([]string, 0, len(parts))
			for _, part := range parts {
				if word, ok := part.(string); ok {
					words = append(words, word)
				}
			}
			summary = strings.Join(words, " ")
		}
	case "search_text":
		summary = strconv.Quote(text("query"))
		if path := text("path"); path != "" && path != "." {
			summary += " in " + path
		}
	case "apply_patch":
		files, _, _ := patchStats(text("patch"))
		summary = strings.Join(files, ", ")
	case erkaruntime.AskUserToolName:
		summary = text("question")
	case "memory_search":
		summary = strconv.Quote(text("query"))
	case "memory_save":
		summary = text("text")
	default:
		summary = text("path")
		if summary == "" && len(arguments) > 0 && string(call.Arguments) != "{}" {
			summary = string(call.Arguments)
		}
	}
	return shorten(strings.Join(strings.Fields(summary), " "), maxSummaryWidth)
}

// resultSummary condenses a tool result to a short outcome.
func resultSummary(call erkaruntime.ToolCall, result erkaruntime.ToolResult) string {
	output := strings.TrimSpace(result.Output)
	lines := nonEmptyLines(output)
	if result.IsError {
		if len(lines) == 0 {
			return "failed"
		}
		last := lines[len(lines)-1]
		if strings.HasPrefix(last, "exit_code=") {
			return "exit " + strings.TrimPrefix(last, "exit_code=")
		}
		return shorten(lines[0], 48)
	}
	switch call.Tool {
	case "run_command":
		return "ok"
	case "search_text":
		return plural(len(lines), "match", "matches")
	case "list_files":
		return plural(len(lines), "entry", "entries")
	case "read_file":
		return plural(len(strings.Split(result.Output, "\n")), "line", "lines")
	case "apply_patch":
		var arguments struct {
			Patch string `json:"patch"`
		}
		_ = json.Unmarshal(call.Arguments, &arguments)
		if _, added, removed := patchStats(arguments.Patch); added+removed > 0 {
			return fmt.Sprintf("+%d −%d", added, removed)
		}
		return ""
	case "memory_save":
		return ""
	case erkaruntime.AskUserToolName:
		return shorten(strings.TrimPrefix(output, "User answer: "), 40)
	case "git_diff":
		_, added, removed := patchStats(output)
		return fmt.Sprintf("+%d −%d", added, removed)
	}
	if len(lines) == 0 {
		return ""
	}
	return shorten(lines[0], 40)
}

func patchStats(patch string) ([]string, int, int) {
	files := make([]string, 0)
	added, removed := 0, 0
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			name := strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			if name != "/dev/null" {
				files = append(files, name)
			}
		case strings.HasPrefix(line, "--- "):
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return files, added, removed
}

func stageRule(theme theme, label string, width int) string {
	title := "── " + label + " "
	remaining := width - lipgloss.Width(title)
	if remaining > 0 {
		title += strings.Repeat("─", remaining)
	}
	return theme.rule.Render(ansi.Truncate(title, width, ""))
}

// hang wraps text after a prefix and indents continuation lines under it.
func hang(prefix string, style lipgloss.Style, text string, width int) string {
	indent := lipgloss.Width(prefix)
	body := style.Width(width - indent).Render(text)
	rows := strings.Split(body, "\n")
	for index := range rows {
		if index == 0 {
			rows[index] = prefix + rows[index]
		} else {
			rows[index] = strings.Repeat(" ", indent) + rows[index]
		}
	}
	return strings.Join(rows, "\n")
}

// bullet places a marker on the first line of glamour output, which already
// carries a two-column left margin.
func bullet(marker, rendered string) string {
	rows := strings.Split(rendered, "\n")
	for index, row := range rows {
		plain := ansi.Strip(row)
		if strings.TrimSpace(plain) == "" {
			continue
		}
		if strings.HasPrefix(plain, "  ") {
			rows[index] = marker + " " + ansi.Cut(row, 2, ansi.StringWidth(row))
		} else {
			rows[index] = marker + " " + row
		}
		break
	}
	return strings.Join(rows, "\n")
}

func trimBlankLines(value string) string {
	rows := strings.Split(value, "\n")
	start, end := 0, len(rows)
	for start < end && strings.TrimSpace(ansi.Strip(rows[start])) == "" {
		start++
	}
	for end > start && strings.TrimSpace(ansi.Strip(rows[end-1])) == "" {
		end--
	}
	return strings.Join(rows[start:end], "\n")
}

func nonEmptyLines(value string) []string {
	result := make([]string, 0)
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) != "" {
			result = append(result, strings.TrimSpace(line))
		}
	}
	return result
}

func shorten(value string, limit int) string {
	if ansi.StringWidth(value) <= limit {
		return value
	}
	return ansi.Truncate(value, limit, "…")
}

func plural(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return strconv.Itoa(count) + " " + many
}
