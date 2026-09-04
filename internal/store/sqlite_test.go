package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestSQLiteDSNPlacesWindowsDriveInURLPath(t *testing.T) {
	t.Parallel()
	dsn := sqliteDSN(`C:\Users\erka\state\erka.db`)
	if !strings.HasPrefix(dsn, "file:///C:/Users/erka/state/erka.db?") {
		t.Fatalf("sqliteDSN() = %q", dsn)
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Query().Get("_txlock") != "immediate" {
		t.Fatalf("sqliteDSN() transaction locking = %q, %v", parsed.Query().Get("_txlock"), err)
	}
}

func TestSQLiteMigratesPersistsAndReopens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	filename := filepath.Join(t.TempDir(), "state with spaces", "erka.db")
	now := time.Date(2026, 9, 2, 12, 0, 0, 123, time.UTC)

	database, err := OpenSQLite(ctx, filename)
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	seedSQLite(t, ctx, database, now)
	if _, err := database.Commit(ctx, erkaruntime.CommitRequest{
		TaskID: "task-1",
		Events: []erkaruntime.Event{{
			ID: "event-1", Sequence: 1, TaskID: "task-1", RunID: "run-1",
			Type: "task.started", Time: now,
		}},
		Transition: &erkaruntime.StateTransition{
			TaskID: "task-1", RunID: "run-1",
			From: erkaruntime.TaskQueued, To: erkaruntime.TaskPreparing,
		},
	}); err != nil {
		t.Fatalf("prepare Commit() error = %v", err)
	}
	request := erkaruntime.CommitRequest{
		TaskID: "task-1", ExpectedEventSequence: 1,
		Items: []erkaruntime.RuntimeItem{
			toolCallItem(now, 1, "item-1", "call-1"),
			toolResultItem(now.Add(time.Second), 2, "item-2", "call-1"),
		},
		Events: []erkaruntime.Event{{
			ID: "event-2", Sequence: 2, TaskID: "task-1", RunID: "run-1",
			Type: "tool.completed", Time: now.Add(time.Second), Message: "read x",
			Data: json.RawMessage(`{"tool":"read_file"}`),
		}},
		Transition: &erkaruntime.StateTransition{
			TaskID: "task-1", RunID: "run-1",
			From: erkaruntime.TaskPreparing, To: erkaruntime.TaskCoding,
		},
		RunTransition: &erkaruntime.RunStateTransition{
			TaskID: "task-1", RunID: "run-1",
			From: erkaruntime.RunQueued, To: erkaruntime.RunRunning,
		},
	}
	if _, err := database.Commit(ctx, request); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := OpenSQLite(ctx, filename)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	items, err := reopened.LoadItems(ctx, "task-1", 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("LoadItems() = %#v, %v", items, err)
	}
	if items[0].ToolCall == nil || string(items[0].ToolCall.Arguments) != `{"path":"x"}` {
		t.Fatalf("tool call did not round trip: %#v", items[0])
	}
	events, err := reopened.LoadEvents(ctx, "task-1", 0)
	if err != nil || len(events) != 2 || string(events[1].Data) != `{"tool":"read_file"}` {
		t.Fatalf("LoadEvents() = %#v, %v", events, err)
	}
	snapshot, err := reopened.LoadTask(ctx, "task-1")
	if err != nil || snapshot.Status != erkaruntime.TaskCoding || snapshot.LastItemSequence != 2 || snapshot.Objective != "fix tests" || snapshot.Pipeline != "default" {
		t.Fatalf("LoadTask() = %#v, %v", snapshot, err)
	}
	project, err := reopened.LoadProject(ctx, snapshot.ProjectID)
	if err != nil || project.ID != "project-1" {
		t.Fatalf("LoadProject() = %#v, %v", project, err)
	}
	run, err := reopened.LoadRun(ctx, "run-1")
	if err != nil || !run.CreatedAt.Equal(now) || run.Status != erkaruntime.RunRunning || run.Connection != "scripted-main" || run.Attempt != 2 {
		t.Fatalf("LoadRun() = %#v, %v", run, err)
	}

	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(filename)
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("database mode = %v, %v", info.Mode().Perm(), statErr)
		}
	}
}

