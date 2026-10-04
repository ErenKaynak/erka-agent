package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/hooks"
	"github.com/ErenKaynak/erka-agent/internal/mcp"
	"github.com/ErenKaynak/erka-agent/internal/permissions"
	"github.com/ErenKaynak/erka-agent/internal/projectdocs"
	"github.com/ErenKaynak/erka-agent/internal/routing"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/sandbox"
	"github.com/ErenKaynak/erka-agent/internal/store"
	"github.com/ErenKaynak/erka-agent/internal/tools"
)

const leaseDuration = 20 * time.Second

type SessionOptions struct {
	WorkingDirectory   string
	StateDir           string
	Provider           erkaruntime.ModelProvider
	Output             io.Writer
	Observer           erkaruntime.LiveObserver
	IDs                erkaruntime.IDGenerator
	Clock              func() time.Time
	Limits             erkaruntime.NativeLimits
	AllowFullExecution bool
	Pipeline           config.Resolved
	ProviderFactory    ProviderFactory
	BridgeFactory      BridgeFactory
	RebuildProvider    func(string) (erkaruntime.ModelProvider, error)
	NativeModel        string
	// PermissionRules are the configured allow/ask/deny tool rules.
	PermissionRules permissions.Rules
	// Approver answers "ask" verdicts; when nil, PermissionMode decides.
	Approver       erkaruntime.PermissionApprover
	PermissionMode PermissionMode
	// Hooks are post-edit commands from configuration.
	Hooks hooks.Config
	// MCPServers are started lazily on the first turn and closed with the
	// session.
	MCPServers map[string]mcp.ServerConfig
}

type ProviderFactory func(context.Context, routing.Candidate) (erkaruntime.ModelProvider, error)
type BridgeFactory func(context.Context, routing.Candidate) (erkaruntime.SubscriptionBridge, error)

type Session struct {
	mu             sync.Mutex
	options        SessionOptions
	root           string
	project        erkaruntime.ProjectID
	database       *store.SQLite
	sandbox        *sandbox.Local
	holder         string
	readOnly       bool
	closed         bool
	cancel         context.CancelFunc
	turnActive     bool
	modelOverrides map[erkaruntime.Stage]string
	interactive    *interactiveApprover
	mcpOnce        sync.Once
	mcp            *mcp.Manager
}

type Conversation struct {
	Task   erkaruntime.TaskSnapshot
	Turns  []store.Turn
	Runs   []erkaruntime.Run
	Items  []erkaruntime.RuntimeItem
	Events []erkaruntime.Event
}

func OpenSession(ctx context.Context, options SessionOptions) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(options.StateDir) {
		return nil, errors.New("local session requires an absolute state directory")
	}
	if options.Pipeline.ActivePipeline != "" && options.ProviderFactory == nil {
		return nil, errors.New("local pipeline session requires a provider factory")
	}
	if pipelineUsesBridge(options.Pipeline) && options.BridgeFactory == nil {
		return nil, errors.New("local pipeline session requires a subscription bridge factory")
	}
	root, err := DiscoverRepository(options.WorkingDirectory)
	if err != nil {
		return nil, err
	}
	localSandbox, err := sandbox.NewLocal(root, sandbox.Options{})
	if err != nil {
		return nil, fmt.Errorf("initialize workspace sandbox: %w", err)
	}
	database, err := store.OpenSQLite(ctx, filepath.Join(options.StateDir, "erka.db"))
	if err != nil {
		return nil, err
	}
	if options.IDs == nil {
		options.IDs = erkaruntime.RandomIDGenerator{}
	}
	if options.Clock == nil {
		options.Clock = func() time.Time { return time.Now().UTC() }
	}
	project := projectID(root)
	if err := ensureProject(ctx, database, erkaruntime.Project{ID: project, Root: root, CreatedAt: options.Clock()}); err != nil {
		_ = database.Close()
		return nil, err
	}
	holder, err := (erkaruntime.RandomIDGenerator{}).NewID("worker")
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	now := options.Clock().UTC()
	acquired, err := database.AcquireLease(ctx, store.Lease{ProjectID: project, Holder: holder, UpdatedAt: now, ExpiresAt: now.Add(leaseDuration)})
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := options.Hooks.Validate(); err != nil {
		_ = database.Close()
		return nil, err
	}
	if _, err := permissions.New(options.PermissionRules, options.AllowFullExecution); err != nil {
		_ = database.Close()
		return nil, err
	}
	return &Session{options: options, root: root, project: project, database: database, sandbox: localSandbox, holder: holder, readOnly: !acquired, interactive: &interactiveApprover{observer: options.Observer}}, nil
}

