package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestNativeBackendPersistsToolLoopInOrder(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{
		{
			toolDelta("call-1", "read_file", `{"pa`),
			toolDelta("call-1", "", `th":"x"}`),
			{Kind: erkaruntime.ModelUsage, Usage: &erkaruntime.Usage{InputTokens: 10, OutputTokens: 3}},
			{Kind: erkaruntime.ModelCompleted},
		},
		{
			{Kind: erkaruntime.ModelTextDelta, Text: "Change complete."},
			{Kind: erkaruntime.ModelCompleted},
		},
	}}
	tool := &fakeTool{name: "read_file", output: "contents"}
	sink := &recordingSink{}
	backend := testBackend(provider, sink, now, tool)

	result, err := backend.RunStage(context.Background(), request, memory)
	if err != nil {
		t.Fatalf("RunStage() error = %v", err)
	}
	if result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("RunStage() status = %q", result.Status)
	}
	run, err := memory.LoadRun(context.Background(), request.RunID)
	if err != nil || run.Status != erkaruntime.RunCompleted {
		t.Fatalf("LoadRun() = %#v, %v", run, err)
	}
	if tool.calls != 1 {
		t.Fatalf("tool calls = %d, want 1", tool.calls)
	}

	items, err := memory.LoadItems(context.Background(), request.TaskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []erkaruntime.ItemKind{
		erkaruntime.ItemUserMessage,
		erkaruntime.ItemToolCall,
		erkaruntime.ItemUsage,
		erkaruntime.ItemToolResult,
		erkaruntime.ItemAssistantMessage,
		erkaruntime.ItemCheckpoint,
	}
	if len(items) != len(wantKinds) {
		t.Fatalf("item count = %d, want %d: %#v", len(items), len(wantKinds), items)
	}
	for index, want := range wantKinds {
		if items[index].Kind != want || items[index].Sequence != uint64(index+1) {
			t.Fatalf("item %d = %#v, want kind %q", index, items[index], want)
		}
	}
	if items[3].ToolResult.CallID != items[1].ToolCall.CallID {
		t.Fatalf("tool result call ID = %q, call = %q", items[3].ToolResult.CallID, items[1].ToolCall.CallID)
	}

	requests := provider.Requests()
	if len(requests) != 2 || !containsToolResult(requests[1].Items, "call-1") {
		t.Fatalf("second model request did not contain durable tool result: %#v", requests)
	}
	events, err := memory.LoadEvents(context.Background(), request.TaskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{
		"task.started", "stage.started", "tool.started", "model.usage", "tool.completed",
		"model.completed", "task.validating", "task.completed",
	}
	if len(events) != len(wantEvents) {
		t.Fatalf("event count = %d, want %d: %#v", len(events), len(wantEvents), events)
	}
	for index, want := range wantEvents {
		if events[index].Type != want || events[index].Sequence != uint64(index+1) {
			t.Fatalf("event %d = %#v, want type %q", index, events[index], want)
		}
	}
	if sink.Count() != len(events) {
		t.Fatalf("published event count = %d, want %d", sink.Count(), len(events))
	}
	var usageData struct {
		InputTokens     int64 `json:"input_tokens"`
		OutputTokens    int64 `json:"output_tokens"`
		CacheReadTokens int64 `json:"cache_read_tokens"`
		CacheHit        bool  `json:"cache_hit"`
	}
	if err := json.Unmarshal(events[3].Data, &usageData); err != nil || usageData.InputTokens != 10 || usageData.OutputTokens != 3 || usageData.CacheHit {
		t.Fatalf("model.usage data = %s, %v", events[3].Data, err)
	}
}

func TestNativeBackendPublishesEphemeralUpdatesBeforeDurableCommitNotice(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "live "},
		{Kind: erkaruntime.ModelTextDelta, Text: "answer"},
		{Kind: erkaruntime.ModelCompleted},
	}}}
	backend := testBackend(provider, nil, now)
	var updates []erkaruntime.LiveUpdate
	backend.Observer = erkaruntime.LiveObserverFunc(func(_ context.Context, update erkaruntime.LiveUpdate) {
		updates = append(updates, update)
	})
	if _, err := backend.RunStage(context.Background(), request, memory); err != nil {
		t.Fatal(err)
	}
	if len(updates) < 3 || updates[0].Kind != erkaruntime.LiveTextDelta || updates[0].Text != "live " || updates[1].Text != "answer" || updates[2].Kind != erkaruntime.LiveCommitted {
		t.Fatalf("updates = %#v", updates)
	}
	items, err := memory.LoadItems(context.Background(), request.TaskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if items[1].Kind != erkaruntime.ItemAssistantMessage || items[1].Message.Content != "live answer" {
		t.Fatalf("durable assistant item = %#v", items[1])
	}
}

