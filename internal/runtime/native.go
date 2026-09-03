package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	ErrMalformedStream            = errors.New("malformed model stream")
	ErrMaximumIterations          = errors.New("maximum model iterations reached")
	ErrUnknownMutatingToolOutcome = errors.New("mutating tool outcome is unknown after restart")
)

// IDGenerator supplies stable prefixed identifiers. Tests inject deterministic
// implementations; production uses RandomIDGenerator.
type IDGenerator interface {
	NewID(string) (string, error)
}

type IDGeneratorFunc func(string) (string, error)

func (function IDGeneratorFunc) NewID(prefix string) (string, error) { return function(prefix) }

type RandomIDGenerator struct{}

func (RandomIDGenerator) NewID(prefix string) (string, error) {
	if prefix == "" {
		return "", errors.New("ID prefix is empty")
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate %s ID: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(value[:]), nil
}

type NativeLimits struct {
	MaxIterations                int
	MaxOutputTokens              int64
	MaxModelOutputBytes          int64
	MaxCumulativeToolOutputBytes int64
	StageTimeout                 time.Duration
}

func DefaultNativeLimits() NativeLimits {
	return NativeLimits{
		MaxIterations:                24,
		MaxOutputTokens:              8_192,
		MaxModelOutputBytes:          1 << 20,
		MaxCumulativeToolOutputBytes: 4 << 20,
		StageTimeout:                 30 * time.Minute,
	}
}

func (limits NativeLimits) validate() error {
	if limits.MaxIterations <= 0 || limits.MaxOutputTokens <= 0 ||
		limits.MaxModelOutputBytes <= 0 || limits.MaxCumulativeToolOutputBytes <= 0 ||
		limits.StageTimeout <= 0 {
		return errors.New("native runtime limits must all be positive")
	}
	return nil
}

// NativeBackend owns the provider-neutral model/tool loop for native APIs.
type NativeBackend struct {
	Provider     ModelProvider
	Tools        []Tool
	Sink         EventSink
	Observer     LiveObserver
	Instructions string
	Limits       NativeLimits
	IDs          IDGenerator
	Clock        func() time.Time
}

func (backend *NativeBackend) RunStage(ctx context.Context, request StageRequest, store Store) (StageResult, error) {
	if err := request.validate(); err != nil {
		return StageResult{}, err
	}
	if store == nil {
		return StageResult{}, errors.New("native backend requires a store")
	}
	prepared, err := backend.prepare()
	if err != nil {
		return StageResult{}, err
	}
	if prepared.Limits.StageTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, prepared.Limits.StageTimeout)
		defer cancel()
	}

	appender, err := newDurableAppender(ctx, request, store, prepared.Sink, prepared.IDs, prepared.Clock)
	if err != nil {
		return StageResult{}, err
	}
	if err := appender.start(ctx, request.Objective, request.Stage); err != nil {
		return StageResult{}, err
	}

	history, err := store.LoadItems(ctx, request.TaskID, 0)
	if err != nil {
		return StageResult{}, err
	}
	toolOutputBytes := int64(0)
	if history, toolOutputBytes, err = prepared.recoverPending(ctx, appender, history, toolOutputBytes); err != nil {
		if errors.Is(err, ErrUnknownMutatingToolOutcome) {
			return StageResult{Status: TaskWaiting}, err
		}
		return StageResult{}, prepared.fail(ctx, appender, "tool_recovery_failed", err)
	}

	definitions := prepared.definitions()
	var cumulativeUsage Usage
	mutated := false
	for iteration := 1; iteration <= prepared.Limits.MaxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return StageResult{}, err
		}
		instructions, modelItems := prepared.Instructions, history
		if request.Stage != "" {
			runs, loadErr := loadContextRuns(ctx, store, request.RunID, history)
			if loadErr != nil {
				return StageResult{}, prepared.fail(ctx, appender, "context_runs_failed", loadErr)
			}
			packet, compileErr := CompileStageContext(ContextRequest{Stage: request.Stage, TaskID: request.TaskID, RunID: request.RunID, Items: history, Runs: runs, MaxBytes: request.ContextMaxBytes})
			if compileErr != nil {
				return StageResult{}, prepared.fail(ctx, appender, "context_compile_failed", compileErr)
			}
			instructions, modelItems = packet.Instructions, packet.Items
		}
		modelRequest := ModelRequest{
			Instructions:    instructions,
			Items:           modelItems,
			Tools:           definitions,
			MaxOutputTokens: prepared.Limits.MaxOutputTokens,
		}
		if err := modelRequest.Validate(); err != nil {
			return StageResult{}, prepared.fail(ctx, appender, "invalid_model_request", err)
		}
		response, err := prepared.readModelResponse(ctx, request, modelRequest)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return StageResult{}, err
			}
			return StageResult{}, prepared.fail(ctx, appender, "model_stream_failed", err)
		}
		if response.usage != nil {
			addUsage(&cumulativeUsage, *response.usage)
		}

		items, calls, err := prepared.responseItems(request, response)
		if err != nil {
			return StageResult{}, prepared.fail(ctx, appender, "invalid_model_output", err)
		}
		if len(items) > 0 {
			committed, err := appender.append(ctx, items, responseEvents(response))
			if err != nil {
				return StageResult{}, err
			}
			history = append(history, committed...)
			prepared.observe(ctx, LiveUpdate{TaskID: request.TaskID, RunID: request.RunID, Kind: LiveCommitted})
		}

		if len(calls) == 0 {
			if strings.TrimSpace(response.text) == "" {
				err := fmt.Errorf("%w: completed response has no text or tool call", ErrMalformedStream)
				return StageResult{}, prepared.fail(ctx, appender, "empty_model_response", err)
			}
			return prepared.complete(ctx, appender, request, response.text, cumulativeUsage, iteration, mutated)
		}

		for _, call := range calls {
			prepared.observe(ctx, LiveUpdate{TaskID: request.TaskID, RunID: request.RunID, Kind: LiveToolStarted, Tool: call.Tool, CallID: call.CallID})
			if call.Mutating {
				mutated = true
				appender.mutated = true
			}
			result, executeErr := prepared.executeTool(ctx, call, toolOutputBytes)
			if executeErr != nil {
				if errors.Is(executeErr, context.Canceled) || errors.Is(executeErr, context.DeadlineExceeded) {
					return StageResult{}, executeErr
				}
				result = ToolExecutionResult{Output: executeErr.Error(), IsError: true}
			}
			result, consumed := boundToolResult(result, prepared.Limits.MaxCumulativeToolOutputBytes-toolOutputBytes)
			toolOutputBytes += consumed
			item := RuntimeItem{
				Kind: ItemToolResult,
				ToolResult: &ToolResult{
					CallID: call.CallID, Output: result.Output,
					IsError: result.IsError, Truncated: result.Truncated,
				},
			}
			eventType := "tool.completed"
			if result.IsError {
				eventType = "tool.failed"
			}
			event := pendingEvent{
				Type: eventType, Message: fmt.Sprintf("%s finished", call.Tool),
				Data: eventData(map[string]any{"tool": call.Tool, "call_id": call.CallID, "truncated": result.Truncated}),
			}
			committed, err := appender.append(ctx, []RuntimeItem{item}, []pendingEvent{event})
			if err != nil {
				return StageResult{}, err
			}
			history = append(history, committed...)
			prepared.observe(ctx, LiveUpdate{TaskID: request.TaskID, RunID: request.RunID, Kind: LiveToolFinished, Tool: call.Tool, CallID: call.CallID, IsError: result.IsError})
		}
	}

	return StageResult{}, prepared.fail(ctx, appender, "maximum_iterations", ErrMaximumIterations)
}