func (session *Session) Root() string                     { return session.root }
func (session *Session) ProjectID() erkaruntime.ProjectID { return session.project }
func (session *Session) ReadOnly() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.readOnly
}

func (session *Session) Close() error {
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil
	}
	session.closed = true
	if session.cancel != nil {
		session.cancel()
	}
	manager := session.mcp
	session.mu.Unlock()
	_ = manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = session.database.ReleaseLease(ctx, session.project, session.holder)
	return session.database.Close()
}

func (session *Session) ListConversations(ctx context.Context) ([]erkaruntime.TaskSnapshot, error) {
	return session.database.ListTasks(ctx, session.project)
}

func (session *Session) OpenConversation(ctx context.Context, taskID erkaruntime.TaskID) (Conversation, error) {
	task, err := session.database.LoadTask(ctx, taskID)
	if err != nil {
		return Conversation{}, err
	}
	if task.ProjectID != session.project {
		return Conversation{}, errors.New("conversation belongs to another repository")
	}
	turns, err := session.database.ListTurns(ctx, taskID)
	if err != nil {
		return Conversation{}, err
	}
	runs, err := session.database.ListRuns(ctx, taskID)
	if err != nil {
		return Conversation{}, err
	}
	items, err := session.database.LoadItems(ctx, taskID, 0)
	if err != nil {
		return Conversation{}, err
	}
	events, err := session.database.LoadEvents(ctx, taskID, 0)
	if err != nil {
		return Conversation{}, err
	}
	return Conversation{Task: task, Turns: turns, Runs: runs, Items: items, Events: events}, nil
}

func (session *Session) Submit(ctx context.Context, taskID erkaruntime.TaskID, prompt string) (erkaruntime.TaskID, error) {
	if session.ReadOnly() {
		return "", errors.New("repository is read-only while another erka process holds the execution lease")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", errors.New("prompt is empty")
	}
	now := session.options.Clock().UTC()
	if taskID == "" {
		value, err := session.options.IDs.NewID("task")
		if err != nil {
			return "", err
		}
		taskID = erkaruntime.TaskID(value)
		task := erkaruntime.Task{ID: taskID, ProjectID: session.project, Objective: prompt, Pipeline: session.options.Pipeline.ActivePipeline, Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now}
		if err := session.database.CreateTask(ctx, task); err != nil {
			return "", err
		}
	} else {
		task, err := session.database.LoadTask(ctx, taskID)
		if err != nil {
			return "", err
		}
		if task.ProjectID != session.project {
			return "", errors.New("conversation belongs to another repository")
		}
		if task.Pipeline != session.options.Pipeline.ActivePipeline {
			return "", fmt.Errorf("conversation uses pipeline %q, current configuration selects %q", task.Pipeline, session.options.Pipeline.ActivePipeline)
		}
		if task.Status == erkaruntime.TaskWaiting {
			return "", errors.New("conversation has an unresolved tool outcome")
		}
	}
	value, err := session.options.IDs.NewID("turn")
	if err != nil {
		return "", err
	}
	if err := session.database.EnqueueTurn(ctx, store.Turn{ID: value, TaskID: taskID, Prompt: prompt, Status: store.TurnQueued, CreatedAt: now}); err != nil {
		return "", err
	}
	return taskID, nil
}

