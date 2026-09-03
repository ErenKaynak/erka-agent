package routing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestPipelineHandsDurablePlanToDifferentCodingProvider(t *testing.T) {
	t.Parallel()
	memory, initial, ids, now := seededPipeline(t)
	planner := &pipelineProvider{name: "openai", responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "Inspect checkout, then patch it."},
		{Kind: erkaruntime.ModelUsage, Usage: &erkaruntime.Usage{InputTokens: 10, OutputTokens: 5}},
		{Kind: erkaruntime.ModelCompleted},
	}}}
	coder := &pipelineProvider{name: "deepseek", responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "Implemented checkout fix."},
		{Kind: erkaruntime.ModelUsage, Usage: &erkaruntime.Usage{InputTokens: 8, OutputTokens: 4}},
		{Kind: erkaruntime.ModelCompleted},
	}}}
	engine := testPipelineEngine(memory, ids, now, pipelineConfig(), func(_ context.Context, candidate Candidate, stage Stage, limits erkaruntime.NativeLimits) (*erkaruntime.NativeBackend, error) {
		provider := coder
		if stage == StagePlanning {
			provider = planner
		}
		return &erkaruntime.NativeBackend{Provider: provider, Instructions: "pipeline", Limits: limits, IDs: ids, Clock: func() time.Time { return now }}, nil
	})

	result, err := engine.Run(context.Background(), PipelineRequest{TaskID: "task-1", Objective: "fix checkout", InitialRun: initial})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("result = %#v", result)
	}
	requests := coder.Requests()
	if len(requests) != 1 {
		t.Fatalf("coding requests = %#v", requests)
	}
	var hasPlan, leakedPlannerText bool
	for _, item := range requests[0].Items {
		if item.Kind == erkaruntime.ItemCheckpoint && item.Checkpoint.ApprovedPlan == "Inspect checkout, then patch it." {
			hasPlan = true
		}
		if item.Kind == erkaruntime.ItemAssistantMessage && item.Message.Content == "Inspect checkout, then patch it." {
			leakedPlannerText = true
		}
	}
	if !hasPlan || leakedPlannerText {
		t.Fatalf("coding context hasPlan=%v leakedPlannerText=%v: %#v", hasPlan, leakedPlannerText, requests[0].Items)
	}
	items, err := memory.LoadItems(context.Background(), "task-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := 0
	for _, item := range items {
		if item.Kind == erkaruntime.ItemCheckpoint {
			checkpoints++
		}
	}
	if checkpoints != 2 {
		t.Fatalf("durable checkpoints = %d, items = %#v", checkpoints, items)
	}
}

func TestPipelineFallsBackInOrderBeforeMutation(t *testing.T) {
	t.Parallel()
	memory, initial, ids, now := seededPipeline(t)
	resolved := pipelineConfig()
	plan := resolved.Pipelines["default"]
	plan.Stages["planning"] = config.StageRoute{Candidates: []string{"planner/p", "backup/p"}}
	resolved.Pipelines["default"] = plan
	resolved.Connections["backup"] = config.Connection{Provider: "deepseek", BaseURL: "https://backup.example", APIKeyEnv: "BACKUP_KEY"}
	backup := &pipelineProvider{name: "deepseek", responses: [][]erkaruntime.ModelOutput{{{Kind: erkaruntime.ModelTextDelta, Text: "Backup plan."}, {Kind: erkaruntime.ModelCompleted}}}}
	coder := &pipelineProvider{name: "deepseek", responses: [][]erkaruntime.ModelOutput{{{Kind: erkaruntime.ModelTextDelta, Text: "Done."}, {Kind: erkaruntime.ModelCompleted}}}}
	var opened []string
	engine := testPipelineEngine(memory, ids, now, resolved, func(_ context.Context, candidate Candidate, stage Stage, limits erkaruntime.NativeLimits) (*erkaruntime.NativeBackend, error) {
		opened = append(opened, candidate.Reference)
		if candidate.Reference == "planner/p" {
			return nil, &AttemptError{Kind: FailureRateLimit, Err: errors.New("rate limited")}
		}
		provider := backup
		if stage == StageCoding {
			provider = coder
		}
		return &erkaruntime.NativeBackend{Provider: provider, Instructions: "pipeline", Limits: limits, IDs: ids, Clock: func() time.Time { return now }}, nil
	})
	result, err := engine.Run(context.Background(), PipelineRequest{TaskID: "task-1", Objective: "fix checkout", InitialRun: initial})
	if err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if len(opened) < 2 || opened[0] != "planner/p" || opened[1] != "backup/p" {
		t.Fatalf("opened routes = %#v", opened)
	}
	first, err := memory.LoadRun(context.Background(), initial.ID)
	if err != nil || first.Status != erkaruntime.RunFailed || first.FailureCode != "provider_open_failed" {
		t.Fatalf("first attempt = %#v, %v", first, err)
	}
}

