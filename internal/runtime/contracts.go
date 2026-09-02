package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

type ModelCapabilities struct {
	Tools             bool
	ParallelToolCalls bool
	StructuredOutput  bool
	Vision            bool
	ContextTokens     int64
}

type ModelRequest struct {
	Instructions    string
	Items           []RuntimeItem
	Tools           []ToolDefinition
	MaxOutputTokens int64
}

func (request ModelRequest) Validate() error {
	if strings.TrimSpace(request.Instructions) == "" {
		return errors.New("model request requires instructions")
	}
	if request.MaxOutputTokens <= 0 {
		return errors.New("model request requires a positive output-token limit")
	}
	for index, item := range request.Items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("model request item %d: %w", index, err)
		}
	}
	toolNames := make(map[string]struct{}, len(request.Tools))
	for index, tool := range request.Tools {
		if err := tool.Validate(); err != nil {
			return fmt.Errorf("model request tool %d: %w", index, err)
		}
		if _, exists := toolNames[tool.Name]; exists {
			return fmt.Errorf("model request contains duplicate tool %q", tool.Name)
		}
		toolNames[tool.Name] = struct{}{}
	}
	return nil
}

type ModelOutputKind string

const (
	ModelTextDelta     ModelOutputKind = "text_delta"
	ModelToolCallDelta ModelOutputKind = "tool_call_delta"
	ModelUsage         ModelOutputKind = "usage"
	ModelCompleted     ModelOutputKind = "completed"
	ModelFailed        ModelOutputKind = "failed"
)

type ModelOutput struct {
	Kind     ModelOutputKind
	Text     string
	ToolCall *ToolCallDelta
	Usage    *Usage
	Error    *RuntimeError
}

type ToolCallDelta struct {
	CallID            ToolCallID
	Tool              string
	ArgumentsFragment string
}

func (output ModelOutput) Validate() error {
	switch output.Kind {
	case ModelTextDelta:
		if output.Text == "" || output.ToolCall != nil || output.Usage != nil || output.Error != nil {
			return errors.New("text delta requires only non-empty text")
		}
	case ModelToolCallDelta:
		if output.Text != "" || output.ToolCall == nil || output.Usage != nil || output.Error != nil {
			return errors.New("tool-call delta requires only a tool-call payload")
		}
		if output.ToolCall.CallID == "" {
			return errors.New("tool-call delta requires a call ID")
		}
		if output.ToolCall.Tool == "" && output.ToolCall.ArgumentsFragment == "" {
			return errors.New("tool-call delta requires a tool name or argument fragment")
		}
	case ModelUsage:
		if output.Text != "" || output.ToolCall != nil || output.Usage == nil || output.Error != nil {
			return errors.New("usage output requires only a usage payload")
		}
		return output.Usage.Validate()
	case ModelCompleted:
		if output.Text != "" || output.ToolCall != nil || output.Usage != nil || output.Error != nil {
			return errors.New("completed output must not contain a payload")
		}
	case ModelFailed:
		if output.Text != "" || output.ToolCall != nil || output.Usage != nil || output.Error == nil {
			return errors.New("failed output requires only an error payload")
		}
		if output.Error.Code == "" || output.Error.Message == "" {
			return errors.New("failed output requires an error code and message")
		}
	default:
		return fmt.Errorf("unknown model output kind %q", output.Kind)
	}
	return nil
}

// ModelProvider translates one native provider protocol into canonical stream
// output. It never executes tools.
type ModelProvider interface {
	Name() string
	Capabilities(context.Context) (ModelCapabilities, error)
	Stream(context.Context, ModelRequest) (ModelStream, error)
}

type ModelStream interface {
	Next(context.Context) (ModelOutput, error)
	Close() error
}

type ToolDefinition struct {
	Name             string
	Description      string
	InputSchema      json.RawMessage
	Mutating         bool
	ParallelSafe     bool
	OutputLimitBytes int64
}

func (definition ToolDefinition) Validate() error {
	if definition.Name == "" || strings.TrimSpace(definition.Description) == "" {
		return errors.New("tool definition requires a name and description")
	}
	if !isJSONObject(definition.InputSchema) {
		return errors.New("tool input schema must be one valid JSON object")
	}
	if definition.OutputLimitBytes <= 0 {
		return errors.New("tool definition requires a positive output limit")
	}
	return nil
}

type ToolInvocation struct {
	CallID         ToolCallID
	Arguments      json.RawMessage
	IdempotencyKey string
}

func (invocation ToolInvocation) Validate(definition ToolDefinition) error {
	if invocation.CallID == "" {
		return errors.New("tool invocation requires a call ID")
	}
	if !isJSONObject(invocation.Arguments) {
		return errors.New("tool invocation arguments must be one valid JSON object")
	}
	if definition.Mutating && invocation.IdempotencyKey == "" {
		return errors.New("mutating tool invocation requires an idempotency key")
	}
	return nil
}

type ToolExecutionResult struct {
	Output    string
	IsError   bool
	Truncated bool
}

type Tool interface {
	Definition() ToolDefinition
	Execute(context.Context, ToolInvocation) (ToolExecutionResult, error)
}

type CommandRequest struct {
	Executable       string
	Arguments        []string
	Directory        string
	Environment      map[string]string
	StandardInput    []byte
	Timeout          time.Duration
	OutputLimitBytes int64
}

type CommandResult struct {
	ExitCode       int
	StandardOutput string
	StandardError  string
	Truncated      bool
	Duration       time.Duration
}