func (session *Session) RunNext(ctx context.Context) (LocalResult, bool, error) {
	if session.ReadOnly() {
		return LocalResult{}, false, errors.New("repository is read-only while another erka process holds the execution lease")
	}
	if session.options.Provider == nil && session.options.Pipeline.ActivePipeline == "" {
		return LocalResult{}, false, errors.New("local session requires a model provider")
	}
	if err := session.beginTurn(); err != nil {
		return LocalResult{}, false, err
	}
	defer session.endTurn()
	now := session.options.Clock().UTC()
	renewed, err := session.database.RenewLease(ctx, store.Lease{ProjectID: session.project, Holder: session.holder, UpdatedAt: now, ExpiresAt: now.Add(leaseDuration)})
	if err != nil || !renewed {
		session.mu.Lock()
		session.readOnly = true
		session.mu.Unlock()
		if err == nil {
			err = errors.New("repository execution lease was lost")
		}
		return LocalResult{}, false, err
	}
	run, err := session.newInitialRun(now)
	if err != nil {
		return LocalResult{}, false, err
	}
	turn, ok, err := session.database.ClaimNextTurn(ctx, session.project, session.options.Pipeline.ActivePipeline, run, now)
	if err != nil || !ok {
		return LocalResult{}, ok, err
	}
	run.TaskID = turn.TaskID
	return session.execute(ctx, turn, run, false)
}

func (session *Session) newInitialRun(now time.Time) (erkaruntime.Run, error) {
	value, err := session.options.IDs.NewID("run")
	if err != nil {
		return erkaruntime.Run{}, err
	}
	run := erkaruntime.Run{ID: erkaruntime.RunID(value), Stage: "coding", BackendClass: "native", Status: erkaruntime.RunQueued, CreatedAt: now, UpdatedAt: now, Attempt: 1}
	if session.options.Pipeline.ActivePipeline != "" {
		route, err := routing.ResolveFixed(session.effectivePipeline(), routing.StagePlanning, session.options.Limits)
		if err != nil {
			return erkaruntime.Run{}, err
		}
		candidate := route.Candidates[0]
		run.Stage, run.BackendClass, run.Connection, run.Provider, run.Model = string(routing.StagePlanning), candidate.BackendClass, candidate.Connection, candidate.Provider, candidate.Model
		return run, nil
	}
	run.Provider = session.options.Provider.Name()
	run.Model = strings.TrimSpace(session.options.NativeModel)
	if run.Model == "" {
		run.Model = run.Provider
	}
	return run, nil
}

func (session *Session) Resume(ctx context.Context, taskID erkaruntime.TaskID) (LocalResult, error) {
	if err := session.beginTurn(); err != nil {
		return LocalResult{}, err
	}
	defer session.endTurn()
	conversation, err := session.OpenConversation(ctx, taskID)
	if err != nil {
		return LocalResult{}, err
	}
	if len(conversation.Runs) == 0 {
		return LocalResult{}, errors.New("conversation has no run to resume")
	}
	if conversation.Task.Pipeline != session.options.Pipeline.ActivePipeline {
		return LocalResult{}, fmt.Errorf("conversation uses pipeline %q, current configuration selects %q", conversation.Task.Pipeline, session.options.Pipeline.ActivePipeline)
	}
	run := conversation.Runs[len(conversation.Runs)-1]
	prompt := conversation.Task.Objective
	turn := store.Turn{TaskID: taskID, RunID: run.ID, Prompt: prompt, Status: store.TurnRunning}
	if len(conversation.Turns) > 0 {
		turn = conversation.Turns[len(conversation.Turns)-1]
		prompt = turn.Prompt
	}
	for _, candidate := range conversation.Turns {
		if candidate.RunID == run.ID {
			turn = candidate
			break
		}
	}
	turn.Prompt = prompt
	result, _, err := session.execute(ctx, turn, run, session.options.Pipeline.ActivePipeline != "")
	return result, err
}

