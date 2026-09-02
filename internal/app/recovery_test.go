package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestRunLocalRestartRepeatsIncompleteReadOnlyTool(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	taskID := seedPendingCall(t, root, state, erkaruntime.ToolCall{
		CallID: "pending-read", Tool: "read_file", Arguments: json.RawMessage(`{"path":"add.go"}`),
	})
	provider := &scriptedProvider{name: "fixture", responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "Recovered the read and completed safely."},
		{Kind: erkaruntime.ModelCompleted},
	}}}
	result, err := RunLocal(context.Background(), LocalOptions{
		WorkingDirectory: root, StateDir: state, ResumeTaskID: taskID,
		Provider: provider, IDs: &sequenceIDs{}, Clock: fixedClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("result = %#v", result)
	}
	database := openDatabase(t, state)
	defer database.Close()
	items, err := database.LoadItems(context.Background(), taskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 3 || items[2].Kind != erkaruntime.ItemToolResult || items[2].ToolResult.CallID != "pending-read" {
		t.Fatalf("items = %#v", items)
	}
	events, err := database.LoadEvents(context.Background(), taskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Type != "tool.recovered" {
		t.Fatalf("events = %#v", events)
	}
	requests := provider.Requests()
	if len(requests) != 1 || !hasResult(requests[0].Items, "pending-read") {
		t.Fatalf("provider did not receive recovered durable result: %#v", requests)
	}
}

func TestRunLocalRestartPausesUnknownMutatingTool(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	taskID := seedPendingCall(t, root, state, erkaruntime.ToolCall{
		CallID: "pending-patch", Tool: "apply_patch", Arguments: json.RawMessage(`{"patch":"--- a/add.go\\n+++ b/add.go\\n"}`),
		Mutating: true, IdempotencyKey: "task/run/pending-patch",
	})
	provider := &scriptedProvider{name: "fixture"}
	result, err := RunLocal(context.Background(), LocalOptions{
		WorkingDirectory: root, StateDir: state, ResumeTaskID: taskID,
		Provider: provider, IDs: &sequenceIDs{}, Clock: fixedClock,
	})
	if !errors.Is(err, erkaruntime.ErrUnknownMutatingToolOutcome) {
		t.Fatalf("RunLocal() error = %v", err)
	}
	if result.Status != erkaruntime.TaskWaiting {
		t.Fatalf("result = %#v", result)
	}
	if len(provider.Requests()) != 0 {
		t.Fatal("provider was called before the unknown mutation was resolved")
	}
	database := openDatabase(t, state)
	defer database.Close()
	snapshot, err := database.LoadTask(context.Background(), taskID)
	if err != nil || snapshot.Status != erkaruntime.TaskWaiting {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
	run, err := database.LoadRun(context.Background(), runIDForTask(taskID))
	if err != nil || run.Status != erkaruntime.RunWaiting {
		t.Fatalf("run = %#v, %v", run, err)
	}
}

func seedPendingCall(t *testing.T, root, state string, call erkaruntime.ToolCall) erkaruntime.TaskID {
	t.Helper()
	canonicalRoot, err := DiscoverRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	database := openDatabase(t, state)
	defer database.Close()
	now := fixedClock()
	taskID := erkaruntime.TaskID("task_restart")
	runID := runIDForTask(taskID)
	project := erkaruntime.Project{ID: projectID(canonicalRoot), Root: canonicalRoot, CreatedAt: now}
	task := erkaruntime.Task{ID: taskID, ProjectID: project.ID, Objective: "resume fixture", Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now}
	run := erkaruntime.Run{ID: runID, TaskID: taskID, Stage: "coding", BackendClass: "native", Provider: "fixture", Model: "fixture", Status: erkaruntime.RunQueued, CreatedAt: now, UpdatedAt: now}
	if err := database.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Commit(context.Background(), erkaruntime.CommitRequest{
		TaskID: taskID, Transition: &erkaruntime.StateTransition{TaskID: taskID, RunID: runID, From: erkaruntime.TaskQueued, To: erkaruntime.TaskPreparing},
	}); err != nil {
		t.Fatal(err)
	}
	items := []erkaruntime.RuntimeItem{
		{ID: "seed-user", Sequence: 1, TaskID: taskID, RunID: runID, Kind: erkaruntime.ItemUserMessage, Time: now, Message: &erkaruntime.Message{Content: task.Objective}},
		{ID: "seed-call", Sequence: 2, TaskID: taskID, RunID: runID, Kind: erkaruntime.ItemToolCall, Time: now, ToolCall: &call},
	}
	if _, err := database.Commit(context.Background(), erkaruntime.CommitRequest{
		TaskID: taskID, Items: items,
		Transition:    &erkaruntime.StateTransition{TaskID: taskID, RunID: runID, From: erkaruntime.TaskPreparing, To: erkaruntime.TaskCoding},
		RunTransition: &erkaruntime.RunStateTransition{TaskID: taskID, RunID: runID, From: erkaruntime.RunQueued, To: erkaruntime.RunRunning},
	}); err != nil {
		t.Fatal(err)
	}
	return taskID
}

func openDatabase(t *testing.T, state string) *store.SQLite {
	t.Helper()
	database, err := store.OpenSQLite(context.Background(), filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func fixedClock() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