func (request StageRequest) validate() error {
	if request.TaskID == "" || request.RunID == "" {
		return errors.New("stage request requires task and run IDs")
	}
	if strings.TrimSpace(request.Objective) == "" {
		return errors.New("stage request requires an objective")
	}
	if request.ContextMaxBytes < 0 {
		return errors.New("stage context byte limit must not be negative")
	}
	if request.Stage != "" {
		if _, err := StageInstructions(request.Stage); err != nil {
			return err
		}
	}
	return nil
}

func loadContextRuns(ctx context.Context, store Store, current RunID, items []RuntimeItem) ([]Run, error) {
	ids := map[RunID]bool{current: true}
	for _, item := range items {
		ids[item.RunID] = true
	}
	runs := make([]Run, 0, len(ids))
	for id := range ids {
		run, err := store.LoadRun(ctx, id)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

type preparedBackend struct {
	*NativeBackend
	toolByName  map[string]Tool
	toolSchemas map[string]*jsonschema.Schema
}

func (backend *NativeBackend) prepare() (*preparedBackend, error) {
	if backend == nil || backend.Provider == nil {
		return nil, errors.New("native backend requires a model provider")
	}
	if strings.TrimSpace(backend.Instructions) == "" {
		return nil, errors.New("native backend requires instructions")
	}
	limits := backend.Limits
	if limits == (NativeLimits{}) {
		limits = DefaultNativeLimits()
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	ids := backend.IDs
	if ids == nil {
		ids = RandomIDGenerator{}
	}
	clock := backend.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	tools := make(map[string]Tool, len(backend.Tools))
	schemas := make(map[string]*jsonschema.Schema, len(backend.Tools))
	for index, tool := range backend.Tools {
		if tool == nil {
			return nil, fmt.Errorf("tool %d is nil", index)
		}
		definition := tool.Definition()
		if err := definition.Validate(); err != nil {
			return nil, fmt.Errorf("tool %d: %w", index, err)
		}
		if _, exists := tools[definition.Name]; exists {
			return nil, fmt.Errorf("duplicate tool %q", definition.Name)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(definition.InputSchema))
		if err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", definition.Name, err)
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("schema.json", document); err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", definition.Name, err)
		}
		schema, err := compiler.Compile("schema.json")
		if err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", definition.Name, err)
		}
		tools[definition.Name] = tool
		schemas[definition.Name] = schema
	}
	copy := *backend
	copy.Limits = limits
	copy.IDs = ids
	copy.Clock = clock
	return &preparedBackend{NativeBackend: &copy, toolByName: tools, toolSchemas: schemas}, nil
}

func (backend *preparedBackend) definitions() []ToolDefinition {
	definitions := make([]ToolDefinition, 0, len(backend.Tools))
	for _, tool := range backend.Tools {
		definitions = append(definitions, tool.Definition())
	}
	return definitions
}

type assembledResponse struct {
	text      string
	calls     []assembledCall
	usage     *Usage
	byteCount int64
}

type assembledCall struct {
	CallID    ToolCallID
	Tool      string
	Arguments string
}

func (backend *preparedBackend) readModelResponse(ctx context.Context, stage StageRequest, request ModelRequest) (response assembledResponse, err error) {
	stream, err := backend.Provider.Stream(ctx, request)
	if err != nil {
		return response, err
	}
	if stream == nil {
		return response, fmt.Errorf("%w: provider returned a nil stream", ErrMalformedStream)
	}
	defer func() { err = errors.Join(err, stream.Close()) }()

	callIndex := make(map[ToolCallID]int)
	for {
		output, nextErr := stream.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			return response, fmt.Errorf("%w: stream ended before completion", ErrMalformedStream)
		}
		if nextErr != nil {
			return response, nextErr
		}
		if err := output.Validate(); err != nil {
			return response, fmt.Errorf("%w: %v", ErrMalformedStream, err)
		}
		switch output.Kind {
		case ModelTextDelta:
			response.text += output.Text
			response.byteCount += int64(len(output.Text))
			backend.observe(ctx, LiveUpdate{TaskID: stage.TaskID, RunID: stage.RunID, Kind: LiveTextDelta, Text: output.Text})
		case ModelToolCallDelta:
			delta := output.ToolCall
			index, exists := callIndex[delta.CallID]
			if !exists {
				index = len(response.calls)
				callIndex[delta.CallID] = index
				response.calls = append(response.calls, assembledCall{CallID: delta.CallID})
			}
			call := &response.calls[index]
			if delta.Tool != "" {
				if call.Tool != "" && call.Tool != delta.Tool {
					return response, fmt.Errorf("%w: tool call %q changed name", ErrMalformedStream, delta.CallID)
				}
				call.Tool = delta.Tool
			}
			call.Arguments += delta.ArgumentsFragment
			response.byteCount += int64(len(delta.Tool) + len(delta.ArgumentsFragment))
		case ModelUsage:
			if response.usage == nil {
				response.usage = &Usage{}
			}
			if response.usage.Currency != "" && output.Usage.Currency != "" && response.usage.Currency != output.Usage.Currency {
				return response, fmt.Errorf("%w: usage currency changed from %q to %q", ErrMalformedStream, response.usage.Currency, output.Usage.Currency)
			}
			addUsage(response.usage, *output.Usage)
		case ModelFailed:
			return response, fmt.Errorf("provider %s: %s", output.Error.Code, output.Error.Message)
		case ModelCompleted:
			if response.byteCount > backend.Limits.MaxModelOutputBytes {
				return response, fmt.Errorf("model output exceeds %d bytes", backend.Limits.MaxModelOutputBytes)
			}
			return response, nil
		}
		if response.byteCount > backend.Limits.MaxModelOutputBytes {
			return response, fmt.Errorf("model output exceeds %d bytes", backend.Limits.MaxModelOutputBytes)
		}
	}
}

