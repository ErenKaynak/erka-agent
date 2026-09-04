package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/app"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

const defaultClaimDuration = 30 * time.Second

type ControlStore interface {
	CreateServerProject(context.Context, store.ServerProject) error
	LoadServerProject(context.Context, string) (store.ServerProject, error)
	ListServerProjects(context.Context) ([]store.ServerProject, error)
	CreateServerTask(context.Context, store.ServerTask) error
	LoadServerTask(context.Context, erkaruntime.TaskID) (store.ServerTask, error)
	ListServerTasks(context.Context, string) ([]store.ServerTask, error)
	ClaimServerTask(context.Context, string, time.Time, time.Duration) (store.ServerTask, bool, error)
	RenewServerTaskClaim(context.Context, erkaruntime.TaskID, string, time.Time, time.Duration) (bool, error)
	FinishServerTask(context.Context, erkaruntime.TaskID, string, store.ServerTaskStatus, time.Time) error
	CompleteServerTaskWithApproval(context.Context, erkaruntime.TaskID, string, store.Approval) error
	ReleaseServerTaskClaim(context.Context, erkaruntime.TaskID, string, time.Time) error
	QueueServerTask(context.Context, erkaruntime.TaskID, time.Time) error
	CancelServerTask(context.Context, erkaruntime.TaskID, time.Time) (bool, error)
	CreateApproval(context.Context, store.Approval) error
	LoadApproval(context.Context, string) (store.Approval, error)
	ListApprovals(context.Context, erkaruntime.TaskID, store.ApprovalStatus) ([]store.Approval, error)
	DecideApproval(context.Context, string, string, store.ApprovalStatus, time.Time) error
}

type TaskSession interface {
	Submit(context.Context, erkaruntime.TaskID, string) (erkaruntime.TaskID, error)
	RunNext(context.Context) (app.LocalResult, bool, error)
	Resume(context.Context, erkaruntime.TaskID) (app.LocalResult, error)
	OpenConversation(context.Context, erkaruntime.TaskID) (app.Conversation, error)
	Cancel()
	Close() error
}

type SessionFactory func(context.Context, string) (TaskSession, error)

type ServiceOptions struct {
	StateDir       string
	Store          ControlStore
	RuntimeStore   erkaruntime.Store
	Worktrees      WorktreeManager
	OpenSession    SessionFactory
	IDs            erkaruntime.IDGenerator
	Clock          func() time.Time
	ClaimDuration  time.Duration
	WorkerPoll     time.Duration
	ApprovalExpiry time.Duration
}

type Service struct {
	stateDir       string
	store          ControlStore
	runtimeStore   erkaruntime.Store
	worktrees      WorktreeManager
	openSession    SessionFactory
	ids            erkaruntime.IDGenerator
	clock          func() time.Time
	claimDuration  time.Duration
	workerPoll     time.Duration
	approvalExpiry time.Duration
	wake           chan struct{}
	mu             sync.Mutex
	active         map[erkaruntime.TaskID]TaskSession
}

func NewService(options ServiceOptions) (*Service, error) {
	if !filepath.IsAbs(options.StateDir) || options.Store == nil || options.RuntimeStore == nil || options.OpenSession == nil {
		return nil, errors.New("server service requires absolute state directory, stores, and session factory")
	}
	if options.Worktrees == nil {
		options.Worktrees = GitWorktrees{}
	}
	if options.IDs == nil {
		options.IDs = erkaruntime.RandomIDGenerator{}
	}
	if options.Clock == nil {
		options.Clock = func() time.Time { return time.Now().UTC() }
	}
	if options.ClaimDuration <= 0 {
		options.ClaimDuration = defaultClaimDuration
	}
	if options.WorkerPoll <= 0 {
		options.WorkerPoll = time.Second
	}
	if options.ApprovalExpiry <= 0 {
		options.ApprovalExpiry = 7 * 24 * time.Hour
	}
	return &Service{stateDir: filepath.Clean(options.StateDir), store: options.Store, runtimeStore: options.RuntimeStore, worktrees: options.Worktrees, openSession: options.OpenSession, ids: options.IDs, clock: options.Clock, claimDuration: options.ClaimDuration, workerPoll: options.WorkerPoll, approvalExpiry: options.ApprovalExpiry, wake: make(chan struct{}, 1), active: make(map[erkaruntime.TaskID]TaskSession)}, nil
}

