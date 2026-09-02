package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"testing"
	"time"
)

func TestTranscriptValidator(t *testing.T) {
	t.Parallel()

	validator := NewTranscriptValidator()
	call := RuntimeItem{
		ID: "item-1", Sequence: 1, TaskID: "task-1", RunID: "run-1",
		Kind: ItemToolCall, Time: time.Now(),
		ToolCall: &ToolCall{
			CallID: "call-1", Tool: "read_file", Arguments: json.RawMessage(`{"path":"x"}`),
		},
	}
	result := RuntimeItem{
		ID: "item-2", Sequence: 2, TaskID: "task-1", RunID: "run-1",
		Kind: ItemToolResult, Time: time.Now(),
		ToolResult: &ToolResult{CallID: "call-1", Output: "ok"},
	}

	if err := validator.Apply(call); err != nil {
		t.Fatalf("Apply(call) error = %v", err)
	}
	if err := validator.Apply(result); err != nil {
		t.Fatalf("Apply(result) error = %v", err)
	}
	if validator.LastSequence() != 2 {
		t.Fatalf("LastSequence() = %d, want 2", validator.LastSequence())
	}
}

func TestTranscriptValidatorRejectsResultBeforeCall(t *testing.T) {
	t.Parallel()

	item := RuntimeItem{
		ID: "item-1", Sequence: 1, TaskID: "task-1", RunID: "run-1",
		Kind: ItemToolResult, Time: time.Now(),
		ToolResult: &ToolResult{CallID: "missing"},
	}
	if err := NewTranscriptValidator().Apply(item); err == nil {
		t.Fatal("Apply() error = nil, want unknown call error")
	}
}

func TestTranscriptValidatorRejectsSequenceGap(t *testing.T) {
	t.Parallel()

	if err := NewTranscriptValidator().Apply(messageItem(2, "item-2")); err == nil {
		t.Fatal("Apply() error = nil, want sequence error")
	}
}

func TestValidateEvents(t *testing.T) {
	t.Parallel()

	events := []Event{
		{ID: "event-1", Sequence: 5, TaskID: "task-1", Type: "task.started", Time: time.Now()},
		{ID: "event-2", Sequence: 6, TaskID: "task-1", Type: "task.completed", Time: time.Now()},
	}
	if err := ValidateEvents(4, events); err != nil {
		t.Fatalf("ValidateEvents() error = %v", err)
	}

	events[1].Sequence = 7
	if err := ValidateEvents(4, events); err == nil {
		t.Fatal("ValidateEvents() error = nil, want gap error")
	}
}

func TestStateTransitionValidate(t *testing.T) {
	t.Parallel()

	valid := StateTransition{TaskID: "task-1", From: TaskQueued, To: TaskPreparing}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	invalid := StateTransition{TaskID: "task-1", From: TaskCompleted, To: TaskCoding}
	if !errors.Is(invalid.Validate(), ErrInvalidTransition) {
		t.Fatalf("Validate() error = %v, want ErrInvalidTransition", invalid.Validate())
	}
}

func TestModelOutputValidateAllowsFragmentedToolArguments(t *testing.T) {
	t.Parallel()

	outputs := []ModelOutput{
		{
			Kind:     ModelToolCallDelta,
			ToolCall: &ToolCallDelta{CallID: "call-1", Tool: "read_file", ArgumentsFragment: `{"pa`},
		},
		{
			Kind:     ModelToolCallDelta,
			ToolCall: &ToolCallDelta{CallID: "call-1", ArgumentsFragment: `th":"x"}`},
		},
	}
	for index, output := range outputs {
		if err := output.Validate(); err != nil {
			t.Fatalf("output %d Validate() error = %v", index, err)
		}
	}
}

func TestToolDefinitionAndInvocationValidate(t *testing.T) {
	t.Parallel()

	definition := ToolDefinition{
		Name: "write_file", Description: "Write one workspace file",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Mutating:    true, OutputLimitBytes: 1024,
	}
	if err := definition.Validate(); err != nil {
		t.Fatalf("definition Validate() error = %v", err)
	}
	invocation := ToolInvocation{
		CallID: "call-1", Arguments: json.RawMessage(`{"path":"x"}`), IdempotencyKey: "key-1",
	}
	if err := invocation.Validate(definition); err != nil {
		t.Fatalf("invocation Validate() error = %v", err)
	}

	invocation.IdempotencyKey = ""
	if err := invocation.Validate(definition); err == nil {
		t.Fatal("invocation Validate() error = nil without idempotency key")
	}
}

