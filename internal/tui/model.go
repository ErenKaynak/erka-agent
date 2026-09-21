package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

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
	SetPlanMode(bool)
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
	// Prompter delivers ask_user questions and command permission requests
	// from the running turn to the TUI.
	Prompter *Prompter
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
	ctx                       context.Context
	controller                Controller
	composer                  textarea.Model
	transcript                viewport.Model
	diffView                  viewport.Model
	width, height             int
	taskID                    erkaruntime.TaskID
	conversation              app.Conversation
	conversations             []erkaruntime.TaskSnapshot
	provider, model, branch   string
	full, active, ready       bool
	live                      <-chan erkaruntime.LiveUpdate
	liveText, status, errText string
	overlay                   string
	pickerIndex               int
	diff                      string
	unknown                   erkaruntime.ToolCall
	providers                 ProviderStatusFunc
	providerList              []ProviderInfo
	models                    ModelCatalogFunc
	modelOptions              []ModelOption
	modelFiltered             []int
	modelIssues               []string
	modelFilter               string
	modelIndex                int
	modelSelections           map[erkaruntime.Stage]string

	theme          theme
	spinner        spinner.Model
	markdown       markdownCache
	turnStarted    time.Time
	lastElapsed    time.Duration
	currentTool    string
	expanded       bool
	prompter       *Prompter
	pending        *PromptRequest
	pickerFilter   string
	pickerFiltered []int
	followBottom   bool
	quitArmed      time.Time
	mode           permissionMode
}

// quitWindow is how long a first Ctrl+C waits for the second one.
const quitWindow = 2 * time.Second

func (model *Model) quitPending() bool {
	return !model.quitArmed.IsZero() && time.Since(model.quitArmed) < quitWindow
}

// quit stops any active turn and exits. Queued prompts remain durable.
func (model *Model) quit() tea.Cmd {
	if model.pending != nil {
		model.pending.answer(erkaruntime.UserResponse{Approved: false})
		model.pending = nil
	}
	if model.controller != nil {
		model.controller.Cancel()
	}
	return tea.Quit
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
type promptMsg struct{ request *PromptRequest }

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
	{name: "plan", usage: "/plan"},
	{name: "auto", usage: "/auto"},
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
	theme := newTheme(true)
	composer := textarea.New()
	composer.Placeholder = "Ask erka to change this repository…"
	composer.Prompt = ""
	composer.ShowLineNumbers = false
	composer.CharLimit = 32 << 10
	composer.DynamicHeight = true
	composer.MinHeight = 1
	composer.MaxHeight = 8
	composer.SetStyles(theme.composerStyles())
	composer.Focus()
	transcript := viewport.New()
	transcript.SoftWrap = true
	diffView := viewport.New()
	model := &Model{ctx: ctx, controller: options.Controller, composer: composer, transcript: transcript, diffView: diffView, taskID: options.TaskID, provider: options.Provider, model: options.Model, branch: options.Branch, full: options.Full, live: options.Live, providers: options.Providers, models: options.Models, modelSelections: make(map[erkaruntime.Stage]string), status: "ready", theme: theme, spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot)), prompter: options.Prompter, followBottom: true}
	if options.Full {
		model.mode = modeGod
	}
	return model
}

func (model *Model) Init() tea.Cmd {
	return tea.Batch(model.load(), model.waitLive(), model.waitPrompt(), tea.RequestBackgroundColor, idleTick())
}

func (model *Model) waitPrompt() tea.Cmd {
	if model.prompter == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case request := <-model.prompter.Requests():
			return promptMsg{request: request}
		case <-model.ctx.Done():
			return nil
		}
	}
}

// startTurn marks a turn active and starts the elapsed timer. The idle tick
// loop notices and switches to the fast spinner.
func (model *Model) startTurn() {
	model.active = true
	model.turnStarted = time.Now()
	model.currentTool = ""
}

type idleTickMsg struct{}