func TestNativeBackendRejectsMalformedToolArguments(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{{
		toolDelta("call-1", "read_file", `{"path":`),
		{Kind: erkaruntime.ModelCompleted},
	}}}
	backend := testBackend(provider, nil, now, &fakeTool{name: "read_file", output: "unused"})

	if _, err := backend.RunStage(context.Background(), request, memory); err == nil {
		t.Fatal("RunStage() error = nil, want malformed argument error")
	}
	snapshot, err := memory.LoadTask(context.Background(), request.TaskID)
	if err != nil || snapshot.Status != erkaruntime.TaskFailed {
		t.Fatalf("LoadTask() = %#v, %v", snapshot, err)
	}
	items, _ := memory.LoadItems(context.Background(), request.TaskID, 0)
	if len(items) != 2 || items[1].Kind != erkaruntime.ItemError {
		t.Fatalf("items = %#v, want user message and durable error", items)
	}
}

func TestNativeBackendReturnsSchemaViolationToModel(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{
		{toolDelta("call-1", "read_file", `{"wrong":true}`), {Kind: erkaruntime.ModelCompleted}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "Corrected after tool error."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	tool := &fakeTool{
		name: "read_file", output: "unused",
		schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
	}
	backend := testBackend(provider, nil, now, tool)

	if _, err := backend.RunStage(context.Background(), request, memory); err != nil {
		t.Fatal(err)
	}
	if tool.calls != 0 {
		t.Fatalf("tool calls = %d, want schema rejection before execution", tool.calls)
	}
	items, _ := memory.LoadItems(context.Background(), request.TaskID, 0)
	for _, item := range items {
		if item.Kind == erkaruntime.ItemToolResult {
			if !item.ToolResult.IsError {
				t.Fatalf("tool result = %#v, want error", item.ToolResult)
			}
			return
		}
	}
	t.Fatal("schema-error tool result not found")
}

func TestNativeBackendStopsAtMaximumIterations(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{
		{toolDelta("call-1", "read_file", `{}`), {Kind: erkaruntime.ModelCompleted}},
		{toolDelta("call-2", "read_file", `{}`), {Kind: erkaruntime.ModelCompleted}},
	}}
	tool := &fakeTool{name: "read_file", output: "ok"}
	backend := testBackend(provider, nil, now, tool)
	backend.Limits.MaxIterations = 2

	_, err := backend.RunStage(context.Background(), request, memory)
	if !errors.Is(err, erkaruntime.ErrMaximumIterations) {
		t.Fatalf("RunStage() error = %v, want ErrMaximumIterations", err)
	}
	if tool.calls != 2 {
		t.Fatalf("tool calls = %d, want 2", tool.calls)
	}
	snapshot, _ := memory.LoadTask(context.Background(), request.TaskID)
	if snapshot.Status != erkaruntime.TaskFailed {
		t.Fatalf("task status = %q, want failed", snapshot.Status)
	}
}

func TestNativeBackendPropagatesCancellation(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := blockingProvider{started: make(chan struct{})}
	backend := testBackend(provider, nil, now)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := backend.RunStage(ctx, request, memory)
		result <- err
	}()
	select {
	case <-provider.started:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("provider stream did not start")
	}
	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunStage() error = %v, want context.Canceled", err)
	}
}

func TestNativeBackendRecoversIncompleteReadOnlyTool(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	seedIncompleteCall(t, memory, request, now, false)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "Recovered."},
		{Kind: erkaruntime.ModelCompleted},
	}}}
	tool := &fakeTool{name: "read_file", output: "replayed"}
	backend := testBackend(provider, nil, now, tool)

	result, err := backend.RunStage(context.Background(), request, memory)
	if err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("RunStage() = %#v, %v", result, err)
	}
	if tool.calls != 1 {
		t.Fatalf("tool calls = %d, want replay once", tool.calls)
	}
	requests := provider.Requests()
	if len(requests) != 1 || !containsToolResult(requests[0].Items, "call-pending") {
		t.Fatalf("model request missing recovered result: %#v", requests)
	}
}

func TestNativeBackendPausesUnknownMutatingOutcome(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	seedIncompleteCall(t, memory, request, now, true)
	provider := &scriptedProvider{}
	tool := &fakeTool{name: "apply_patch", mutating: true, output: "unused"}
	backend := testBackend(provider, nil, now, tool)

	result, err := backend.RunStage(context.Background(), request, memory)
	if !errors.Is(err, erkaruntime.ErrUnknownMutatingToolOutcome) {
		t.Fatalf("RunStage() error = %v", err)
	}
	if result.Status != erkaruntime.TaskWaiting || tool.calls != 0 || len(provider.Requests()) != 0 {
		t.Fatalf("RunStage() result = %#v, tool calls = %d, requests = %d", result, tool.calls, len(provider.Requests()))
	}
	snapshot, _ := memory.LoadTask(context.Background(), request.TaskID)
	if snapshot.Status != erkaruntime.TaskWaiting {
		t.Fatalf("task status = %q, want waiting", snapshot.Status)
	}
	run, _ := memory.LoadRun(context.Background(), request.RunID)
	if run.Status != erkaruntime.RunWaiting {
		t.Fatalf("run status = %q, want waiting", run.Status)
	}
}

