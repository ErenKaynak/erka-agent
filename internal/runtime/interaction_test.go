package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestNativeBackendParksOnQuestionAndResumesPendingMutation(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{
		{
			toolDelta("call-ask", erkaruntime.AskUserToolName, `{"question":"Use tabs?"}`),
			toolDelta("call-patch", "apply_patch", `{}`),
			{Kind: erkaruntime.ModelCompleted},
		},
		{{Kind: erkaruntime.ModelTextDelta, Text: "Done."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	patch := &fakeTool{name: "apply_patch", mutating: true, output: "patched"}
	interactor := newFakeInteractor()
	backend := testBackend(provider, nil, now, patch)
	backend.Interactor = interactor

	result, err := backend.RunStage(context.Background(), request, memory)
	if !errors.Is(err, erkaruntime.ErrAwaitingUser) || result.Status != erkaruntime.TaskWaitingApproval {
		t.Fatalf("first RunStage() = %#v, %v; want waiting approval", result, err)
	}
	snapshot, _ := memory.LoadTask(context.Background(), request.TaskID)
	run, _ := memory.LoadRun(context.Background(), request.RunID)
	if snapshot.Status != erkaruntime.TaskWaitingApproval || run.Status != erkaruntime.RunWaiting || patch.calls != 0 {
		t.Fatalf("parked task = %q run = %q patch calls = %d", snapshot.Status, run.Status, patch.calls)
	}
	requests := interactor.Requests()
	if len(requests) != 1 || requests[0].Kind != erkaruntime.UserQuestion || requests[0].Message != "Use tabs?" {
		t.Fatalf("interactor requests = %#v", requests)
	}

	interactor.Answer(requests[0].Key, erkaruntime.UserResponse{Approved: true, Answer: "yes, tabs"})
	result, err = backend.RunStage(context.Background(), request, memory)
	if err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("resumed RunStage() = %#v, %v", result, err)
	}
	if patch.calls != 1 {
		t.Fatalf("patch calls = %d, want exactly one after resume", patch.calls)
	}
	items, _ := memory.LoadItems(context.Background(), request.TaskID, 0)
	if !hasToolOutput(items, "call-ask", "User answer: yes, tabs") || !hasToolOutput(items, "call-patch", "patched") {
		t.Fatalf("resumed items missing answer or patch result: %#v", items)
	}
}

func TestNativeBackendDeniedPermissionNeverExecutes(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{
		{toolDelta("call-run", "run_command", `{"command":["rm","-rf","build"]}`), {Kind: erkaruntime.ModelCompleted}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "Skipped."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	command := &fakeTool{name: "run_command", mutating: true, output: "ran"}
	interactor := newFakeInteractor()
	backend := testBackend(provider, nil, now, command)
	backend.Interactor, backend.ApprovalTools = interactor, []string{"run_command"}

	if _, err := backend.RunStage(context.Background(), request, memory); !errors.Is(err, erkaruntime.ErrAwaitingUser) {
		t.Fatalf("RunStage() error = %v, want awaiting user", err)
	}
	requests := interactor.Requests()
	if len(requests) != 1 || requests[0].Kind != erkaruntime.UserPermission || requests[0].Message != "Run command: rm -rf build" {
		t.Fatalf("permission requests = %#v", requests)
	}
	interactor.Answer(requests[0].Key, erkaruntime.UserResponse{Approved: false, Answer: "not on this branch"})
	if result, err := backend.RunStage(context.Background(), request, memory); err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("resumed RunStage() = %#v, %v", result, err)
	}
	items, _ := memory.LoadItems(context.Background(), request.TaskID, 0)
	if command.calls != 0 || !hasToolOutput(items, "call-run", "not on this branch") {
		t.Fatalf("command calls = %d, items = %#v", command.calls, items)
	}
}

func TestNativeBackendApprovedMutationStartedBeforeCrashIsNotReplayed(t *testing.T) {
	t.Parallel()
	memory, request, now := seededRuntime(t)
	provider := &scriptedProvider{responses: [][]erkaruntime.ModelOutput{
		{toolDelta("call-run", "run_command", `{"command":["make"]}`), {Kind: erkaruntime.ModelCompleted}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	command := &crashingTool{cancel: cancel}
	interactor := newFakeInteractor()
	backend := testBackend(provider, nil, now, command)
	backend.Interactor, backend.ApprovalTools = interactor, []string{"run_command"}

	if _, err := backend.RunStage(context.Background(), request, memory); !errors.Is(err, erkaruntime.ErrAwaitingUser) {
		t.Fatalf("first RunStage() error = %v, want awaiting user", err)
	}
	interactor.Answer(erkaruntime.UserRequestKey(request.TaskID, "call-run", erkaruntime.UserPermission), erkaruntime.UserResponse{Approved: true})
	if _, err := backend.RunStage(ctx, request, memory); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted RunStage() error = %v, want cancellation", err)
	}
	if command.calls != 1 {
		t.Fatalf("command calls = %d, want 1", command.calls)
	}
	_, err := backend.RunStage(context.Background(), request, memory)
	if !errors.Is(err, erkaruntime.ErrUnknownMutatingToolOutcome) {
		t.Fatalf("recovered RunStage() error = %v, want unknown mutating outcome", err)
	}
}

func hasToolOutput(items []erkaruntime.RuntimeItem, callID erkaruntime.ToolCallID, fragment string) bool {
	for _, item := range items {
		if item.Kind == erkaruntime.ItemToolResult && item.ToolResult.CallID == callID && strings.Contains(item.ToolResult.Output, fragment) {
			return true
		}
	}
	return false
}

// crashingTool simulates a process dying while a mutation is in flight.
type crashingTool struct {
	cancel context.CancelFunc
	calls  int
}

func (*crashingTool) Definition() erkaruntime.ToolDefinition {
	return (&fakeTool{name: "run_command", mutating: true}).Definition()
}

func (tool *crashingTool) Execute(context.Context, erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	tool.calls++
	tool.cancel()
	return erkaruntime.ToolExecutionResult{}, context.Canceled
}

type fakeInteractor struct {
	mu       sync.Mutex
	answers  map[string]erkaruntime.UserResponse
	requests []erkaruntime.UserRequest
}

func newFakeInteractor() *fakeInteractor {
	return &fakeInteractor{answers: make(map[string]erkaruntime.UserResponse)}
}

func (interactor *fakeInteractor) Request(_ context.Context, request erkaruntime.UserRequest) (erkaruntime.UserResponse, error) {
	if err := request.Validate(); err != nil {
		return erkaruntime.UserResponse{}, err
	}
	interactor.mu.Lock()
	defer interactor.mu.Unlock()
	interactor.requests = append(interactor.requests, request)
	response, ok := interactor.answers[request.Key]
	if !ok {
		return erkaruntime.UserResponse{}, erkaruntime.ErrAwaitingUser
	}
	return response, nil
}

func (interactor *fakeInteractor) Answer(key string, response erkaruntime.UserResponse) {
	interactor.mu.Lock()
	defer interactor.mu.Unlock()
	interactor.answers[key] = response
}

func (interactor *fakeInteractor) Requests() []erkaruntime.UserRequest {
	interactor.mu.Lock()
	defer interactor.mu.Unlock()
	return append([]erkaruntime.UserRequest(nil), interactor.requests...)
}
