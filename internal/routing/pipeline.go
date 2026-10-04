package routing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type BackendFactory func(context.Context, Candidate, Stage, erkaruntime.NativeLimits) (erkaruntime.StageBackend, error)

type PipelineEngine struct {
	Config  config.Resolved
	Store   erkaruntime.Store
	Factory BackendFactory
	IDs     erkaruntime.IDGenerator
	Clock   func() time.Time
	Limits  erkaruntime.NativeLimits
}

type PipelineRequest struct {
	TaskID     erkaruntime.TaskID
	Objective  string
	InitialRun erkaruntime.Run
	Resume     bool
}

type PipelineResult struct {
	TaskID     erkaruntime.TaskID
	RunID      erkaruntime.RunID
	Status     erkaruntime.TaskStatus
	Checkpoint erkaruntime.Checkpoint
}

func (engine *PipelineEngine) Run(ctx context.Context, request PipelineRequest) (PipelineResult, error) {
	if engine == nil || engine.Store == nil || engine.Factory == nil {
		return PipelineResult{}, errors.New("pipeline engine requires store and backend factory")
	}
	if request.TaskID == "" || strings.TrimSpace(request.Objective) == "" || request.InitialRun.ID == "" || request.InitialRun.TaskID != request.TaskID {
		return PipelineResult{}, errors.New("pipeline request requires a task, objective, and initial run")
	}
	if _, ok := engine.Config.Pipelines[engine.Config.ActivePipeline]; !ok {
		return PipelineResult{}, fmt.Errorf("pipeline %q is not configured", engine.Config.ActivePipeline)
	}
	if engine.IDs == nil {
		engine.IDs = erkaruntime.RandomIDGenerator{}
	}
	if engine.Clock == nil {
		engine.Clock = func() time.Time { return time.Now().UTC() }
	}
	if engine.Limits == (erkaruntime.NativeLimits{}) {
		engine.Limits = erkaruntime.DefaultNativeLimits()
	}

	pipeline := engine.Config.Pipelines[engine.Config.ActivePipeline]
	attempts := make(map[Stage]int)
	repairs := 0
	var err error
	if request.Resume {
		attempts, repairs, err = engine.historyState(ctx, request.TaskID)
		if err != nil {
			return PipelineResult{}, err
		}
	}
	stage := StagePlanning
	initial := &request.InitialRun
	var last stageAttempt
	if request.Resume {
		stage = Stage(request.InitialRun.Stage)
		if !validStage(stage) {
			return PipelineResult{}, fmt.Errorf("cannot resume unknown stage %q", stage)
		}
		switch request.InitialRun.Status {
		case erkaruntime.RunCompleted:
			checkpoint, loadErr := engine.checkpointForRun(ctx, request.TaskID, request.InitialRun.ID)
			if loadErr != nil {
				return PipelineResult{}, loadErr
			}
			last = stageAttempt{Run: request.InitialRun, Checkpoint: checkpoint}
			initial = nil
			stage, err = nextStage(pipeline, stage, checkpoint, repairs)
		case erkaruntime.RunQueued, erkaruntime.RunRunning, erkaruntime.RunWaiting:
			attempts[stage] = request.InitialRun.Attempt - 1
		default:
			return PipelineResult{}, fmt.Errorf("run %q cannot resume from status %q", request.InitialRun.ID, request.InitialRun.Status)
		}
	}

	for {
		if errors.Is(err, errPipelineComplete) {
			if last.Run.ID == "" {
				return PipelineResult{}, errors.New("pipeline has no completed stage")
			}
			if completeErr := engine.completeTask(ctx, request.TaskID, last.Run.ID); completeErr != nil {
				return engine.result(ctx, request.TaskID, last), completeErr
			}
			last.Status = erkaruntime.TaskCompleted
			return engine.result(ctx, request.TaskID, last), nil
		}
		if err != nil {
			_ = engine.failTask(ctx, request.TaskID, last.Run.ID, "validation_failed", err)
			return engine.result(ctx, request.TaskID, last), err
		}
		last, err = engine.executeStage(ctx, request, stage, initial, attempts)
		initial = nil
		if err != nil {
			return engine.result(ctx, request.TaskID, last), err
		}
		if stage == StageRepair {
			repairs++
		}
		stage, err = nextStage(pipeline, stage, last.Checkpoint, repairs)
	}
}