func (service *Service) RegisterProject(ctx context.Context, name, repositoryRoot, defaultRef string) (store.ServerProject, error) {
	name, defaultRef = strings.TrimSpace(name), strings.TrimSpace(defaultRef)
	if name == "" {
		return store.ServerProject{}, errors.New("server project name is empty")
	}
	root, err := app.DiscoverRepository(repositoryRoot)
	if err != nil {
		return store.ServerProject{}, err
	}
	if defaultRef == "" {
		defaultRef = "HEAD"
	}
	if err := service.worktrees.Validate(ctx, root, defaultRef); err != nil {
		return store.ServerProject{}, err
	}
	id, err := service.ids.NewID("project")
	if err != nil {
		return store.ServerProject{}, err
	}
	project := store.ServerProject{ID: id, Name: name, RepositoryRoot: root, DefaultRef: defaultRef, CreatedAt: service.clock().UTC()}
	if err := service.store.CreateServerProject(ctx, project); err != nil {
		return store.ServerProject{}, err
	}
	return project, nil
}

func (service *Service) SubmitTask(ctx context.Context, projectID, objective string) (store.ServerTask, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return store.ServerTask{}, errors.New("task objective is empty")
	}
	project, err := service.store.LoadServerProject(ctx, projectID)
	if err != nil {
		return store.ServerTask{}, err
	}
	checkoutID, err := service.ids.NewID("worktree")
	if err != nil {
		return store.ServerTask{}, err
	}
	if !safePathSegment(checkoutID) || !safePathSegment(project.ID) {
		return store.ServerTask{}, errors.New("generated worktree identifier is not path-safe")
	}
	worktree := filepath.Join(service.stateDir, "worktrees", project.ID, checkoutID)
	if err := service.worktrees.Create(ctx, project.RepositoryRoot, project.DefaultRef, worktree); err != nil {
		return store.ServerTask{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = service.worktrees.Remove(context.Background(), project.RepositoryRoot, worktree)
		}
	}()
	session, err := service.openSession(ctx, worktree)
	if err != nil {
		return store.ServerTask{}, err
	}
	taskID, submitErr := session.Submit(ctx, "", objective)
	closeErr := session.Close()
	if submitErr != nil {
		return store.ServerTask{}, submitErr
	}
	if closeErr != nil {
		return store.ServerTask{}, closeErr
	}
	now := service.clock().UTC()
	task := store.ServerTask{TaskID: taskID, ServerProjectID: project.ID, WorktreeRoot: worktree, Status: store.ServerTaskQueued, CreatedAt: now, UpdatedAt: now}
	if err := service.store.CreateServerTask(ctx, task); err != nil {
		return store.ServerTask{}, err
	}
	keep = true
	service.Wake()
	return task, nil
}

func (service *Service) AddMessage(ctx context.Context, taskID erkaruntime.TaskID, prompt string) error {
	task, err := service.store.LoadServerTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Status == store.ServerTaskRunning || task.Status == store.ServerTaskQueued || task.Status == store.ServerTaskCancelled {
		return fmt.Errorf("task %q cannot accept a message while %s", taskID, task.Status)
	}
	session, err := service.openSession(ctx, task.WorktreeRoot)
	if err != nil {
		return err
	}
	defer session.Close()
	if _, err := session.Submit(ctx, taskID, prompt); err != nil {
		return err
	}
	if err := service.store.QueueServerTask(ctx, taskID, service.clock().UTC()); err != nil {
		return err
	}
	service.Wake()
	return nil
}

func (service *Service) CancelTask(ctx context.Context, taskID erkaruntime.TaskID) error {
	service.mu.Lock()
	session := service.active[taskID]
	service.mu.Unlock()
	if session != nil {
		session.Cancel()
		return nil
	}
	cancelled, err := service.store.CancelServerTask(ctx, taskID, service.clock().UTC())
	if err != nil {
		return err
	}
	if !cancelled {
		return erkaruntime.ErrConflict
	}
	return nil
}

func (service *Service) Diff(ctx context.Context, taskID erkaruntime.TaskID) (string, error) {
	task, err := service.store.LoadServerTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	return service.worktrees.Diff(ctx, task.WorktreeRoot)
}

func (service *Service) Wake() {
	select {
	case service.wake <- struct{}{}:
	default:
	}
}

func (service *Service) RunWorker(ctx context.Context, holder string) error {
	if strings.TrimSpace(holder) == "" {
		return errors.New("worker holder is empty")
	}
	ticker := time.NewTicker(service.workerPoll)
	defer ticker.Stop()
	for {
		worked, err := service.ExecuteNext(ctx, holder)
		if err != nil && ctx.Err() == nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-service.wake:
		case <-ticker.C:
		}
	}
}

