package tui

import (
	"context"
	"errors"
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
	SetModel(erkaruntime.ModelSelection) error
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
	Providers  ProviderStatusFunc
	Models     ModelCatalogFunc
}

// ModelOption is one provider model eligible for one configured stage.
type ModelOption struct {
	Stage       erkaruntime.Stage
	Connection  string
	Class       string
	Provider    string
	ID          string
	DisplayName string
	Source      string
	State       string
	Detail      string
	Default     bool
	Current     bool
	Selectable  bool
}

type ModelCatalogFunc func(context.Context) ([]ModelOption, []string, error)

// ProviderInfo describes one native connection or subscription bridge and
// whether erka can currently use it.
type ProviderInfo struct {
	Name   string
	Class  string
	State  string
	Detail string
	Models []string
}

// ProviderStatusFunc reports connection and authentication status on demand.
type ProviderStatusFunc func(context.Context) ([]ProviderInfo, error)

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
	providers                        ProviderStatusFunc
	providerList                     []ProviderInfo
	models                           ModelCatalogFunc
	modelOptions                     []ModelOption
	modelFiltered                    []int
	modelIssues                      []string
	modelFilter                      string
	modelIndex                       int
	modelSelections                  map[erkaruntime.Stage]string
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
type providersMsg struct {
	infos []ProviderInfo
	err   error
}
type modelsMsg struct {
	options []ModelOption
	issues  []string
	err     error
}
type resolvedMsg struct{ err error }

type slashCommand struct {
	name  string
	usage string
}

var slashCommands = []slashCommand{
	{name: "help", usage: "/help"},
	{name: "new", usage: "/new"},
	{name: "sessions", usage: "/sessions"},
	{name: "open", usage: "/open <task-id>"},
	{name: "diff", usage: "/diff"},
	{name: "permissions", usage: "/permissions"},
	{name: "full", usage: "/full"},
	{name: "workspace", usage: "/workspace"},
	{name: "cancel", usage: "/cancel"},
	{name: "status", usage: "/status"},
	{name: "provider", usage: "/provider"},
	{name: "model", usage: "/model"},
	{name: "quit", usage: "/quit"},
}

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
	return &Model{ctx: ctx, controller: options.Controller, composer: composer, transcript: transcript, activity: activity, taskID: options.TaskID, provider: options.Provider, model: options.Model, branch: options.Branch, full: options.Full, live: options.Live, providers: options.Providers, models: options.Models, modelSelections: make(map[erkaruntime.Stage]string), status: "ready"}
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
	case providersMsg:
		model.errText = errorText(message.err)
		model.providerList = message.infos
		if message.err == nil {
			model.overlay = "providers"
		}
		return model, nil
	case modelsMsg:
		model.errText = errorText(message.err)
		if message.err != nil {
			model.status = "model discovery failed"
			return model, nil
		}
		model.modelOptions = message.options
		for index := range model.modelOptions {
			if selected := model.modelSelections[model.modelOptions[index].Stage]; selected != "" {
				model.modelOptions[index].Current = selected == model.modelOptions[index].Connection+"/"+model.modelOptions[index].ID
			}
		}
		model.modelIssues = message.issues
		model.modelFilter = ""
		model.refilterModels()
		model.overlay = "models"
		model.status = "choose a model"
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
			model.openPicker()
			return model, nil
		case "ctrl+n":
			model.newConversation()
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
		case "tab":
			if completion := completeSlashCommand(model.composer.Value()); completion != "" {
				model.composer.SetValue(completion)
				return model, nil
			}
		case "enter":
			prompt := strings.TrimSpace(model.composer.Value())
			if prompt == "" {
				return model, nil
			}
			model.composer.Reset()
			if strings.HasPrefix(prompt, "//") {
				prompt = strings.TrimPrefix(prompt, "/")
			} else if strings.HasPrefix(prompt, "/") {
				return model, model.runSlashCommand(prompt)
			}
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