func (session *Session) execute(ctx context.Context, turn store.Turn, run erkaruntime.Run, resumePipeline bool) (LocalResult, bool, error) {
	if turn.ID != "" && turn.Status == store.TurnClaimed {
		_ = session.database.UpdateTurnStatus(ctx, turn.ID, store.TurnClaimed, store.TurnRunning, session.options.Clock())
	}
	runCtx, cancel := context.WithCancel(ctx)
	session.mu.Lock()
	session.cancel = cancel
	allowFullExecution := session.options.AllowFullExecution
	session.mu.Unlock()
	leaseDone := make(chan struct{})
	go session.maintainLease(runCtx, cancel, leaseDone)
	defer func() { close(leaseDone); session.mu.Lock(); session.cancel = nil; session.mu.Unlock(); cancel() }()
	// AGENTS.md is read fresh for every turn so edits take effect on the next
	// prompt. A missing or unreadable file simply contributes nothing.
	projectInstructions, _ := projectdocs.Load(runCtx, session.sandbox)
	policy, toolOptions, err := session.permissionPolicy(allowFullExecution)
	if err != nil {
		return LocalResult{}, false, err
	}
	approver := session.approver()
	extraTools := session.mcpTools()
	var hookRunner erkaruntime.ToolHook
	if !session.options.Hooks.Empty() {
		runner, hookErr := hooks.NewRunner(session.options.Hooks, session.sandbox)
		if hookErr != nil {
			return LocalResult{}, false, hookErr
		}
		hookRunner = runner
	}
	stage := erkaruntime.StageResult{}
	activeRunID := run.ID
	if session.options.Pipeline.ActivePipeline != "" {
		engine := &routing.PipelineEngine{Config: session.effectivePipeline(), Store: session.database, IDs: session.options.IDs, Clock: session.options.Clock, Limits: session.options.Limits}
		engine.Factory = func(factoryCtx context.Context, candidate routing.Candidate, stageName routing.Stage, limits erkaruntime.NativeLimits) (erkaruntime.StageBackend, error) {
			if candidate.BackendClass == config.ConnectionSubscriptionBridge {
				bridge, openErr := session.options.BridgeFactory(factoryCtx, candidate)
				if openErr != nil {
					return nil, openErr
				}
				return &erkaruntime.BridgeBackend{Bridge: bridge, WorkingDirectory: session.root, Model: candidate.Model, Sink: &writerSink{writer: session.options.Output}, Observer: session.options.Observer, Limits: limits, IDs: session.options.IDs, Clock: session.options.Clock}, nil
			}
			provider, openErr := session.options.ProviderFactory(factoryCtx, candidate)
			if openErr != nil {
				return nil, openErr
			}
			availableTools := stageTools(append(tools.Builtins(session.sandbox, toolOptions), extraTools...), stageName)
			return &erkaruntime.NativeBackend{Provider: provider, Tools: availableTools, Permissions: policy, Approver: approver, Hooks: hookRunner, Sink: &writerSink{writer: session.options.Output}, Observer: session.options.Observer, Instructions: nativeInstructions, ProjectInstructions: projectInstructions, Limits: limits, IDs: session.options.IDs, Clock: session.options.Clock}, nil
		}
		pipelineResult, pipelineErr := engine.Run(runCtx, routing.PipelineRequest{TaskID: turn.TaskID, Objective: turn.Prompt, InitialRun: run, Resume: resumePipeline})
		activeRunID, err = pipelineResult.RunID, pipelineErr
		stage = erkaruntime.StageResult{Checkpoint: pipelineResult.Checkpoint, Status: pipelineResult.Status}
	} else {
		backend := &erkaruntime.NativeBackend{Provider: session.options.Provider, Tools: append(tools.Builtins(session.sandbox, toolOptions), extraTools...), Permissions: policy, Approver: approver, Hooks: hookRunner, Sink: &writerSink{writer: session.options.Output}, Observer: session.options.Observer, Instructions: nativeInstructions, ProjectInstructions: projectInstructions, Limits: session.options.Limits, IDs: session.options.IDs, Clock: session.options.Clock}
		stage, err = backend.RunStage(runCtx, erkaruntime.StageRequest{TaskID: turn.TaskID, RunID: run.ID, Objective: turn.Prompt}, session.database)
	}
	if activeRunID == "" {
		activeRunID = run.ID
	}
	status := store.TurnCompleted
	if errors.Is(err, erkaruntime.ErrUnknownMutatingToolOutcome) {
		status = store.TurnWaiting
	} else if errors.Is(err, context.Canceled) {
		status = store.TurnCancelled
		session.persistInterruption(turn.TaskID, activeRunID, erkaruntime.TaskCancelled, erkaruntime.RunCancelled, "task.cancelled")
	} else if errors.Is(err, context.DeadlineExceeded) {
		status = store.TurnTimedOut
		session.persistInterruption(turn.TaskID, activeRunID, erkaruntime.TaskTimedOut, erkaruntime.RunTimedOut, "task.timed_out")
	} else if err != nil {
		status = store.TurnFailed
	}
	if turn.ID != "" {
		_ = session.database.UpdateTurnStatus(context.Background(), turn.ID, store.TurnRunning, status, session.options.Clock())
	}
	result := LocalResult{ProjectID: session.project, TaskID: turn.TaskID, RunID: activeRunID, Status: stage.Status}
	if status == store.TurnCancelled {
		result.Status = erkaruntime.TaskCancelled
	}
	if status == store.TurnTimedOut {
		result.Status = erkaruntime.TaskTimedOut
	}
	result.Summary = checkpointSummary(stage.Checkpoint)
	return result, true, err
}

