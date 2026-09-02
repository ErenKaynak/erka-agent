package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestConversationQueueClaimsInOrderAndListsRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	seedSQLite(t, ctx, db, now)
	for index, prompt := range []string{"follow one", "follow two"} {
		if err := db.EnqueueTurn(ctx, Turn{ID: "turn-" + prompt, TaskID: "task-1", Prompt: prompt, CreatedAt: now.Add(time.Duration(index+1) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := db.ListTurns(ctx, "task-1")
	if err != nil || len(turns) != 2 || turns[0].Prompt != "follow one" || turns[1].Prompt != "follow two" {
		t.Fatalf("turns = %#v, %v", turns, err)
	}
	run := erkaruntime.Run{ID: "run-2", TaskID: "task-1", Stage: "coding", BackendClass: "native", Provider: "fixture", Model: "fixture", Status: erkaruntime.RunQueued, CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second)}
	claimed, ok, err := db.ClaimNextTurn(ctx, "project-1", run, now.Add(3*time.Second))
	if err != nil || !ok || claimed.Prompt != "follow one" {
		t.Fatalf("claim = %#v, %v, %v", claimed, ok, err)
	}
	runs, err := db.ListRuns(ctx, "task-1")
	if err != nil || len(runs) != 2 || runs[1].ID != "run-2" {
		t.Fatalf("runs = %#v, %v", runs, err)
	}
}

func TestProjectLeaseExcludesCompetitorAndCanExpire(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	seedSQLite(t, ctx, db, now)
	ok, err := db.AcquireLease(ctx, Lease{ProjectID: "project-1", Holder: "one", UpdatedAt: now, ExpiresAt: now.Add(time.Minute)})
	if err != nil || !ok {
		t.Fatalf("first lease = %v, %v", ok, err)
	}
	ok, err = db.AcquireLease(ctx, Lease{ProjectID: "project-1", Holder: "two", UpdatedAt: now.Add(time.Second), ExpiresAt: now.Add(time.Minute)})
	if err != nil || ok {
		t.Fatalf("competing lease = %v, %v", ok, err)
	}
	ok, err = db.AcquireLease(ctx, Lease{ProjectID: "project-1", Holder: "two", UpdatedAt: now.Add(2 * time.Minute), ExpiresAt: now.Add(3 * time.Minute)})
	if err != nil || !ok {
		t.Fatalf("expired lease = %v, %v", ok, err)
	}
	ok, err = db.RenewLease(ctx, Lease{ProjectID: "project-1", Holder: "one", UpdatedAt: now.Add(2 * time.Minute), ExpiresAt: now.Add(4 * time.Minute)})
	if err != nil || ok {
		t.Fatalf("wrong-holder renew = %v, %v", ok, err)
	}
}

func TestEnqueueTurnRejectsMissingTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	err = db.EnqueueTurn(ctx, Turn{ID: "turn-1", TaskID: "missing", Prompt: "hello", CreatedAt: time.Now()})
	if !errors.Is(err, erkaruntime.ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}
