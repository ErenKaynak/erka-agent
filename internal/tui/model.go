package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

	"github.com/ErenKaynak/erka-agent/internal/app"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

type Controller interface {
	Root() string
	ReadOnly() bool
	ListConversations(context.Context) ([]erkaruntime.TaskSnapshot, error)
	OpenConversation(context.Context, erkaruntime.TaskID) (app.Conversation, error)
	Submit(context.Context, erkaruntime.TaskID, string) (erkaruntime.TaskID, error)
	RunNext(context.Context) (app.LocalResult, bool, error)
	Resume(context.Context, erkaruntime.TaskID) (app.LocalResult, error)
	Diff(context.Context) (string, error)
	UnknownOutcome(context.Context, erkaruntime.TaskID) (erkaruntime.ToolCall, bool, error)
	ResolveUnknown(context.Context, erkaruntime.TaskID, bool) error
	Cancel()
	SetFullExecution(bool)
}

type Options struct {
	Context    context.Context
	Controller Controller
	TaskID     erkaruntime.TaskID
	Provider   string
	Model      string
	Branch     string
	Full       bool
	Live       <-chan erkaruntime.LiveUpdate
}

type Model struct {
	ctx                              context.Context
	controller                       Controller
	composer                         textarea.Model
	transcript                       viewport.Model
	activity                         viewport.Model
	width, height                    int
	taskID                           erkaruntime.TaskID
	conversation                     app.Conversation
	conversations                    []erkaruntime.TaskSnapshot
	provider, model, branch          string
	full, active, ready, confirmExit bool
	live                             <-chan erkaruntime.LiveUpdate
	liveText, status, errText        string
	overlay                          string
	pickerIndex                      int
	diff                             string
	unknown                          erkaruntime.ToolCall
}

type loadedMsg struct {
	conversation  app.Conversation
	conversations []erkaruntime.TaskSnapshot
	err           error
	unknown       erkaruntime.ToolCall
}
type submittedMsg struct {
	taskID erkaruntime.TaskID
	err    error
}
type runFinishedMsg struct {
	result app.LocalResult
	ran    bool
	err    error
}
type liveMsg erkaruntime.LiveUpdate
type diffMsg struct {
	value string
	err   error
}
type resolvedMsg struct{ err error }

func NewModel(options Options) *Model {
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	composer := textarea.New()
	composer.Placeholder = "Ask erka to change this repository…"
	composer.Prompt = "❯ "
	composer.ShowLineNumbers = false
	composer.CharLimit = 32 << 10
	composer.MaxHeight = 6
	composer.SetHeight(3)
	composer.Focus()
	transcript := viewport.New()
	transcript.SoftWrap = true
	activity := viewport.New()
	activity.SoftWrap = true
	return &Model{ctx: ctx, controller: options.Controller, composer: composer, transcript: transcript, activity: activity, taskID: options.TaskID, provider: options.Provider, model: options.Model, branch: options.Branch, full: options.Full, live: options.Live, status: "ready"}
}

func (model *Model) Init() tea.Cmd { return tea.Batch(model.load(), model.waitLive()) }

func (model *Model) load() tea.Cmd {
	return func() tea.Msg {
		if model.controller == nil {
			return loadedMsg{err: fmt.Errorf("TUI controller is unavailable")}
		}
		conversations, err := model.controller.ListConversations(model.ctx)
		if err != nil {
			return loadedMsg{err: err}
		}
		taskID := model.taskID
		if taskID == "" && len(conversations) > 0 {
			taskID = conversations[0].ID
		}
		if taskID == "" {
			return loadedMsg{conversations: conversations}
		}
		conversation, err := model.controller.OpenConversation(model.ctx, taskID)
		if err != nil {
			return loadedMsg{conversations: conversations, err: err}
		}
		unknown, _, err := model.controller.UnknownOutcome(model.ctx, taskID)
		return loadedMsg{conversation: conversation, conversations: conversations, unknown: unknown, err: err}
	}
}

func (model *Model) waitLive() tea.Cmd {
	if model.live == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case update := <-model.live:
			return liveMsg(update)
		case <-model.ctx.Done():
			return nil
		}
	}
}

