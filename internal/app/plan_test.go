package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/routing"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestPlanModeUsesReadOnlyToolsAndPlanInstructions(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	provider := &scriptedProvider{name: "fixture", responses: [][]erkaruntime.ModelOutput{
		{{Kind: erkaruntime.ModelTextDelta, Text: "1. Fix Add in add.go. 2. Run go test."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	result, err := RunLocal(context.Background(), LocalOptions{
		WorkingDirectory: root, StateDir: t.TempDir(), Objective: "fix Add", Provider: provider,
		AllowFullExecution: true, PlanMode: true,
		IDs: &sequenceIDs{}, Clock: func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("RunLocal() = %#v, %v", result, err)
	}
	requests := provider.Requests()
	if len(requests) != 1 || !strings.Contains(requests[0].Instructions, "plan mode") {
		t.Fatalf("instructions = %q", requests[0].Instructions)
	}
	for _, definition := range requests[0].Tools {
		if definition.Mutating {
			t.Fatalf("plan mode exposed mutating tool %s", definition.Name)
		}
	}
	if len(requests[0].Tools) == 0 {
		t.Fatal("plan mode must keep read-only tools")
	}
}

func TestInteractivePlanDecisionRoutesThroughDecidePermission(t *testing.T) {
	t.Parallel()
	session := &Session{interactive: &interactiveApprover{}, interactivePlan: &interactiveApprover{kind: LivePlanRequested}, options: SessionOptions{PermissionMode: PermissionModeInteractive}}
	approver := session.planApprover()
	done := make(chan bool, 1)
	go func() {
		approved, _ := approver(context.Background(), routing.PlanApproval{TaskID: "task", RunID: "run-1", Plan: "the plan"})
		done <- approved
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if request, ok := session.interactivePlan.current(); ok {
			if request.Summary() != "the plan" || request.CallID != "plan:run-1" {
				t.Fatalf("request = %#v", request)
			}
			if err := session.DecidePermission(request.CallID, true); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("plan request was never published")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !<-done {
		t.Fatal("approved plan was reported as rejected")
	}
	if err := session.DecidePermission("unknown", true); err == nil {
		t.Fatal("deciding an unknown request succeeded")
	}
}
