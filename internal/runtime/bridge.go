package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// BridgeBackend gives subscription-backed vendor agents the same durable stage
// lifecycle as native providers while keeping their private sessions and tool
// transcripts outside erka's canonical history.
type BridgeBackend struct {
	Bridge           SubscriptionBridge
	WorkingDirectory string
	Model            string
	Sink             EventSink
	Observer         LiveObserver
	Limits           NativeLimits
	IDs              IDGenerator
	Clock            func() time.Time
}

func (*BridgeBackend) BackendClass() string { return BackendSubscriptionBridge }

// BridgeNoticer is implemented by bridges that must show the user a notice
// (for example, an experimental status or terms reminder) whenever they run.
// The notice is recorded as a durable bridge.notice event.
type BridgeNoticer interface {
	Notice() string
}

func (backend *BridgeBackend) Capabilities(ctx context.Context) (ModelCapabilities, error) {
	if backend == nil || backend.Bridge == nil {
		return ModelCapabilities{}, errors.New("bridge backend requires a subscription bridge")
	}
	return backend.Bridge.Capabilities(ctx)
}

func (backend *BridgeBackend) RunStage(ctx context.Context, request StageRequest, store Store) (StageResult, error) {
	if err := request.validate(); err != nil {
		return StageResult{}, err
	}
	if store == nil || backend == nil || backend.Bridge == nil {
		return StageResult{}, errors.New("bridge backend requires a bridge and store")
	}
	if request.Stage != StagePlanning && request.Stage != StageReview {
		return StageResult{}, fmt.Errorf("subscription bridge %q cannot run %s stage", backend.Bridge.Name(), request.Stage)
	}
	root, err := filepath.Abs(backend.WorkingDirectory)
	if err != nil || strings.TrimSpace(backend.WorkingDirectory) == "" {
		return StageResult{}, errors.New("bridge backend requires an absolute working directory")
	}
	if root != backend.WorkingDirectory {
		return StageResult{}, errors.New("bridge backend working directory must be absolute and clean")
	}
	limits := backend.Limits
	if limits == (NativeLimits{}) {
		limits = DefaultNativeLimits()
	}
	if err := limits.validate(); err != nil {
		return StageResult{}, err
	}
	ids := backend.IDs
	if ids == nil {
		ids = RandomIDGenerator{}
	}
	clock := backend.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if limits.StageTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limits.StageTimeout)
		defer cancel()
	}

	appender, err := newDurableAppender(ctx, request, store, backend.Sink, ids, clock)
	if err != nil {
		return StageResult{}, err
	}
	if err := appender.start(ctx, request.Objective, request.Stage); err != nil {
		return StageResult{}, err
	}
	if noticer, ok := backend.Bridge.(BridgeNoticer); ok {
		if notice := strings.TrimSpace(noticer.Notice()); notice != "" {
			if _, err := appender.append(ctx, nil, []pendingEvent{{
				Type: "bridge.notice", Message: notice,
				Data: eventData(map[string]any{"bridge": backend.Bridge.Name(), "stage": request.Stage}),
			}}); err != nil {
				return StageResult{}, err
			}
		}
	}
	items, err := store.LoadItems(ctx, request.TaskID, 0)
	if err != nil {
		return StageResult{}, bridgeFail(ctx, appender, "context_items_failed", err)
	}
	runs, err := loadContextRuns(ctx, store, request.RunID, items)
	if err != nil {
		return StageResult{}, bridgeFail(ctx, appender, "context_runs_failed", err)
	}
	packet, err := CompileStageContext(ContextRequest{Stage: request.Stage, TaskID: request.TaskID, RunID: request.RunID, Items: items, Runs: runs, MaxBytes: request.ContextMaxBytes})
	if err != nil {
		return StageResult{}, bridgeFail(ctx, appender, "context_compile_failed", err)
	}
	prompt, err := renderBridgePrompt(packet)
	if err != nil {
		return StageResult{}, bridgeFail(ctx, appender, "context_render_failed", err)
	}

	result, err := backend.Bridge.Execute(ctx, BridgeRequest{
		Stage: request.Stage, Model: backend.Model, WorkingDirectory: root,
		Prompt: prompt, MaxOutputBytes: limits.MaxModelOutputBytes,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return StageResult{}, err
		}
		return StageResult{}, bridgeFail(ctx, appender, "bridge_execution_failed", err)
	}
	result.Checkpoint.Objective = request.Objective
	if strings.TrimSpace(result.Checkpoint.SuggestedNextAction) == "" {
		result.Checkpoint.SuggestedNextAction = "Continue to the next configured pipeline stage."
	}
	if err := result.Usage.Validate(); err != nil {
		return StageResult{}, bridgeFail(ctx, appender, "bridge_usage_invalid", err)
	}
	output := strings.TrimSpace(result.Output)
	if output == "" {
		encoded, marshalErr := json.Marshal(result.Checkpoint)
		if marshalErr != nil {
			return StageResult{}, bridgeFail(ctx, appender, "bridge_output_invalid", marshalErr)
		}
		output = string(encoded)
	}
	if int64(len(output)) > limits.MaxModelOutputBytes {
		return StageResult{}, bridgeFail(ctx, appender, "bridge_output_too_large", fmt.Errorf("bridge output exceeds %d bytes", limits.MaxModelOutputBytes))
	}
	durable := []RuntimeItem{{Kind: ItemAssistantMessage, Message: &Message{Content: output}}}
	if result.Usage != (Usage{}) {
		usage := result.Usage
		durable = append(durable, RuntimeItem{Kind: ItemUsage, Usage: &usage})
	}
	checkpoint := result.Checkpoint
	durable = append(durable, RuntimeItem{Kind: ItemCheckpoint, Checkpoint: &checkpoint})
	if _, err := appender.append(ctx, durable, []pendingEvent{{
		Type: "stage.output", Message: fmt.Sprintf("%s bridge produced a durable %s checkpoint", backend.Bridge.Name(), request.Stage),
		Data: eventData(map[string]any{"stage": request.Stage, "backend_class": BackendSubscriptionBridge, "bridge": backend.Bridge.Name()}),
	}}); err != nil {
		return StageResult{}, err
	}
	if backend.Observer != nil {
		backend.Observer.Observe(ctx, LiveUpdate{TaskID: request.TaskID, RunID: request.RunID, Kind: LiveCommitted})
	}
	return StageResult{Checkpoint: checkpoint, Status: appender.snapshot.Status, Usage: result.Usage, Iterations: 1}, nil
}

func renderBridgePrompt(packet ContextPacket) (string, error) {
	items, err := json.Marshal(packet.Items)
	if err != nil {
		return "", err
	}
	return packet.Instructions + "\n\nUse only read-only inspection. Return exactly one JSON object matching the supplied checkpoint schema.\n\n<durable_context>\n" + string(items) + "\n</durable_context>", nil
}

func bridgeFail(ctx context.Context, appender *durableAppender, code string, cause error) error {
	if cause == nil || appender == nil || ctx.Err() != nil {
		return cause
	}
	item := RuntimeItem{Kind: ItemError, Error: &RuntimeError{Code: code, Message: cause.Error()}}
	_, commitErr := appender.appendRunTransition(ctx, []RuntimeItem{item}, []pendingEvent{{
		Type: "stage.failed", Message: "Subscription bridge stage failed", Data: eventData(map[string]any{"code": code}),
	}}, RunFailed)
	return errors.Join(&StageError{Cause: cause}, commitErr)
}

var _ StageBackend = (*BridgeBackend)(nil)