func pipelineUsesBridge(resolved config.Resolved) bool {
	pipeline, ok := resolved.Pipelines[resolved.ActivePipeline]
	if !ok {
		return false
	}
	for _, route := range pipeline.Stages {
		for _, reference := range route.Candidates {
			connection, _, _ := strings.Cut(reference, "/")
			if resolved.Connections[connection].Class == config.ConnectionSubscriptionBridge {
				return true
			}
		}
	}
	return false
}

func checkpointSummary(checkpoint erkaruntime.Checkpoint) string {
	switch {
	case len(checkpoint.Decisions) > 0:
		return checkpoint.Decisions[len(checkpoint.Decisions)-1]
	case len(checkpoint.ValidationResults) > 0:
		return checkpoint.ValidationResults[len(checkpoint.ValidationResults)-1]
	case checkpoint.DiffSummary != "":
		return checkpoint.DiffSummary
	default:
		return checkpoint.ApprovedPlan
	}
}

// mcpTools starts the configured MCP servers once per session and returns
// their tools. Servers that fail are reported once and skipped.
func (session *Session) mcpTools() []erkaruntime.Tool {
	if len(session.options.MCPServers) == 0 {
		return nil
	}
	session.mcpOnce.Do(func() {
		manager := mcp.Start(context.Background(), session.options.MCPServers, session.root, nil)
		session.mu.Lock()
		closed := session.closed
		session.mcp = manager
		session.mu.Unlock()
		if closed {
			_ = manager.Close()
			return
		}
		if session.options.Output != nil {
			for _, warning := range manager.Warnings() {
				_, _ = fmt.Fprintf(session.options.Output, "warning: %s\n", warning)
			}
		}
	})
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.mcp.Tools()
}

// MCPWarnings reports MCP servers or tools that could not be used.
func (session *Session) MCPWarnings() []string {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.mcp.Warnings()
}

// permissionPolicy builds the tool policy for one turn. Without configured
// rules it returns a nil policy, which keeps erka's original behavior: every
// exposed tool may run and run_command exists only with --full. An allow or
// ask rule for run_command exposes it without --full, restricted by policy.
func (session *Session) permissionPolicy(allowFullExecution bool) (erkaruntime.PermissionPolicy, tools.Options, error) {
	options := tools.Options{AllowFullExecution: allowFullExecution}
	if session.options.PermissionRules.Empty() {
		return nil, options, nil
	}
	policy, err := permissions.New(session.options.PermissionRules, allowFullExecution)
	if err != nil {
		return nil, options, err
	}
	options.AllowFullExecution = allowFullExecution || policy.ExposesCommand()
	return policy, options, nil
}

