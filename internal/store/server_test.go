package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestServerProjectTaskClaimRecoveryAndApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC)
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := filepath.Join(t.TempDir(), "source")
	worktree := filepath.Join(t.TempDir(), "worktree")
	project := ServerProject{ID: "server-project-1", Name: "fixture", RepositoryRoot: repository, DefaultRef: "main", CreatedAt: now}
	if err := db.CreateServerProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProject(ctx, erkaruntime.Project{ID: "runtime-project-1", Root: worktree, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, erkaruntime.Task{ID: "task-1", ProjectID: "runtime-project-1", Objective: "fix", Pipeline: "default", Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateServerTask(ctx, ServerTask{TaskID: "task-1", ServerProjectID: project.ID, WorktreeRoot: worktree, Status: ServerTaskQueued, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := db.ClaimServerTask(ctx, "worker-one", now, time.Minute)
	if err != nil || !ok || claimed.Status != ServerTaskRunning || claimed.ClaimHolder != "worker-one" {
		t.Fatalf("first claim = %#v, %v, %v", claimed, ok, err)
	}
	if _, ok, err := db.ClaimServerTask(ctx, "worker-two", now.Add(30*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("live competing claim = %v, %v", ok, err)
	}
	recovered, ok, err := db.ClaimServerTask(ctx, "worker-two", now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok || recovered.ClaimHolder != "worker-two" {
		t.Fatalf("expired recovery = %#v, %v, %v", recovered, ok, err)
	}
	if err := db.FinishServerTask(ctx, "task-1", "worker-one", ServerTaskCompleted, now.Add(2*time.Minute)); !errors.Is(err, erkaruntime.ErrConflict) {
		t.Fatalf("stale worker finish = %v", err)
	}
	if err := db.FinishServerTask(ctx, "task-1", "worker-two", ServerTaskCompleted, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	approval := Approval{ID: "approval-1", TaskID: "task-1", Kind: "accept_changes", Message: "Accept the worktree diff", Status: ApprovalPending, RequestedAt: now.Add(3 * time.Minute), ExpiresAt: now.Add(time.Hour)}
	if err := db.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if err := db.DecideApproval(ctx, approval.ID, "owner", ApprovalApproved, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	stored, err := db.LoadApproval(ctx, approval.ID)
	if err != nil || stored.Status != ApprovalApproved || stored.Actor != "owner" || stored.DecidedAt.IsZero() {
		t.Fatalf("approval = %#v, %v", stored, err)
	}
	projects, err := db.ListServerProjects(ctx)
	if err != nil || len(projects) != 1 || projects[0].ID != project.ID {
		t.Fatalf("projects = %#v, %v", projects, err)
	}
}

func TestServerAPITokenScopesAndRevocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hash := sha256.Sum256([]byte("secret-token"))
	if err := db.CreateAPIToken(ctx, APIToken{ID: "token-1", Name: "read client", Hash: hash, Scopes: []string{"read"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	principal, err := db.AuthenticateAPIToken(ctx, hash, "read")
	if err != nil || principal.ID != "token-1" {
		t.Fatalf("read authentication = %#v, %v", principal, err)
	}
	if _, err := db.AuthenticateAPIToken(ctx, hash, "write"); err == nil {
		t.Fatal("read token received write scope")
	}
	if err := db.RevokeAPIToken(ctx, "token-1", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AuthenticateAPIToken(ctx, hash, "read"); err == nil {
		t.Fatal("revoked token authenticated")
	}
}