func TestPipelineRepairsFailedValidationAndRevalidates(t *testing.T) {
	t.Parallel()
	memory, initial, ids, now := seededPipeline(t)
	resolved := pipelineConfig()
	pipeline := resolved.Pipelines["default"]
	pipeline.MaxRepairAttempts = 1
	pipeline.Stages["validation"] = config.StageRoute{Candidates: []string{"coder/check"}}
	pipeline.Stages["repair"] = config.StageRoute{Candidates: []string{"coder/c"}}
	pipeline.Stages["review"] = config.StageRoute{Candidates: []string{"planner/review"}}
	resolved.Pipelines["default"] = pipeline
	responses := map[Stage][][]erkaruntime.ModelOutput{
		StagePlanning:   {{{Kind: erkaruntime.ModelTextDelta, Text: "Plan."}, {Kind: erkaruntime.ModelCompleted}}},
		StageCoding:     {{{Kind: erkaruntime.ModelTextDelta, Text: "Code."}, {Kind: erkaruntime.ModelCompleted}}},
		StageValidation: {{{Kind: erkaruntime.ModelTextDelta, Text: "FAIL tests"}, {Kind: erkaruntime.ModelCompleted}}, {{Kind: erkaruntime.ModelTextDelta, Text: "PASS tests"}, {Kind: erkaruntime.ModelCompleted}}},
		StageRepair:     {{{Kind: erkaruntime.ModelTextDelta, Text: "Repair."}, {Kind: erkaruntime.ModelCompleted}}},
		StageReview:     {{{Kind: erkaruntime.ModelTextDelta, Text: "Review passed."}, {Kind: erkaruntime.ModelCompleted}}},
	}
	providers := make(map[Stage]*pipelineProvider)
	engine := testPipelineEngine(memory, ids, now, resolved, func(_ context.Context, _ Candidate, stage Stage, limits erkaruntime.NativeLimits) (*erkaruntime.NativeBackend, error) {
		provider := providers[stage]
		if provider == nil {
			provider = &pipelineProvider{name: "fixture", responses: responses[stage]}
			providers[stage] = provider
		}
		return &erkaruntime.NativeBackend{Provider: provider, Instructions: "pipeline", Limits: limits, IDs: ids, Clock: func() time.Time { return now }}, nil
	})
	result, err := engine.Run(context.Background(), PipelineRequest{TaskID: "task-1", Objective: "fix checkout", InitialRun: initial})
	if err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if len(providers[StageValidation].Requests()) != 2 || len(providers[StageRepair].Requests()) != 1 || len(providers[StageReview].Requests()) != 1 {
		t.Fatalf("validation=%d repair=%d review=%d", len(providers[StageValidation].Requests()), len(providers[StageRepair].Requests()), len(providers[StageReview].Requests()))
	}
	reviewRun, err := memory.LoadRun(context.Background(), result.RunID)
	if err != nil || reviewRun.Stage != "review" || reviewRun.Status != erkaruntime.RunCompleted {
		t.Fatalf("review run = %#v, %v", reviewRun, err)
	}
	items, _ := memory.LoadItems(context.Background(), "task-1", 0)
	validationAttempts := make(map[erkaruntime.RunID]bool)
	for _, item := range items {
		if item.Kind == erkaruntime.ItemCheckpoint && len(item.Checkpoint.ValidationResults) > 0 {
			validationAttempts[item.RunID] = true
		}
	}
	if len(validationAttempts) != 2 {
		t.Fatalf("validation runs = %#v", validationAttempts)
	}
}

