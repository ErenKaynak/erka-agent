package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ErenKaynak/erka-agent/internal/app"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type fakeController struct {
	taskID        erkaruntime.TaskID
	prompts       []string
	opened        []erkaruntime.TaskID
	conversations []erkaruntime.TaskSnapshot
	full          bool
	cancelled     int
	model         erkaruntime.ModelSelection
}

func (*fakeController) Root() string   { return "/repo" }
func (*fakeController) ReadOnly() bool { return false }
func (controller *fakeController) ListConversations(context.Context) ([]erkaruntime.TaskSnapshot, error) {
	return controller.conversations, nil
}
func (controller *fakeController) OpenConversation(_ context.Context, taskID erkaruntime.TaskID) (app.Conversation, error) {
	controller.opened = append(controller.opened, taskID)
	return app.Conversation{Task: erkaruntime.TaskSnapshot{Task: erkaruntime.Task{ID: taskID}}}, nil
}
func (controller *fakeController) Submit(_ context.Context, _ erkaruntime.TaskID, prompt string) (erkaruntime.TaskID, error) {
	controller.prompts = append(controller.prompts, prompt)
	controller.taskID = "task-1"
	return controller.taskID, nil
}
func (*fakeController) RunNext(context.Context) (app.LocalResult, bool, error) {
	return app.LocalResult{Status: erkaruntime.TaskCompleted}, true, nil
}
func (*fakeController) Resume(context.Context, erkaruntime.TaskID) (app.LocalResult, error) {
	return app.LocalResult{Status: erkaruntime.TaskCompleted}, nil
}
func (*fakeController) Diff(context.Context) (string, error) { return "diff", nil }
func (*fakeController) UnknownOutcome(context.Context, erkaruntime.TaskID) (erkaruntime.ToolCall, bool, error) {
	return erkaruntime.ToolCall{}, false, nil
}
func (*fakeController) ResolveUnknown(context.Context, erkaruntime.TaskID, bool) error { return nil }
func (controller *fakeController) Cancel()                                             { controller.cancelled++ }
func (controller *fakeController) SetFullExecution(value bool)                         { controller.full = value }
func (controller *fakeController) SetModel(value erkaruntime.ModelSelection) error {
	controller.model = value
	return nil
}

func TestModelSubmitClearsComposerAndQueuesPrompt(t *testing.T) {
	controller := &fakeController{}
	model := NewModel(Options{Controller: controller, Provider: "deepseek", Model: "fixture"})
	model.composer.InsertString("fix tests")
	_, command := model.Update(key("enter"))
	if model.composer.Value() != "" || command == nil {
		t.Fatalf("composer = %q, command = %v", model.composer.Value(), command)
	}
	message := command()
	if _, ok := message.(submittedMsg); !ok {
		t.Fatalf("message = %#v", message)
	}
	if controller.prompts[0] != "fix tests" {
		t.Fatalf("prompts = %#v", controller.prompts)
	}
}

func TestModelResponsiveLayoutAndHelp(t *testing.T) {
	model := NewModel(Options{Controller: &fakeController{}, Provider: "deepseek", Model: "fixture"})
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if strings.Contains(model.View().Content, "Activity") {
		t.Fatal("narrow view unexpectedly contains side panel")
	}
	model.Update(key("?"))
	if !strings.Contains(model.View().Content, "Help") {
		t.Fatal("help overlay not rendered")
	}
	model.Update(tea.WindowSizeMsg{Width: 130, Height: 34})
	if model.transcript.Width() >= 130 {
		t.Fatal("wide layout did not reserve side panel")
	}
}

