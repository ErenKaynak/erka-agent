package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/app"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestServiceRunsQueuedTaskInIsolatedWorktreeAndCreatesApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := initSourceRepository(t)
	state := t.TempDir()
	database, err := store.OpenSQLite(ctx, filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	provider := &serverProvider{responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "Completed in isolated worktree."},
		{Kind: erkaruntime.ModelCompleted},
	}}}
	openSession := func(ctx context.Context, root string) (TaskSession, error) {
		return app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, NativeModel: "fixture"})
	}
	service, err := NewService(ServiceOptions{StateDir: state, Store: database, RuntimeStore: database, OpenSession: openSession})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.RegisterProject(ctx, "fixture", source, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.SubmitTask(ctx, project.ID, "inspect fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(task.WorktreeRoot, filepath.Join(state, "worktrees")+string(os.PathSeparator)) || task.WorktreeRoot == source {
		t.Fatalf("worktree root = %q", task.WorktreeRoot)
	}
	if _, err := os.Stat(filepath.Join(task.WorktreeRoot, "fixture.txt")); err != nil {
		t.Fatalf("worktree fixture: %v", err)
	}
	worked, err := service.ExecuteNext(ctx, "worker-1")
	if err != nil || !worked {
		t.Fatalf("ExecuteNext() = %v, %v", worked, err)
	}
	stored, err := database.LoadServerTask(ctx, task.TaskID)
	if err != nil || stored.Status != store.ServerTaskCompleted {
		t.Fatalf("stored task = %#v, %v", stored, err)
	}
	approvals, err := database.ListApprovals(ctx, task.TaskID, store.ApprovalPending)
	if err != nil || len(approvals) != 1 || approvals[0].Kind != "accept_changes" {
		t.Fatalf("approvals = %#v, %v", approvals, err)
	}
	conversation, err := openSession(ctx, task.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer conversation.Close()
	loaded, err := conversation.OpenConversation(ctx, task.TaskID)
	if err != nil || loaded.Task.Status != erkaruntime.TaskCompleted {
		t.Fatalf("conversation = %#v, %v", loaded, err)
	}
}

func TestGitWorktreeRejectsExistingDestinationAndKeepsSourceClean(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := initSourceRepository(t)
	destination := filepath.Join(t.TempDir(), "task")
	manager := GitWorktrees{}
	if err := manager.Create(ctx, source, "HEAD", destination); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Remove(context.Background(), source, destination) })
	if err := os.WriteFile(filepath.Join(destination, "fixture.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "new.txt"), []byte("new content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diff, err := manager.Diff(ctx, destination)
	if err != nil || !strings.Contains(diff, "+changed") || !strings.Contains(diff, "+new content") {
		t.Fatalf("diff = %q, %v", diff, err)
	}
	data, err := os.ReadFile(filepath.Join(source, "fixture.txt"))
	if err != nil || string(data) != "source\n" {
		t.Fatalf("source changed = %q, %v", data, err)
	}
	if err := manager.Create(ctx, source, "HEAD", destination); err == nil {
		t.Fatal("existing worktree destination was accepted")
	}
}

func TestServiceRenewsClaimWhileTaskIsRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := initSourceRepository(t)
	state := t.TempDir()
	database, err := store.OpenSQLite(ctx, filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	provider := &blockingServerProvider{started: make(chan struct{}), release: make(chan struct{})}
	openSession := func(ctx context.Context, root string) (TaskSession, error) {
		return app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, NativeModel: "fixture"})
	}
	service, err := NewService(ServiceOptions{
		StateDir:      state,
		Store:         database,
		RuntimeStore:  database,
		OpenSession:   openSession,
		// Long enough that one slow write under the race detector cannot
		// let the claim lapse, short enough to prove renewal below.
		ClaimDuration: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.RegisterProject(ctx, "fixture", source, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.SubmitTask(ctx, project.ID, "hold the claim")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		worked, executeErr := service.ExecuteNext(ctx, "worker-1")
		if executeErr == nil && !worked {
			executeErr = errors.New("worker did not claim queued task")
		}
		result <- executeErr
	}()
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		close(provider.release)
		t.Fatal("model execution did not start")
	}
	select {
	case <-time.After(2500 * time.Millisecond):
	case err := <-result:
		close(provider.release)
		t.Fatalf("worker completed before release: %v", err)
	}
	if claimed, ok, err := database.ClaimServerTask(ctx, "worker-2", time.Now().UTC(), time.Minute); err != nil || ok {
		close(provider.release)
		t.Fatalf("competing claim = %#v, %v, %v", claimed, ok, err)
	}
	close(provider.release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not complete after model release")
	}
	stored, err := database.LoadServerTask(ctx, task.TaskID)
	if err != nil || stored.Status != store.ServerTaskCompleted {
		t.Fatalf("stored task = %#v, %v", stored, err)
	}
}

func TestServiceRetryCreatesNewTurnAndRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := initSourceRepository(t)
	state := t.TempDir()
	database, err := store.OpenSQLite(ctx, filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	provider := &serverProvider{responses: [][]erkaruntime.ModelOutput{
		{{Kind: erkaruntime.ModelFailed, Error: &erkaruntime.RuntimeError{Code: "temporary", Message: "retry", Retryable: true}}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "recovered"}, {Kind: erkaruntime.ModelCompleted}},
	}}
	openSession := func(ctx context.Context, root string) (TaskSession, error) {
		return app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, NativeModel: "fixture"})
	}
	service, err := NewService(ServiceOptions{StateDir: state, Store: database, RuntimeStore: database, OpenSession: openSession})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.RegisterProject(ctx, "fixture", source, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.SubmitTask(ctx, project.ID, "retry the objective")
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := service.ExecuteNext(ctx, "worker-1"); err == nil || !worked {
		t.Fatalf("failed execution = %v, %v", worked, err)
	}
	failed, err := database.LoadServerTask(ctx, task.TaskID)
	if err != nil || failed.Status != store.ServerTaskFailed {
		t.Fatalf("failed server task = %#v, %v", failed, err)
	}
	if err := service.RetryTask(ctx, task.TaskID); err != nil {
		t.Fatal(err)
	}
	if worked, err := service.ExecuteNext(ctx, "worker-2"); err != nil || !worked {
		t.Fatalf("retry execution = %v, %v", worked, err)
	}
	session, err := openSession(ctx, task.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	conversation, err := session.OpenConversation(ctx, task.TaskID)
	if err != nil || conversation.Task.Status != erkaruntime.TaskCompleted || len(conversation.Runs) != 2 || len(conversation.Turns) != 2 {
		t.Fatalf("retried conversation = %#v, %v", conversation, err)
	}
}

func initSourceRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"git", "init", "-q"},
		{"git", "add", "fixture.txt"},
		{"git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "initial"},
	}
	for _, command := range commands {
		process := exec.Command(command[0], command[1:]...)
		process.Dir = root
		if output, err := process.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", command, err, output)
		}
	}
	return root
}

type serverProvider struct {
	mu        sync.Mutex
	responses [][]erkaruntime.ModelOutput
}

func (*serverProvider) Name() string { return "fixture" }
func (*serverProvider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true}, nil
}
func (provider *serverProvider) Stream(context.Context, erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.responses) == 0 {
		return nil, fmt.Errorf("no scripted response")
	}
	outputs := provider.responses[0]
	provider.responses = provider.responses[1:]
	return &serverStream{outputs: outputs}, nil
}

type serverStream struct {
	outputs []erkaruntime.ModelOutput
	index   int
}

func (stream *serverStream) Next(context.Context) (erkaruntime.ModelOutput, error) {
	if stream.index >= len(stream.outputs) {
		return erkaruntime.ModelOutput{}, fmt.Errorf("stream exhausted")
	}
	output := stream.outputs[stream.index]
	stream.index++
	return output, nil
}
func (*serverStream) Close() error { return nil }

type blockingServerProvider struct {
	started chan struct{}
	release chan struct{}
}

func (*blockingServerProvider) Name() string { return "blocking-fixture" }
func (*blockingServerProvider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true}, nil
}
func (provider *blockingServerProvider) Stream(context.Context, erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	return &blockingServerStream{started: provider.started, release: provider.release}, nil
}

type blockingServerStream struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	index   int
}

func (stream *blockingServerStream) Next(ctx context.Context) (erkaruntime.ModelOutput, error) {
	if stream.index == 0 {
		stream.once.Do(func() { close(stream.started) })
		select {
		case <-ctx.Done():
			return erkaruntime.ModelOutput{}, ctx.Err()
		case <-stream.release:
		}
		stream.index++
		return erkaruntime.ModelOutput{Kind: erkaruntime.ModelTextDelta, Text: "lease renewed"}, nil
	}
	stream.index++
	return erkaruntime.ModelOutput{Kind: erkaruntime.ModelCompleted}, nil
}

func (*blockingServerStream) Close() error { return nil }