func (backend *preparedBackend) observe(ctx context.Context, update LiveUpdate) {
	if backend.Observer == nil || ctx.Err() != nil {
		return
	}
	update.Text = boundLiveText(update.Text)
	backend.Observer.Observe(ctx, update)
}

func addUsage(total *Usage, next Usage) {
	total.InputTokens += next.InputTokens
	total.OutputTokens += next.OutputTokens
	total.CacheReadTokens += next.CacheReadTokens
	total.CacheWriteTokens += next.CacheWriteTokens
	total.CostMicrounits += next.CostMicrounits
	if total.Currency == "" {
		total.Currency = next.Currency
	}
}

func (backend *preparedBackend) responseItems(request StageRequest, response assembledResponse) ([]RuntimeItem, []ToolCall, error) {
	items := make([]RuntimeItem, 0, len(response.calls)+2)
	if strings.TrimSpace(response.text) != "" {
		items = append(items, RuntimeItem{Kind: ItemAssistantMessage, Message: &Message{Content: response.text}})
	}
	calls := make([]ToolCall, 0, len(response.calls))
	for _, assembled := range response.calls {
		if assembled.Tool == "" {
			return nil, nil, fmt.Errorf("tool call %q has no tool name", assembled.CallID)
		}
		arguments := json.RawMessage(assembled.Arguments)
		tool := backend.toolByName[assembled.Tool]
		mutating := tool != nil && tool.Definition().Mutating
		call := ToolCall{
			CallID: assembled.CallID, Tool: assembled.Tool, Arguments: arguments,
			Mutating: mutating,
		}
		if mutating {
			call.IdempotencyKey = string(request.TaskID) + "/" + string(request.RunID) + "/" + string(call.CallID)
		}
		if err := call.Validate(); err != nil {
			return nil, nil, fmt.Errorf("tool call %q: %w", call.CallID, err)
		}
		calls = append(calls, call)
		copy := call
		items = append(items, RuntimeItem{Kind: ItemToolCall, ToolCall: &copy})
	}
	if response.usage != nil {
		if err := response.usage.Validate(); err != nil {
			return nil, nil, err
		}
		copy := *response.usage
		items = append(items, RuntimeItem{Kind: ItemUsage, Usage: &copy})
	}
	return items, calls, nil
}