func TestCommitRequestValidateCompletionIsAtomic(t *testing.T) {
	t.Parallel()

	now := time.Now()
	request := CommitRequest{
		TaskID:                "task-1",
		ExpectedItemSequence:  4,
		ExpectedEventSequence: 8,
		Items: []RuntimeItem{{
			ID: "item-5", Sequence: 5, TaskID: "task-1", RunID: "run-1",
			Kind: ItemCheckpoint, Time: now, Checkpoint: &Checkpoint{Objective: "finish M0"},
		}},
		Events: []Event{{
			ID: "event-9", Sequence: 9, TaskID: "task-1", RunID: "run-1",
			Type: "task.completed", Time: now,
		}},
		Transition: &StateTransition{
			TaskID: "task-1", RunID: "run-1", From: TaskReviewing, To: TaskCompleted,
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	request.Transition = nil
	if err := request.Validate(); err == nil {
		t.Fatal("Validate() error = nil for completion event without transition")
	}
}

func TestCommitRequestRejectsCrossTaskData(t *testing.T) {
	t.Parallel()

	request := CommitRequest{
		TaskID: "task-1",
		Items:  []RuntimeItem{messageItem(1, "item-1")},
	}
	request.Items[0].TaskID = "task-2"
	if err := request.Validate(); err == nil {
		t.Fatal("Validate() error = nil for cross-task item")
	}
}

func TestBoundaryInterfacesPropagateCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	provider := fakeProvider{}
	if _, err := provider.Stream(ctx, ModelRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream() error = %v, want context.Canceled", err)
	}
	tool := fakeTool{}
	if _, err := tool.Execute(ctx, ToolInvocation{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context.Canceled", err)
	}
	sandbox := fakeSandbox{}
	if _, err := sandbox.Run(ctx, CommandRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	store := fakeStore{}
	if _, err := store.LoadTask(ctx, "task-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadTask() error = %v, want context.Canceled", err)
	}
	sink := fakeEventSink{}
	if err := sink.Publish(ctx, CommittedBatch{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish() error = %v, want context.Canceled", err)
	}
}

var (
	_ ModelProvider    = fakeProvider{}
	_ ModelStream      = fakeStream{}
	_ Tool             = fakeTool{}
	_ Sandbox          = fakeSandbox{}
	_ Store            = fakeStore{}
	_ EventSink        = fakeEventSink{}
	_ ExecutionBackend = fakeBackend{}
)

type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }
func (fakeProvider) Capabilities(ctx context.Context) (ModelCapabilities, error) {
	return ModelCapabilities{}, ctx.Err()
}
func (fakeProvider) Stream(ctx context.Context, _ ModelRequest) (ModelStream, error) {
	return fakeStream{}, ctx.Err()
}

type fakeStream struct{}

func (fakeStream) Next(context.Context) (ModelOutput, error) { return ModelOutput{}, io.EOF }
func (fakeStream) Close() error                              { return nil }

type fakeTool struct{}

func (fakeTool) Definition() ToolDefinition { return ToolDefinition{Name: "fake"} }
func (fakeTool) Execute(ctx context.Context, _ ToolInvocation) (ToolExecutionResult, error) {
	return ToolExecutionResult{}, ctx.Err()
}

type fakeSandbox struct{}

func (fakeSandbox) ReadFile(ctx context.Context, _ string, _ int64) ([]byte, error) {
	return nil, ctx.Err()
}
func (fakeSandbox) WriteFile(ctx context.Context, _ string, _ []byte, _ fs.FileMode) error {
	return ctx.Err()
}
func (fakeSandbox) Run(ctx context.Context, _ CommandRequest) (CommandResult, error) {
	return CommandResult{}, ctx.Err()
}

type fakeStore struct{}

func (fakeStore) CreateProject(ctx context.Context, _ Project) error { return ctx.Err() }
func (fakeStore) CreateTask(ctx context.Context, _ Task) error       { return ctx.Err() }
func (fakeStore) CreateRun(ctx context.Context, _ Run) error         { return ctx.Err() }
func (fakeStore) LoadTask(ctx context.Context, _ TaskID) (TaskSnapshot, error) {
	return TaskSnapshot{}, ctx.Err()
}
func (fakeStore) LoadRun(ctx context.Context, _ RunID) (Run, error) {
	return Run{}, ctx.Err()
}
func (fakeStore) LoadItems(ctx context.Context, _ TaskID, _ uint64) ([]RuntimeItem, error) {
	return nil, ctx.Err()
}
func (fakeStore) LoadEvents(ctx context.Context, _ TaskID, _ uint64) ([]Event, error) {
	return nil, ctx.Err()
}
func (fakeStore) Commit(ctx context.Context, _ CommitRequest) (CommittedBatch, error) {
	return CommittedBatch{}, ctx.Err()
}

type fakeEventSink struct{}

func (fakeEventSink) Publish(ctx context.Context, _ CommittedBatch) error { return ctx.Err() }

type fakeBackend struct{}

func (fakeBackend) RunStage(
	ctx context.Context,
	_ StageRequest,
	_ Store,
) (StageResult, error) {
	return StageResult{}, ctx.Err()
}
