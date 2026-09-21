package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ErenKaynak/erka-agent/internal/app"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestPrompterQuestionAnsweredByOptionNumber(t *testing.T) {
	prompter := NewPrompter()
	model := NewModel(Options{Controller: &fakeController{}, Prompter: prompter})
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	answered := make(chan erkaruntime.UserResponse, 1)
	request := erkaruntime.UserRequest{Key: "k1", TaskID: "task", CallID: "call", Kind: erkaruntime.UserQuestion, Message: "Which database?", Options: []string{"sqlite", "postgres"}}
	go func() {
		response, err := prompter.Request(context.Background(), request)
		if err != nil {
			t.Error(err)
		}
		answered <- response
	}()
	model.Update(model.waitPrompt()())
	view := ansi.Strip(model.View().Content)
	if !strings.Contains(view, "Which database?") || !strings.Contains(view, "2 postgres") {
		t.Fatalf("question card missing:\n%s", view)
	}
	model.Update(key("2"))
	select {
	case response := <-answered:
		if !response.Approved || response.Answer != "postgres" {
			t.Fatalf("response = %#v", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("question was not answered")
	}
	if model.pending != nil {
		t.Fatal("prompt still pending after answer")
	}
	again, err := prompter.Request(context.Background(), request)
	if err != nil || again.Answer != "postgres" {
		t.Fatalf("repeated request = %#v, %v; want the remembered answer", again, err)
	}
}

func TestPrompterPermissionAllowedForSession(t *testing.T) {
	prompter := NewPrompter()
	model := NewModel(Options{Controller: &fakeController{}, Prompter: prompter})
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	done := make(chan erkaruntime.UserResponse, 1)
	go func() {
		response, _ := prompter.Request(context.Background(), erkaruntime.UserRequest{Key: "p1", TaskID: "task", CallID: "c1", Kind: erkaruntime.UserPermission, Tool: "run_command", Message: "Run command: go test ./..."})
		done <- response
	}()
	model.Update(model.waitPrompt()())
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "Permission requested") || !strings.Contains(view, "go test ./...") {
		t.Fatalf("permission card missing:\n%s", view)
	}
	model.Update(key("x"))
	if model.pending == nil {
		t.Fatal("an unrelated key resolved the permission request")
	}
	model.Update(key("a"))
	if response := <-done; !response.Approved {
		t.Fatalf("response = %#v", response)
	}
	next, err := prompter.Request(context.Background(), erkaruntime.UserRequest{Key: "p2", TaskID: "task", CallID: "c2", Kind: erkaruntime.UserPermission, Tool: "run_command", Message: "Run command: make"})
	if err != nil || !next.Approved {
		t.Fatalf("session grant = %#v, %v", next, err)
	}
}

func TestPrompterReturnsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewPrompter().Request(ctx, erkaruntime.UserRequest{Key: "k", TaskID: "t", CallID: "c", Kind: erkaruntime.UserQuestion, Message: "?"}); err == nil {
		t.Fatal("cancelled request returned without error")
	}
}

func TestConversationPickerFiltersByTitle(t *testing.T) {
	now := time.Now()
	controller := &fakeController{conversations: []erkaruntime.TaskSnapshot{
		{Task: erkaruntime.Task{ID: "task-1", Objective: "Fix checkout totals", Status: erkaruntime.TaskCompleted, UpdatedAt: now}},
		{Task: erkaruntime.Task{ID: "task-2", Objective: "Add dark mode", Status: erkaruntime.TaskFailed, UpdatedAt: now}},
	}}
	model := NewModel(Options{Controller: controller})
	model.Update(tea.WindowSizeMsg{Width: 90, Height: 24})
	model.conversations = controller.conversations
	model.Update(key("ctrl+o"))
	for _, value := range "dark" {
		model.Update(key(string(value)))
	}
	view := ansi.Strip(model.View().Content)
	if !strings.Contains(view, "Add dark mode") || strings.Contains(view, "Fix checkout totals") {
		t.Fatalf("filtered picker:\n%s", view)
	}
	_, command := model.Update(key("enter"))
	if command == nil || model.taskID != "task-2" {
		t.Fatalf("picker opened %q", model.taskID)
	}
}