func stageTools(all []erkaruntime.Tool, stage routing.Stage) []erkaruntime.Tool {
	if stage == routing.StageCoding || stage == routing.StageRepair {
		return all
	}
	result := make([]erkaruntime.Tool, 0, len(all))
	for _, tool := range all {
		if !tool.Definition().Mutating {
			result = append(result, tool)
		}
	}
	return result
}

func (session *Session) Cancel() {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.cancel != nil {
		session.cancel()
	}
}

func (session *Session) SetFullExecution(enabled bool) {
	session.mu.Lock()
	session.options.AllowFullExecution = enabled
	session.mu.Unlock()
}

// SetModel changes one stage's primary connection/model route for subsequent
// turns in this process. The configured route remains unchanged and continues
// to supply deterministic fallbacks.
func (session *Session) SetModel(selection erkaruntime.ModelSelection) error {
	selection.Connection = strings.TrimSpace(selection.Connection)
	selection.Model = strings.TrimSpace(selection.Model)
	if err := (erkaruntime.ModelInfo{ID: selection.Model}).Validate(); err != nil {
		return fmt.Errorf("invalid model selection: %w", err)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.turnActive || session.cancel != nil {
		return errors.New("cancel the active turn before switching models")
	}
	if session.options.Pipeline.ActivePipeline != "" {
		pipeline, ok := session.options.Pipeline.Pipelines[session.options.Pipeline.ActivePipeline]
		if !ok {
			return errors.New("active pipeline is unavailable")
		}
		route, ok := pipeline.Stages[string(selection.Stage)]
		if !ok {
			return fmt.Errorf("stage %q is not configured", selection.Stage)
		}
		connectionFound := false
		for _, reference := range route.Candidates {
			connection, _, _ := strings.Cut(reference, "/")
			if connection == selection.Connection {
				connectionFound = true
				break
			}
		}
		if !connectionFound {
			return fmt.Errorf("connection %q is not routed for stage %q", selection.Connection, selection.Stage)
		}
		connection := session.options.Pipeline.Connections[selection.Connection]
		if connection.Class == config.ConnectionSubscriptionBridge && selection.Stage != erkaruntime.StagePlanning && selection.Stage != erkaruntime.StageReview {
			return fmt.Errorf("subscription bridge %q cannot run stage %q", selection.Connection, selection.Stage)
		}
		if route.Budget.MaxCostMicrounits > 0 {
			metadata, configured := connection.Models[selection.Model]
			priced := strings.TrimSpace(metadata.Currency) != "" && (metadata.InputCostPerMillionMicrounits > 0 || metadata.OutputCostPerMillionMicrounits > 0)
			if !configured || !priced {
				return fmt.Errorf("model %q requires configured pricing for stage %q cost enforcement", selection.Model, selection.Stage)
			}
		}
		if session.modelOverrides == nil {
			session.modelOverrides = make(map[erkaruntime.Stage]string)
		}
		selected := selection.Connection + "/" + selection.Model
		if len(route.Candidates) > 0 && route.Candidates[0] == selected {
			delete(session.modelOverrides, selection.Stage)
		} else {
			session.modelOverrides[selection.Stage] = selected
		}
		return nil
	}
	if selection.Stage != "" && selection.Stage != erkaruntime.StageCoding {
		return fmt.Errorf("single-provider sessions only support the coding stage")
	}
	if selection.Connection != "" && selection.Connection != "default" {
		return fmt.Errorf("single-provider sessions use the default connection")
	}
	if session.options.RebuildProvider == nil {
		return errors.New("model switching is unavailable in this session")
	}
	provider, err := session.options.RebuildProvider(selection.Model)
	if err != nil {
		return err
	}
	session.options.Provider = provider
	session.options.NativeModel = selection.Model
	return nil
}

func (session *Session) beginTurn() error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.turnActive {
		return errors.New("another turn is already active")
	}
	session.turnActive = true
	return nil
}