func responseEvents(response assembledResponse) []pendingEvent {
	events := make([]pendingEvent, 0, len(response.calls)+1)
	for _, call := range response.calls {
		events = append(events, pendingEvent{
			Type: "tool.started", Message: fmt.Sprintf("%s started", call.Tool),
			Data: eventData(map[string]any{"tool": call.Tool, "call_id": call.CallID}),
		})
	}
	if len(response.calls) == 0 && strings.TrimSpace(response.text) != "" {
		events = append(events, pendingEvent{Type: "model.completed", Message: "Model response completed"})
	}
	return events
}

func (backend *preparedBackend) executeTool(ctx context.Context, call ToolCall, alreadyConsumed int64) (ToolExecutionResult, error) {
	tool := backend.toolByName[call.Tool]
	if tool == nil {
		return ToolExecutionResult{Output: fmt.Sprintf("unknown tool %q", call.Tool), IsError: true}, nil
	}
	definition := tool.Definition()
	invocation := ToolInvocation{
		CallID: call.CallID, Arguments: append(json.RawMessage(nil), call.Arguments...),
		IdempotencyKey: call.IdempotencyKey,
	}
	if err := invocation.Validate(definition); err != nil {
		return ToolExecutionResult{Output: err.Error(), IsError: true}, nil
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(invocation.Arguments))
	if err != nil {
		return ToolExecutionResult{Output: fmt.Sprintf("invalid tool arguments: %v", err), IsError: true}, nil
	}
	if err := backend.toolSchemas[call.Tool].Validate(value); err != nil {
		return ToolExecutionResult{Output: fmt.Sprintf("tool arguments do not match schema: %v", err), IsError: true}, nil
	}
	if alreadyConsumed >= backend.Limits.MaxCumulativeToolOutputBytes {
		return ToolExecutionResult{Output: "tool output budget exhausted", IsError: true, Truncated: true}, nil
	}
	result, err := tool.Execute(ctx, invocation)
	if err != nil {
		return result, err
	}
	result, _ = boundToolResult(result, definition.OutputLimitBytes)
	return result, nil
}