func TestPipelineResumeContinuesAfterCompletedPlanningRun(t *testing.T) {
	t.Parallel()
	memory, initial, ids, now := seededPipeline(t)
	planner := &pipelineProvider{name: "openai", responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "Durable restart plan."},
		{Kind: erkaruntime.ModelCompleted},
	}}}
	first := testPipelineEngine(memory, ids, now, pipelineConfig(), func(_ context.Context, _ Candidate, _ Stage, limits erkaruntime.NativeLimits) (*erkaruntime.NativeBackend, error) {
		return &erkaruntime.NativeBackend{Provider: planner, Instructions: "pipeline", Limits: limits, IDs: ids, Clock: func() time.Time { return now }}, nil
	})
	attempt, err := first.executeStage(context.Background(), PipelineRequest{TaskID: "task-1", Objective: "fix checkout", InitialRun: initial}, StagePlanning, &initial, map[Stage]int{})
	if err != nil {
		t.Fatal(err)
	}
	completedPlanning, err := memory.LoadRun(context.Background(), attempt.Run.ID)
	if err != nil || completedPlanning.Status != erkaruntime.RunCompleted {
		t.Fatalf("planning run = %#v, %v", completedPlanning, err)
	}

	coder := &pipelineProvider{name: "deepseek", responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "Resumed at coding."},
		{Kind: erkaruntime.ModelCompleted},
	}}}
	resumed := testPipelineEngine(memory, ids, now, pipelineConfig(), func(_ context.Context, _ Candidate, stage Stage, limits erkaruntime.NativeLimits) (*erkaruntime.NativeBackend, error) {
		if stage == StagePlanning {
			return nil, errors.New("completed planning stage was replayed")
		}
		return &erkaruntime.NativeBackend{Provider: coder, Instructions: "pipeline", Limits: limits, IDs: ids, Clock: func() time.Time { return now }}, nil
	})
	result, err := resumed.Run(context.Background(), PipelineRequest{TaskID: "task-1", Objective: "fix checkout", InitialRun: completedPlanning, Resume: true})
	if err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("Run(resume) = %#v, %v", result, err)
	}
	if len(planner.Requests()) != 1 || len(coder.Requests()) != 1 {
		t.Fatalf("planner requests=%d coder requests=%d", len(planner.Requests()), len(coder.Requests()))
	}
	codingRun, loadErr := memory.LoadRun(context.Background(), result.RunID)
	if loadErr != nil || codingRun.ID == completedPlanning.ID || codingRun.Stage != "coding" || codingRun.Status != erkaruntime.RunCompleted {
		t.Fatalf("coding run = %#v, %v", codingRun, loadErr)
	}
}

func pipelineConfig() config.Resolved {
	return config.Resolved{
		ActivePipeline: "default",
		Connections: map[string]config.Connection{
			"planner": {Provider: "openai", BaseURL: "https://planner.example", APIKeyEnv: "PLANNER_KEY"},
			"coder":   {Provider: "deepseek", BaseURL: "https://coder.example", APIKeyEnv: "CODER_KEY"},
		},
		Pipelines: map[string]config.Pipeline{"default": {Routing: "fixed", Stages: map[string]config.StageRoute{
			"planning": {Candidates: []string{"planner/p"}},
			"coding":   {Candidates: []string{"coder/c"}},
		}}},
	}
}

func seededPipeline(t *testing.T) (*store.Memory, erkaruntime.Run, *sequenceIDs, time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)
	memory := store.NewMemory()
	if err := memory.CreateProject(ctx, erkaruntime.Project{ID: "project-1", Root: filepath.Join(t.TempDir(), "repo"), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := memory.CreateTask(ctx, erkaruntime.Task{ID: "task-1", ProjectID: "project-1", Objective: "fix checkout", Pipeline: "default", Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	initial := erkaruntime.Run{ID: "run-initial", TaskID: "task-1", Stage: "planning", BackendClass: "native", Connection: "planner", Provider: "openai", Model: "p", Attempt: 1, Status: erkaruntime.RunQueued, CreatedAt: now, UpdatedAt: now}
	if err := memory.CreateRun(ctx, initial); err != nil {
		t.Fatal(err)
	}
	return memory, initial, &sequenceIDs{}, now
}

func testPipelineEngine(memory *store.Memory, ids *sequenceIDs, now time.Time, resolved config.Resolved, factory BackendFactory) *PipelineEngine {
	return &PipelineEngine{Config: resolved, Store: memory, Factory: factory, IDs: ids, Clock: func() time.Time { return now }, Limits: erkaruntime.DefaultNativeLimits()}
}

type sequenceIDs struct {
	mu   sync.Mutex
	next int
}

func (ids *sequenceIDs) NewID(prefix string) (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	return fmt.Sprintf("%s-%d", prefix, ids.next), nil
}

type pipelineProvider struct {
	mu        sync.Mutex
	name      string
	responses [][]erkaruntime.ModelOutput
	requests  []erkaruntime.ModelRequest
}

func (provider *pipelineProvider) Name() string { return provider.name }
func (*pipelineProvider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true, StructuredOutput: true, ContextTokens: 1_000_000}, nil
}
func (provider *pipelineProvider) Stream(_ context.Context, request erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.requests = append(provider.requests, request)
	if len(provider.responses) == 0 {
		return nil, errors.New("no scripted response")
	}
	response := provider.responses[0]
	provider.responses = provider.responses[1:]
	return &pipelineStream{outputs: response}, nil
}
func (provider *pipelineProvider) Requests() []erkaruntime.ModelRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]erkaruntime.ModelRequest(nil), provider.requests...)
}

type pipelineStream struct {
	outputs []erkaruntime.ModelOutput
}

func (stream *pipelineStream) Next(context.Context) (erkaruntime.ModelOutput, error) {
	if len(stream.outputs) == 0 {
		return erkaruntime.ModelOutput{}, io.EOF
	}
	output := stream.outputs[0]
	stream.outputs = stream.outputs[1:]
	return output, nil
}
func (*pipelineStream) Close() error { return nil }