// idleTick polls slowly while no turn runs so the spinner can start without
// every caller having to batch a tick command.
func idleTick() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return idleTickMsg{} })
}

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
				model.startTurn()
				return model, model.runNext()
			}
			if !model.active && hasActiveTurn(message.conversation.Turns) {
				model.startTurn()
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
		model.followBottom = true
		if model.active {
			return model, nil
		}
		model.startTurn()
		return model, model.runNext()
	case runFinishedMsg:
		model.active = false
		if !model.turnStarted.IsZero() {
			model.lastElapsed = time.Since(model.turnStarted)
		}
		model.currentTool = ""
		model.liveText = ""
		if model.pending != nil {
			model.pending = nil
			model.composer.Placeholder = "Ask erka to change this repository…"
		}
		model.errText = errorText(message.err)
		if message.err != nil {
			model.status = "paused"
		} else {
			model.status = "done"
		}
		return model, model.load()
	case tea.BackgroundColorMsg:
		model.theme = newTheme(message.IsDark())
		model.composer.SetStyles(model.theme.composerStyles())
		model.refresh()
		return model, nil
	case idleTickMsg:
		if model.active {
			return model, model.spinner.Tick
		}
		return model, idleTick()
	case spinner.TickMsg:
		if !model.active {
			return model, idleTick()
		}
		var command tea.Cmd
		model.spinner, command = model.spinner.Update(message)
		if model.currentTool != "" {
			model.refresh()
		}
		return model, command
	case promptMsg:
		model.pending = message.request
		model.composer.Reset()
		if message.request.Request.Kind == erkaruntime.UserQuestion {
			model.composer.Placeholder = "Type your answer…"
		}
		model.followBottom = true
		model.status = "waiting for you"
		return model, model.waitPrompt()
	case liveMsg:
		update := erkaruntime.LiveUpdate(message)
		switch update.Kind {
		case erkaruntime.LiveTextDelta:
			model.liveText += Sanitize(update.Text)
			model.status = "writing"
		case erkaruntime.LiveCommitted:
			model.liveText = ""
			model.status = "working"
			return model, tea.Batch(model.reloadConversation(), model.waitLive())
		case erkaruntime.LiveToolStarted:
			model.currentTool = Sanitize(update.Tool)
			model.status = "running"
		case erkaruntime.LiveToolFinished:
			model.currentTool = ""
			model.status = "working"
			return model, tea.Batch(model.reloadConversation(), model.waitLive())
		}
		model.refresh()
		return model, model.waitLive()
	case diffMsg:
		model.errText = errorText(message.err)
		model.diff = Sanitize(message.value)
		if message.err == nil {
			model.overlay = "diff"
			model.diffView.SetContent(renderDiff(model.theme, model.diff))
			model.diffView.GotoTop()
			model.resize()
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
		if model.active {
			model.status = "cancel the active turn before switching models"
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
	case conversationMsg:
		if message.conversation.Task.ID == model.taskID {
			model.conversation = message.conversation
			model.refresh()
		}
		return model, nil
	case resolvedMsg:
		model.errText = errorText(message.err)
		if message.err != nil {
			return model, nil
		}
		model.overlay = ""
		model.status = "recovering"
		model.startTurn()
		return model, model.resume()
	case tea.KeyPressMsg:
		key := message.String()
		if key == "ctrl+c" {
			if model.quitPending() {
				return model, model.quit()
			}
			model.quitArmed = time.Now()
			if model.composer.Value() != "" {
				model.composer.Reset()
				model.resize()
			}
			return model, nil
		}
		model.quitArmed = time.Time{}
		if model.overlay != "" {
			return model.handleOverlay(key)
		}
		if model.pending != nil {
			if handled, command := model.handlePrompt(key); handled {
				return model, command
			}
		}
		switch key {
		case "ctrl+t":
			model.expanded = !model.expanded
			model.refresh()
			return model, nil
		case "pgup", "pgdown", "ctrl+u":
			var command tea.Cmd
			if key == "ctrl+u" {
				model.transcript.HalfPageUp()
			} else {
				model.transcript, command = model.transcript.Update(message)
			}
			model.followBottom = model.transcript.AtBottom()
			return model, command
		case "ctrl+g":
			model.transcript.GotoBottom()
			model.followBottom = true
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
		case "shift+tab":
			model.setMode(model.mode.next())
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
	before := model.composer.Height()
	model.composer, command = model.composer.Update(message)
	if model.composer.Height() != before {
		model.resize()
	}
	return model, command
}

// reloadConversation refreshes durable items mid-turn so finished tool steps
// appear as soon as they are committed.
func (model *Model) reloadConversation() tea.Cmd {
	taskID := model.taskID
	if taskID == "" || model.controller == nil {
		return nil
	}
	return func() tea.Msg {
		conversation, err := model.controller.OpenConversation(model.ctx, taskID)
		if err != nil {
			return nil
		}
		return conversationMsg{conversation: conversation}
	}
}

type conversationMsg struct{ conversation app.Conversation }

// handlePrompt answers a pending question or permission request from the
// keyboard. It reports false for keys the composer should still receive.
func (model *Model) handlePrompt(key string) (bool, tea.Cmd) {
	request := model.pending
	reply := func(response erkaruntime.UserResponse, status string) (bool, tea.Cmd) {
		request.answer(response)
		model.pending = nil
		model.composer.Reset()
		model.composer.Placeholder = "Ask erka to change this repository…"
		model.status = status
		model.refresh()
		return true, nil
	}
	if request.Request.Kind == erkaruntime.UserPermission {
		switch key {
		case "y", "enter":
			return reply(erkaruntime.UserResponse{Approved: true}, "allowed")
		case "a":
			model.prompter.grantSession(request.Request.Tool)
			return reply(erkaruntime.UserResponse{Approved: true}, "allowed for this session")
		case "n", "esc":
			return reply(erkaruntime.UserResponse{Approved: false}, "denied")
		case "ctrl+c":
			return false, nil
		}
		return true, nil
	}
	switch key {
	case "enter":
		answer := strings.TrimSpace(model.composer.Value())
		if answer == "" {
			return true, nil
		}
		return reply(erkaruntime.UserResponse{Approved: true, Answer: answer}, "answered")
	case "esc":
		return reply(erkaruntime.UserResponse{Approved: false}, "left the decision to erka")
	}
	if model.composer.Value() == "" && len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		index := int(key[0] - '1')
		if index < len(request.Request.Options) {
			return reply(erkaruntime.UserResponse{Approved: true, Answer: request.Request.Options[index]}, "answered")
		}
	}
	return false, nil
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
			model.status = "god mode is already enabled"
		} else {
			model.overlay = "permission"
		}
	case "workspace", "auto":
		if requireNoArguments() {
			model.setMode(modeAuto)
		}
	case "plan":
		if requireNoArguments() {
			model.setMode(modePlan)
		}
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
			return model.quit()
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
	model.pickerFilter = ""
	model.refilterPicker()
}

func (model *Model) handleOverlay(key string) (tea.Model, tea.Cmd) {
	switch model.overlay {
	case "help", "status", "providers":
		if key == "esc" || (model.overlay == "help" && key == "?") {
			model.overlay = ""
		}
	case "diff":
		switch key {
		case "esc", "q":
			model.overlay = ""
		case "up", "k":
			model.diffView.ScrollUp(1)
		case "down", "j":
			model.diffView.ScrollDown(1)
		case "pgup", "b":
			model.diffView.PageUp()
		case "pgdown", "space", "f":
			model.diffView.PageDown()
		case "g", "home":
			model.diffView.GotoTop()
		case "G", "end":
			model.diffView.GotoBottom()
		}
	case "picker":
		switch key {
		case "esc":
			model.overlay = ""
		case "up", "ctrl+p":
			if model.pickerIndex > 0 {
				model.pickerIndex--
			}
		case "down", "ctrl+n":
			if model.pickerIndex+1 < len(model.pickerFiltered) {
				model.pickerIndex++
			}
		case "backspace":
			if value := []rune(model.pickerFilter); len(value) > 0 {
				model.pickerFilter = string(value[:len(value)-1])
				model.refilterPicker()
			}
		case "enter":
			if len(model.pickerFiltered) > 0 {
				selected := model.conversations[model.pickerFiltered[model.pickerIndex]].ID
				if model.active && selected != model.taskID {
					model.overlay = ""
					model.status = "cancel the active turn before switching conversations"
					return model, nil
				}
				model.taskID = selected
				model.overlay = ""
				model.followBottom = true
				return model, model.load()
			}
		default:
			value := key
			if key == "space" {
				value = " "
			}
			if len([]rune(value)) == 1 {
				model.pickerFilter += value
				model.pickerIndex = 0
				model.refilterPicker()
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
			if model.active {
				model.overlay = ""
				model.status = "cancel the active turn before switching models"
				return model, nil
			}
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
			if model.full {
				model.setMode(modeAuto)
			} else {
				model.setMode(modeGod)
			}
			model.overlay = ""
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
	switch model.overlay {
	case "help":
		return overlay(model.width, model.height, "Help", helpContents())
	case "picker":
		return overlay(model.width, model.height, "Conversations", model.pickerContents())
	case "diff":
		return model.renderDiffScreen()
	case "permission":
		action := "enable unrestricted command execution"
		if model.full {
			action = "return to workspace-only execution"
		}
		return overlay(model.width, model.height, "Session permissions", "Confirm "+action+"? This affects only the current process. [y/N]")
	case "cancel":
		return overlay(model.width, model.height, "Cancel active turn?", "The active process will stop; queued prompts remain durable. [y/N]")
	case "recovery":
		return overlay(model.width, model.height, "Interrupted mutation", fmt.Sprintf("%s (%s) may have run before interruption.\n\n[s] record success  [f] record failure  [Esc] leave paused", Sanitize(model.unknown.Tool), Sanitize(string(model.unknown.CallID))))
	case "status":
		task := string(model.taskID)
		if task == "" {
			task = "new conversation"
		}
		permission := model.mode.label()
		return overlay(model.width, model.height, "Session status", fmt.Sprintf("Task: %s\nState: %s\nProvider: %s/%s\nPermission: %s\nRead-only: %t\nUsage: %s\n\nEsc close", Sanitize(task), Sanitize(model.status), Sanitize(model.provider), Sanitize(model.model), permission, model.controller.ReadOnly(), model.usageText()))
	case "providers":
		return overlay(model.width, model.height, "Providers and authentication", providersContents(model.providerList))
	case "models":
		return model.modelsContents()
	}
	header := model.renderHeader()
	footer := model.renderFooter()
	height := model.height - 2 - lipgloss.Height(footer)
	if height < 3 {
		height = 3
	}
	model.transcript.SetHeight(height)
	if model.followBottom {
		model.transcript.GotoBottom()
	}
	return header + "\n\n" + model.transcript.View() + "\n" + footer
}

func (model *Model) projectName() string {
	if model.controller == nil {
		return "repository"
	}
	return filepath.Base(model.controller.Root())
}

func (model *Model) renderHeader() string {
	theme := model.theme
	header := theme.accent.Render("erka") + "  " + theme.text.Bold(true).Render(Sanitize(model.projectName()))
	if model.branch != "" {
		header += theme.muted.Render(" (" + Sanitize(model.branch) + ")")
	}
	header += theme.muted.Render(" · "+Sanitize(model.provider)+"/"+Sanitize(model.model)+" · ") + theme.modeBadge(model.mode)
	if model.controller != nil && model.controller.ReadOnly() {
		header += "  " + theme.warn.Render("READ-ONLY")
	}
	return ansi.Truncate(header, model.width, "…")
}

func (model *Model) renderFooter() string {
	theme := model.theme
	rows := make([]string, 0, 5)
	if model.pending != nil {
		rows = append(rows, model.renderPromptCard())
	}
	rows = append(rows, model.statusLine())
	composer := theme.composerFor(model.mode).Width(model.width).Render(model.composer.View())
	rows = append(rows, composer)
	hints := "⏎ send · ⌥⏎ newline · ⇧⇥ mode · / commands · ctrl+t details · ? help"
	if model.pending != nil && model.pending.Request.Kind == erkaruntime.UserQuestion {
		hints = "⏎ answer · esc let erka decide"
		if len(model.pending.Request.Options) > 0 {
			hints = "1-" + fmt.Sprint(min(len(model.pending.Request.Options), 9)) + " pick · " + hints
		}
	}
	if suggestions := slashCommandSuggestions(model.composer.Value(), 5); len(suggestions) > 0 {
		hints = "Commands: " + strings.Join(suggestions, " · ") + "  (Tab complete)"
	}
	if model.quitPending() {
		hints = "Press Ctrl+C again to quit"
		if model.active {
			hints += " (the running turn stops; queued prompts are kept)"
		}
	}
	queued := 0
	for _, turn := range model.conversation.Turns {
		if turn.Status == store.TurnQueued {
			queued++
		}
	}
	if queued > 0 {
		hints += fmt.Sprintf(" · %d queued", queued)
	}
	rows = append(rows, ansi.Truncate(theme.faint.Render("  "+hints), model.width, "…"))
	if model.errText != "" {
		rows = append(rows, ansi.Truncate(theme.danger.Render("  ✗ "+model.errText), model.width, "…"))
	}
	return strings.Join(rows, "\n")
}

// statusLine shows what the turn is doing now, how long it has run, and what
// the task has cost so far.
func (model *Model) statusLine() string {
	theme := model.theme
	parts := make([]string, 0, 4)
	var lead string
	if model.active {
		phase := "thinking"
		switch {
		case model.pending != nil:
			phase = "waiting for you"
		case model.currentTool != "":
			phase = "running " + model.currentTool
		case model.liveText != "":
			phase = "writing"
		}
		if stage := model.currentStage(); stage != "" {
			phase = stage + " · " + phase
		}
		lead = theme.accent.Render(model.spinner.View()) + " " + theme.text.Render(phase)
		parts = append(parts, formatElapsed(time.Since(model.turnStarted)))
	} else {
		lead = theme.muted.Render(Sanitize(model.status))
		if model.lastElapsed > 0 {
			parts = append(parts, formatElapsed(model.lastElapsed))
		}
	}
	if usage := model.usageText(); usage != "" {
		parts = append(parts, usage)
	}
	line := "  " + lead
	if len(parts) > 0 {
		line += theme.muted.Render(" · " + strings.Join(parts, " · "))
	}
	if model.active && model.pending == nil {
		line += theme.faint.Render("  esc cancel")
	}
	return ansi.Truncate(line, model.width, "…")
}

func (model *Model) currentStage() string {
	if model.conversation.Task.Pipeline == "" || len(model.conversation.Runs) == 0 {
		return ""
	}
	return model.conversation.Runs[len(model.conversation.Runs)-1].Stage
}

func (model *Model) usageText() string {
	var input, output, cost int64
	currency := ""
	for _, item := range model.conversation.Items {
		if item.Kind == erkaruntime.ItemUsage && item.Usage != nil {
			input += item.Usage.InputTokens
			output += item.Usage.OutputTokens
			cost += item.Usage.CostMicrounits
			if currency == "" {
				currency = item.Usage.Currency
			}
		}
	}
	if input+output == 0 {
		return ""
	}
	text := formatTokens(input) + " in / " + formatTokens(output) + " out"
	if cost > 0 {
		if currency == "" || currency == "USD" {
			text += fmt.Sprintf(" · $%.4f", float64(cost)/1e6)
		} else {
			text += fmt.Sprintf(" · %.4f %s", float64(cost)/1e6, currency)
		}
	}
	return text
}

func (model *Model) renderPromptCard() string {
	theme := model.theme
	request := model.pending.Request
	width := model.width - 4
	var rows []string
	if request.Kind == erkaruntime.UserPermission {
		rows = append(rows, theme.warn.Bold(true).Render("! Permission requested"))
		rows = append(rows, lipgloss.NewStyle().Width(width).Render(Sanitize(request.Message)))
		rows = append(rows, theme.muted.Render("y allow · a allow for this session · n deny"))
	} else {
		rows = append(rows, theme.warn.Bold(true).Render("? erka has a question"))
		rows = append(rows, lipgloss.NewStyle().Width(width).Render(Sanitize(request.Message)))
		if len(request.Options) > 0 {
			options := make([]string, 0, len(request.Options))
			for index, option := range request.Options {
				if index == 9 {
					break
				}
				options = append(options, theme.accent.Render(fmt.Sprint(index+1))+" "+Sanitize(option))
			}
			rows = append(rows, strings.Join(options, "   "))
		}
	}
	return theme.card.Width(model.width).Render(strings.Join(rows, "\n"))
}

func (model *Model) renderDiffScreen() string {
	theme := model.theme
	files, added, removed := 0, 0, 0
	for _, line := range strings.Split(model.diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git"):
			files++
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			added++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			removed++
		}
	}
	title := theme.accent.Render("Working tree diff") + theme.muted.Render(fmt.Sprintf("  %s · ", plural(files, "file", "files"))) + theme.diffAdd.Render(fmt.Sprintf("+%d", added)) + " " + theme.diffDel.Render(fmt.Sprintf("−%d", removed))
	body := model.diffView.View()
	if strings.TrimSpace(model.diff) == "" {
		body = theme.muted.Render("  Working tree has no unstaged diff.")
	}
	footer := theme.faint.Render(fmt.Sprintf("  ↑↓ scroll · pgup/pgdn page · g/G top/bottom · esc close · %d%%", int(model.diffView.ScrollPercent()*100)))
	return ansi.Truncate(title, model.width, "…") + "\n" + body + "\n" + ansi.Truncate(footer, model.width, "…")
}

func renderDiff(theme theme, diff string) string {
	rows := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	for index, line := range rows {
		switch {
		case strings.HasPrefix(line, "diff --git"):
			name := line
			if fields := strings.Fields(line); len(fields) == 4 {
				name = strings.TrimPrefix(fields[3], "b/")
			}
			prefix := ""
			if index > 0 {
				prefix = "\n"
			}
			rows[index] = prefix + theme.diffFile.Render("▍ "+name)
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "index "):
			rows[index] = theme.faint.Render(line)
		case strings.HasPrefix(line, "@@"):
			rows[index] = theme.diffHunk.Render(line)
		case strings.HasPrefix(line, "+"):
			rows[index] = theme.diffAdd.Render(line)
		case strings.HasPrefix(line, "-"):
			rows[index] = theme.diffDel.Render(line)
		default:
			rows[index] = theme.muted.Render(line)
		}
	}
	return strings.Join(rows, "\n")
}

func (model *Model) pickerContents() string {
	theme := model.theme
	var builder strings.Builder
	builder.WriteString(theme.muted.Render("Filter: ") + Sanitize(model.pickerFilter) + "▌\n\n")
	if len(model.pickerFiltered) == 0 {
		if len(model.conversations) == 0 {
			builder.WriteString("No conversations yet.")
		} else {
			builder.WriteString("No matching conversations.")
		}
		return builder.String()
	}
	start := 0
	if model.pickerIndex > 8 {
		start = model.pickerIndex - 8
	}
	for position := start; position < len(model.pickerFiltered) && position < start+12; position++ {
		task := model.conversations[model.pickerFiltered[position]]
		title := strings.Join(strings.Fields(Sanitize(task.Objective)), " ")
		meta := string(task.Status) + " · " + relativeTime(task.UpdatedAt)
		line := "  " + shorten(title, 56) + "  " + theme.muted.Render(meta)
		if position == model.pickerIndex {
			line = theme.selected.Render("› "+shorten(title, 56)) + "  " + theme.muted.Render(meta)
		}
		builder.WriteString(line + "\n")
	}
	builder.WriteString("\n" + theme.faint.Render("type to filter · ↑↓ move · ⏎ open · esc close"))
	return builder.String()
}

func (model *Model) refilterPicker() {
	filter := strings.ToLower(strings.TrimSpace(model.pickerFilter))
	model.pickerFiltered = model.pickerFiltered[:0]
	for index, task := range model.conversations {
		if filter == "" || fuzzyMatch(strings.ToLower(task.Objective+" "+string(task.ID)+" "+string(task.Status)), filter) {
			model.pickerFiltered = append(model.pickerFiltered, index)
		}
	}
	if model.pickerIndex >= len(model.pickerFiltered) {
		model.pickerIndex = max(0, len(model.pickerFiltered)-1)
	}
}

// fuzzyMatch reports whether every word of the filter appears in the value.
func fuzzyMatch(value, filter string) bool {
	for _, word := range strings.Fields(filter) {
		if !strings.Contains(value, word) {
			return false
		}
	}
	return true
}

func relativeTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	elapsed := time.Since(value)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	}
}