func (service *Service) ExecuteNext(ctx context.Context, holder string) (bool, error) {
	now := service.clock().UTC()
	task, ok, err := service.store.ClaimServerTask(ctx, holder, now, service.claimDuration)
	if err != nil || !ok {
		return ok, err
	}
	executionCtx, cancelExecution := context.WithCancel(ctx)
	claimCtx, cancelClaim := context.WithCancel(ctx)
	claimDone := make(chan error, 1)
	go func() {
		claimDone <- service.maintainTaskClaim(claimCtx, cancelExecution, task.TaskID, holder)
	}()
	var stopOnce sync.Once
	var claimErr error
	stopClaim := func() error {
		stopOnce.Do(func() {
			cancelClaim()
			claimErr = <-claimDone
			cancelExecution()
		})
		return claimErr
	}
	defer stopClaim()
	finish := func(status store.ServerTaskStatus) error {
		if err := stopClaim(); err != nil {
			return err
		}
		return service.store.FinishServerTask(context.Background(), task.TaskID, holder, status, service.clock().UTC())
	}
	release := func() error {
		if err := stopClaim(); err != nil {
			return err
		}
		return service.store.ReleaseServerTaskClaim(context.Background(), task.TaskID, holder, service.clock().UTC())
	}

	session, err := service.openSession(executionCtx, task.WorktreeRoot)
	if err != nil {
		_ = release()
		return true, err
	}
	service.mu.Lock()
	service.active[task.TaskID] = session
	service.mu.Unlock()
	defer func() {
		service.mu.Lock()
		delete(service.active, task.TaskID)
		service.mu.Unlock()
		_ = session.Close()
	}()

	conversation, err := session.OpenConversation(executionCtx, task.TaskID)
	if err != nil {
		_ = finish(store.ServerTaskFailed)
		return true, err
	}
	if conversation.Task.Status == erkaruntime.TaskCompleted {
		if err := stopClaim(); err != nil {
			return true, err
		}
		return true, service.completeClaim(context.Background(), task.TaskID, holder)
	}
	if conversation.Task.Status == erkaruntime.TaskFailed || conversation.Task.Status == erkaruntime.TaskCancelled || conversation.Task.Status == erkaruntime.TaskTimedOut {
		return true, finish(store.ServerTaskFailed)
	}

	var result app.LocalResult
	if len(conversation.Runs) == 0 {
		var claimed bool
		result, claimed, err = session.RunNext(executionCtx)
		if err == nil && !claimed {
			err = errors.New("queued server task had no claimable turn")
		}
	} else {
		result, err = session.Resume(executionCtx, task.TaskID)
	}
	if err != nil {
		if ctx.Err() != nil {
			_ = release()
			return true, ctx.Err()
		}
		if executionCtx.Err() != nil {
			return true, stopClaim()
		}
		conversation, loadErr := session.OpenConversation(context.Background(), task.TaskID)
		if loadErr == nil && conversation.Task.Status == erkaruntime.TaskWaiting {
			return true, finish(store.ServerTaskWaiting)
		}
		_ = finish(store.ServerTaskFailed)
		return true, err
	}
	switch result.Status {
	case erkaruntime.TaskCompleted:
		if err := stopClaim(); err != nil {
			return true, err
		}
		return true, service.completeClaim(context.Background(), task.TaskID, holder)
	case erkaruntime.TaskWaiting:
		return true, finish(store.ServerTaskWaiting)
	case erkaruntime.TaskCancelled:
		return true, finish(store.ServerTaskCancelled)
	default:
		return true, finish(store.ServerTaskFailed)
	}
}

func (service *Service) maintainTaskClaim(ctx context.Context, cancelExecution context.CancelFunc, taskID erkaruntime.TaskID, holder string) error {
	interval := service.claimDuration / 3
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			now := service.clock().UTC()
			ok, err := service.store.RenewServerTaskClaim(ctx, taskID, holder, now, service.claimDuration)
			if err != nil {
				cancelExecution()
				return err
			}
			if !ok {
				cancelExecution()
				return errors.New("server task claim was lost")
			}
		}
	}
}

func (service *Service) completeClaim(ctx context.Context, taskID erkaruntime.TaskID, holder string) error {
	now := service.clock().UTC()
	id, err := service.ids.NewID("approval")
	if err != nil {
		return err
	}
	return service.store.CompleteServerTaskWithApproval(ctx, taskID, holder, store.Approval{ID: id, TaskID: taskID, Kind: "accept_changes", Message: "Review and accept the completed worktree diff.", Status: store.ApprovalPending, RequestedAt: now, ExpiresAt: now.Add(service.approvalExpiry)})
}

func safePathSegment(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !strings.ContainsAny(value, `/\\`)
}

var _ ControlStore = (*store.SQLite)(nil)
var _ TaskSession = (*app.Session)(nil)
