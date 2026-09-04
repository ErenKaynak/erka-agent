package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/routing"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestSessionRunsConfiguredPipelineAcrossProviders(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	planner := &scriptedProvider{name: "openai", responses: [][]erkaruntime.ModelOutput{
		{{Kind: erkaruntime.ModelTextDelta, Text: "Inspect add.go and correct the arithmetic."}, {Kind: erkaruntime.ModelCompleted}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "Inspect the follow-up request."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	coder := &scriptedProvider{name: "deepseek", responses: [][]erkaruntime.ModelOutput{
		{{Kind: erkaruntime.ModelTextDelta, Text: "Implemented the approved arithmetic correction."}, {Kind: erkaruntime.ModelCompleted}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "Implemented the follow-up."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	resolved := testSessionPipeline()
	session, err := OpenSession(context.Background(), SessionOptions{
		WorkingDirectory: root,
		StateDir:         state,
		Pipeline:         resolved,
		ProviderFactory: func(_ context.Context, candidate routing.Candidate) (erkaruntime.ModelProvider, error) {
			switch candidate.Provider {
			case "openai":
				return planner, nil
			case "deepseek":
				return coder, nil
			default:
				return nil, errors.New("unexpected provider")
			}
		},
		IDs:   &sequenceIDs{},
		Clock: fixedClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.SetModel(erkaruntime.ModelSelection{Stage: erkaruntime.StagePlanning, Connection: "planner", Model: "gpt-selected"}); err != nil {
		t.Fatal(err)
	}
	if err := session.SetModel(erkaruntime.ModelSelection{Stage: erkaruntime.StageCoding, Connection: "coder", Model: "deepseek-selected"}); err != nil {
		t.Fatal(err)
	}
	taskID, err := session.Submit(context.Background(), "", "fix arithmetic")
	if err != nil {
		t.Fatal(err)
	}
	result, ok, err := session.RunNext(context.Background())
	if err != nil || !ok || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("RunNext() = %#v, %v, %v", result, ok, err)
	}
	conversation, err := session.OpenConversation(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Task.Pipeline != "default" || len(conversation.Runs) != 2 {
		t.Fatalf("conversation = %#v", conversation)
	}
	if conversation.Runs[0].Stage != "planning" || conversation.Runs[0].Provider != "openai" || conversation.Runs[0].Model != "gpt-selected" || conversation.Runs[0].Status != erkaruntime.RunCompleted {
		t.Fatalf("planning run = %#v", conversation.Runs[0])
	}
	if conversation.Runs[1].Stage != "coding" || conversation.Runs[1].Provider != "deepseek" || conversation.Runs[1].Model != "deepseek-selected" || conversation.Runs[1].Status != erkaruntime.RunCompleted {
		t.Fatalf("coding run = %#v", conversation.Runs[1])
	}
	codingRequests := coder.Requests()
	if len(codingRequests) != 1 {
		t.Fatalf("coding requests = %#v", codingRequests)
	}
	var durablePlan, leakedPlannerMessage bool
	for _, item := range codingRequests[0].Items {
		if item.Kind == erkaruntime.ItemCheckpoint && item.Checkpoint.ApprovedPlan == "Inspect add.go and correct the arithmetic." {
			durablePlan = true
		}
		if item.Kind == erkaruntime.ItemAssistantMessage && item.Message.Content == "Inspect add.go and correct the arithmetic." {
			leakedPlannerMessage = true
		}
	}
	if !durablePlan || leakedPlannerMessage {
		t.Fatalf("coding context durablePlan=%v leakedPlannerMessage=%v: %#v", durablePlan, leakedPlannerMessage, codingRequests[0].Items)
	}
	if _, err := session.Submit(context.Background(), taskID, "add a follow-up comment"); err != nil {
		t.Fatal(err)
	}
	if followUp, claimed, err := session.RunNext(context.Background()); err != nil || !claimed || followUp.Status != erkaruntime.TaskCompleted {
		t.Fatalf("follow-up RunNext() = %#v, %v, %v", followUp, claimed, err)
	}
	conversation, err = session.OpenConversation(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversation.Runs) != 4 {
		t.Fatalf("follow-up runs = %#v", conversation.Runs)
	}
	for _, run := range conversation.Runs {
		if run.Attempt != 1 {
			t.Fatalf("attempt leaked across turns: %#v", conversation.Runs)
		}
	}
	codingRequests = coder.Requests()
	if len(codingRequests) != 2 || codingRequests[1].Items[0].Message.Content != "add a follow-up comment" {
		t.Fatalf("follow-up coding context = %#v", codingRequests)
	}
}

func TestStageToolsOnlyAllowMutationInCodingAndRepair(t *testing.T) {
	all := []erkaruntime.Tool{
		permissionTool{definition: erkaruntime.ToolDefinition{Name: "read", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`), OutputLimitBytes: 1}},
		permissionTool{definition: erkaruntime.ToolDefinition{Name: "write", Description: "write", InputSchema: json.RawMessage(`{"type":"object"}`), Mutating: true, OutputLimitBytes: 1}},
	}
	for _, stage := range []routing.Stage{routing.StagePlanning, routing.StageValidation, routing.StageReview} {
		if got := stageTools(all, stage); len(got) != 1 || got[0].Definition().Mutating {
			t.Fatalf("stageTools(%s) = %#v", stage, got)
		}
	}
	for _, stage := range []routing.Stage{routing.StageCoding, routing.StageRepair} {
		if got := stageTools(all, stage); len(got) != 2 {
			t.Fatalf("stageTools(%s) = %#v", stage, got)
		}
	}
}

func testSessionPipeline() config.Resolved {
	return config.Resolved{
		ActivePipeline: "default",
		Connections: map[string]config.Connection{
			"planner": {Provider: "openai", BaseURL: "https://planner.example"},
			"coder":   {Provider: "deepseek", BaseURL: "https://coder.example"},
		},
		Pipelines: map[string]config.Pipeline{
			"default": {Routing: "fixed", Stages: map[string]config.StageRoute{
				"planning": {Candidates: []string{"planner/gpt-test"}},
				"coding":   {Candidates: []string{"coder/deepseek-test"}},
			}},
		},
	}
}

type permissionTool struct{ definition erkaruntime.ToolDefinition }

func (tool permissionTool) Definition() erkaruntime.ToolDefinition { return tool.definition }
func (permissionTool) Execute(context.Context, erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	return erkaruntime.ToolExecutionResult{}, nil
}

func TestSessionRunsQueuedFollowUpsWithConversationHistory(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	provider := &scriptedProvider{name: "fixture", responses: [][]erkaruntime.ModelOutput{
		{{Kind: erkaruntime.ModelTextDelta, Text: "first answer"}, {Kind: erkaruntime.ModelCompleted}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "second answer"}, {Kind: erkaruntime.ModelCompleted}},
	}}
	session, err := OpenSession(context.Background(), SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, IDs: &sequenceIDs{}, Clock: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	taskID, err := session.Submit(context.Background(), "", "first prompt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Submit(context.Background(), taskID, "second prompt"); err != nil {
		t.Fatal(err)
	}
	first, ok, err := session.RunNext(context.Background())
	if err != nil || !ok || first.Status != erkaruntime.TaskCompleted {
		t.Fatalf("first = %#v, %v, %v", first, ok, err)
	}
	second, ok, err := session.RunNext(context.Background())
	if err != nil || !ok || second.Status != erkaruntime.TaskCompleted || second.RunID == first.RunID {
		t.Fatalf("second = %#v, %v, %v", second, ok, err)
	}
	conversation, err := session.OpenConversation(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversation.Turns) != 2 || len(conversation.Runs) != 2 {
		t.Fatalf("conversation = %#v", conversation)
	}
	requests := provider.Requests()
	if len(requests) != 2 || len(requests[1].Items) < 3 || requests[1].Items[len(requests[1].Items)-1].Message.Content != "second prompt" {
		t.Fatalf("second request history = %#v", requests)
	}
}

func TestSessionCancellationIsDurable(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	provider := &blockingProvider{started: make(chan struct{})}
	session, err := OpenSession(context.Background(), SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, IDs: &sequenceIDs{}, Clock: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	taskID, err := session.Submit(context.Background(), "", "wait")
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, _, runErr := session.RunNext(context.Background()); finished <- runErr }()
	<-provider.started
	if err := session.SetModel(erkaruntime.ModelSelection{Stage: erkaruntime.StageCoding, Connection: "default", Model: "another-model"}); err == nil || !strings.Contains(err.Error(), "active turn") {
		t.Fatalf("SetModel() during active turn error = %v", err)
	}
	session.Cancel()
	if err := <-finished; err == nil {
		t.Fatal("cancelled run returned nil")
	}
	conversation, err := session.OpenConversation(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Task.Status != erkaruntime.TaskCancelled || conversation.Runs[0].Status != erkaruntime.RunCancelled || conversation.Turns[0].Status != store.TurnCancelled {
		t.Fatalf("cancelled conversation = %#v", conversation)
	}
}

func TestSessionResumesTurnClaimedBeforeRestart(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	first, err := OpenSession(context.Background(), SessionOptions{WorkingDirectory: root, StateDir: state, Provider: &scriptedProvider{name: "fixture"}, IDs: &sequenceIDs{}, Clock: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := first.Submit(context.Background(), "", "resume claimed")
	if err != nil {
		t.Fatal(err)
	}
	now := fixedClock()
	turn, ok, err := first.database.ClaimNextTurn(context.Background(), first.project, "", erkaruntime.Run{ID: "run-claimed", Stage: "coding", BackendClass: "native", Provider: "fixture", Model: "fixture", Status: erkaruntime.RunQueued, CreatedAt: now, UpdatedAt: now}, now)
	if err != nil || !ok || turn.Status != store.TurnClaimed {
		t.Fatalf("claim = %#v, %v, %v", turn, ok, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{name: "fixture", responses: [][]erkaruntime.ModelOutput{{{Kind: erkaruntime.ModelTextDelta, Text: "restored"}, {Kind: erkaruntime.ModelCompleted}}}}
	second, err := OpenSession(context.Background(), SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, IDs: &sequenceIDs{}, Clock: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	result, err := second.Resume(context.Background(), taskID)
	if err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("resume = %#v, %v", result, err)
	}
	conversation, err := second.OpenConversation(context.Background(), taskID)
	if err != nil || conversation.Turns[0].Status != store.TurnCompleted {
		t.Fatalf("conversation = %#v, %v", conversation, err)
	}
}

func TestSessionResolvesUnknownMutationBeforeResume(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	taskID := seedPendingCall(t, root, state, erkaruntime.ToolCall{CallID: "pending-patch", Tool: "apply_patch", Arguments: json.RawMessage(`{"patch":"--- a/add.go\\n+++ b/add.go\\n"}`), Mutating: true, IdempotencyKey: "task/run/pending-patch"})
	_, err := RunLocal(context.Background(), LocalOptions{WorkingDirectory: root, StateDir: state, ResumeTaskID: taskID, Provider: &scriptedProvider{name: "fixture"}, IDs: &sequenceIDs{}, Clock: fixedClock})
	if err == nil {
		t.Fatal("unknown mutation did not pause")
	}
	provider := &scriptedProvider{name: "fixture", responses: [][]erkaruntime.ModelOutput{{{Kind: erkaruntime.ModelTextDelta, Text: "recovered"}, {Kind: erkaruntime.ModelCompleted}}}}
	session, err := OpenSession(context.Background(), SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, IDs: &sequenceIDs{}, Clock: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call, ok, err := session.UnknownOutcome(context.Background(), taskID)
	if err != nil || !ok || call.CallID != "pending-patch" {
		t.Fatalf("unknown = %#v, %v, %v", call, ok, err)
	}
	if err := session.ResolveUnknown(context.Background(), taskID, true); err != nil {
		t.Fatal(err)
	}
	result, err := session.Resume(context.Background(), taskID)
	if err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("resume = %#v, %v", result, err)
	}
	if requests := provider.Requests(); len(requests) != 1 || !hasResult(requests[0].Items, "pending-patch") {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestSecondSessionIsReadOnlyWhileLeaseIsHeld(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	options := SessionOptions{WorkingDirectory: filepath.Join(root, "nested"), StateDir: state, IDs: &sequenceIDs{}, Clock: fixedClock}
	first, err := OpenSession(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenSession(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.ReadOnly() || !second.ReadOnly() {
		t.Fatalf("read-only state = %v/%v", first.ReadOnly(), second.ReadOnly())
	}
}

type blockingProvider struct {
	once    sync.Once
	started chan struct{}
}

func (*blockingProvider) Name() string { return "blocking" }
func (*blockingProvider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true}, nil
}
func (provider *blockingProvider) Stream(context.Context, erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	return &blockingStream{provider: provider}, nil
}

type blockingStream struct{ provider *blockingProvider }

func (stream *blockingStream) Next(ctx context.Context) (erkaruntime.ModelOutput, error) {
	stream.provider.once.Do(func() { close(stream.provider.started) })
	<-ctx.Done()
	return erkaruntime.ModelOutput{}, ctx.Err()
}
func (*blockingStream) Close() error { return nil }

var _ erkaruntime.ModelProvider = (*blockingProvider)(nil)
var _ erkaruntime.ModelStream = (*blockingStream)(nil)

func TestSetModelOverridesNativeModelInPipeline(t *testing.T) {
	session := &Session{options: SessionOptions{Pipeline: config.Resolved{ActivePipeline: "default"}}}
	session.options.Pipeline.Connections = map[string]config.Connection{"coder": {Provider: "deepseek"}}
	session.options.Pipeline.Pipelines = map[string]config.Pipeline{"default": {Stages: map[string]config.StageRoute{"coding": {Candidates: []string{"coder/deepseek-v4-flash"}}}}}
	if err := session.SetModel(erkaruntime.ModelSelection{Stage: erkaruntime.StageCoding, Connection: "coder", Model: "deepseek-v4-pro"}); err != nil {
		t.Fatalf("SetModel() error = %v", err)
	}
	if session.modelOverrides[erkaruntime.StageCoding] != "coder/deepseek-v4-pro" {
		t.Fatalf("model override = %q, want coder/deepseek-v4-pro", session.modelOverrides[erkaruntime.StageCoding])
	}
	if err := session.SetModel(erkaruntime.ModelSelection{Stage: erkaruntime.StageCoding, Connection: "coder", Model: "  "}); err == nil {
		t.Fatal("SetModel() accepted an empty model")
	}
}

func TestSetModelRecordsSelectedModelInSingleProviderRun(t *testing.T) {
	session := &Session{options: SessionOptions{
		Provider:    &scriptedProvider{name: "deepseek"},
		NativeModel: "deepseek-v4-flash",
		IDs:         &sequenceIDs{},
		RebuildProvider: func(model string) (erkaruntime.ModelProvider, error) {
			return &scriptedProvider{name: "deepseek"}, nil
		},
	}}
	if err := session.SetModel(erkaruntime.ModelSelection{Stage: erkaruntime.StageCoding, Connection: "default", Model: "deepseek-v4-pro"}); err != nil {
		t.Fatal(err)
	}
	run, err := session.newInitialRun(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if run.Provider != "deepseek" || run.Model != "deepseek-v4-pro" {
		t.Fatalf("run = %#v", run)
	}
}

func TestSetModelOverridesStagesBeforeRunCreationAndPreservesFallbacks(t *testing.T) {
	resolved := config.Resolved{
		ActivePipeline: "default",
		Connections: map[string]config.Connection{
			"chatgpt": {Class: config.ConnectionSubscriptionBridge, Provider: "codex"},
			"coder":   {Class: config.ConnectionNative, Provider: "deepseek"},
		},
		Pipelines: map[string]config.Pipeline{"default": {Stages: map[string]config.StageRoute{
			"planning": {Candidates: []string{"chatgpt/default"}},
			"coding":   {Candidates: []string{"coder/deepseek-v4-flash", "coder/deepseek-v4-pro"}},
		}}},
	}
	session := &Session{options: SessionOptions{Pipeline: resolved, IDs: &sequenceIDs{}}}
	if err := session.SetModel(erkaruntime.ModelSelection{Stage: erkaruntime.StagePlanning, Connection: "chatgpt", Model: "gpt-5.6-sol"}); err != nil {
		t.Fatal(err)
	}
	if err := session.SetModel(erkaruntime.ModelSelection{Stage: erkaruntime.StageCoding, Connection: "coder", Model: "deepseek-v4-pro"}); err != nil {
		t.Fatal(err)
	}
	effective := session.effectivePipeline()
	planning := effective.Pipelines["default"].Stages["planning"].Candidates
	coding := effective.Pipelines["default"].Stages["coding"].Candidates
	if len(planning) != 2 || planning[0] != "chatgpt/gpt-5.6-sol" || planning[1] != "chatgpt/default" {
		t.Fatalf("planning route = %#v", planning)
	}
	if len(coding) != 2 || coding[0] != "coder/deepseek-v4-pro" || coding[1] != "coder/deepseek-v4-flash" {
		t.Fatalf("coding route = %#v", coding)
	}
	if got := resolved.Pipelines["default"].Stages["planning"].Candidates[0]; got != "chatgpt/default" {
		t.Fatalf("configured pipeline was mutated: %q", got)
	}
	run, err := session.newInitialRun(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if run.Model != "gpt-5.6-sol" || run.Connection != "chatgpt" || run.BackendClass != config.ConnectionSubscriptionBridge {
		t.Fatalf("initial run = %#v", run)
	}
	if err := session.SetModel(erkaruntime.ModelSelection{Stage: erkaruntime.StageCoding, Connection: "coder", Model: "deepseek-v4-flash"}); err != nil {
		t.Fatal(err)
	}
	if _, exists := session.modelOverrides[erkaruntime.StageCoding]; exists {
		t.Fatalf("configured selection did not clear override: %#v", session.modelOverrides)
	}
}

func TestSetModelPreservesOpenCodeProviderQualifiedModel(t *testing.T) {
	session := &Session{options: SessionOptions{Pipeline: config.Resolved{
		ActivePipeline: "default",
		Connections:    map[string]config.Connection{"open": {Class: config.ConnectionSubscriptionBridge, Provider: "opencode"}},
		Pipelines: map[string]config.Pipeline{"default": {Stages: map[string]config.StageRoute{
			"planning": {Candidates: []string{"open/default"}},
		}}},
	}}}
	if err := session.SetModel(erkaruntime.ModelSelection{Stage: erkaruntime.StagePlanning, Connection: "open", Model: "openai/gpt-5.6-sol"}); err != nil {
		t.Fatal(err)
	}
	route, err := routing.ResolveFixed(session.effectivePipeline(), erkaruntime.StagePlanning, erkaruntime.DefaultNativeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if route.Candidates[0].Connection != "open" || route.Candidates[0].Model != "openai/gpt-5.6-sol" {
		t.Fatalf("candidate = %#v", route.Candidates[0])
	}
}