func TestNativeBackendCapsToolOutput(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{
		{toolDelta("call-1", "read_file", `{}`), {Kind: erkaruntime.ModelCompleted}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "Done."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	backend := testBackend(provider, nil, now, &fakeTool{name: "read_file", output: "123456"})
	backend.Limits.MaxCumulativeToolOutputBytes = 4

	if _, err := backend.RunStage(context.Background(), request, memory); err != nil {
		t.Fatal(err)
	}
	items, _ := memory.LoadItems(context.Background(), request.TaskID, 0)
	for _, item := range items {
		if item.Kind == erkaruntime.ItemToolResult {
			if item.ToolResult.Output != "1234" || !item.ToolResult.Truncated {
				t.Fatalf("tool result = %#v", item.ToolResult)
			}
			return
		}
	}
	t.Fatal("tool result not found")
}

func TestNativeBackendRejectsPrematureEOF(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "partial"},
	}}}
	backend := testBackend(provider, nil, now)

	_, err := backend.RunStage(context.Background(), request, memory)
	if !errors.Is(err, erkaruntime.ErrMalformedStream) {
		t.Fatalf("RunStage() error = %v, want ErrMalformedStream", err)
	}
}

func seededRuntime(t *testing.T) (*store.Memory, erkaruntime.StageRequest, time.Time) {
	t.Helper()
	ctx := context.Background()
	memory := store.NewMemory()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
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
	return memory, erkaruntime.StageRequest{TaskID: task.ID, RunID: run.ID, Objective: task.Objective}, now
}