func (session *Session) endTurn() {
	session.mu.Lock()
	session.turnActive = false
	session.mu.Unlock()
}

func (session *Session) effectivePipeline() config.Resolved {
	session.mu.Lock()
	defer session.mu.Unlock()
	resolved := session.options.Pipeline
	if resolved.ActivePipeline == "" || len(session.modelOverrides) == 0 {
		return resolved
	}
	pipeline, ok := resolved.Pipelines[resolved.ActivePipeline]
	if !ok {
		return resolved
	}
	stages := make(map[string]config.StageRoute, len(pipeline.Stages))
	for name, route := range pipeline.Stages {
		copyRoute := route
		copyRoute.Candidates = append([]string(nil), route.Candidates...)
		stages[name] = copyRoute
	}
	for stage, selected := range session.modelOverrides {
		route, ok := stages[string(stage)]
		if !ok {
			continue
		}
		candidates := []string{selected}
		for _, candidate := range route.Candidates {
			if candidate != selected {
				candidates = append(candidates, candidate)
			}
		}
		route.Candidates = candidates
		stages[string(stage)] = route
	}
	pipeline.Stages = stages
	pipelines := make(map[string]config.Pipeline, len(resolved.Pipelines))
	for name, configured := range resolved.Pipelines {
		pipelines[name] = configured
	}
	pipelines[resolved.ActivePipeline] = pipeline
	resolved.Pipelines = pipelines
	return resolved
}

func (session *Session) maintainLease(ctx context.Context, cancel context.CancelFunc, done <-chan struct{}) {
	ticker := time.NewTicker(leaseDuration / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			now := session.options.Clock().UTC()
			ok, err := session.database.RenewLease(ctx, store.Lease{ProjectID: session.project, Holder: session.holder, UpdatedAt: now, ExpiresAt: now.Add(leaseDuration)})
			if err != nil || !ok {
				session.mu.Lock()
				session.readOnly = true
				session.mu.Unlock()
				cancel()
				return
			}
		}
	}
}

func (session *Session) Diff(ctx context.Context) (string, error) {
	result, err := session.sandbox.Run(ctx, erkaruntime.CommandRequest{Executable: "git", Arguments: []string{"diff", "--no-ext-diff", "--"}, Directory: ".", Timeout: 10 * time.Second, OutputLimitBytes: 1 << 20})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git diff: %s", strings.TrimSpace(result.StandardError))
	}
	return result.StandardOutput, nil
}

func (session *Session) Branch(ctx context.Context) string {
	result, err := session.sandbox.Run(ctx, erkaruntime.CommandRequest{Executable: "git", Arguments: []string{"branch", "--show-current"}, Directory: ".", Timeout: 5 * time.Second, OutputLimitBytes: 1024})
	if err != nil || result.ExitCode != 0 {
		return "detached"
	}
	branch := strings.TrimSpace(result.StandardOutput)
	if branch == "" {
		return "detached"
	}
	return branch
}

func (session *Session) UnknownOutcome(ctx context.Context, taskID erkaruntime.TaskID) (erkaruntime.ToolCall, bool, error) {
	items, err := session.database.LoadItems(ctx, taskID, 0)
	if err != nil {
		return erkaruntime.ToolCall{}, false, err
	}
	pending := make(map[erkaruntime.ToolCallID]erkaruntime.ToolCall)
	order := make([]erkaruntime.ToolCallID, 0)
	for _, item := range items {
		switch item.Kind {
		case erkaruntime.ItemToolCall:
			pending[item.ToolCall.CallID] = *item.ToolCall
			order = append(order, item.ToolCall.CallID)
		case erkaruntime.ItemToolResult:
			delete(pending, item.ToolResult.CallID)
		}
	}
	for index := len(order) - 1; index >= 0; index-- {
		if call, ok := pending[order[index]]; ok && call.Mutating {
			return call, true, nil
		}
	}
	return erkaruntime.ToolCall{}, false, nil
}

