package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestMemoryLifecycleAndReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	seedMemory(t, ctx, memory, now)

	request := erkaruntime.CommitRequest{
		TaskID: "task-1",
		Items: []erkaruntime.RuntimeItem{
			toolCallItem(now, 1, "item-1", "call-1"),
			toolResultItem(now, 2, "item-2", "call-1"),
		},
		Events: []erkaruntime.Event{
			{ID: "event-1", Sequence: 1, TaskID: "task-1", RunID: "run-1", Type: "task.started", Time: now},
		},
		Transition: &erkaruntime.StateTransition{TaskID: "task-1", RunID: "run-1", From: erkaruntime.TaskQueued, To: erkaruntime.TaskPreparing},
	}
	batch, err := memory.Commit(ctx, request)
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if batch.LastItemSequence != 2 || batch.LastEventSequence != 1 {
		t.Fatalf("Commit() batch = %#v", batch)
	}

	items, err := memory.LoadItems(ctx, "task-1", 1)
	if err != nil || len(items) != 1 || items[0].Kind != erkaruntime.ItemToolResult {
		t.Fatalf("LoadItems() = %#v, %v", items, err)
	}
	events, err := memory.LoadEvents(ctx, "task-1", 0)
	if err != nil || len(events) != 1 || events[0].ID != "event-1" {
		t.Fatalf("LoadEvents() = %#v, %v", events, err)
	}
	snapshot, err := memory.LoadTask(ctx, "task-1")
	if err != nil || snapshot.Status != erkaruntime.TaskPreparing {
		t.Fatalf("LoadTask() = %#v, %v", snapshot, err)
	}
}

func TestMemoryCommitIsAtomicOnTranscriptFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	seedMemory(t, ctx, memory, now)

	request := erkaruntime.CommitRequest{
		TaskID: "task-1",
		Items:  []erkaruntime.RuntimeItem{toolResultItem(now, 1, "item-1", "unknown")},
		Events: []erkaruntime.Event{{
			ID: "event-1", Sequence: 1, TaskID: "task-1", RunID: "run-1",
			Type: "tool.completed", Time: now,
		}},
	}
	if _, err := memory.Commit(ctx, request); err == nil {
		t.Fatal("Commit() error = nil, want transcript failure")
	}
	snapshot, err := memory.LoadTask(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LastItemSequence != 0 || snapshot.LastEventSequence != 0 || snapshot.Status != erkaruntime.TaskQueued {
		t.Fatalf("task changed after failed commit: %#v", snapshot)
	}
}

func TestMemoryRejectsOptimisticConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	seedMemory(t, ctx, memory, now)

	first := erkaruntime.CommitRequest{TaskID: "task-1", Items: []erkaruntime.RuntimeItem{messageItem(now, 1, "item-1")}}
	if _, err := memory.Commit(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := erkaruntime.CommitRequest{TaskID: "task-1", Items: []erkaruntime.RuntimeItem{messageItem(now, 1, "item-2")}}
	if _, err := memory.Commit(ctx, second); !errors.Is(err, erkaruntime.ErrConflict) {
		t.Fatalf("Commit() error = %v, want ErrConflict", err)
	}
}

func TestMemoryRejectsUnknownRunAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	seedMemory(t, ctx, memory, now)

	item := messageItem(now, 1, "item-1")
	item.RunID = "missing-run"
	_, err := memory.Commit(ctx, erkaruntime.CommitRequest{TaskID: "task-1", Items: []erkaruntime.RuntimeItem{item}})
	if !errors.Is(err, erkaruntime.ErrNotFound) {
		t.Fatalf("Commit() error = %v, want ErrNotFound", err)
	}
	snapshot, loadErr := memory.LoadTask(ctx, "task-1")
	if loadErr != nil || snapshot.LastItemSequence != 0 {
		t.Fatalf("LoadTask() = %#v, %v", snapshot, loadErr)
	}
}

