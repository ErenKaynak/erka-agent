package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/app"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

type reviewFixture struct {
	service  *Service
	database *store.SQLite
	source   string
	task     store.ServerTask
}

// newReviewFixture runs one task to completion so it waits for review.
func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	ctx := context.Background()
	source, state := initSourceRepository(t), t.TempDir()
	database, err := store.OpenSQLite(ctx, filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	provider := &serverProvider{responses: [][]erkaruntime.ModelOutput{
		{{Kind: erkaruntime.ModelTextDelta, Text: "Done."}, {Kind: erkaruntime.ModelCompleted}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "Follow-up done."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	openSession := func(ctx context.Context, root string) (TaskSession, error) {
		return app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, NativeModel: "fixture"})
	}
	var failures []error
	service, err := NewService(ServiceOptions{StateDir: state, Store: database, RuntimeStore: database, OpenSession: openSession, OnError: func(err error) { failures = append(failures, err) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if len(failures) > 0 {
			t.Errorf("background failures: %v", failures)
		}
	})
	project, err := service.RegisterProject(ctx, "fixture", source, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.SubmitTask(ctx, project.ID, "Add a greeting file")
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := service.ExecuteNext(ctx, "worker-1"); err != nil || !worked {
		t.Fatalf("ExecuteNext() = %v, %v", worked, err)
	}
	return &reviewFixture{service: service, database: database, source: source, task: task}
}

func (fixture *reviewFixture) decide(t *testing.T, decision store.ApprovalStatus) store.ServerTask {
	t.Helper()
	ctx := context.Background()
	pending, err := fixture.database.ListApprovals(ctx, fixture.task.TaskID, store.ApprovalPending)
	if err != nil || len(pending) != 1 || pending[0].Kind != "accept_changes" {
		t.Fatalf("pending review = %#v, %v", pending, err)
	}
	if _, err := fixture.service.DecideApproval(ctx, store.ApprovalDecision{ID: pending[0].ID, Actor: "owner", Status: decision, Time: pending[0].RequestedAt.Add(1)}); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.database.LoadServerTask(ctx, fixture.task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func gitOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Agent", "GIT_AUTHOR_EMAIL=agent@example.test", "GIT_COMMITTER_NAME=Agent", "GIT_COMMITTER_EMAIL=agent@example.test")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptCommitsWorktreeToBranchAndClosesTask(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	worktree := fixture.task.WorktreeRoot
	// The agent committed one change itself and left another uncommitted.
	writeFile(t, filepath.Join(worktree, "agent.txt"), "committed by agent\n")
	gitOutput(t, worktree, "add", "agent.txt")
	gitOutput(t, worktree, "commit", "-qm", "agent commit")
	writeFile(t, filepath.Join(worktree, "greeting.txt"), "hello\n")
	writeFile(t, filepath.Join(worktree, "fixture.txt"), "changed\n")
	// A hostile hook in the shared repository must never run.
	hook := filepath.Join(fixture.source, ".git", "hooks", "pre-commit")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeFile(t, hook, "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(hook, 0o700); err != nil {
		t.Fatal(err)
	}

	task := fixture.decide(t, store.ApprovalApproved)
	branch := ResultBranch(task.TaskID)
	if task.Outcome != store.TaskAccepted || task.ResultRef != branch || task.ResultCommit == "" || task.FinalizedAt.IsZero() {
		t.Fatalf("accepted task = %#v", task)
	}
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree still exists: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("repository pre-commit hook ran during finalization")
	}
	if got := gitOutput(t, fixture.source, "show", branch+":greeting.txt"); got != "hello" {
		t.Fatalf("branch greeting = %q", got)
	}
	if got := gitOutput(t, fixture.source, "show", branch+":agent.txt"); got != "committed by agent" {
		t.Fatalf("branch lost the agent's own commit: %q", got)
	}
	if data, _ := os.ReadFile(filepath.Join(fixture.source, "fixture.txt")); string(data) != "source\n" {
		t.Fatalf("source working tree changed: %q", data)
	}
	diff, err := fixture.service.Diff(context.Background(), task.TaskID)
	if err != nil || !strings.Contains(diff, "+hello") || !strings.Contains(diff, "+committed by agent") || !strings.Contains(diff, "+changed") {
		t.Fatalf("recorded diff = %q, %v", diff, err)
	}
	if err := fixture.service.AddMessage(context.Background(), task.TaskID, "one more thing"); !errors.Is(err, erkaruntime.ErrConflict) {
		t.Fatalf("follow-up on accepted task error = %v, want conflict", err)
	}
}

func TestRejectKeepsChangesUnderHiddenRef(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	writeFile(t, filepath.Join(fixture.task.WorktreeRoot, "greeting.txt"), "hello\n")
	task := fixture.decide(t, store.ApprovalRejected)
	if task.Outcome != store.TaskRejected || !strings.HasPrefix(task.ResultRef, "refs/erka/rejected/") {
		t.Fatalf("rejected task = %#v", task)
	}
	if branches := gitOutput(t, fixture.source, "branch", "--list", "erka/*"); branches != "" {
		t.Fatalf("rejection created a branch: %q", branches)
	}
	if got := gitOutput(t, fixture.source, "show", task.ResultRef+":greeting.txt"); got != "hello" {
		t.Fatalf("rejected changes not recoverable: %q", got)
	}
	if _, err := os.Stat(fixture.task.WorktreeRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected worktree was not removed")
	}
}

func TestAcceptWithoutChangesClosesTaskWithoutBranch(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	task := fixture.decide(t, store.ApprovalApproved)
	if task.Outcome != store.TaskAccepted || task.ResultRef != "" || task.ResultCommit != "" {
		t.Fatalf("unchanged accepted task = %#v", task)
	}
	if branches := gitOutput(t, fixture.source, "branch", "--list", "erka/*"); branches != "" {
		t.Fatalf("unchanged task created a branch: %q", branches)
	}
}

func TestFinalizationRecoversAfterCrashBeforeRecording(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(fixture.task.WorktreeRoot, "greeting.txt"), "hello\n")
	pending, _ := fixture.database.ListApprovals(ctx, fixture.task.TaskID, store.ApprovalPending)
	if err := fixture.database.RecordApprovalDecision(ctx, store.ApprovalDecision{ID: pending[0].ID, Actor: "owner", Status: store.ApprovalApproved, Time: pending[0].RequestedAt.Add(1)}); err != nil {
		t.Fatal(err)
	}
	// The Git work completes, then the process dies before the outcome is stored.
	ref := "refs/heads/" + ResultBranch(fixture.task.TaskID)
	if _, err := (GitWorktrees{}).Finalize(ctx, FinalizeRequest{Source: fixture.source, Worktree: fixture.task.WorktreeRoot, Ref: ref, Message: "partial", BaseRef: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	fixture.service.finalizePending(ctx)
	task, err := fixture.database.LoadServerTask(ctx, fixture.task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Outcome != store.TaskAccepted || task.ResultRef != ResultBranch(task.TaskID) || task.ResultBase == "" || task.ResultCommit == "" {
		t.Fatalf("recovered task = %#v", task)
	}
	if diff, err := fixture.service.Diff(ctx, task.TaskID); err != nil || !strings.Contains(diff, "+hello") {
		t.Fatalf("recovered diff = %q, %v", diff, err)
	}
}

func TestFollowUpSupersedesPendingReview(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	ctx := context.Background()
	if err := fixture.service.AddMessage(ctx, fixture.task.TaskID, "also add a farewell"); err != nil {
		t.Fatal(err)
	}
	pending, err := fixture.database.ListApprovals(ctx, fixture.task.TaskID, store.ApprovalPending)
	if err != nil || len(pending) != 0 {
		t.Fatalf("stale review still pending: %#v, %v", pending, err)
	}
	if worked, err := fixture.service.ExecuteNext(ctx, "worker-1"); err != nil || !worked {
		t.Fatalf("follow-up ExecuteNext() = %v, %v", worked, err)
	}
	if pending, _ := fixture.database.ListApprovals(ctx, fixture.task.TaskID, store.ApprovalPending); len(pending) != 1 {
		t.Fatalf("follow-up did not request a fresh review: %#v", pending)
	}
}