func (backend *preparedBackend) recoverPending(ctx context.Context, appender *durableAppender, history []RuntimeItem, consumed int64) ([]RuntimeItem, int64, error) {
	pending := make(map[ToolCallID]ToolCall)
	order := make([]ToolCallID, 0)
	for _, item := range history {
		switch item.Kind {
		case ItemToolCall:
			pending[item.ToolCall.CallID] = *item.ToolCall
			order = append(order, item.ToolCall.CallID)
		case ItemToolResult:
			delete(pending, item.ToolResult.CallID)
			consumed += int64(len(item.ToolResult.Output))
		}
	}
	for _, callID := range order {
		call, exists := pending[callID]
		if !exists {
			continue
		}
		tool := backend.toolByName[call.Tool]
		mutating := call.Mutating
		if tool != nil {
			mutating = tool.Definition().Mutating
		}
		if mutating {
			event := pendingEvent{
				Type: "task.waiting", Message: "A mutating tool outcome requires inspection",
				Data: eventData(map[string]any{"tool": call.Tool, "call_id": call.CallID}),
			}
			if err := appender.transition(ctx, TaskWaiting, []pendingEvent{event}); err != nil {
				return history, consumed, err
			}
			return history, consumed, fmt.Errorf("%w: %s (%s)", ErrUnknownMutatingToolOutcome, call.Tool, call.CallID)
		}
		result, executeErr := backend.executeTool(ctx, call, consumed)
		if executeErr != nil {
			if errors.Is(executeErr, context.Canceled) || errors.Is(executeErr, context.DeadlineExceeded) {
				return history, consumed, executeErr
			}
			result = ToolExecutionResult{Output: executeErr.Error(), IsError: true}
		}
		result, used := boundToolResult(result, backend.Limits.MaxCumulativeToolOutputBytes-consumed)
		consumed += used
		item := RuntimeItem{Kind: ItemToolResult, ToolResult: &ToolResult{
			CallID: call.CallID, Output: result.Output, IsError: result.IsError, Truncated: result.Truncated,
		}}
		committed, err := appender.append(ctx, []RuntimeItem{item}, []pendingEvent{{
			Type: "tool.recovered", Message: fmt.Sprintf("%s safely repeated after restart", call.Tool),
			Data: eventData(map[string]any{"tool": call.Tool, "call_id": call.CallID}),
		}})
		if err != nil {
			return history, consumed, err
		}
		history = append(history, committed...)
	}
	return history, consumed, nil
}