func (model *Model) loadProviders() tea.Cmd {
	if model.providers == nil {
		return func() tea.Msg {
			return providersMsg{infos: []ProviderInfo{{Name: "default", Class: "native", State: "unavailable", Detail: "provider status is unavailable in this session"}}}
		}
	}
	return func() tea.Msg {
		infos, err := model.providers(model.ctx)
		return providersMsg{infos: infos, err: err}
	}
}

func (model *Model) loadModels() tea.Cmd {
	if model.models == nil {
		return func() tea.Msg { return modelsMsg{err: errors.New("model discovery is unavailable in this session")} }
	}
	return func() tea.Msg {
		options, issues, err := model.models(model.ctx)
		if err == nil && len(options) == 0 {
			err = errors.New("no configured stage has an available model")
		}
		return modelsMsg{options: options, issues: issues, err: err}
	}
}

func (model *Model) resolve(success bool) tea.Cmd {
	return func() tea.Msg {
		return resolvedMsg{err: model.controller.ResolveUnknown(model.ctx, model.taskID, success)}
	}
}

func (model *Model) runSlashCommand(input string) tea.Cmd {
	fields := strings.Fields(input)
	name := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	requireNoArguments := func() bool {
		if len(fields) == 1 {
			return true
		}
		model.status = "usage: /" + name
		return false
	}
	switch name {
	case "help", "commands", "?":
		if requireNoArguments() {
			model.overlay = "help"
		}
	case "new":
		if requireNoArguments() {
			model.newConversation()
		}
	case "sessions", "conversations":
		if requireNoArguments() {
			model.openPicker()
		}
	case "open":
		if len(fields) != 2 {
			model.status = "usage: /open <task-id>"
			return nil
		}
		if model.active && erkaruntime.TaskID(fields[1]) != model.taskID {
			model.status = "cancel the active turn before opening another conversation"
			return nil
		}
		model.taskID = erkaruntime.TaskID(fields[1])
		model.overlay = ""
		model.status = "opening conversation"
		return model.load()
	case "diff":
		if requireNoArguments() {
			model.status = "loading diff"
			return model.loadDiff()
		}
	case "permissions":
		if requireNoArguments() {
			model.overlay = "permission"
		}
	case "full":
		if !requireNoArguments() {
			return nil
		}
		if model.full {
			model.status = "full execution is already enabled"
		} else {
			model.overlay = "permission"
		}
	case "workspace":
		if !requireNoArguments() {
			return nil
		}
		model.full = false
		model.controller.SetFullExecution(false)
		model.status = "workspace-only execution enabled"
	case "cancel":
		if !requireNoArguments() {
			return nil
		}
		if model.active {
			model.overlay = "cancel"
		} else {
			model.status = "no active turn to cancel"
		}
	case "status":
		if requireNoArguments() {
			model.overlay = "status"
		}
	case "provider", "providers":
		if requireNoArguments() {
			model.status = "loading providers"
			return model.loadProviders()
		}
	case "model":
		if len(fields) != 1 {
			model.status = "usage: /model"
			return nil
		}
		if model.active {
			model.status = "cancel the active turn before switching models"
			return nil
		}
		model.status = "loading models"
		return model.loadModels()
	case "quit", "exit":
		if requireNoArguments() {
			model.confirmExit = true
		}
	default:
		model.status = "unknown command /" + name + " — use /help"
	}
	return nil
}

func (model *Model) newConversation() {
	if model.active {
		model.status = "cancel the active turn before starting a new conversation"
		return
	}
	model.taskID = ""
	model.conversation = app.Conversation{}
	model.liveText = ""
	model.status = "new conversation"
	model.refresh()
}

func (model *Model) openPicker() {
	model.overlay = "picker"
	model.pickerIndex = 0
}

