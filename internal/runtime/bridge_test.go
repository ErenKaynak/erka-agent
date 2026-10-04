package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestBridgeBackendPersistsCanonicalCheckpointBeforeStageOutput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC)
	root := t.TempDir()
	memory := store.NewMemory()
	project := erkaruntime.Project{ID: "project-bridge", Root: root, CreatedAt: now}
	task := erkaruntime.Task{ID: "task-bridge", ProjectID: project.ID, Objective: "fix checkout", Pipeline: "default", Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now}
	run := erkaruntime.Run{ID: "run-bridge", TaskID: task.ID, Stage: string(erkaruntime.StagePlanning), BackendClass: erkaruntime.BackendSubscriptionBridge, Connection: "chatgpt", Provider: "codex", Model: "default", Attempt: 1, Status: erkaruntime.RunQueued, CreatedAt: now, UpdatedAt: now}
	if err := memory.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := memory.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := memory.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	bridge := &recordingBridge{result: erkaruntime.BridgeResult{
		Output:     `{"objective":"untrusted","approved_plan":"inspect then patch"}`,
		Checkpoint: erkaruntime.Checkpoint{Objective: "untrusted", ApprovedPlan: "inspect then patch", Decisions: []string{"bounded change"}},
		Usage:      erkaruntime.Usage{InputTokens: 20, OutputTokens: 8},
	}}
	counter := 0
	backend := &erkaruntime.BridgeBackend{
		Bridge: bridge, WorkingDirectory: root, Model: "default",
		Limits: erkaruntime.NativeLimits{MaxIterations: 1, MaxOutputTokens: 1024, MaxModelOutputBytes: 4096, MaxCumulativeToolOutputBytes: 1, StageTimeout: time.Minute},
		IDs: erkaruntime.IDGeneratorFunc(func(prefix string) (string, error) {
			counter++
			return fmt.Sprintf("bridge-%s-%d", prefix, counter), nil
		}),
		Clock: func() time.Time { return now },
	}
	result, err := backend.RunStage(ctx, erkaruntime.StageRequest{TaskID: task.ID, RunID: run.ID, Objective: task.Objective, Stage: erkaruntime.StagePlanning, ContextMaxBytes: 8192}, memory)
	if err != nil {
		t.Fatal(err)
	}
	if result.Checkpoint.Objective != task.Objective || result.Checkpoint.ApprovedPlan != "inspect then patch" || result.Mutated {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(bridge.request.Prompt, "<durable_context>") || strings.Contains(bridge.request.Prompt, "untrusted") {
		t.Fatalf("bridge prompt = %q", bridge.request.Prompt)
	}
	items, err := memory.LoadItems(ctx, task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []erkaruntime.ItemKind{erkaruntime.ItemUserMessage, erkaruntime.ItemAssistantMessage, erkaruntime.ItemUsage, erkaruntime.ItemCheckpoint}
	if len(items) != len(wantKinds) {
		t.Fatalf("items = %#v", items)
	}
	for index, want := range wantKinds {
		if items[index].Kind != want {
			t.Fatalf("item %d kind = %q, want %q", index, items[index].Kind, want)
		}
	}
	events, err := memory.LoadEvents(ctx, task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Type != "stage.output" {
		t.Fatalf("events = %#v", events)
	}
	snapshot, _ := memory.LoadTask(ctx, task.ID)
	storedRun, _ := memory.LoadRun(ctx, run.ID)
	if snapshot.Status != erkaruntime.TaskPlanning || storedRun.Status != erkaruntime.RunRunning || storedRun.Usage.InputTokens != 20 {
		t.Fatalf("task = %#v, run = %#v", snapshot, storedRun)
	}
}

func TestBridgeBackendRejectsMutatingStage(t *testing.T) {
	t.Parallel()
	backend := &erkaruntime.BridgeBackend{Bridge: &recordingBridge{}, WorkingDirectory: t.TempDir()}
	_, err := backend.RunStage(context.Background(), erkaruntime.StageRequest{TaskID: "task", RunID: "run", Objective: "code", Stage: erkaruntime.StageCoding}, store.NewMemory())
	if err == nil || !strings.Contains(err.Error(), "cannot run coding") {
		t.Fatalf("error = %v", err)
	}
}

type recordingBridge struct {
	request erkaruntime.BridgeRequest
	result  erkaruntime.BridgeResult
}

func (*recordingBridge) Name() string { return "fixture" }
func (*recordingBridge) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true, StructuredOutput: true}, nil
}
func (*recordingBridge) Availability(context.Context) (erkaruntime.BridgeAvailability, error) {
	return erkaruntime.BridgeAvailability{Available: true, Authenticated: true}, nil
}
func (bridge *recordingBridge) Execute(_ context.Context, request erkaruntime.BridgeRequest) (erkaruntime.BridgeResult, error) {
	bridge.request = request
	return bridge.result, nil
}

var _ erkaruntime.SubscriptionBridge = (*recordingBridge)(nil)

type noticingBridge struct {
	*recordingBridge
}

func (noticingBridge) Notice() string { return "Experimental bridge: subject to the vendor's terms." }

func TestBridgeBackendRecordsBridgeNoticeEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
	root := t.TempDir()
	memory := store.NewMemory()
	project := erkaruntime.Project{ID: "project-notice", Root: root, CreatedAt: now}
	task := erkaruntime.Task{ID: "task-notice", ProjectID: project.ID, Objective: "plan", Pipeline: "default", Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now}
	run := erkaruntime.Run{ID: "run-notice", TaskID: task.ID, Stage: string(erkaruntime.StagePlanning), BackendClass: erkaruntime.BackendSubscriptionBridge, Connection: "claude", Provider: "claude_code", Model: "default", Attempt: 1, Status: erkaruntime.RunQueued, CreatedAt: now, UpdatedAt: now}
	for _, err := range []error{memory.CreateProject(ctx, project), memory.CreateTask(ctx, task), memory.CreateRun(ctx, run)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	bridge := noticingBridge{&recordingBridge{result: erkaruntime.BridgeResult{Checkpoint: erkaruntime.Checkpoint{Objective: "plan", ApprovedPlan: "do it"}}}}
	backend := &erkaruntime.BridgeBackend{
		Bridge: bridge, WorkingDirectory: root, Model: "default",
		Limits: erkaruntime.NativeLimits{MaxIterations: 1, MaxOutputTokens: 1024, MaxModelOutputBytes: 4096, MaxCumulativeToolOutputBytes: 1, StageTimeout: time.Minute},
		Clock:  func() time.Time { return now },
	}
	if _, err := backend.RunStage(ctx, erkaruntime.StageRequest{TaskID: task.ID, RunID: run.ID, Objective: task.Objective, Stage: erkaruntime.StagePlanning, ContextMaxBytes: 8192}, memory); err != nil {
		t.Fatal(err)
	}
	events, err := memory.LoadEvents(ctx, task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var notices int
	for _, event := range events {
		if event.Type == "bridge.notice" {
			notices++
			if !strings.Contains(event.Message, "vendor's terms") || string(event.Data) != `{"bridge":"fixture","stage":"planning"}` {
				t.Fatalf("notice event = %#v", event)
			}
		}
	}
	if notices != 1 || events[len(events)-1].Type != "stage.output" {
		t.Fatalf("events = %#v", events)
	}
}