func (backend *preparedBackend) complete(ctx context.Context, appender *durableAppender, request StageRequest, finalText string, usage Usage, iterations int, mutated bool) (StageResult, error) {
	if request.Stage != "" {
		checkpoint, err := NewStageCheckpoint(request.Stage, request.Objective, finalText)
		if err != nil {
			return StageResult{}, backend.fail(ctx, appender, "invalid_checkpoint", err)
		}
		item := RuntimeItem{Kind: ItemCheckpoint, Checkpoint: &checkpoint}
		if _, err := appender.append(ctx, []RuntimeItem{item}, []pendingEvent{{
			Type: "stage.output", Message: fmt.Sprintf("%s stage produced a durable checkpoint", request.Stage),
			Data: eventData(map[string]any{"stage": request.Stage}),
		}}); err != nil {
			return StageResult{}, err
		}
		return StageResult{Checkpoint: checkpoint, Status: appender.snapshot.Status, Usage: usage, Iterations: iterations, Mutated: mutated}, nil
	}
	if err := appender.transition(ctx, TaskValidating, []pendingEvent{{
		Type: "task.validating", Message: "Finalizing durable task state",
	}}); err != nil {
		return StageResult{}, err
	}
	checkpoint := Checkpoint{
		Objective: request.Objective, SuggestedNextAction: "Review the working tree diff.",
		Decisions: []string{strings.TrimSpace(finalText)},
	}
	item := RuntimeItem{Kind: ItemCheckpoint, Checkpoint: &checkpoint}
	if _, err := appender.appendTransition(ctx, []RuntimeItem{item}, []pendingEvent{{
		Type: "task.completed", Message: "Task completed",
	}}, TaskCompleted); err != nil {
		return StageResult{}, err
	}
	return StageResult{Checkpoint: checkpoint, Status: TaskCompleted, Usage: usage, Iterations: iterations, Mutated: mutated}, nil
}

func (backend *preparedBackend) fail(ctx context.Context, appender *durableAppender, code string, cause error) error {
	if cause == nil || appender == nil || ctx.Err() != nil {
		return cause
	}
	item := RuntimeItem{Kind: ItemError, Error: &RuntimeError{Code: code, Message: cause.Error()}}
	if appender.pipeline {
		_, commitErr := appender.appendRunTransition(ctx, []RuntimeItem{item}, []pendingEvent{{
			Type: "stage.failed", Message: "Pipeline stage failed", Data: eventData(map[string]any{"code": code}),
		}}, RunFailed)
		return errors.Join(&StageError{Cause: cause, AfterMutation: appender.mutated}, commitErr)
	}
	_, commitErr := appender.appendTransition(ctx, []RuntimeItem{item}, []pendingEvent{{
		Type: "task.failed", Message: "Task failed", Data: eventData(map[string]any{"code": code}),
	}}, TaskFailed)
	return errors.Join(cause, commitErr)
}

func boundToolResult(result ToolExecutionResult, remaining int64) (ToolExecutionResult, int64) {
	if remaining < 0 {
		remaining = 0
	}
	original := []byte(result.Output)
	if int64(len(original)) <= remaining {
		return result, int64(len(original))
	}
	bounded := original[:remaining]
	for len(bounded) > 0 && !utf8.Valid(bounded) {
		bounded = bounded[:len(bounded)-1]
	}
	result.Output = string(bounded)
	result.Truncated = true
	return result, int64(len(bounded))
}

type pendingEvent struct {
	Type    string
	Message string
	Data    json.RawMessage
}

type durableAppender struct {
	store     Store
	sink      EventSink
	ids       IDGenerator
	clock     func() time.Time
	taskID    TaskID
	runID     RunID
	snapshot  TaskSnapshot
	runStatus RunStatus
	pipeline  bool
	mutated   bool
}