func (session *Session) ResolveUnknown(ctx context.Context, taskID erkaruntime.TaskID, success bool) error {
	call, ok, err := session.UnknownOutcome(ctx, taskID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("conversation has no unresolved mutating tool")
	}
	conversation, err := session.OpenConversation(ctx, taskID)
	if err != nil {
		return err
	}
	if conversation.Task.Status != erkaruntime.TaskWaiting {
		return errors.New("conversation is not waiting for recovery")
	}
	var run erkaruntime.Run
	for _, item := range conversation.Items {
		if item.Kind == erkaruntime.ItemToolCall && item.ToolCall.CallID == call.CallID {
			for _, candidate := range conversation.Runs {
				if candidate.ID == item.RunID {
					run = candidate
					break
				}
			}
			break
		}
	}
	if run.ID == "" || run.Status != erkaruntime.RunWaiting {
		return errors.New("unresolved tool run is not waiting")
	}
	itemID, err := session.options.IDs.NewID("item")
	if err != nil {
		return err
	}
	eventID, err := session.options.IDs.NewID("event")
	if err != nil {
		return err
	}
	now := session.options.Clock().UTC()
	resultText := "User confirmed the interrupted tool completed successfully."
	if !success {
		resultText = "User confirmed the interrupted tool did not complete."
	}
	_, err = session.database.Commit(ctx, erkaruntime.CommitRequest{TaskID: taskID, ExpectedItemSequence: conversation.Task.LastItemSequence, ExpectedEventSequence: conversation.Task.LastEventSequence, Items: []erkaruntime.RuntimeItem{{ID: erkaruntime.ItemID(itemID), Sequence: conversation.Task.LastItemSequence + 1, TaskID: taskID, RunID: run.ID, Kind: erkaruntime.ItemToolResult, Time: now, ToolResult: &erkaruntime.ToolResult{CallID: call.CallID, Output: resultText, IsError: !success}}}, Events: []erkaruntime.Event{{ID: erkaruntime.EventID(eventID), Sequence: conversation.Task.LastEventSequence + 1, TaskID: taskID, RunID: run.ID, Type: "tool.resolved", Time: now, Message: "Interrupted tool outcome recorded"}}, Transition: &erkaruntime.StateTransition{TaskID: taskID, RunID: run.ID, From: erkaruntime.TaskWaiting, To: erkaruntime.TaskCoding}, RunTransition: &erkaruntime.RunStateTransition{TaskID: taskID, RunID: run.ID, From: erkaruntime.RunWaiting, To: erkaruntime.RunRunning}})
	if err != nil {
		return err
	}
	for _, turn := range conversation.Turns {
		if turn.RunID == run.ID && turn.Status == store.TurnWaiting {
			_ = session.database.UpdateTurnStatus(ctx, turn.ID, store.TurnWaiting, store.TurnRunning, now)
		}
	}
	return nil
}

func (session *Session) persistInterruption(taskID erkaruntime.TaskID, runID erkaruntime.RunID, taskStatus erkaruntime.TaskStatus, runStatus erkaruntime.RunStatus, eventType string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	task, err := session.database.LoadTask(ctx, taskID)
	if err != nil {
		return
	}
	run, err := session.database.LoadRun(ctx, runID)
	if err != nil {
		return
	}
	id, err := session.options.IDs.NewID("event")
	if err != nil {
		return
	}
	now := session.options.Clock().UTC()
	_, _ = session.database.Commit(ctx, erkaruntime.CommitRequest{TaskID: taskID, ExpectedItemSequence: task.LastItemSequence, ExpectedEventSequence: task.LastEventSequence, Events: []erkaruntime.Event{{ID: erkaruntime.EventID(id), Sequence: task.LastEventSequence + 1, TaskID: taskID, RunID: runID, Type: eventType, Time: now}}, Transition: &erkaruntime.StateTransition{TaskID: taskID, RunID: runID, From: task.Status, To: taskStatus}, RunTransition: &erkaruntime.RunStateTransition{TaskID: taskID, RunID: runID, From: run.Status, To: runStatus}})
}