var errPipelineComplete = errors.New("pipeline complete")

func nextStage(pipeline config.Pipeline, current Stage, checkpoint erkaruntime.Checkpoint, repairs int) (Stage, error) {
	switch current {
	case StagePlanning:
		return StageCoding, nil
	case StageCoding:
		if _, ok := pipeline.Stages[string(StageValidation)]; ok {
			return StageValidation, nil
		}
		if _, ok := pipeline.Stages[string(StageReview)]; ok {
			return StageReview, nil
		}
		return "", errPipelineComplete
	case StageValidation:
		if validationPassed(checkpoint) {
			if _, ok := pipeline.Stages[string(StageReview)]; ok {
				return StageReview, nil
			}
			return "", errPipelineComplete
		}
		if _, ok := pipeline.Stages[string(StageRepair)]; ok && repairs < pipeline.MaxRepairAttempts {
			return StageRepair, nil
		}
		return "", errors.New("validation did not pass within the configured repair budget")
	case StageRepair:
		return StageValidation, nil
	case StageReview:
		return "", errPipelineComplete
	default:
		return "", fmt.Errorf("unknown pipeline stage %q", current)
	}
}

func validStage(stage Stage) bool {
	switch stage {
	case StagePlanning, StageCoding, StageValidation, StageRepair, StageReview:
		return true
	default:
		return false
	}
}

type stageAttempt struct {
	Run        erkaruntime.Run
	Checkpoint erkaruntime.Checkpoint
	Status     erkaruntime.TaskStatus
}

func (engine *PipelineEngine) executeStage(ctx context.Context, request PipelineRequest, stage Stage, initial *erkaruntime.Run, attempts map[Stage]int) (stageAttempt, error) {
	route, err := ResolveFixed(engine.Config, stage, engine.Limits)
	if err != nil {
		return stageAttempt{}, err
	}
	startIndex := 0
	if initial != nil {
		startIndex = -1
		for index, candidate := range route.Candidates {
			if initial.Stage == string(stage) && initial.Provider == candidate.Provider && initial.Model == candidate.Model && initial.Connection == candidate.Connection {
				startIndex = index
				break
			}
		}
		if startIndex < 0 {
			return stageAttempt{}, errors.New("initial pipeline run does not match its fixed route")
		}
	}
	for index := startIndex; index < len(route.Candidates); index++ {
		candidate := route.Candidates[index]
		attempts[stage]++
		attempt := attempts[stage]
		var run erkaruntime.Run
		if initial != nil && index == startIndex {
			run = *initial
		} else {
			run, err = engine.createRun(ctx, request.TaskID, stage, candidate, attempt)
			if err != nil {
				return stageAttempt{}, err
			}
		}

		limits := engine.Limits
		limits.MaxIterations = route.Budget.MaxIterations
		limits.MaxOutputTokens = route.Budget.MaxOutputTokens
		limits.StageTimeout = route.Budget.Timeout
		backend, openErr := engine.Factory(ctx, candidate, stage, limits)
		if openErr != nil {
			wrapped := normalizeAttemptError(openErr)
			_ = engine.failRun(ctx, request.TaskID, run.ID, "provider_open_failed", wrapped)
			if index+1 < len(route.Candidates) && CanFallback(wrapped) {
				continue
			}
			_ = engine.failTask(ctx, request.TaskID, run.ID, "provider_open_failed", wrapped)
			return stageAttempt{Run: run}, wrapped
		}
		capabilities, capabilityErr := backend.Capabilities(ctx)
		if capabilityErr == nil {
			capabilityErr = Eligible(route, candidate, capabilities)
		}
		if capabilityErr != nil {
			_ = engine.failRun(ctx, request.TaskID, run.ID, "model_ineligible", capabilityErr)
			if index+1 < len(route.Candidates) && (errors.Is(capabilityErr, ErrNoRoute) || CanFallback(capabilityErr)) {
				continue
			}
			_ = engine.failTask(ctx, request.TaskID, run.ID, "model_ineligible", capabilityErr)
			return stageAttempt{Run: run}, capabilityErr
		}
		contextBytes, sizeErr := contextByteBudget(route.Budget.MaxInputTokens)
		if sizeErr != nil {
			return stageAttempt{Run: run}, sizeErr
		}
		result, runErr := backend.RunStage(ctx, erkaruntime.StageRequest{TaskID: request.TaskID, RunID: run.ID, Objective: request.Objective, Stage: stage, ContextMaxBytes: contextBytes}, engine.Store)
		attemptResult := stageAttempt{Run: run, Checkpoint: result.Checkpoint, Status: result.Status}
		if runErr != nil {
			if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, erkaruntime.ErrUnknownMutatingToolOutcome) {
				return attemptResult, runErr
			}
			if index+1 < len(route.Candidates) && CanFallback(runErr) {
				continue
			}
			_ = engine.failTask(ctx, request.TaskID, run.ID, "stage_failed", runErr)
			return attemptResult, runErr
		}
		priced, priceErr := PriceUsage(result.Usage, candidate.Metadata)
		if priceErr == nil {
			priceErr = CheckBudget(route.Budget, priced, result.Iterations, 0)
		}
		if priceErr != nil {
			_ = engine.failRunningStage(ctx, request.TaskID, run.ID, "budget_exceeded", priceErr)
			return attemptResult, priceErr
		}
		if err := engine.completeStage(ctx, request.TaskID, run.ID, stage, result.Usage, priced); err != nil {
			return attemptResult, err
		}
		attemptResult.Status = result.Status
		return attemptResult, nil
	}
	return stageAttempt{}, fmt.Errorf("stage %q: %w", stage, ErrNoRoute)
}