func (model *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = message.Width, message.Height
		model.resize()
		return model, nil
	case loadedMsg:
		model.ready = true
		model.errText = errorText(message.err)
		model.conversations = message.conversations
		if message.err == nil && message.conversation.Task.ID != "" {
			model.taskID = message.conversation.Task.ID
			model.conversation = message.conversation
			model.unknown = message.unknown
			if model.conversation.Task.Status == erkaruntime.TaskWaiting && model.unknown.CallID != "" {
				model.overlay = "recovery"
			}
			model.refresh()
			if !model.active && hasQueuedTurn(message.conversation.Turns) {
				model.active = true
				return model, model.runNext()
			}
			if !model.active && hasActiveTurn(message.conversation.Turns) {
				model.active = true
				return model, model.resume()
			}
		}
		return model, nil
	case submittedMsg:
		if message.err != nil {
			model.errText = errorText(message.err)
			model.status = "submit failed"
			return model, nil
		}
		model.taskID = message.taskID
		model.status = "queued"
		if model.active {
			return model, nil
		}
		model.active = true
		return model, model.runNext()
	case runFinishedMsg:
		model.active = false
		model.liveText = ""
		model.errText = errorText(message.err)
		if message.err != nil {
			model.status = "paused"
		} else {
			model.status = "ready"
		}
		return model, model.load()
	case liveMsg:
		update := erkaruntime.LiveUpdate(message)
		switch update.Kind {
		case erkaruntime.LiveTextDelta:
			model.liveText += Sanitize(update.Text)
			model.status = "thinking"
		case erkaruntime.LiveCommitted:
			model.liveText = ""
			model.status = "working"
		case erkaruntime.LiveToolStarted:
			model.status = "tool: " + Sanitize(update.Tool)
		case erkaruntime.LiveToolFinished:
			model.status = "working"
		}
		model.refresh()
		return model, model.waitLive()
	case diffMsg:
		model.errText = errorText(message.err)
		model.diff = Sanitize(message.value)
		if message.err == nil {
			model.overlay = "diff"
		}
		return model, nil
	case resolvedMsg:
		model.errText = errorText(message.err)
		if message.err != nil {
			return model, nil
		}
		model.overlay = ""
		model.active = true
		model.status = "recovering"
		return model, model.resume()
	case tea.KeyPressMsg:
		key := message.String()
		if model.confirmExit {
			if key == "y" || key == "enter" {
				model.controller.Cancel()
				return model, tea.Quit
			}
			if key == "n" || key == "esc" {
				model.confirmExit = false
			}
			return model, nil
		}
		if model.overlay != "" {
			return model.handleOverlay(key)
		}
		switch key {
		case "ctrl+c":
			model.confirmExit = true
			return model, nil
		case "?":
			if model.composer.Value() == "" {
				model.overlay = "help"
				return model, nil
			}
		case "ctrl+o":
			model.overlay = "picker"
			model.pickerIndex = 0
			return model, nil
		case "ctrl+n":
			model.taskID = ""
			model.conversation = app.Conversation{}
			model.liveText = ""
			model.status = "new conversation"
			model.refresh()
			return model, nil
		case "ctrl+d":
			return model, model.loadDiff()
		case "ctrl+p":
			model.overlay = "permission"
			return model, nil
		case "esc":
			if model.active {
				model.overlay = "cancel"
			}
			return model, nil
		case "shift+enter", "alt+enter":
			model.composer.InsertString("\n")
			return model, nil
		case "enter":
			prompt := strings.TrimSpace(model.composer.Value())
			if prompt == "" {
				return model, nil
			}
			model.composer.Reset()
			model.status = "queueing"
			return model, model.submit(prompt)
		}
	}
	var command tea.Cmd
	model.composer, command = model.composer.Update(message)
	return model, command
}

func (model *Model) submit(prompt string) tea.Cmd {
	return func() tea.Msg {
		taskID, err := model.controller.Submit(model.ctx, model.taskID, prompt)
		return submittedMsg{taskID: taskID, err: err}
	}
}
func (model *Model) runNext() tea.Cmd {
	return func() tea.Msg {
		result, ran, err := model.controller.RunNext(model.ctx)
		return runFinishedMsg{result: result, ran: ran, err: err}
	}
}

