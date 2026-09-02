package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

// Memory is the reference Store implementation used by deterministic runtime
// tests. It applies the same optimistic sequence and atomicity rules as SQLite.
type Memory struct {
	mu       sync.RWMutex
	projects map[erkaruntime.ProjectID]erkaruntime.Project
	tasks    map[erkaruntime.TaskID]erkaruntime.StoredTask
	runs     map[erkaruntime.RunID]erkaruntime.Run
	items    map[erkaruntime.TaskID][]erkaruntime.RuntimeItem
	events   map[erkaruntime.TaskID][]erkaruntime.Event
}

func NewMemory() *Memory {
	return &Memory{
		projects: make(map[erkaruntime.ProjectID]erkaruntime.Project),
		tasks:    make(map[erkaruntime.TaskID]erkaruntime.StoredTask),
		runs:     make(map[erkaruntime.RunID]erkaruntime.Run),
		items:    make(map[erkaruntime.TaskID][]erkaruntime.RuntimeItem),
		events:   make(map[erkaruntime.TaskID][]erkaruntime.Event),
	}
}

func (memory *Memory) CreateProject(ctx context.Context, project erkaruntime.Project) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := erkaruntime.ValidateProject(project); err != nil {
		return err
	}

	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, exists := memory.projects[project.ID]; exists {
		return fmt.Errorf("project %q: %w", project.ID, erkaruntime.ErrAlreadyExists)
	}
	for _, existing := range memory.projects {
		if existing.Root == project.Root {
			return fmt.Errorf("project root %q: %w", project.Root, erkaruntime.ErrAlreadyExists)
		}
	}
	memory.projects[project.ID] = project
	return nil
}

func (memory *Memory) CreateTask(ctx context.Context, task erkaruntime.Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := erkaruntime.ValidateTask(task); err != nil {
		return err
	}

	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, exists := memory.projects[task.ProjectID]; !exists {
		return fmt.Errorf("project %q: %w", task.ProjectID, erkaruntime.ErrNotFound)
	}
	if _, exists := memory.tasks[task.ID]; exists {
		return fmt.Errorf("task %q: %w", task.ID, erkaruntime.ErrAlreadyExists)
	}
	memory.tasks[task.ID] = erkaruntime.StoredTask{Task: task}
	return nil
}

func (memory *Memory) CreateRun(ctx context.Context, run erkaruntime.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := erkaruntime.ValidateRun(run); err != nil {
		return err
	}

	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, exists := memory.tasks[run.TaskID]; !exists {
		return fmt.Errorf("task %q: %w", run.TaskID, erkaruntime.ErrNotFound)
	}
	if _, exists := memory.runs[run.ID]; exists {
		return fmt.Errorf("run %q: %w", run.ID, erkaruntime.ErrAlreadyExists)
	}
	memory.runs[run.ID] = run
	return nil
}