func (engine *PipelineEngine) createRun(ctx context.Context, taskID erkaruntime.TaskID, stage Stage, candidate Candidate, attempt int) (erkaruntime.Run, error) {
	id, err := engine.IDs.NewID("run")
	if err != nil {
		return erkaruntime.Run{}, err
	}
	now := engine.Clock().UTC()
	run := erkaruntime.Run{ID: erkaruntime.RunID(id), TaskID: taskID, Stage: string(stage), BackendClass: candidate.BackendClass, Connection: candidate.Connection, Provider: candidate.Provider, Model: candidate.Model, Attempt: attempt, Status: erkaruntime.RunQueued, CreatedAt: now, UpdatedAt: now}
	return run, engine.Store.CreateRun(ctx, run)
}

func (engine *PipelineEngine) historyState(ctx context.Context, taskID erkaruntime.TaskID) (map[Stage]int, int, error) {
	items, err := engine.Store.LoadItems(ctx, taskID, 0)
	if err != nil {
		return nil, 0, err
	}
	var turnStart uint64
	for _, item := range items {
		if item.Kind == erkaruntime.ItemUserMessage {
			turnStart = item.Sequence
		}
	}
	runIDs := make(map[erkaruntime.RunID]bool)
	for _, item := range items {
		if item.Sequence >= turnStart {
			runIDs[item.RunID] = true
		}
	}
	attempts := make(map[Stage]int)
	repairs := 0
	for runID := range runIDs {
		run, loadErr := engine.Store.LoadRun(ctx, runID)
		if loadErr != nil {
			return nil, 0, loadErr
		}
		stage := Stage(run.Stage)
		if run.Attempt > attempts[stage] {
			attempts[stage] = run.Attempt
		}
		if stage == StageRepair && run.Status == erkaruntime.RunCompleted {
			repairs++
		}
	}
	return attempts, repairs, nil
}