func (model *Model) resume() tea.Cmd {
	return func() tea.Msg {
		result, err := model.controller.Resume(model.ctx, model.taskID)
		return runFinishedMsg{result: result, ran: true, err: err}
	}
}

func (model *Model) loadDiff() tea.Cmd {
	return func() tea.Msg { value, err := model.controller.Diff(model.ctx); return diffMsg{value: value, err: err} }
}

func (model *Model) resolve(success bool) tea.Cmd {
	return func() tea.Msg {
		return resolvedMsg{err: model.controller.ResolveUnknown(model.ctx, model.taskID, success)}
	}
}

func (model *Model) handleOverlay(key string) (tea.Model, tea.Cmd) {
	switch model.overlay {
	case "help", "diff":
		if key == "esc" || (model.overlay == "help" && key == "?") {
			model.overlay = ""
		}
	case "picker":
		switch key {
		case "esc":
			model.overlay = ""
		case "up":
			if model.pickerIndex > 0 {
				model.pickerIndex--
			}
		case "down":
			if model.pickerIndex+1 < len(model.conversations) {
				model.pickerIndex++
			}
		case "enter":
			if len(model.conversations) > 0 {
				model.taskID = model.conversations[model.pickerIndex].ID
				model.overlay = ""
				return model, model.load()
			}
		}
	case "permission":
		if key == "y" || key == "enter" {
			model.full = !model.full
			model.controller.SetFullExecution(model.full)
			model.overlay = ""
			model.status = "permission changed for this session"
		}
		if key == "n" || key == "esc" {
			model.overlay = ""
		}
	case "cancel":
		if key == "y" || key == "enter" {
			model.controller.Cancel()
			model.overlay = ""
			model.status = "cancelling"
		}
		if key == "n" || key == "esc" {
			model.overlay = ""
		}
	case "recovery":
		if key == "s" {
			return model, model.resolve(true)
		}
		if key == "f" {
			return model, model.resolve(false)
		}
		if key == "esc" {
			model.overlay = ""
		}
	}
	return model, nil
}

func (model *Model) View() tea.View {
	content := model.render()
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "erka — " + filepath.Base(model.controller.Root())
	return view
}

func (model *Model) render() string {
	if model.width == 0 {
		model.width, model.height = 100, 30
		model.resize()
	}
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("#7C8492"))
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("#7AA2F7")).Bold(true)
	mode := "workspace"
	if model.full {
		mode = "full"
	}
	readOnly := ""
	if model.controller.ReadOnly() {
		readOnly = "  READ-ONLY"
	}
	header := accent.Render("erka") + "  " + Sanitize(filepath.Base(model.controller.Root())) + "  " + muted.Render(Sanitize(model.branch)+"  "+Sanitize(model.provider)+"/"+Sanitize(model.model)+"  connected  "+mode+readOnly)
	body := model.transcript.View()
	if model.width >= 110 {
		body = lipgloss.JoinHorizontal(lipgloss.Top, lipgloss.NewStyle().Width(model.width-34).Render(body), lipgloss.NewStyle().Width(32).BorderLeft(true).PaddingLeft(1).Render(model.activity.View()))
	}
	footer := model.composer.View() + "\n" + muted.Render(model.status+"  Enter send · Alt+Enter newline · ? help · Ctrl+C quit")
	queued := 0
	for _, turn := range model.conversation.Turns {
		if turn.Status == store.TurnQueued {
			queued++
		}
	}
	if queued > 0 {
		footer += muted.Render(fmt.Sprintf("  · %d queued", queued))
	}
	if model.errText != "" {
		footer += "\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#F7768E")).Render(model.errText)
	}
	result := header + "\n" + body + "\n" + footer
	if model.overlay == "help" {
		result = overlay(result, "Keyboard", "Enter  submit\nAlt/Shift+Enter  newline\nCtrl+O  conversations\nCtrl+N  new conversation\nCtrl+D  diff\nCtrl+P  permissions\nEsc  cancel\nCtrl+C  exit")
	}
	if model.overlay == "picker" {
		var choices strings.Builder
		for index, task := range model.conversations {
			marker := "  "
			if index == model.pickerIndex {
				marker = "› "
			}
			fmt.Fprintf(&choices, "%s%s  %s\n", marker, task.ID, Sanitize(task.Objective))
		}
		if choices.Len() == 0 {
			choices.WriteString("No conversations yet.")
		}
		result = overlay(result, "Conversations", choices.String())
	}
	if model.overlay == "diff" {
		value := model.diff
		if strings.TrimSpace(value) == "" {
			value = "Working tree has no unstaged diff."
		}
		result = overlay(result, "Working tree diff", value+"\n\nEsc close")
	}
	if model.overlay == "permission" {
		action := "enable unrestricted command execution"
		if model.full {
			action = "return to workspace-only execution"
		}
		result = overlay(result, "Session permissions", "Confirm "+action+"? This affects only the current process. [y/N]")
	}
	if model.overlay == "cancel" {
		result = overlay(result, "Cancel active turn?", "The active process will stop; queued prompts remain durable. [y/N]")
	}
	if model.overlay == "recovery" {
		result = overlay(result, "Interrupted mutation", fmt.Sprintf("%s (%s) may have run before interruption.\n\n[s] record success  [f] record failure  [Esc] leave paused", Sanitize(model.unknown.Tool), Sanitize(string(model.unknown.CallID))))
	}
	if model.confirmExit {
		result = overlay(result, "Exit erka?", "Cancel the active process and exit? Queued prompts remain durable. [y/N]")
	}
	return result
}