func seedIncompleteCall(t *testing.T, memory *store.Memory, request erkaruntime.StageRequest, now time.Time, mutating bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := memory.Commit(ctx, erkaruntime.CommitRequest{
		TaskID: request.TaskID,
		Transition: &erkaruntime.StateTransition{
			TaskID: request.TaskID, RunID: request.RunID,
			From: erkaruntime.TaskQueued, To: erkaruntime.TaskPreparing,
		},
	}); err != nil {
		t.Fatal(err)
	}
	toolName := "read_file"
	idempotency := ""
	if mutating {
		toolName = "apply_patch"
		idempotency = "task-1/run-1/call-pending"
	}
	items := []erkaruntime.RuntimeItem{
		{
			ID: "item-1", Sequence: 1, TaskID: request.TaskID, RunID: request.RunID,
			Kind: erkaruntime.ItemUserMessage, Time: now,
			Message: &erkaruntime.Message{Content: request.Objective},
		},
		{
			ID: "item-2", Sequence: 2, TaskID: request.TaskID, RunID: request.RunID,
			Kind: erkaruntime.ItemToolCall, Time: now,
			ToolCall: &erkaruntime.ToolCall{
				CallID: "call-pending", Tool: toolName, Arguments: json.RawMessage(`{}`),
				Mutating: mutating, IdempotencyKey: idempotency,
			},
		},
	}
	if _, err := memory.Commit(ctx, erkaruntime.CommitRequest{
		TaskID: request.TaskID, Items: items,
		Transition: &erkaruntime.StateTransition{
			TaskID: request.TaskID, RunID: request.RunID,
			From: erkaruntime.TaskPreparing, To: erkaruntime.TaskCoding,
		},
		RunTransition: &erkaruntime.RunStateTransition{
			TaskID: request.TaskID, RunID: request.RunID,
			From: erkaruntime.RunQueued, To: erkaruntime.RunRunning,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func testBackend(provider erkaruntime.ModelProvider, sink erkaruntime.EventSink, now time.Time, tools ...erkaruntime.Tool) *erkaruntime.NativeBackend {
	counter := 0
	return &erkaruntime.NativeBackend{
		Provider: provider, Tools: tools, Sink: sink,
		Instructions: "Use the provided tools to complete the task.",
		Limits: erkaruntime.NativeLimits{
			MaxIterations: 4, MaxOutputTokens: 1024,
			MaxModelOutputBytes: 1024, MaxCumulativeToolOutputBytes: 1024,
			StageTimeout: time.Minute,
		},
		IDs: erkaruntime.IDGeneratorFunc(func(prefix string) (string, error) {
			counter++
			return fmt.Sprintf("generated-%s-%d", prefix, counter), nil
		}),
		Clock: func() time.Time { return now },
	}
}

func toolDelta(callID erkaruntime.ToolCallID, tool, arguments string) erkaruntime.ModelOutput {
	return erkaruntime.ModelOutput{
		Kind: erkaruntime.ModelToolCallDelta,
		ToolCall: &erkaruntime.ToolCallDelta{
			CallID: callID, Tool: tool, ArgumentsFragment: arguments,
		},
	}
}

func containsToolResult(items []erkaruntime.RuntimeItem, callID erkaruntime.ToolCallID) bool {
	for _, item := range items {
		if item.Kind == erkaruntime.ItemToolResult && item.ToolResult.CallID == callID {
			return true
		}
	}
	return false
}

type scriptedProvider struct {
	mu        sync.Mutex
	responses [][]erkaruntime.ModelOutput
	requests  []erkaruntime.ModelRequest
}

func (*scriptedProvider) Name() string { return "scripted" }

func (*scriptedProvider) Capabilities(ctx context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true}, ctx.Err()
}

func (provider *scriptedProvider) Stream(ctx context.Context, request erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.requests = append(provider.requests, copyRequest(request))
	index := len(provider.requests) - 1
	if index >= len(provider.responses) {
		return nil, fmt.Errorf("unexpected model request %d", index+1)
	}
	outputs := append([]erkaruntime.ModelOutput(nil), provider.responses[index]...)
	return &scriptedStream{outputs: outputs}, nil
}

func (provider *scriptedProvider) Requests() []erkaruntime.ModelRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	result := make([]erkaruntime.ModelRequest, len(provider.requests))
	copy(result, provider.requests)
	return result
}

func copyRequest(request erkaruntime.ModelRequest) erkaruntime.ModelRequest {
	request.Items = append([]erkaruntime.RuntimeItem(nil), request.Items...)
	request.Tools = append([]erkaruntime.ToolDefinition(nil), request.Tools...)
	return request
}

type scriptedStream struct {
	outputs []erkaruntime.ModelOutput
	index   int
}

func (stream *scriptedStream) Next(ctx context.Context) (erkaruntime.ModelOutput, error) {
	if err := ctx.Err(); err != nil {
		return erkaruntime.ModelOutput{}, err
	}
	if stream.index >= len(stream.outputs) {
		return erkaruntime.ModelOutput{}, io.EOF
	}
	output := stream.outputs[stream.index]
	stream.index++
	return output, nil
}

func (*scriptedStream) Close() error { return nil }

type blockingProvider struct {
	started chan struct{}
}

func (blockingProvider) Name() string { return "blocking" }
func (blockingProvider) Capabilities(ctx context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true}, ctx.Err()
}
func (provider blockingProvider) Stream(ctx context.Context, _ erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	return &blockingStream{started: provider.started}, ctx.Err()
}

type blockingStream struct {
	started chan struct{}
}

func (stream *blockingStream) Next(ctx context.Context) (erkaruntime.ModelOutput, error) {
	close(stream.started)
	<-ctx.Done()
	return erkaruntime.ModelOutput{}, ctx.Err()
}
func (*blockingStream) Close() error { return nil }

type fakeTool struct {
	name     string
	mutating bool
	output   string
	schema   json.RawMessage
	calls    int
}

func (tool *fakeTool) Definition() erkaruntime.ToolDefinition {
	schema := tool.schema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return erkaruntime.ToolDefinition{
		Name: tool.name, Description: "Fixture tool", InputSchema: schema,
		Mutating: tool.mutating, OutputLimitBytes: 1024,
	}
}

func (tool *fakeTool) Execute(ctx context.Context, _ erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return erkaruntime.ToolExecutionResult{}, err
	}
	tool.calls++
	return erkaruntime.ToolExecutionResult{Output: tool.output}, nil
}

type recordingSink struct {
	mu     sync.Mutex
	events []erkaruntime.Event
}

func (sink *recordingSink) Publish(ctx context.Context, batch erkaruntime.CommittedBatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, batch.Events...)
	return nil
}

func (sink *recordingSink) Count() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return len(sink.events)
}

func TestNativeBackendAppendsProjectInstructionsAfterStageInstructions(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "Done."}, {Kind: erkaruntime.ModelCompleted},
	}}}
	backend := testBackend(provider, &recordingSink{}, now)
	backend.ProjectInstructions = "<agents_md path=\"AGENTS.md\">Run go test.</agents_md>"
	if _, err := backend.RunStage(context.Background(), request, memory); err != nil {
		t.Fatal(err)
	}
	requests := provider.requests
	if len(requests) != 1 || !strings.HasPrefix(requests[0].Instructions, "Use the provided tools") || !strings.HasSuffix(requests[0].Instructions, "Run go test.</agents_md>") {
		t.Fatalf("instructions = %q", requests[0].Instructions)
	}
}
