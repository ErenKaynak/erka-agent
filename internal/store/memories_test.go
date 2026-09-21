package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSearchMemoriesScopesResultsAndNeutralizesQuerySyntax(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	notes := []MemoryNote{
		{ID: "m1", Scope: MemoryGlobal, Kind: "preference", Origin: "user", Text: "Prefer table-driven tests", CreatedAt: now, UpdatedAt: now},
		{ID: "m2", Scope: MemoryProject, ProjectKey: "repo_a", Kind: "fact", Origin: "model", Text: "Tests run with make check", CreatedAt: now, UpdatedAt: now},
		{ID: "m3", Scope: MemoryProject, ProjectKey: "repo_b", Kind: "fact", Origin: "user", Text: "Tests run with npm test", CreatedAt: now, UpdatedAt: now},
	}
	for _, note := range notes {
		if _, err := db.SaveMemory(ctx, note); err != nil {
			t.Fatal(err)
		}
	}
	found, err := db.SearchMemories(ctx, "repo_a", "how do the tests run?", 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, note := range found {
		ids[note.ID] = true
	}
	if !ids["m1"] || !ids["m2"] || ids["m3"] {
		t.Fatalf("search results = %#v", found)
	}
	for _, hostile := range []string{`tests" OR text:*`, `NEAR(tests run) ^`, `***`, `""`} {
		if _, err := db.SearchMemories(ctx, "repo_a", hostile, 10); err != nil {
			t.Fatalf("SearchMemories(%q) error = %v", hostile, err)
		}
	}
	duplicate := notes[1]
	duplicate.ID, duplicate.UpdatedAt = "m2-again", now.Add(time.Hour)
	saved, err := db.SaveMemory(ctx, duplicate)
	if err != nil || saved.ID != "m2" {
		t.Fatalf("duplicate save = %#v, %v; want the existing memory", saved, err)
	}
	if err := db.DeleteMemory(ctx, "m2"); err != nil {
		t.Fatal(err)
	}
	if found, _ := db.SearchMemories(ctx, "repo_a", "make check", 10); len(found) != 0 {
		t.Fatalf("deleted memory still searchable: %#v", found)
	}
}
