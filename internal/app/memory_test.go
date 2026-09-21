package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/routing"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestMemoryReachesEveryProviderInAPipeline(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	planner := &scriptedProvider{name: "openai", responses: [][]erkaruntime.ModelOutput{
		{{Kind: erkaruntime.ModelTextDelta, Text: "Plan the arithmetic fix."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	coder := &scriptedProvider{name: "deepseek", responses: [][]erkaruntime.ModelOutput{
		{{Kind: erkaruntime.ModelTextDelta, Text: "Implemented."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	session, err := OpenSession(context.Background(), SessionOptions{
		WorkingDirectory: root, StateDir: state, Pipeline: testSessionPipeline(),
		ProviderFactory: func(_ context.Context, candidate routing.Candidate) (erkaruntime.ModelProvider, error) {
			switch candidate.Provider {
			case "openai":
				return planner, nil
			case "deepseek":
				return coder, nil
			}
			return nil, errors.New("unexpected provider")
		},
		IDs: &sequenceIDs{}, Clock: fixedClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.MemoryKey() == "" {
		t.Fatal("repository has no memory key")
	}
	now := fixedClock()
	for _, note := range []store.MemoryNote{
		{ID: "memory-global", Scope: store.MemoryGlobal, Kind: "preference", Origin: "user", Text: "Answer in concise British English", CreatedAt: now, UpdatedAt: now},
		{ID: "memory-project", Scope: store.MemoryProject, ProjectKey: session.MemoryKey(), Kind: "decision", Origin: "model", Text: "Arithmetic helpers live in add.go", CreatedAt: now, UpdatedAt: now},
		{ID: "memory-other", Scope: store.MemoryProject, ProjectKey: "repo_other", Kind: "fact", Origin: "user", Text: "Arithmetic belongs to another repository", CreatedAt: now, UpdatedAt: now},
	} {
		if _, err := session.Database().SaveMemory(context.Background(), note); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := session.Submit(context.Background(), "", "fix arithmetic"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.RunNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []*scriptedProvider{planner, coder} {
		requests := provider.Requests()
		if len(requests) == 0 {
			t.Fatalf("%s received no request", provider.name)
		}
		instructions := requests[0].Instructions
		if !strings.Contains(instructions, "concise British English") || !strings.Contains(instructions, "[project decision, unverified] Arithmetic helpers live in add.go") {
			t.Fatalf("%s instructions missing memory:\n%s", provider.name, instructions)
		}
		if strings.Contains(instructions, "another repository") {
			t.Fatalf("%s received another project's memory", provider.name)
		}
		hasSearch := false
		for _, tool := range requests[0].Tools {
			hasSearch = hasSearch || tool.Name == "memory_search"
		}
		if !hasSearch {
			t.Fatalf("%s was not offered memory_search", provider.name)
		}
	}
}