func (memory *Memory) LoadTask(ctx context.Context, taskID erkaruntime.TaskID) (erkaruntime.TaskSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return erkaruntime.TaskSnapshot{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	task, exists := memory.tasks[taskID]
	if !exists {
		return erkaruntime.TaskSnapshot{}, fmt.Errorf("task %q: %w", taskID, erkaruntime.ErrNotFound)
	}
	return erkaruntime.TaskSnapshot{
		TaskID: task.ID, Status: task.Status,
		LastItemSequence: task.LastItemSequence, LastEventSequence: task.LastEventSequence,
	}, nil
}

func (memory *Memory) LoadRun(ctx context.Context, runID erkaruntime.RunID) (erkaruntime.Run, error) {
	if err := ctx.Err(); err != nil {
		return erkaruntime.Run{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	run, exists := memory.runs[runID]
	if !exists {
		return erkaruntime.Run{}, fmt.Errorf("run %q: %w", runID, erkaruntime.ErrNotFound)
	}
	return run, nil
}

func (memory *Memory) LoadItems(ctx context.Context, taskID erkaruntime.TaskID, after uint64) ([]erkaruntime.RuntimeItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	if _, exists := memory.tasks[taskID]; !exists {
		return nil, fmt.Errorf("task %q: %w", taskID, erkaruntime.ErrNotFound)
	}
	return cloneItemsAfter(memory.items[taskID], after), nil
}

func (memory *Memory) LoadEvents(ctx context.Context, taskID erkaruntime.TaskID, after uint64) ([]erkaruntime.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	if _, exists := memory.tasks[taskID]; !exists {
		return nil, fmt.Errorf("task %q: %w", taskID, erkaruntime.ErrNotFound)
	}
	return cloneEventsAfter(memory.events[taskID], after), nil
}

func (memory *Memory) Commit(ctx context.Context, request erkaruntime.CommitRequest) (erkaruntime.CommittedBatch, error) {
	if err := ctx.Err(); err != nil {
		return erkaruntime.CommittedBatch{}, err
	}
	if err := erkaruntime.ValidateCommitRequest(request); err != nil {
		return erkaruntime.CommittedBatch{}, err
	}

	memory.mu.Lock()
	defer memory.mu.Unlock()
	task, exists := memory.tasks[request.TaskID]
	if !exists {
		return erkaruntime.CommittedBatch{}, fmt.Errorf("task %q: %w", request.TaskID, erkaruntime.ErrNotFound)
	}
	if task.LastItemSequence != request.ExpectedItemSequence || task.LastEventSequence != request.ExpectedEventSequence {
		return erkaruntime.CommittedBatch{}, fmt.Errorf("task %q sequences changed: %w", request.TaskID, erkaruntime.ErrConflict)
	}
	if request.Transition != nil && task.Status != request.Transition.From {
		return erkaruntime.CommittedBatch{}, fmt.Errorf("task %q status changed: %w", request.TaskID, erkaruntime.ErrConflict)
	}
	if err := memory.validateRunReferences(request); err != nil {
		return erkaruntime.CommittedBatch{}, err
	}

	validator := erkaruntime.NewTranscriptValidator()
	for _, item := range memory.items[request.TaskID] {
		if err := validator.Apply(item); err != nil {
			return erkaruntime.CommittedBatch{}, fmt.Errorf("stored transcript is invalid: %w", err)
		}
	}
	for _, item := range request.Items {
		if err := validator.Apply(item); err != nil {
			return erkaruntime.CommittedBatch{}, err
		}
	}
	if err := rejectDuplicateIDs(memory.items[request.TaskID], memory.events[request.TaskID], request); err != nil {
		return erkaruntime.CommittedBatch{}, err
	}

	items := cloneItems(request.Items)
	events := cloneEvents(request.Events)
	memory.items[request.TaskID] = append(memory.items[request.TaskID], items...)
	memory.events[request.TaskID] = append(memory.events[request.TaskID], events...)
	if len(items) > 0 {
		task.LastItemSequence = items[len(items)-1].Sequence
	}
	if len(events) > 0 {
		task.LastEventSequence = events[len(events)-1].Sequence
	}
	if request.Transition != nil {
		task.Status = request.Transition.To
	}
	memory.tasks[request.TaskID] = task

	return erkaruntime.CommittedBatch{
		TaskID: request.TaskID, LastItemSequence: task.LastItemSequence,
		LastEventSequence: task.LastEventSequence, Events: cloneEvents(events),
	}, nil
}

func (memory *Memory) validateRunReferences(request erkaruntime.CommitRequest) error {
	check := func(runID erkaruntime.RunID) error {
		if runID == "" {
			return nil
		}
		run, exists := memory.runs[runID]
		if !exists {
			return fmt.Errorf("run %q: %w", runID, erkaruntime.ErrNotFound)
		}
		if run.TaskID != request.TaskID {
			return fmt.Errorf("run %q belongs to task %q", runID, run.TaskID)
		}
		return nil
	}
	for _, item := range request.Items {
		if err := check(item.RunID); err != nil {
			return err
		}
	}
	for _, event := range request.Events {
		if err := check(event.RunID); err != nil {
			return err
		}
	}
	if request.Transition != nil {
		return check(request.Transition.RunID)
	}
	return nil
}

func rejectDuplicateIDs(existingItems []erkaruntime.RuntimeItem, existingEvents []erkaruntime.Event, request erkaruntime.CommitRequest) error {
	itemIDs := make(map[erkaruntime.ItemID]struct{}, len(existingItems))
	for _, item := range existingItems {
		itemIDs[item.ID] = struct{}{}
	}
	for _, item := range request.Items {
		if _, exists := itemIDs[item.ID]; exists {
			return fmt.Errorf("runtime item %q: %w", item.ID, erkaruntime.ErrAlreadyExists)
		}
		itemIDs[item.ID] = struct{}{}
	}
	eventIDs := make(map[erkaruntime.EventID]struct{}, len(existingEvents))
	for _, event := range existingEvents {
		eventIDs[event.ID] = struct{}{}
	}
	for _, event := range request.Events {
		if _, exists := eventIDs[event.ID]; exists {
			return fmt.Errorf("event %q: %w", event.ID, erkaruntime.ErrAlreadyExists)
		}
		eventIDs[event.ID] = struct{}{}
	}
	return nil
}

func cloneItemsAfter(items []erkaruntime.RuntimeItem, after uint64) []erkaruntime.RuntimeItem {
	result := make([]erkaruntime.RuntimeItem, 0, len(items))
	for _, item := range items {
		if item.Sequence > after {
			result = append(result, cloneItem(item))
		}
	}
	return result
}

func cloneItems(items []erkaruntime.RuntimeItem) []erkaruntime.RuntimeItem {
	result := make([]erkaruntime.RuntimeItem, len(items))
	for index, item := range items {
		result[index] = cloneItem(item)
	}
	return result
}

func cloneItem(item erkaruntime.RuntimeItem) erkaruntime.RuntimeItem {
	result := item
	if item.Message != nil {
		value := *item.Message
		result.Message = &value
	}
	if item.ToolCall != nil {
		value := *item.ToolCall
		value.Arguments = append(json.RawMessage(nil), item.ToolCall.Arguments...)
		result.ToolCall = &value
	}
	if item.ToolResult != nil {
		value := *item.ToolResult
		result.ToolResult = &value
	}
	if item.Usage != nil {
		value := *item.Usage
		result.Usage = &value
	}
	if item.Checkpoint != nil {
		value := *item.Checkpoint
		value.Constraints = append([]string(nil), item.Checkpoint.Constraints...)
		value.Decisions = append([]string(nil), item.Checkpoint.Decisions...)
		value.RelevantFiles = append([]string(nil), item.Checkpoint.RelevantFiles...)
		value.ValidationResults = append([]string(nil), item.Checkpoint.ValidationResults...)
		value.UnresolvedProblems = append([]string(nil), item.Checkpoint.UnresolvedProblems...)
		result.Checkpoint = &value
	}
	if item.Error != nil {
		value := *item.Error
		result.Error = &value
	}
	return result
}

func cloneEventsAfter(events []erkaruntime.Event, after uint64) []erkaruntime.Event {
	result := make([]erkaruntime.Event, 0, len(events))
	for _, event := range events {
		if event.Sequence > after {
			result = append(result, cloneEvent(event))
		}
	}
	return result
}

func cloneEvents(events []erkaruntime.Event) []erkaruntime.Event {
	result := make([]erkaruntime.Event, len(events))
	for index, event := range events {
		result[index] = cloneEvent(event)
	}
	return result
}

func cloneEvent(event erkaruntime.Event) erkaruntime.Event {
	result := event
	result.Data = append(json.RawMessage(nil), event.Data...)
	return result
}

var _ erkaruntime.Store = (*Memory)(nil)