func (engine *PipelineEngine) checkpointForRun(ctx context.Context, taskID erkaruntime.TaskID, runID erkaruntime.RunID) (erkaruntime.Checkpoint, error) {
	items, err := engine.Store.LoadItems(ctx, taskID, 0)
	if err != nil {
		return erkaruntime.Checkpoint{}, err
	}
	for index := len(items) - 1; index >= 0; index-- {
		if items[index].RunID == runID && items[index].Kind == erkaruntime.ItemCheckpoint {
			return *items[index].Checkpoint, nil
		}
	}
	return erkaruntime.Checkpoint{}, fmt.Errorf("completed run %q has no durable checkpoint", runID)
}

func (engine *PipelineEngine) completeStage(ctx context.Context, taskID erkaruntime.TaskID, runID erkaruntime.RunID, stage Stage, reported, priced erkaruntime.Usage) error {
	snapshot, err := engine.Store.LoadTask(ctx, taskID)
	if err != nil {
		return err
	}
	run, err := engine.Store.LoadRun(ctx, runID)
	if err != nil {
		return err
	}
	items := []erkaruntime.RuntimeItem(nil)
	if priced.CostMicrounits > reported.CostMicrounits {
		id, idErr := engine.IDs.NewID("item")
		if idErr != nil {
			return idErr
		}
		items = []erkaruntime.RuntimeItem{{ID: erkaruntime.ItemID(id), Sequence: snapshot.LastItemSequence + 1, TaskID: taskID, RunID: runID, Kind: erkaruntime.ItemUsage, Time: engine.Clock().UTC(), Usage: &erkaruntime.Usage{CostMicrounits: priced.CostMicrounits - reported.CostMicrounits, Currency: priced.Currency}}}
	}
	event, err := engine.newEvent(snapshot.LastEventSequence+1, taskID, runID, "stage.completed", fmt.Sprintf("%s stage completed", stage))
	if err != nil {
		return err
	}
	_, err = engine.Store.Commit(ctx, erkaruntime.CommitRequest{TaskID: taskID, ExpectedItemSequence: snapshot.LastItemSequence, ExpectedEventSequence: snapshot.LastEventSequence, Items: items, Events: []erkaruntime.Event{event}, RunTransition: &erkaruntime.RunStateTransition{TaskID: taskID, RunID: runID, From: run.Status, To: erkaruntime.RunCompleted}})
	return err
}

func (engine *PipelineEngine) completeTask(ctx context.Context, taskID erkaruntime.TaskID, runID erkaruntime.RunID) error {
	snapshot, err := engine.Store.LoadTask(ctx, taskID)
	if err != nil {
		return err
	}
	event, err := engine.newEvent(snapshot.LastEventSequence+1, taskID, runID, "task.completed", "Pipeline completed")
	if err != nil {
		return err
	}
	// The final run is already durable and complete; task completion is a later
	// committed state boundary owned by the orchestrator.
	_, err = engine.Store.Commit(ctx, erkaruntime.CommitRequest{TaskID: taskID, ExpectedItemSequence: snapshot.LastItemSequence, ExpectedEventSequence: snapshot.LastEventSequence, Events: []erkaruntime.Event{event}, Transition: &erkaruntime.StateTransition{TaskID: taskID, RunID: runID, From: snapshot.Status, To: erkaruntime.TaskCompleted}})
	return err
}

func (engine *PipelineEngine) failRun(ctx context.Context, taskID erkaruntime.TaskID, runID erkaruntime.RunID, code string, cause error) error {
	return engine.commitFailure(ctx, taskID, runID, code, cause, false, true)
}

func (engine *PipelineEngine) failRunningStage(ctx context.Context, taskID erkaruntime.TaskID, runID erkaruntime.RunID, code string, cause error) error {
	return engine.commitFailure(ctx, taskID, runID, code, cause, true, true)
}

func (engine *PipelineEngine) failTask(ctx context.Context, taskID erkaruntime.TaskID, runID erkaruntime.RunID, code string, cause error) error {
	return engine.commitFailure(ctx, taskID, runID, code, cause, true, false)
}