func newDurableAppender(ctx context.Context, request StageRequest, store Store, sink EventSink, ids IDGenerator, clock func() time.Time) (*durableAppender, error) {
	snapshot, err := store.LoadTask(ctx, request.TaskID)
	if err != nil {
		return nil, err
	}
	run, err := store.LoadRun(ctx, request.RunID)
	if err != nil {
		return nil, err
	}
	if run.TaskID != request.TaskID {
		return nil, fmt.Errorf("run %q belongs to task %q", request.RunID, run.TaskID)
	}
	return &durableAppender{
		store: store, sink: sink, ids: ids, clock: clock,
		taskID: request.TaskID, runID: request.RunID, snapshot: snapshot,
		runStatus: run.Status, pipeline: request.Stage != "",
	}, nil
}

func (appender *durableAppender) start(ctx context.Context, objective string, stage Stage) error {
	if stage != "" {
		return appender.startPipeline(ctx, objective, stage)
	}
	switch appender.snapshot.Status {
	case TaskCompleted, TaskFailed, TaskCancelled, TaskTimedOut:
		if err := appender.transition(ctx, TaskPreparing, []pendingEvent{{
			Type: "task.started", Message: "Follow-up turn started",
		}}); err != nil {
			return err
		}
		fallthrough
	case TaskQueued:
		if appender.snapshot.Status == TaskQueued {
			if err := appender.transition(ctx, TaskPreparing, []pendingEvent{{
				Type: "task.started", Message: "Task started",
			}}); err != nil {
				return err
			}
		}
		fallthrough
	case TaskPreparing:
		item := RuntimeItem{Kind: ItemUserMessage, Message: &Message{Content: objective}}
		_, err := appender.appendTransition(ctx, []RuntimeItem{item}, []pendingEvent{{
			Type: "stage.started", Message: "Coding stage started",
		}}, TaskCoding)
		return err
	case TaskCoding:
		return nil
	default:
		return fmt.Errorf("task %q cannot run from status %q", appender.taskID, appender.snapshot.Status)
	}
}

func (appender *durableAppender) startPipeline(ctx context.Context, objective string, stage Stage) error {
	target := taskStatusForStage(stage)
	if target == "" {
		return fmt.Errorf("unknown pipeline stage %q", stage)
	}
	switch appender.snapshot.Status {
	case TaskCompleted, TaskFailed, TaskCancelled, TaskTimedOut:
		if err := appender.transition(ctx, TaskPreparing, []pendingEvent{{Type: "task.started", Message: "Follow-up turn started"}}); err != nil {
			return err
		}
	case TaskQueued:
		if err := appender.transition(ctx, TaskPreparing, []pendingEvent{{Type: "task.started", Message: "Task started"}}); err != nil {
			return err
		}
	}
	if appender.snapshot.Status == target {
		if appender.runStatus == RunQueued {
			_, err := appender.appendRunTransition(ctx, nil, []pendingEvent{{Type: "stage.started", Message: fmt.Sprintf("%s stage started", stage), Data: eventData(map[string]any{"stage": stage})}}, RunRunning)
			return err
		}
		if appender.runStatus == RunRunning {
			return nil
		}
		return fmt.Errorf("run %q cannot start from status %q", appender.runID, appender.runStatus)
	}
	items := []RuntimeItem(nil)
	if appender.snapshot.Status == TaskPreparing {
		items = []RuntimeItem{{Kind: ItemUserMessage, Message: &Message{Content: objective}}}
	}
	_, err := appender.appendTransition(ctx, items, []pendingEvent{{Type: "stage.started", Message: fmt.Sprintf("%s stage started", stage), Data: eventData(map[string]any{"stage": stage})}}, target)
	return err
}

func (appender *durableAppender) append(ctx context.Context, items []RuntimeItem, events []pendingEvent) ([]RuntimeItem, error) {
	return appender.appendState(ctx, items, events, "", "")
}

func (appender *durableAppender) transition(ctx context.Context, to TaskStatus, events []pendingEvent) error {
	_, err := appender.appendTransition(ctx, nil, events, to)
	return err
}

func (appender *durableAppender) appendTransition(ctx context.Context, items []RuntimeItem, pendingEvents []pendingEvent, to TaskStatus) ([]RuntimeItem, error) {
	return appender.appendState(ctx, items, pendingEvents, to, "")
}

