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
	case "y":
		return tea.KeyPressMsg{Code: 'y', Text: "y"}
	default:
		return tea.KeyPressMsg{}
	}
}