func TestTranscriptSummarizesToolSteps(t *testing.T) {
	patch, _ := json.Marshal(map[string]string{"patch": "--- a/x.go\n+++ b/x.go\n@@\n-old\n+new\n+more\n"})
	model := NewModel(Options{Controller: &fakeController{}})
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model.conversation = app.Conversation{Items: []erkaruntime.RuntimeItem{
		{ID: "1", Kind: erkaruntime.ItemUserMessage, Message: &erkaruntime.Message{Content: "fix x"}},
		{ID: "2", Kind: erkaruntime.ItemToolCall, ToolCall: &erkaruntime.ToolCall{CallID: "a", Tool: "apply_patch", Arguments: patch}},
		{ID: "3", Kind: erkaruntime.ItemToolResult, ToolResult: &erkaruntime.ToolResult{CallID: "a", Output: "ok"}},
		{ID: "4", Kind: erkaruntime.ItemToolCall, ToolCall: &erkaruntime.ToolCall{CallID: "b", Tool: "run_command", Arguments: json.RawMessage(`{"command":["go","test"]}`)}},
		{ID: "5", Kind: erkaruntime.ItemToolResult, ToolResult: &erkaruntime.ToolResult{CallID: "b", Output: "FAIL\nexit_code=1", IsError: true}},
	}}
	model.refresh()
	view := ansi.Strip(model.View().Content)
	for _, want := range []string{"› fix x", "✓ apply_patch", "x.go  +2 −1", "✗ run_command", "go test  exit 1"} {
		if !strings.Contains(view, want) {
			t.Fatalf("transcript missing %q:\n%s", want, view)
		}
	}
	model.Update(key("ctrl+t"))
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "│ FAIL") {
		t.Fatalf("expanded details missing output:\n%s", view)
	}
}

func TestModelPickerGroupsByStageAndMarksCurrent(t *testing.T) {
	options := []ModelOption{
		{Stage: erkaruntime.StageCoding, Connection: "coder", Provider: "deepseek", ID: "deepseek-v4-flash", Source: "live", Current: true, Selectable: true},
		{Stage: erkaruntime.StagePlanning, Connection: "chatgpt", Provider: "codex", Class: "subscription_bridge", ID: "gpt-5.1-codex", Source: "live", Selectable: true},
		{Stage: erkaruntime.StageCoding, Connection: "coder", Provider: "deepseek", ID: "deepseek-v4-pro", Source: "configured", Selectable: true},
	}
	model := NewModel(Options{Controller: &fakeController{}, Models: func(context.Context) ([]ModelOption, []string, error) {
		return options, []string{"opencode: not signed in"}, nil
	}})
	model.Update(tea.WindowSizeMsg{Width: 90, Height: 24})
	model.Update(runSlash(t, model, "/model")())
	view := ansi.Strip(model.View().Content)
	planning, coding := strings.Index(view, "PLANNING"), strings.Index(view, "CODING")
	if planning < 0 || coding < planning || !strings.Contains(view, "chatgpt · codex bridge") || !strings.Contains(view, "● current") || !strings.Contains(view, "Discovery warnings") {
		t.Fatalf("model picker:\n%s", view)
	}
	for _, value := range "pro" {
		model.Update(key(string(value)))
	}
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "› deepseek-v4-pro") || strings.Contains(view, "gpt-5.1-codex ") {
		t.Fatalf("filtered model picker:\n%s", view)
	}
}

func TestDoubleCtrlCQuitsWithoutConfirmation(t *testing.T) {
	controller := &fakeController{}
	model := NewModel(Options{Controller: controller})
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	model.composer.InsertString("draft")
	if _, command := model.Update(key("ctrl+c")); command != nil || model.composer.Value() != "" {
		t.Fatalf("first Ctrl+C = command %v, composer %q; want cleared input and no quit", command, model.composer.Value())
	}
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "Press Ctrl+C again to quit") {
		t.Fatalf("quit hint missing:\n%s", view)
	}
	_, command := model.Update(key("ctrl+c"))
	if command == nil {
		t.Fatal("second Ctrl+C did not quit")
	}
	if _, ok := command().(tea.QuitMsg); !ok || controller.cancelled != 1 {
		t.Fatalf("second Ctrl+C = %T, cancelled %d", command(), controller.cancelled)
	}
	model.quitArmed = time.Now().Add(-3 * time.Second)
	if _, command := model.Update(key("ctrl+c")); command != nil {
		t.Fatal("a Ctrl+C after the window expired quit immediately")
	}
}

func TestShiftTabCyclesAutoPlanAndGodModes(t *testing.T) {
	controller := &fakeController{}
	model := NewModel(Options{Controller: controller, Provider: "deepseek", Model: "fixture"})
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "· auto") {
		t.Fatalf("default mode not shown:\n%s", view)
	}
	model.Update(key("shift+tab"))
	if model.mode != modePlan || !controller.plan || controller.full || !strings.Contains(ansi.Strip(model.View().Content), "plan mode") {
		t.Fatalf("plan = mode %v plan %v full %v", model.mode, controller.plan, controller.full)
	}
	model.Update(key("shift+tab"))
	if model.mode != modeGod || controller.plan || !controller.full || !model.full || !strings.Contains(ansi.Strip(model.View().Content), "god mode") {
		t.Fatalf("god = mode %v plan %v full %v", model.mode, controller.plan, controller.full)
	}
	model.Update(key("shift+tab"))
	if model.mode != modeAuto || controller.plan || controller.full {
		t.Fatalf("auto = mode %v plan %v full %v", model.mode, controller.plan, controller.full)
	}
	runSlash(t, model, "/plan")
	if model.mode != modePlan || !controller.plan {
		t.Fatal("/plan did not enter plan mode")
	}
}
