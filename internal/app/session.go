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
}

type Session struct {
	mu       sync.Mutex
	options  SessionOptions
	root     string
	project  erkaruntime.ProjectID
	database *store.SQLite
	sandbox  *sandbox.Local
	holder   string
	readOnly bool
	closed   bool
	cancel   context.CancelFunc
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
	return &Session{options: options, root: root, project: project, database: database, sandbox: localSandbox, holder: holder, readOnly: !acquired}, nil
}

func (session *Session) Root() string                     { return session.root }
func (session *Session) ProjectID() erkaruntime.ProjectID { return session.project }
func (session *Session) ReadOnly() bool                   { return session.readOnly }

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
	session.mu.Unlock()
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
	if session.readOnly {
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
		task := erkaruntime.Task{ID: taskID, ProjectID: session.project, Objective: prompt, Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now}
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
	if session.readOnly {
		return LocalResult{}, false, errors.New("repository is read-only while another erka process holds the execution lease")
	}
	if session.options.Provider == nil {
		return LocalResult{}, false, errors.New("local session requires a model provider")
	}
	now := session.options.Clock().UTC()
	renewed, err := session.database.RenewLease(ctx, store.Lease{ProjectID: session.project, Holder: session.holder, UpdatedAt: now, ExpiresAt: now.Add(leaseDuration)})
	if err != nil || !renewed {
		session.readOnly = true
		if err == nil {
			err = errors.New("repository execution lease was lost")
		}
		return LocalResult{}, false, err
	}
	value, err := session.options.IDs.NewID("run")
	if err != nil {
		return LocalResult{}, false, err
	}
	run := erkaruntime.Run{ID: erkaruntime.RunID(value), Stage: "coding", BackendClass: "native", Provider: session.options.Provider.Name(), Model: session.options.Provider.Name(), Status: erkaruntime.RunQueued, CreatedAt: now, UpdatedAt: now}
	tasks, err := session.database.ListTasks(ctx, session.project)
	if err != nil {
		return LocalResult{}, false, err
	}
	for _, task := range tasks {
		turns, loadErr := session.database.ListTurns(ctx, task.ID)
		if loadErr != nil {
			return LocalResult{}, false, loadErr
		}
		for _, turn := range turns {
			if turn.Status == store.TurnQueued {
				run.TaskID = turn.TaskID
				break
			}
		}
		if run.TaskID != "" {
			break
		}
	}
	if run.TaskID == "" {
		return LocalResult{}, false, nil
	}
	turn, ok, err := session.database.ClaimNextTurn(ctx, session.project, run, now)
	if err != nil || !ok {
		return LocalResult{}, ok, err
	}
	return session.execute(ctx, turn, run)
}

func (session *Session) Resume(ctx context.Context, taskID erkaruntime.TaskID) (LocalResult, error) {
	conversation, err := session.OpenConversation(ctx, taskID)
	if err != nil {
		return LocalResult{}, err
	}
	if len(conversation.Runs) == 0 {
		return LocalResult{}, errors.New("conversation has no run to resume")
	}
	run := conversation.Runs[len(conversation.Runs)-1]
	prompt := conversation.Task.Objective
	turn := store.Turn{TaskID: taskID, RunID: run.ID, Prompt: prompt, Status: store.TurnRunning}
	for _, candidate := range conversation.Turns {
		if candidate.RunID == run.ID {
			turn = candidate
			break
		}
	}
	result, _, err := session.execute(ctx, turn, run)
	return result, err
}

func (session *Session) execute(ctx context.Context, turn store.Turn, run erkaruntime.Run) (LocalResult, bool, error) {
	if turn.ID != "" && turn.Status == store.TurnClaimed {
		_ = session.database.UpdateTurnStatus(ctx, turn.ID, store.TurnClaimed, store.TurnRunning, session.options.Clock())
	}
	runCtx, cancel := context.WithCancel(ctx)
	session.mu.Lock()
	session.cancel = cancel
	session.mu.Unlock()
	defer func() { session.mu.Lock(); session.cancel = nil; session.mu.Unlock(); cancel() }()
	backend := &erkaruntime.NativeBackend{Provider: session.options.Provider, Tools: tools.Builtins(session.sandbox, tools.Options{AllowFullExecution: session.options.AllowFullExecution}), Sink: &writerSink{writer: session.options.Output}, Observer: session.options.Observer, Instructions: nativeInstructions, Limits: session.options.Limits, IDs: session.options.IDs, Clock: session.options.Clock}
	stage, err := backend.RunStage(runCtx, erkaruntime.StageRequest{TaskID: turn.TaskID, RunID: run.ID, Objective: turn.Prompt}, session.database)
	status := store.TurnCompleted
	if errors.Is(err, erkaruntime.ErrUnknownMutatingToolOutcome) {
		status = store.TurnWaiting
	} else if errors.Is(err, context.Canceled) {
		status = store.TurnCancelled
		session.persistInterruption(turn.TaskID, run.ID, erkaruntime.TaskCancelled, erkaruntime.RunCancelled, "task.cancelled")
	} else if errors.Is(err, context.DeadlineExceeded) {
		status = store.TurnTimedOut
		session.persistInterruption(turn.TaskID, run.ID, erkaruntime.TaskTimedOut, erkaruntime.RunTimedOut, "task.timed_out")
	} else if err != nil {
		status = store.TurnFailed
	}
	if turn.ID != "" {
		_ = session.database.UpdateTurnStatus(context.Background(), turn.ID, store.TurnRunning, status, session.options.Clock())
	}
	result := LocalResult{ProjectID: session.project, TaskID: turn.TaskID, RunID: run.ID, Status: stage.Status}
	if status == store.TurnCancelled {
		result.Status = erkaruntime.TaskCancelled
	}
	if status == store.TurnTimedOut {
		result.Status = erkaruntime.TaskTimedOut
	}
	if len(stage.Checkpoint.Decisions) > 0 {
		result.Summary = stage.Checkpoint.Decisions[len(stage.Checkpoint.Decisions)-1]
	}
	return result, true, err
}

func (session *Session) Cancel() {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.cancel != nil {
		session.cancel()
	}
}

func (session *Session) SetFullExecution(enabled bool) { session.options.AllowFullExecution = enabled }

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