// Sandbox is the enforcement boundary for scoped filesystem and process work.
// WriteFile implementations must replace files atomically.
type Sandbox interface {
	ReadFile(context.Context, string, int64) ([]byte, error)
	WriteFile(context.Context, string, []byte, fs.FileMode) error
	Run(context.Context, CommandRequest) (CommandResult, error)
}

type TaskStatus string

const (
	TaskQueued          TaskStatus = "queued"
	TaskPreparing       TaskStatus = "preparing"
	TaskPlanning        TaskStatus = "planning"
	TaskCoding          TaskStatus = "coding"
	TaskValidating      TaskStatus = "validating"
	TaskReviewing       TaskStatus = "reviewing"
	TaskWaiting         TaskStatus = "waiting"
	TaskWaitingApproval TaskStatus = "waiting_approval"
	TaskCompleted       TaskStatus = "completed"
	TaskFailed          TaskStatus = "failed"
	TaskCancelled       TaskStatus = "cancelled"
	TaskTimedOut        TaskStatus = "timed_out"
)

type StateTransition struct {
	TaskID TaskID
	RunID  RunID
	From   TaskStatus
	To     TaskStatus
}

func (transition StateTransition) Validate() error {
	if transition.TaskID == "" {
		return ErrInvalidTransition
	}
	allowed := allowedTransitions[transition.From]
	if !allowed[transition.To] {
		return ErrInvalidTransition
	}
	return nil
}

var ErrInvalidTransition = errors.New("invalid task state transition")

var allowedTransitions = map[TaskStatus]map[TaskStatus]bool{
	TaskQueued:          {TaskPreparing: true, TaskCancelled: true},
	TaskPreparing:       {TaskPlanning: true, TaskCoding: true, TaskFailed: true, TaskCancelled: true, TaskTimedOut: true},
	TaskPlanning:        {TaskCoding: true, TaskWaitingApproval: true, TaskFailed: true, TaskCancelled: true, TaskTimedOut: true},
	TaskCoding:          {TaskValidating: true, TaskWaiting: true, TaskWaitingApproval: true, TaskFailed: true, TaskCancelled: true, TaskTimedOut: true},
	TaskValidating:      {TaskCoding: true, TaskReviewing: true, TaskCompleted: true, TaskWaiting: true, TaskFailed: true, TaskCancelled: true, TaskTimedOut: true},
	TaskReviewing:       {TaskCompleted: true, TaskCoding: true, TaskWaiting: true, TaskFailed: true, TaskCancelled: true, TaskTimedOut: true},
	TaskWaiting:         {TaskCoding: true, TaskValidating: true, TaskReviewing: true, TaskCancelled: true, TaskTimedOut: true},
	TaskWaitingApproval: {TaskPlanning: true, TaskCoding: true, TaskValidating: true, TaskReviewing: true, TaskCancelled: true, TaskTimedOut: true},
}

type CommitRequest struct {
	TaskID                TaskID
	ExpectedItemSequence  uint64
	ExpectedEventSequence uint64
	Items                 []RuntimeItem
	Events                []Event
	Transition            *StateTransition
}

type CommittedBatch struct {
	TaskID            TaskID
	LastItemSequence  uint64
	LastEventSequence uint64
	Events            []Event
}

func (request CommitRequest) Validate() error {
	if request.TaskID == "" {
		return errors.New("commit requires a task ID")
	}
	if len(request.Items) == 0 && len(request.Events) == 0 && request.Transition == nil {
		return errors.New("commit must contain items, events, or a state transition")
	}

	for index, item := range request.Items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("commit item %d: %w", index, err)
		}
		if item.TaskID != request.TaskID {
			return fmt.Errorf("commit item %d belongs to task %q", index, item.TaskID)
		}
		want := request.ExpectedItemSequence + uint64(index) + 1
		if item.Sequence != want {
			return fmt.Errorf("commit item sequence %d, want %d", item.Sequence, want)
		}
	}
	if err := ValidateEvents(request.ExpectedEventSequence, request.Events); err != nil {
		return err
	}
	for index, event := range request.Events {
		if event.TaskID != request.TaskID {
			return fmt.Errorf("commit event %d belongs to task %q", index, event.TaskID)
		}
	}

	if request.Transition != nil {
		if err := request.Transition.Validate(); err != nil {
			return err
		}
		if request.Transition.TaskID != request.TaskID {
			return errors.New("commit transition belongs to another task")
		}
	}

	hasCompletionEvent := false
	for _, event := range request.Events {
		if event.Type == "task.completed" {
			hasCompletionEvent = true
		}
	}
	transitionCompletes := request.Transition != nil && request.Transition.To == TaskCompleted
	if hasCompletionEvent != transitionCompletes {
		return errors.New("task completion event and completed state transition must be committed together")
	}

	return nil
}

type TaskSnapshot struct {
	TaskID            TaskID
	Status            TaskStatus
	LastItemSequence  uint64
	LastEventSequence uint64
}

// Store atomically persists items, events, and state transitions. Commit must
// return only after the entire batch is durable.
type Store interface {
	LoadTask(context.Context, TaskID) (TaskSnapshot, error)
	Commit(context.Context, CommitRequest) (CommittedBatch, error)
}

// EventSink receives only batches already returned successfully by Store.Commit.
// Sinks must tolerate retry and deduplicate by event ID.
type EventSink interface {
	Publish(context.Context, CommittedBatch) error
}

type StageRequest struct {
	TaskID     TaskID
	RunID      RunID
	Objective  string
	Checkpoint *Checkpoint
}

type StageResult struct {
	Checkpoint Checkpoint
	Status     TaskStatus
}

// ExecutionBackend runs one stage and records durable progress through Store.
type ExecutionBackend interface {
	RunStage(context.Context, StageRequest, Store) (StageResult, error)
}