func (appender *durableAppender) appendRunTransition(ctx context.Context, items []RuntimeItem, pendingEvents []pendingEvent, to RunStatus) ([]RuntimeItem, error) {
	return appender.appendState(ctx, items, pendingEvents, "", to)
}

func (appender *durableAppender) appendState(ctx context.Context, items []RuntimeItem, pendingEvents []pendingEvent, to TaskStatus, explicitRunTo RunStatus) ([]RuntimeItem, error) {
	now := appender.clock().UTC()
	committedItems := make([]RuntimeItem, len(items))
	for index, item := range items {
		id, err := appender.ids.NewID("item")
		if err != nil {
			return nil, err
		}
		item.ID = ItemID(id)
		item.Sequence = appender.snapshot.LastItemSequence + uint64(index) + 1
		item.TaskID = appender.taskID
		item.RunID = appender.runID
		item.Time = now
		committedItems[index] = item
	}
	events := make([]Event, len(pendingEvents))
	for index, event := range pendingEvents {
		id, err := appender.ids.NewID("event")
		if err != nil {
			return nil, err
		}
		events[index] = Event{
			ID: EventID(id), Sequence: appender.snapshot.LastEventSequence + uint64(index) + 1,
			TaskID: appender.taskID, RunID: appender.runID, Type: event.Type,
			Time: now, Message: event.Message, Data: event.Data,
		}
	}
	request := CommitRequest{
		TaskID:                appender.taskID,
		ExpectedItemSequence:  appender.snapshot.LastItemSequence,
		ExpectedEventSequence: appender.snapshot.LastEventSequence,
		Items:                 committedItems, Events: events,
	}
	if to != "" {
		request.Transition = &StateTransition{
			TaskID: appender.taskID, RunID: appender.runID,
			From: appender.snapshot.Status, To: to,
		}
		if runTo := runStatusForTaskStatus(to); runTo != "" && runTo != appender.runStatus {
			request.RunTransition = &RunStateTransition{
				TaskID: appender.taskID, RunID: appender.runID,
				From: appender.runStatus, To: runTo,
			}
		}
	}
	if explicitRunTo != "" && explicitRunTo != appender.runStatus {
		request.RunTransition = &RunStateTransition{TaskID: appender.taskID, RunID: appender.runID, From: appender.runStatus, To: explicitRunTo}
	}
	batch, err := appender.store.Commit(ctx, request)
	if err != nil {
		return nil, err
	}
	appender.snapshot.LastItemSequence = batch.LastItemSequence
	appender.snapshot.LastEventSequence = batch.LastEventSequence
	if to != "" {
		appender.snapshot.Status = to
	}
	if request.RunTransition != nil {
		appender.runStatus = request.RunTransition.To
	}
	if appender.sink != nil && len(batch.Events) > 0 {
		if err := appender.sink.Publish(ctx, batch); err != nil {
			return committedItems, err
		}
	}
	return committedItems, nil
}

func runStatusForTaskStatus(status TaskStatus) RunStatus {
	switch status {
	case TaskPlanning, TaskCoding, TaskValidating, TaskReviewing:
		return RunRunning
	case TaskWaiting:
		return RunWaiting
	case TaskCompleted:
		return RunCompleted
	case TaskFailed:
		return RunFailed
	case TaskCancelled:
		return RunCancelled
	case TaskTimedOut:
		return RunTimedOut
	default:
		return ""
	}
}

func taskStatusForStage(stage Stage) TaskStatus {
	switch stage {
	case StagePlanning:
		return TaskPlanning
	case StageCoding, StageRepair:
		return TaskCoding
	case StageValidation:
		return TaskValidating
	case StageReview:
		return TaskReviewing
	default:
		return ""
	}
}

type StageError struct {
	Cause         error
	AfterMutation bool
}

func (failure *StageError) Error() string {
	if failure == nil || failure.Cause == nil {
		return "pipeline stage failed"
	}
	return failure.Cause.Error()
}

func (failure *StageError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

func eventData(value map[string]any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

var _ ExecutionBackend = (*NativeBackend)(nil)