func TestHelpOverlayFitsInsideSmallTerminal(t *testing.T) {
	model := NewModel(Options{Controller: &fakeController{}, Provider: "deepseek", Model: "fixture"})
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	model.Update(key("?"))
	view := model.View().Content
	if !strings.Contains(view, "Help") {
		t.Fatalf("help overlay = %q", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines > 12 {
		t.Fatalf("help overlay has %d lines for a 12-line terminal:\n%s", lines, view)
	}
}

func TestOverlayBoundsUntrustedLongContent(t *testing.T) {
	view := overlay(32, 10, "Diff", strings.Repeat("0123456789", 20)+"\n"+strings.Repeat("line\n", 20))
	if lines := strings.Count(view, "\n") + 1; lines > 10 {
		t.Fatalf("overlay has %d lines:\n%s", lines, view)
	}
	if !strings.Contains(view, "…") {
		t.Fatalf("bounded overlay has no truncation marker:\n%s", view)
	}
}

func TestPermissionDialogRequiresConfirmationAndIsSessionOnly(t *testing.T) {
	controller := &fakeController{}
	model := NewModel(Options{Controller: controller})
	model.Update(key("ctrl+p"))
	if model.overlay != "permission" || model.full {
		t.Fatalf("permission overlay = %q, full = %v", model.overlay, model.full)
	}
	model.Update(key("y"))
	if !model.full || !controller.full || model.overlay != "" {
		t.Fatalf("full = %v/%v overlay=%q", model.full, controller.full, model.overlay)
	}
}

func TestSlashCommandsAreLocalAndDiscoverable(t *testing.T) {
	controller := &fakeController{}
	model := NewModel(Options{Controller: controller, Provider: "deepseek", Model: "fixture"})
	model.Update(tea.WindowSizeMsg{Width: 90, Height: 28})

	model.composer.InsertString("/")
	if view := model.View().Content; !strings.Contains(view, "Commands:") || !strings.Contains(view, "/help") {
		t.Fatalf("slash suggestions missing:\n%s", view)
	}
	model.composer.SetValue("/help")
	_, command := model.Update(key("enter"))
	if command != nil || model.overlay != "help" || len(controller.prompts) != 0 {
		t.Fatalf("help command = overlay %q, command %v, prompts %#v", model.overlay, command, controller.prompts)
	}
	if view := model.View().Content; !strings.Contains(view, "Slash commands") || !strings.Contains(view, "/workspace") {
		t.Fatalf("slash help missing:\n%s", view)
	}
	model.Update(key("esc"))

	model.composer.SetValue("/unknown")
	_, command = model.Update(key("enter"))
	if command != nil || len(controller.prompts) != 0 || !strings.Contains(model.status, "unknown command") {
		t.Fatalf("unknown command = command %v, prompts %#v, status %q", command, controller.prompts, model.status)
	}

	model.composer.SetValue("//explain this path")
	_, command = model.Update(key("enter"))
	if command == nil {
		t.Fatal("literal slash prompt did not submit")
	}
	message := command()
	if _, ok := message.(submittedMsg); !ok || len(controller.prompts) != 1 || controller.prompts[0] != "/explain this path" {
		t.Fatalf("literal slash message = %#v, prompts = %#v", message, controller.prompts)
	}
}

func TestSlashCommandsMapToSafeSessionActions(t *testing.T) {
	controller := &fakeController{conversations: []erkaruntime.TaskSnapshot{{Task: erkaruntime.Task{ID: "task-2", Objective: "second"}}}}
	model := NewModel(Options{Controller: controller, Provider: "deepseek", Model: "fixture"})

	runSlash(t, model, "/sessions")
	if model.overlay != "picker" {
		t.Fatalf("sessions overlay = %q", model.overlay)
	}
	model.Update(key("esc"))

	command := runSlash(t, model, "/open task-2")
	if command == nil {
		t.Fatal("open command did not load conversation")
	}
	model.Update(command())
	if model.taskID != "task-2" || len(controller.opened) != 1 || controller.opened[0] != "task-2" {
		t.Fatalf("open = task %q, opened %#v", model.taskID, controller.opened)
	}

	command = runSlash(t, model, "/diff")
	if command == nil {
		t.Fatal("diff command was nil")
	}
	model.Update(command())
	if model.overlay != "diff" || model.diff != "diff" {
		t.Fatalf("diff = overlay %q, value %q", model.overlay, model.diff)
	}
	model.Update(key("esc"))

	runSlash(t, model, "/full")
	if model.overlay != "permission" || model.full {
		t.Fatalf("full command bypassed confirmation: overlay=%q full=%v", model.overlay, model.full)
	}
	model.Update(key("y"))
	runSlash(t, model, "/workspace")
	if model.full || controller.full {
		t.Fatal("workspace command did not drop full execution")
	}

	model.active = true
	runSlash(t, model, "/cancel")
	if model.overlay != "cancel" || controller.cancelled != 0 {
		t.Fatalf("cancel command bypassed confirmation: overlay=%q cancelled=%d", model.overlay, controller.cancelled)
	}
	model.Update(key("y"))
	if controller.cancelled != 1 {
		t.Fatalf("cancel count = %d", controller.cancelled)
	}
}

func TestSlashCommandTabCompletion(t *testing.T) {
	model := NewModel(Options{Controller: &fakeController{}})
	model.composer.SetValue("/ope")
	model.Update(key("tab"))
	if model.composer.Value() != "/open " {
		t.Fatalf("open completion = %q", model.composer.Value())
	}
	model.composer.SetValue("/wor")
	model.Update(key("tab"))
	if model.composer.Value() != "/workspace" {
		t.Fatalf("workspace completion = %q", model.composer.Value())
	}
}

func TestProviderSlashCommandShowsConnectionStatus(t *testing.T) {
	controller := &fakeController{}
	model := NewModel(Options{Controller: controller, Provider: "deepseek", Model: "fixture",
		Providers: func(context.Context) ([]ProviderInfo, error) {
			return []ProviderInfo{
				{Name: "chatgpt", Class: "subscription_bridge", State: "authenticated", Detail: "authenticated with ChatGPT", Models: []string{"default", "gpt-5.1-codex"}},
				{Name: "coder", Class: "native", State: "ready"},
			}, nil
		},
	})
	model.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	command := runSlash(t, model, "/provider")
	if command == nil {
		t.Fatal("provider command did not load status")
	}
	model.Update(command())
	if model.overlay != "providers" {
		t.Fatalf("provider overlay = %q", model.overlay)
	}
	if view := model.View().Content; !strings.Contains(view, "Providers and authentication") || !strings.Contains(view, "chatgpt") || !strings.Contains(view, "authenticated") || !strings.Contains(view, "configured models: default, gpt-5.1-codex") {
		t.Fatalf("provider overlay missing status:\n%s", view)
	}
	model.Update(key("esc"))
	if model.overlay != "" {
		t.Fatalf("provider overlay did not close: %q", model.overlay)
	}
}

func TestProviderSlashCommandWithoutReporterStillRenders(t *testing.T) {
	model := NewModel(Options{Controller: &fakeController{}})
	model.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	command := runSlash(t, model, "/provider")
	if command == nil {
		t.Fatal("provider command did not load status")
	}
	model.Update(command())
	if model.overlay != "providers" {
		t.Fatalf("provider overlay = %q", model.overlay)
	}
	if view := model.View().Content; !strings.Contains(view, "unavailable") {
		t.Fatalf("provider overlay missing unavailable state:\n%s", view)
	}
}

func TestModelSlashCommandRejectsArguments(t *testing.T) {
	controller := &fakeController{}
	model := NewModel(Options{Controller: controller, Provider: "deepseek", Model: "fixture"})
	runSlash(t, model, "/model deepseek-v4-pro")
	if controller.model.Model != "" || !strings.Contains(model.status, "usage: /model") {
		t.Fatalf("status = %q", model.status)
	}
}

func TestModelSlashCommandOpensPickerAndSelects(t *testing.T) {
	controller := &fakeController{}
	model := NewModel(Options{Controller: controller, Provider: "deepseek", Model: "fixture",
		Models: func(context.Context) ([]ModelOption, []string, error) {
			return []ModelOption{
				{Stage: erkaruntime.StagePlanning, Connection: "chatgpt", Class: "subscription_bridge", Provider: "codex", ID: "gpt-5.6-sol", DisplayName: "GPT-5.6-Sol", Source: "live", Selectable: true, Current: true},
				{Stage: erkaruntime.StageCoding, Connection: "coder", Class: "native", Provider: "deepseek", ID: "deepseek-v4-flash", Source: "configured+live", Selectable: true, Current: true},
				{Stage: erkaruntime.StageCoding, Connection: "coder", Class: "native", Provider: "deepseek", ID: "deepseek-v4-pro", Source: "live", Selectable: true},
			}, []string{"opencode: login required"}, nil
		},
	})
	model.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	command := runSlash(t, model, "/model")
	if command == nil || model.overlay == "models" {
		t.Fatalf("model command = overlay %q, command %v", model.overlay, command)
	}
	model.Update(command())
	if view := model.View().Content; !strings.Contains(view, "Choose stage model") || !strings.Contains(view, "deepseek-v4-pro") || !strings.Contains(view, "Discovery warnings") {
		t.Fatalf("model picker missing:\n%s", view)
	}
	model.Update(key("d"))
	model.Update(key("e"))
	model.Update(key("e"))
	model.Update(key("p"))
	if len(model.modelFiltered) != 2 {
		t.Fatalf("filtered models = %d, want 2", len(model.modelFiltered))
	}
	model.Update(key("down"))
	model.Update(key("enter"))
	if controller.model.Stage != erkaruntime.StageCoding || controller.model.Connection != "coder" || controller.model.Model != "deepseek-v4-pro" || model.model != "deepseek-v4-pro" || model.overlay != "" {
		t.Fatalf("selection = controller %#v, model %q, overlay %q", controller.model, model.model, model.overlay)
	}
	command = runSlash(t, model, "/model")
	model.Update(command())
	for _, option := range model.modelOptions {
		if option.Stage == erkaruntime.StageCoding && option.ID == "deepseek-v4-pro" && !option.Current {
			t.Fatalf("session selection was not retained after rediscovery: %#v", model.modelOptions)
		}
	}
}

func TestModelPickerKeepsDisabledUnpricedModelUnselected(t *testing.T) {
	controller := &fakeController{}
	model := NewModel(Options{Controller: controller, Models: func(context.Context) ([]ModelOption, []string, error) {
		return []ModelOption{{Stage: erkaruntime.StageCoding, Connection: "coder", Class: "native", Provider: "deepseek", ID: "deepseek-v4-pro", Source: "live", Selectable: false, Detail: "configure model pricing"}}, nil, nil
	}})
	command := runSlash(t, model, "/model")
	model.Update(command())
	model.Update(key("enter"))
	if controller.model.Model != "" || model.overlay != "models" || model.status != "configure model pricing" {
		t.Fatalf("disabled selection = %#v, overlay %q, status %q", controller.model, model.overlay, model.status)
	}
}

func TestModelPickerFitsInsideSmallTerminal(t *testing.T) {
	options := make([]ModelOption, 30)
	for index := range options {
		options[index] = ModelOption{Stage: erkaruntime.StagePlanning, Connection: "chatgpt", Class: "subscription_bridge", Provider: "codex", ID: "gpt-fixture-" + strings.Repeat("x", index+1), Source: "live", Selectable: true}
	}
	model := NewModel(Options{Controller: &fakeController{}, Models: func(context.Context) ([]ModelOption, []string, error) { return options, nil, nil }})
	model.Update(tea.WindowSizeMsg{Width: 46, Height: 14})
	command := runSlash(t, model, "/model")
	model.Update(command())
	if view := model.View().Content; strings.Count(view, "\n")+1 > 14 {
		t.Fatalf("model overlay exceeds terminal:\n%s", view)
	}
}

func runSlash(t *testing.T, model *Model, value string) tea.Cmd {
	t.Helper()
	model.composer.SetValue(value)
	_, command := model.Update(key("enter"))
	return command
}

func TestSanitizeRemovesTerminalInjectionAndBoundsOutput(t *testing.T) {
	input := "safe\x1b[31mred\x1b[0m\x1b]8;;https://bad.example\x07link\x1b]8;;\x07\x00end"
	got := Sanitize(input)
	if strings.ContainsAny(got, "\x1b\x00") || got != "saferedlinkend" {
		t.Fatalf("Sanitize() = %q", got)
	}
	if len(Sanitize(strings.Repeat("x", maxRenderedInputBytes+100))) != maxRenderedInputBytes {
		t.Fatal("Sanitize did not bound output")
	}
}

func TestCredentialEntryMasksSecret(t *testing.T) {
	model := newCredentialModel(context.Background(), "deepseek")
	model.input.SetValue("top-secret")
	view := model.View().Content
	if strings.Contains(view, "top-secret") || !strings.Contains(view, "••••") {
		t.Fatalf("credential view = %q", view)
	}
}

func key(value string) tea.KeyPressMsg {
	switch value {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "?":
		return tea.KeyPressMsg{Code: '?', Text: "?"}
	case "ctrl+p":
		return tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}
	case "ctrl+d":
		return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "y":
		return tea.KeyPressMsg{Code: 'y', Text: "y"}
	default:
		runes := []rune(value)
		if len(runes) == 1 {
			return tea.KeyPressMsg{Code: runes[0], Text: value}
		}
		return tea.KeyPressMsg{}
	}
}
