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

type BackendFactory func(context.Context, Candidate, Stage, erkaruntime.NativeLimits) (*erkaruntime.NativeBackend, error)

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

	initial := request.InitialRun
	attempts := make(map[Stage]int)
	last, err := engine.executeStage(ctx, request, StagePlanning, &initial, attempts)
	if err != nil {
		return engine.result(ctx, request.TaskID, last), err
	}
	last, err = engine.executeStage(ctx, request, StageCoding, nil, attempts)
	if err != nil {
		return engine.result(ctx, request.TaskID, last), err
	}

	pipeline := engine.Config.Pipelines[engine.Config.ActivePipeline]
	if _, configured := pipeline.Stages[string(StageValidation)]; configured {
		last, err = engine.executeStage(ctx, request, StageValidation, nil, attempts)
		if err != nil {
			return engine.result(ctx, request.TaskID, last), err
		}
		for repairs := 0; !validationPassed(last.Checkpoint) && repairs < pipeline.MaxRepairAttempts; repairs++ {
			if _, configured := pipeline.Stages[string(StageRepair)]; !configured {
				break
			}
			if last, err = engine.executeStage(ctx, request, StageRepair, nil, attempts); err != nil {
				return engine.result(ctx, request.TaskID, last), err
			}
			if last, err = engine.executeStage(ctx, request, StageValidation, nil, attempts); err != nil {
				return engine.result(ctx, request.TaskID, last), err
			}
		}
		if !validationPassed(last.Checkpoint) {
			err = errors.New("validation did not pass within the configured repair budget")
			_ = engine.failTask(ctx, request.TaskID, last.Run.ID, "validation_failed", err)
			return engine.result(ctx, request.TaskID, last), err
		}
	}

	if _, configured := pipeline.Stages[string(StageReview)]; configured {
		last, err = engine.executeStage(ctx, request, StageReview, nil, attempts)
		if err != nil {
			return engine.result(ctx, request.TaskID, last), err
		}
	}
	if err := engine.completeTask(ctx, request.TaskID, last.Run.ID); err != nil {
		return engine.result(ctx, request.TaskID, last), err
	}
	last.Status = erkaruntime.TaskCompleted
	return engine.result(ctx, request.TaskID, last), nil
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
	for index, candidate := range route.Candidates {
		attempts[stage]++
		attempt := attempts[stage]
		var run erkaruntime.Run
		if initial != nil && index == 0 {
			run = *initial
			if run.Stage != string(stage) || run.Provider != candidate.Provider || run.Model != candidate.Model || run.Connection != candidate.Connection {
				return stageAttempt{}, errors.New("initial pipeline run does not match the first fixed route")
			}
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
		capabilities, capabilityErr := backend.Provider.Capabilities(ctx)
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
	run := erkaruntime.Run{ID: erkaruntime.RunID(id), TaskID: taskID, Stage: string(stage), BackendClass: "native", Connection: candidate.Connection, Provider: candidate.Provider, Model: candidate.Model, Attempt: attempt, Status: erkaruntime.RunQueued, CreatedAt: now, UpdatedAt: now}
	return run, engine.Store.CreateRun(ctx, run)
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

func validationPassed(checkpoint erkaruntime.Checkpoint) bool {
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