func (model *Model) resize() {
	mainWidth := model.width
	if mainWidth >= 110 {
		mainWidth -= 34
	}
	contentHeight := model.height - 8
	if contentHeight < 4 {
		contentHeight = 4
	}
	model.transcript.SetWidth(mainWidth)
	model.transcript.SetHeight(contentHeight)
	model.activity.SetWidth(30)
	model.activity.SetHeight(contentHeight)
	model.composer.SetWidth(model.width)
	model.composer.SetHeight(3)
	model.refresh()
}

func (model *Model) refresh() {
	var markdown strings.Builder
	for _, item := range model.conversation.Items {
		switch item.Kind {
		case erkaruntime.ItemUserMessage:
			fmt.Fprintf(&markdown, "## You\n\n%s\n\n", Sanitize(item.Message.Content))
		case erkaruntime.ItemAssistantMessage:
			fmt.Fprintf(&markdown, "## erka\n\n%s\n\n", Sanitize(item.Message.Content))
		case erkaruntime.ItemToolCall:
			fmt.Fprintf(&markdown, "`%s`\n\n", Sanitize(item.ToolCall.Tool))
		}
	}
	if model.liveText != "" {
		fmt.Fprintf(&markdown, "## erka\n\n%s ▌\n", Sanitize(model.liveText))
	}
	width := model.width - 4
	if width < 20 {
		width = 20
	}
	if width > 100 {
		width = 100
	}
	rendered := markdown.String()
	if renderer, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(width)); err == nil {
		if value, err := renderer.Render(rendered); err == nil {
			rendered = value
		}
	}
	model.transcript.SetContent(rendered)
	model.transcript.GotoBottom()
	var activity strings.Builder
	for _, event := range model.conversation.Events {
		fmt.Fprintf(&activity, "• %s\n  %s\n", Sanitize(event.Type), Sanitize(event.Message))
	}
	model.activity.SetContent(activity.String())
	model.activity.GotoBottom()
}

func overlay(background, title, contents string) string {
	return background + "\n\n" + lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2).Render(title+"\n\n"+contents)
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return Sanitize(err.Error())
}

func hasQueuedTurn(turns []store.Turn) bool {
	for _, turn := range turns {
		if turn.Status == store.TurnQueued {
			return true
		}
	}
	return false
}

func hasActiveTurn(turns []store.Turn) bool {
	for _, turn := range turns {
		if turn.Status == store.TurnClaimed || turn.Status == store.TurnRunning {
			return true
		}
	}
	return false
}

var _ tea.Model = (*Model)(nil)
