package app

import (
	"context"
	"errors"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/routing"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestPlanModeIsReadOnlyAndRunsOnlyPlanning(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	planner := &scriptedProvider{name: "openai", responses: [][]erkaruntime.ModelOutput{{{Kind: erkaruntime.ModelTextDelta, Text: "Plan: change add.go."}, {Kind: erkaruntime.ModelCompleted}}}}
	coder := &scriptedProvider{name: "deepseek"}
	session, err := OpenSession(context.Background(), SessionOptions{
		WorkingDirectory: root, StateDir: state, Pipeline: testSessionPipeline(), IDs: &sequenceIDs{}, Clock: fixedClock, AllowFullExecution: true,
		ProviderFactory: func(_ context.Context, candidate routing.Candidate) (erkaruntime.ModelProvider, error) {
			if candidate.Provider == "openai" {
				return planner, nil
			}
			if candidate.Provider == "deepseek" {
				return coder, nil
			}
			return nil, errors.New("unexpected provider")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	session.SetPlanMode(true)
	if _, err := session.Submit(context.Background(), "", "fix arithmetic"); err != nil {
		t.Fatal(err)
	}
	result, _, err := session.RunNext(context.Background())
	if err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("RunNext() = %#v, %v", result, err)
	}
	if len(coder.Requests()) != 0 {
		t.Fatal("plan mode ran the coding stage")
	}
	for _, tool := range planner.Requests()[0].Tools {
		if tool.Mutating || tool.Name == "run_command" || tool.Name == "apply_patch" {
			t.Fatalf("plan mode offered mutating tool %q", tool.Name)
		}
	}
}