func (model *Model) handleOverlay(key string) (tea.Model, tea.Cmd) {
	switch model.overlay {
	case "help", "diff", "status", "providers":
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
				selected := model.conversations[model.pickerIndex].ID
				if model.active && selected != model.taskID {
					model.overlay = ""
					model.status = "cancel the active turn before switching conversations"
					return model, nil
				}
				model.taskID = selected
				model.overlay = ""
				return model, model.load()
			}
		}
	case "models":
		switch key {
		case "esc":
			model.overlay = ""
		case "up":
			if model.modelIndex > 0 {
				model.modelIndex--
			}
		case "down":
			if model.modelIndex+1 < len(model.modelFiltered) {
				model.modelIndex++
			}
		case "backspace":
			value := []rune(model.modelFilter)
			if len(value) > 0 {
				model.modelFilter = string(value[:len(value)-1])
				model.refilterModels()
			}
		case "enter":
			if len(model.modelFiltered) == 0 {
				return model, nil
			}
			selected := model.modelOptions[model.modelFiltered[model.modelIndex]]
			if !selected.Selectable {
				model.status = selected.Detail
				return model, nil
			}
			selection := erkaruntime.ModelSelection{Stage: selected.Stage, Connection: selected.Connection, Model: selected.ID}
			if err := model.controller.SetModel(selection); err != nil {
				model.errText = errorText(err)
				model.overlay = ""
				model.status = "model switch failed"
				return model, nil
			}
			for index := range model.modelOptions {
				if model.modelOptions[index].Stage == selected.Stage {
					model.modelOptions[index].Current = model.modelOptions[index].Connection == selected.Connection && model.modelOptions[index].ID == selected.ID
				}
			}
			model.modelSelections[selected.Stage] = selected.Connection + "/" + selected.ID
			model.model = selected.ID
			model.overlay = ""
			model.status = fmt.Sprintf("%s model set to %s/%s", selected.Stage, selected.Connection, selected.ID)
		default:
			value := key
			if key == "space" {
				value = " "
			}
			if len([]rune(value)) == 1 {
				model.modelFilter += value
				model.refilterModels()
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
	footerStatus := model.status + "  Enter send · Alt+Enter newline · ? help · Ctrl+C quit"
	if suggestions := slashCommandSuggestions(model.composer.Value(), 4); len(suggestions) > 0 {
		footerStatus = "Commands: " + strings.Join(suggestions, " · ") + "  (Tab complete)"
	}
	footer := model.composer.View() + "\n" + muted.Render(footerStatus)
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
		return overlay(model.width, model.height, "Help", helpContents())
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
		return overlay(model.width, model.height, "Conversations", choices.String())
	}
	if model.overlay == "diff" {
		value := model.diff
		if strings.TrimSpace(value) == "" {
			value = "Working tree has no unstaged diff."
		}
		return overlay(model.width, model.height, "Working tree diff", value+"\n\nEsc close")
	}
	if model.overlay == "permission" {
		action := "enable unrestricted command execution"
		if model.full {
			action = "return to workspace-only execution"
		}
		return overlay(model.width, model.height, "Session permissions", "Confirm "+action+"? This affects only the current process. [y/N]")
	}
	if model.overlay == "cancel" {
		return overlay(model.width, model.height, "Cancel active turn?", "The active process will stop; queued prompts remain durable. [y/N]")
	}
	if model.overlay == "recovery" {
		return overlay(model.width, model.height, "Interrupted mutation", fmt.Sprintf("%s (%s) may have run before interruption.\n\n[s] record success  [f] record failure  [Esc] leave paused", Sanitize(model.unknown.Tool), Sanitize(string(model.unknown.CallID))))
	}
	if model.overlay == "status" {
		task := string(model.taskID)
		if task == "" {
			task = "new conversation"
		}
		permission := "workspace"
		if model.full {
			permission = "full"
		}
		return overlay(model.width, model.height, "Session status", fmt.Sprintf("Task: %s\nState: %s\nProvider: %s/%s\nPermission: %s\nRead-only: %t\n\nEsc close", Sanitize(task), Sanitize(model.status), Sanitize(model.provider), Sanitize(model.model), permission, model.controller.ReadOnly()))
	}
	if model.overlay == "providers" {
		return overlay(model.width, model.height, "Providers and authentication", providersContents(model.providerList))
	}
	if model.overlay == "models" {
		return overlay(model.width, model.height, "Choose stage model", model.modelsContents())
	}
	if model.confirmExit {
		return overlay(model.width, model.height, "Exit erka?", "Cancel the active process and exit? Queued prompts remain durable. [y/N]")
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

func overlay(width, height int, title, contents string) string {
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 30
	}
	outerWidth := width - 4
	if outerWidth > 88 {
		outerWidth = 88
	}
	if outerWidth < 12 {
		outerWidth = 12
	}
	contentWidth := outerWidth - 6
	maxBodyLines := height - 8
	if maxBodyLines < 1 {
		maxBodyLines = 1
	}
	title = fitOverlayText(Sanitize(title), contentWidth, 1)
	contents = fitOverlayText(Sanitize(contents), contentWidth, maxBodyLines)
	box := lipgloss.NewStyle().Width(outerWidth).Border(lipgloss.RoundedBorder()).Padding(1, 2).Render(title + "\n\n" + contents)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

func fitOverlayText(value string, width, maxLines int) string {
	if width < 1 || maxLines < 1 {
		return ""
	}
	lines := make([]string, 0, maxLines)
	omitted := false
	for _, source := range strings.Split(value, "\n") {
		runes := []rune(source)
		if len(runes) == 0 {
			if len(lines) == maxLines {
				omitted = true
				break
			}
			lines = append(lines, "")
			continue
		}
		for len(runes) > 0 {
			if len(lines) == maxLines {
				omitted = true
				break
			}
			length := width
			if len(runes) < length {
				length = len(runes)
			}
			lines = append(lines, string(runes[:length]))
			runes = runes[length:]
		}
		if omitted {
			break
		}
	}
	if omitted && len(lines) > 0 {
		last := []rune(lines[len(lines)-1])
		if len(last) >= width && width > 1 {
			last = last[:width-1]
		}
		lines[len(lines)-1] = string(last) + "…"
	}
	return strings.Join(lines, "\n")
}

func helpContents() string {
	return "Keys\n" +
		"Enter send · Alt+Enter newline\n" +
		"Ctrl+O sessions · Ctrl+N new\n" +
		"Ctrl+D diff · Ctrl+P permissions\n" +
		"Esc cancel/close · Ctrl+C exit\n\n" +
		"Slash commands\n" +
		"/help /new /sessions /diff\n" +
		"/open <task-id>\n" +
		"/permissions /full /workspace\n" +
		"/cancel /status /provider /model /quit\n" +
		"Tab completes · // sends literal /\n\n" +
		"Esc or ? closes help"
}

func providersContents(infos []ProviderInfo) string {
	if len(infos) == 0 {
		infos = []ProviderInfo{{Name: "default", Class: "native", State: "unavailable", Detail: "no provider status reported"}}
	}
	var builder strings.Builder
	for _, info := range infos {
		fmt.Fprintf(&builder, "%s  %s  %s\n", info.Class, info.Name, info.State)
		if strings.TrimSpace(info.Detail) != "" {
			fmt.Fprintf(&builder, "  %s\n", info.Detail)
		}
		if len(info.Models) > 0 {
			fmt.Fprintf(&builder, "  configured models: %s\n", strings.Join(info.Models, ", "))
		}
	}
	builder.WriteString("\nConnect a native key:\n  export ERKA_DEEPSEEK_API_KEY=… (or ERKA_OPENAI_API_KEY=…), then restart erka\n\nConnect a subscription bridge:\n  erka auth login codex --device\n  erka auth login opencode\n\nUse /model to discover and switch configured stage models.\n\nEsc close")
	return builder.String()
}

func (model *Model) refilterModels() {
	query := strings.ToLower(strings.TrimSpace(model.modelFilter))
	model.modelFiltered = model.modelFiltered[:0]
	for index, option := range model.modelOptions {
		if query == "" || strings.Contains(modelOptionSearchText(option), query) {
			model.modelFiltered = append(model.modelFiltered, index)
		}
	}
	if model.modelIndex >= len(model.modelFiltered) {
		model.modelIndex = len(model.modelFiltered) - 1
	}
	if model.modelIndex < 0 {
		model.modelIndex = 0
	}
}

func modelOptionSearchText(option ModelOption) string {
	return strings.ToLower(strings.Join([]string{string(option.Stage), option.Connection, option.Class, option.Provider, option.ID, option.DisplayName}, " "))
}

func (model *Model) modelsContents() string {
	var builder strings.Builder
	filter := model.modelFilter
	if filter == "" {
		filter = "type to filter"
	}
	fmt.Fprintf(&builder, "Search: %s\n\n", filter)
	if len(model.modelFiltered) == 0 {
		builder.WriteString("No matching models.\n")
	} else {
		visible := model.height - 14
		if visible < 3 {
			visible = 3
		}
		if visible > 12 {
			visible = 12
		}
		start := model.modelIndex - visible/2
		if start < 0 {
			start = 0
		}
		if start+visible > len(model.modelFiltered) {
			start = len(model.modelFiltered) - visible
			if start < 0 {
				start = 0
			}
		}
		end := start + visible
		if end > len(model.modelFiltered) {
			end = len(model.modelFiltered)
		}
		for position := start; position < end; position++ {
			option := model.modelOptions[model.modelFiltered[position]]
			marker := "  "
			if position == model.modelIndex {
				marker = "› "
			}
			name := option.ID
			if option.DisplayName != "" && option.DisplayName != option.ID {
				name = option.DisplayName + " (" + option.ID + ")"
			}
			backend := option.Provider + "/" + option.Class
			tags := []string{backend, option.Source}
			if option.State != "" && option.State != option.Source {
				tags = append(tags, option.State)
			}
			if option.Current {
				tags = append(tags, "current")
			}
			if option.Default {
				tags = append(tags, "default")
			}
			if !option.Selectable {
				tags = append(tags, "disabled")
			}
			fmt.Fprintf(&builder, "%s%s · %s · %s  [%s]\n", marker, option.Stage, option.Connection, name, strings.Join(tags, ", "))
		}
	}
	if len(model.modelIssues) > 0 {
		builder.WriteString("\nDiscovery warnings:\n")
		for _, issue := range model.modelIssues {
			fmt.Fprintf(&builder, "  %s\n", issue)
		}
	}
	builder.WriteString("\nType search · ↑/↓ navigate · Enter select · Esc close")
	return builder.String()
}

func slashCommandSuggestions(value string, limit int) []string {
	value = strings.TrimSpace(value)
	if limit <= 0 || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.Contains(value, "\n") {
		return nil
	}
	prefix := strings.ToLower(strings.TrimPrefix(strings.Fields(value)[0], "/"))
	result := make([]string, 0, limit)
	for _, command := range slashCommands {
		if strings.HasPrefix(command.name, prefix) {
			result = append(result, command.usage)
			if len(result) == limit {
				break
			}
		}
	}
	return result
}

func completeSlashCommand(value string) string {
	if strings.Contains(value, "\n") {
		return ""
	}
	suggestions := slashCommandSuggestions(value, 1)
	if len(suggestions) == 0 {
		return ""
	}
	if strings.HasSuffix(suggestions[0], " <task-id>") {
		return strings.TrimSuffix(suggestions[0], "<task-id>")
	}
	return suggestions[0]
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