func formatElapsed(value time.Duration) string {
	value = value.Round(time.Second)
	if value < time.Minute {
		return fmt.Sprintf("%ds", int(value.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(value.Minutes()), int(value.Seconds())%60)
}

func formatTokens(value int64) string {
	if value < 1000 {
		return fmt.Sprint(value)
	}
	return fmt.Sprintf("%.1fk", float64(value)/1000)
}

func (model *Model) resize() {
	model.transcript.SetWidth(model.width)
	model.transcript.Style = lipgloss.NewStyle().PaddingLeft(1)
	model.composer.SetWidth(max(10, model.width-4))
	model.diffView.SetWidth(model.width)
	model.diffView.SetHeight(max(3, model.height-2))
	model.refresh()
}

func (model *Model) refresh() {
	model.transcript.SetContent(model.renderTranscript(model.width - 2))
	if model.followBottom {
		model.transcript.GotoBottom()
	}
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
		"Ctrl+T tool details · PgUp/PgDn scroll · Ctrl+G latest\n" +
		"Esc cancel/close · Ctrl+C twice quit\n\n" +
		"Questions and permissions\n" +
		"1-9 pick · Enter answer · y/a/n allow/session/deny\n\n" +
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

var stageOrder = map[erkaruntime.Stage]int{
	erkaruntime.StagePlanning: 0, erkaruntime.StageCoding: 1, erkaruntime.StageValidation: 2,
	erkaruntime.StageRepair: 3, erkaruntime.StageReview: 4,
}

func (model *Model) refilterModels() {
	query := strings.ToLower(strings.TrimSpace(model.modelFilter))
	model.modelFiltered = model.modelFiltered[:0]
	for index, option := range model.modelOptions {
		if query == "" || fuzzyMatch(modelOptionSearchText(option), query) {
			model.modelFiltered = append(model.modelFiltered, index)
		}
	}
	sort.SliceStable(model.modelFiltered, func(i, j int) bool {
		left, right := model.modelOptions[model.modelFiltered[i]].Stage, model.modelOptions[model.modelFiltered[j]].Stage
		return stageRank(left) < stageRank(right)
	})
	if model.modelIndex >= len(model.modelFiltered) {
		model.modelIndex = len(model.modelFiltered) - 1
	}
	if model.modelIndex < 0 {
		model.modelIndex = 0
	}
}

func stageRank(stage erkaruntime.Stage) int {
	if rank, ok := stageOrder[stage]; ok {
		return rank
	}
	return len(stageOrder)
}

func modelOptionSearchText(option ModelOption) string {
	return strings.ToLower(strings.Join([]string{string(option.Stage), option.Connection, option.Provider, option.ID, option.DisplayName, option.Class}, " "))
}

type modelRow struct {
	header   erkaruntime.Stage
	position int
}

// modelsContents renders the model picker: models grouped by stage in aligned
// columns, with the highlighted model's details and discovery warnings.
func (model *Model) modelsContents() string {
	theme := model.theme
	width := max(30, model.width-2)
	lines := []string{theme.accent.Render("Choose stage model") + theme.muted.Render("  models are switched for this session only")}
	search := theme.faint.Render("type to filter")
	if model.modelFilter != "" {
		search = theme.text.Render(Sanitize(model.modelFilter)) + "▌"
	}
	lines = append(lines, theme.muted.Render("Search: ")+search, "")

	footer := []string{}
	if len(model.modelFiltered) > 0 {
		selected := model.modelOptions[model.modelFiltered[model.modelIndex]]
		detail := selected.Detail
		if detail == "" {
			detail = strings.TrimPrefix(Sanitize(selected.Provider)+" · "+Sanitize(selected.Class), " · ")
		}
		footer = append(footer, "", ansi.Truncate(theme.muted.Render("  "+Sanitize(selected.Connection)+"/"+Sanitize(selected.ID)+" — "+Sanitize(detail)), width, "…"))
	}
	if len(model.modelIssues) > 0 {
		footer = append(footer, "", theme.warn.Render("Discovery warnings"))
		for index, issue := range model.modelIssues {
			if index == 2 {
				footer = append(footer, theme.faint.Render(fmt.Sprintf("  … %d more", len(model.modelIssues)-2)))
				break
			}
			footer = append(footer, ansi.Truncate(theme.warn.Render("  ! ")+theme.muted.Render(Sanitize(issue)), width, "…"))
		}
	}
	footer = append(footer, "", theme.faint.Render("↑↓ move · ⏎ select · type to filter · esc close"))

	if len(model.modelFiltered) == 0 {
		lines = append(lines, theme.muted.Render("  No matching models."))
		return strings.Join(append(lines, footer...), "\n")
	}

	rows := make([]modelRow, 0, len(model.modelFiltered)+5)
	selectedRow := 0
	nameWidth := 0
	for position, index := range model.modelFiltered {
		option := model.modelOptions[index]
		if position == 0 || model.modelOptions[model.modelFiltered[position-1]].Stage != option.Stage {
			rows = append(rows, modelRow{header: option.Stage, position: -1})
		}
		if position == model.modelIndex {
			selectedRow = len(rows)
		}
		rows = append(rows, modelRow{position: position})
		nameWidth = max(nameWidth, ansi.StringWidth(modelName(option)))
	}
	nameWidth = min(nameWidth, max(12, width/2-4))

	capacity := max(2, model.height-len(lines)-len(footer)-1)
	start := 0
	if len(rows) > capacity {
		start = min(max(0, selectedRow-capacity/2), len(rows)-capacity)
	}
	end := min(len(rows), start+capacity)
	visible := append([]modelRow(nil), rows[start:end]...)
	if start > 0 && visible[0].position >= 0 && visible[0].position != model.modelIndex {
		// Show the stage heading of a scrolled group in place of its first row.
		for back := start - 1; back >= 0; back-- {
			if rows[back].position < 0 {
				visible[0] = rows[back]
				break
			}
		}
	}
	for _, row := range visible {
		if row.position < 0 {
			lines = append(lines, theme.muted.Bold(true).Render(strings.ToUpper(string(row.header))))
			continue
		}
		lines = append(lines, model.modelLine(model.modelOptions[model.modelFiltered[row.position]], row.position == model.modelIndex, nameWidth, width))
	}
	if end < len(rows) {
		lines = append(lines, theme.faint.Render(fmt.Sprintf("  … %d more", len(rows)-end)))
	}
	return strings.Join(append(lines, footer...), "\n")
}

func modelName(option ModelOption) string {
	if option.DisplayName != "" && option.DisplayName != option.ID {
		return Sanitize(option.DisplayName) + " (" + Sanitize(option.ID) + ")"
	}
	return Sanitize(option.ID)
}

func (model *Model) modelLine(option ModelOption, selected bool, nameWidth, width int) string {
	theme := model.theme
	name := ansi.Truncate(modelName(option), nameWidth, "…")
	name += strings.Repeat(" ", max(0, nameWidth-ansi.StringWidth(name)))
	where := Sanitize(option.Connection) + " · " + Sanitize(option.Provider)
	if option.Class == "subscription_bridge" {
		where += " bridge"
	}
	tags := make([]string, 0, 3)
	if option.Current {
		tags = append(tags, theme.success.Render("● current"))
	}
	if option.Default {
		tags = append(tags, theme.muted.Render("default"))
	}
	if option.Source != "" {
		tags = append(tags, theme.faint.Render(Sanitize(option.Source)))
	}
	if option.State != "" && option.State != option.Source {
		tags = append(tags, theme.faint.Render(Sanitize(option.State)))
	}
	nameStyle, marker := theme.text, "  "
	switch {
	case !option.Selectable:
		nameStyle = theme.faint
		tags = append(tags, theme.faint.Render("unavailable"))
	case selected:
		nameStyle, marker = theme.selected, theme.selected.Render("› ")
	}
	if selected && !option.Selectable {
		marker = theme.faint.Render("› ")
	}
	line := marker + nameStyle.Render(name) + "  " + theme.muted.Render(where) + "  " + strings.Join(tags, theme.faint.Render(" · "))
	return ansi.Truncate(line, width, "…")
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

// TaskID is the conversation shown when the TUI exited, if any.
func (model *Model) TaskID() erkaruntime.TaskID { return model.taskID }