func (engine *PipelineEngine) commitFailure(ctx context.Context, taskID erkaruntime.TaskID, runID erkaruntime.RunID, code string, cause error, failTask, failRun bool) error {
	snapshot, err := engine.Store.LoadTask(ctx, taskID)
	if err != nil {
		return err
	}
	run, err := engine.Store.LoadRun(ctx, runID)
	if err != nil {
		return err
	}
	itemID, err := engine.IDs.NewID("item")
	if err != nil {
		return err
	}
	eventID, err := engine.IDs.NewID("event")
	if err != nil {
		return err
	}
	now := engine.Clock().UTC()
	request := erkaruntime.CommitRequest{TaskID: taskID, ExpectedItemSequence: snapshot.LastItemSequence, ExpectedEventSequence: snapshot.LastEventSequence, Items: []erkaruntime.RuntimeItem{{ID: erkaruntime.ItemID(itemID), Sequence: snapshot.LastItemSequence + 1, TaskID: taskID, RunID: runID, Kind: erkaruntime.ItemError, Time: now, Error: &erkaruntime.RuntimeError{Code: code, Message: cause.Error(), Retryable: CanFallback(cause)}}}, Events: []erkaruntime.Event{{ID: erkaruntime.EventID(eventID), Sequence: snapshot.LastEventSequence + 1, TaskID: taskID, RunID: runID, Type: "stage.failed", Time: now, Message: cause.Error()}}}
	if failTask && snapshot.Status != erkaruntime.TaskFailed {
		request.Transition = &erkaruntime.StateTransition{TaskID: taskID, RunID: runID, From: snapshot.Status, To: erkaruntime.TaskFailed}
	}
	if failRun && run.Status != erkaruntime.RunFailed {
		request.RunTransition = &erkaruntime.RunStateTransition{TaskID: taskID, RunID: runID, From: run.Status, To: erkaruntime.RunFailed}
	}
	_, err = engine.Store.Commit(ctx, request)
	return err
}

func (engine *PipelineEngine) newEvent(sequence uint64, taskID erkaruntime.TaskID, runID erkaruntime.RunID, eventType, message string) (erkaruntime.Event, error) {
	id, err := engine.IDs.NewID("event")
	if err != nil {
		return erkaruntime.Event{}, err
	}
	return erkaruntime.Event{ID: erkaruntime.EventID(id), Sequence: sequence, TaskID: taskID, RunID: runID, Type: eventType, Time: engine.Clock().UTC(), Message: message}, nil
}

func (engine *PipelineEngine) result(ctx context.Context, taskID erkaruntime.TaskID, attempt stageAttempt) PipelineResult {
	result := PipelineResult{TaskID: taskID, RunID: attempt.Run.ID, Status: attempt.Status, Checkpoint: attempt.Checkpoint}
	if snapshot, err := engine.Store.LoadTask(ctx, taskID); err == nil {
		result.Status = snapshot.Status
	}
	return result
}

// validationPassed reports the verdict recorded in a validation checkpoint.
// Structured reports are authoritative; anything unparseable was already
// recorded as a fail report when the checkpoint was created.
func validationPassed(checkpoint erkaruntime.Checkpoint) bool {
	if checkpoint.Validation != nil {
		return checkpoint.Validation.Passed()
	}
	return legacyValidationPassed(checkpoint)
}

// legacyValidationPassed replays the verdict of checkpoints recorded before
// structured validation existed, so resuming an older task follows the
// decision that was durably made at the time. New checkpoints never use it.
func legacyValidationPassed(checkpoint erkaruntime.Checkpoint) bool {
	if len(checkpoint.ValidationResults) == 0 {
		return false
	}
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(checkpoint.ValidationResults[len(checkpoint.ValidationResults)-1])), "PASS")
}

func normalizeAttemptError(err error) error {
	if CanFallback(err) {
		return err
	}
	var attempt *AttemptError
	if errors.As(err, &attempt) {
		return err
	}
	return &AttemptError{Kind: FailureUnavailable, Err: err}
}

func contextByteBudget(tokens int64) (int64, error) {
	if tokens == 0 {
		return 0, nil
	}
	if tokens > math.MaxInt64/4 {
		return 0, errors.New("input-token budget is too large")
	}
	return tokens * 4, nil
}