func TestSQLitePersistsIncompleteToolCallForRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	filename := filepath.Join(t.TempDir(), "erka.db")
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	database, err := OpenSQLite(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	seedSQLite(t, ctx, database, now)
	if _, err := database.Commit(ctx, erkaruntime.CommitRequest{
		TaskID: "task-1", Items: []erkaruntime.RuntimeItem{toolCallItem(now, 1, "item-1", "call-1")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	items, err := reopened.LoadItems(ctx, "task-1", 0)
	if err != nil || len(items) != 1 || items[0].ToolCall == nil || items[0].ToolCall.CallID != "call-1" {
		t.Fatalf("LoadItems() = %#v, %v", items, err)
	}
}

func TestSQLiteFailedCommitRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	seedSQLite(t, ctx, database, now)

	request := erkaruntime.CommitRequest{
		TaskID: "task-1",
		Items: []erkaruntime.RuntimeItem{
			toolCallItem(now, 1, "item-1", "call-1"),
			toolResultItem(now, 2, "item-2", "missing-call"),
		},
	}
	if _, err := database.Commit(ctx, request); err == nil {
		t.Fatal("Commit() error = nil, want transcript error")
	}
	snapshot, loadErr := database.LoadTask(ctx, "task-1")
	if loadErr != nil || snapshot.LastItemSequence != 0 {
		t.Fatalf("LoadTask() = %#v, %v", snapshot, loadErr)
	}
	items, loadErr := database.LoadItems(ctx, "task-1", 0)
	if loadErr != nil || len(items) != 0 {
		t.Fatalf("LoadItems() = %#v, %v", items, loadErr)
	}
}

func TestSQLiteRollsBackWhenLaterInsertFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	seedSQLite(t, ctx, database, now)
	first := erkaruntime.CommitRequest{
		TaskID: "task-1",
		Items:  []erkaruntime.RuntimeItem{messageItem(now, 1, "item-1")},
		Events: []erkaruntime.Event{{
			ID: "event-1", Sequence: 1, TaskID: "task-1", RunID: "run-1",
			Type: "task.started", Time: now,
		}},
	}
	if _, err := database.Commit(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := erkaruntime.CommitRequest{
		TaskID: "task-1", ExpectedItemSequence: 1, ExpectedEventSequence: 1,
		Items: []erkaruntime.RuntimeItem{messageItem(now.Add(time.Second), 2, "item-2")},
		Events: []erkaruntime.Event{{
			ID: "event-1", Sequence: 2, TaskID: "task-1", RunID: "run-1",
			Type: "task.progress", Time: now.Add(time.Second),
		}},
	}
	if _, err := database.Commit(ctx, second); !errors.Is(err, erkaruntime.ErrAlreadyExists) {
		t.Fatalf("Commit() error = %v, want ErrAlreadyExists", err)
	}
	items, err := database.LoadItems(ctx, "task-1", 0)
	if err != nil || len(items) != 1 || items[0].ID != "item-1" {
		t.Fatalf("LoadItems() after rollback = %#v, %v", items, err)
	}
	snapshot, err := database.LoadTask(ctx, "task-1")
	if err != nil || snapshot.LastItemSequence != 1 || snapshot.LastEventSequence != 1 {
		t.Fatalf("LoadTask() after rollback = %#v, %v", snapshot, err)
	}
}

func TestSQLiteRejectsSequenceConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	seedSQLite(t, ctx, database, now)

	if _, err := database.Commit(ctx, erkaruntime.CommitRequest{
		TaskID: "task-1", Items: []erkaruntime.RuntimeItem{messageItem(now, 1, "item-1")},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = database.Commit(ctx, erkaruntime.CommitRequest{
		TaskID: "task-1", Items: []erkaruntime.RuntimeItem{messageItem(now, 1, "item-2")},
	})
	if !errors.Is(err, erkaruntime.ErrConflict) {
		t.Fatalf("Commit() error = %v, want ErrConflict", err)
	}
}

func TestSQLiteRejectsMissingRelationships(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	task := erkaruntime.Task{
		ID: "task-1", ProjectID: "missing", Objective: "fix tests",
		Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now,
	}
	if err := database.CreateTask(ctx, task); !errors.Is(err, erkaruntime.ErrNotFound) {
		t.Fatalf("CreateTask() error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteHonorsCancellationAndRequiresAbsolutePath(t *testing.T) {
	t.Parallel()
	if _, err := OpenSQLite(context.Background(), "relative.db"); err == nil {
		t.Fatal("OpenSQLite() error = nil for relative path")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db")); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenSQLite() error = %v, want context.Canceled", err)
	}
}

func TestSQLiteRejectsDirtyMigrationState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	filename := filepath.Join(t.TempDir(), "erka.db")
	database, err := OpenSQLite(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE schema_migrations SET dirty = 1`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenSQLite(ctx, filename); err == nil || !strings.Contains(strings.ToLower(err.Error()), "dirty") {
		t.Fatalf("OpenSQLite() error = %v, want dirty migration failure", err)
	}
}

func seedSQLite(t *testing.T, ctx context.Context, database *SQLite, now time.Time) {
	t.Helper()
	project := erkaruntime.Project{ID: "project-1", Root: filepath.Join(t.TempDir(), "project"), CreatedAt: now}
	if err := database.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	task := erkaruntime.Task{
		ID: "task-1", ProjectID: project.ID, Objective: "fix tests",
		Pipeline: "default", Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now,
	}
	if err := database.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	run := erkaruntime.Run{
		ID: "run-1", TaskID: task.ID, Stage: "coding", BackendClass: "native",
		Connection: "scripted-main", Provider: "scripted", Model: "fixture", Attempt: 2, Status: erkaruntime.RunQueued,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := database.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
}
