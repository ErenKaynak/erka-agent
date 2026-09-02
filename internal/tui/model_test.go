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
	taskID  erkaruntime.TaskID
	prompts []string
	full    bool
}

func (*fakeController) Root() string   { return "/repo" }
func (*fakeController) ReadOnly() bool { return false }
func (*fakeController) ListConversations(context.Context) ([]erkaruntime.TaskSnapshot, error) {
	return nil, nil
}
func (*fakeController) OpenConversation(context.Context, erkaruntime.TaskID) (app.Conversation, error) {
	return app.Conversation{}, nil
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
func (*fakeController) Diff(context.Context) (string, error) { return "diff --git a/a b/a", nil }
func (*fakeController) UnknownOutcome(context.Context, erkaruntime.TaskID) (erkaruntime.ToolCall, bool, error) {
	return erkaruntime.ToolCall{}, false, nil
}
func (*fakeController) ResolveUnknown(context.Context, erkaruntime.TaskID, bool) error { return nil }
func (*fakeController) Cancel()                                                        {}
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
	if !strings.Contains(model.View().Content, "Keyboard") {
		t.Fatal("help overlay not rendered")
	}
	model.Update(tea.WindowSizeMsg{Width: 130, Height: 34})
	if model.transcript.Width() >= 130 {
		t.Fatal("wide layout did not reserve side panel")
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

func key(value string) tea.KeyPressMsg {
	switch value {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "?":
		return tea.KeyPressMsg{Code: '?', Text: "?"}
	case "ctrl+p":
		return tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}
	case "y":
		return tea.KeyPressMsg{Code: 'y', Text: "y"}
	default:
		return tea.KeyPressMsg{}
	}
}