func TestMemoryCopiesMutablePayloads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	seedMemory(t, ctx, memory, now)

	item := toolCallItem(now, 1, "item-1", "call-1")
	if _, err := memory.Commit(ctx, erkaruntime.CommitRequest{TaskID: "task-1", Items: []erkaruntime.RuntimeItem{item}}); err != nil {
		t.Fatal(err)
	}
	item.ToolCall.Arguments[2] = 'X'
	loaded, err := memory.LoadItems(ctx, "task-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded[0].ToolCall.Arguments) != `{"path":"x"}` {
		t.Fatalf("stored arguments mutated: %s", loaded[0].ToolCall.Arguments)
	}
	loaded[0].ToolCall.Arguments[2] = 'Y'
	reloaded, _ := memory.LoadItems(ctx, "task-1", 0)
	if string(reloaded[0].ToolCall.Arguments) != `{"path":"x"}` {
		t.Fatalf("returned arguments alias store: %s", reloaded[0].ToolCall.Arguments)
	}
}

func TestMemoryAccumulatesRunUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	seedMemory(t, ctx, memory, now)
	item := erkaruntime.RuntimeItem{ID: "usage-1", Sequence: 1, TaskID: "task-1", RunID: "run-1", Kind: erkaruntime.ItemUsage, Time: now, Usage: &erkaruntime.Usage{InputTokens: 5, OutputTokens: 2, CostMicrounits: 7, Currency: "USD"}}
	if _, err := memory.Commit(ctx, erkaruntime.CommitRequest{TaskID: "task-1", Items: []erkaruntime.RuntimeItem{item}}); err != nil {
		t.Fatal(err)
	}
	run, err := memory.LoadRun(ctx, "run-1")
	if err != nil || run.Attempt != 1 || run.Usage.InputTokens != 5 || run.Usage.OutputTokens != 2 || run.Usage.CostMicrounits != 7 || run.Usage.Currency != "USD" {
		t.Fatalf("run = %#v, %v", run, err)
	}
}

func TestMemoryHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	memory := NewMemory()
	if err := memory.CreateProject(ctx, erkaruntime.Project{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateProject() error = %v", err)
	}
	if _, err := memory.LoadItems(ctx, "task-1", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadItems() error = %v", err)
	}
}

func seedMemory(t *testing.T, ctx context.Context, memory *Memory, now time.Time) {
	t.Helper()
	project := erkaruntime.Project{ID: "project-1", Root: filepath.Clean(t.TempDir()), CreatedAt: now}
	if err := memory.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	task := erkaruntime.Task{
		ID: "task-1", ProjectID: project.ID, Objective: "fix tests",
		Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now,
	}
	if err := memory.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	run := erkaruntime.Run{
		ID: "run-1", TaskID: task.ID, Stage: "coding", BackendClass: "native",
		Provider: "scripted", Model: "fixture", Status: erkaruntime.RunQueued,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := memory.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
}

func messageItem(now time.Time, sequence uint64, id erkaruntime.ItemID) erkaruntime.RuntimeItem {
	return erkaruntime.RuntimeItem{
		ID: id, Sequence: sequence, TaskID: "task-1", RunID: "run-1",
		Kind: erkaruntime.ItemUserMessage, Time: now,
		Message: &erkaruntime.Message{Content: "fix tests"},
	}
}

func toolCallItem(now time.Time, sequence uint64, id erkaruntime.ItemID, callID erkaruntime.ToolCallID) erkaruntime.RuntimeItem {
	return erkaruntime.RuntimeItem{
		ID: id, Sequence: sequence, TaskID: "task-1", RunID: "run-1",
		Kind: erkaruntime.ItemToolCall, Time: now,
		ToolCall: &erkaruntime.ToolCall{CallID: callID, Tool: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)},
	}
}

func toolResultItem(now time.Time, sequence uint64, id erkaruntime.ItemID, callID erkaruntime.ToolCallID) erkaruntime.RuntimeItem {
	return erkaruntime.RuntimeItem{
		ID: id, Sequence: sequence, TaskID: "task-1", RunID: "run-1",
		Kind: erkaruntime.ItemToolResult, Time: now,
		ToolResult: &erkaruntime.ToolResult{CallID: callID, Output: "ok"},
	}
}
