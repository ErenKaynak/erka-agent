package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCompileStageContextExcludesPriorTranscriptAndKeepsCheckpoints(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	taskID := TaskID("task-1")
	runs := []Run{
		{ID: "run-plan", TaskID: taskID, Stage: string(StagePlanning)},
		{ID: "run-code", TaskID: taskID, Stage: string(StageCoding)},
		{ID: "run-review", TaskID: taskID, Stage: string(StageReview)},
	}
	items := []RuntimeItem{
		contextMessage("user", 1, taskID, "run-plan", ItemUserMessage, "fix checkout", now),
		contextMessage("plan-chat", 2, taskID, "run-plan", ItemAssistantMessage, "private planner chatter", now),
		contextCheckpoint("plan", 3, taskID, "run-plan", "approved steps", now),
		contextMessage("code-chat", 4, taskID, "run-code", ItemAssistantMessage, "private coding chatter", now),
		contextCheckpoint("code", 5, taskID, "run-code", "changed checkout.go", now),
		contextMessage("review-chat", 6, taskID, "run-review", ItemAssistantMessage, "current review", now),
	}
	packet, err := CompileStageContext(ContextRequest{Stage: StageReview, TaskID: taskID, RunID: "run-review", Items: items, Runs: runs, MaxBytes: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Items) != 4 {
		t.Fatalf("context items = %#v", packet.Items)
	}
	gotIDs := []ItemID{packet.Items[0].ID, packet.Items[1].ID, packet.Items[2].ID, packet.Items[3].ID}
	wantIDs := []ItemID{"user", "plan", "code", "review-chat"}
	for index := range wantIDs {
		if gotIDs[index] != wantIDs[index] {
			t.Fatalf("context IDs = %#v", gotIDs)
		}
	}
	for _, item := range packet.Items {
		if item.ID == "plan-chat" || item.ID == "code-chat" {
			t.Fatalf("prior transcript leaked into context: %#v", item)
		}
	}
}

func TestCompileStageContextUsesLatestTurnAndBoundsInput(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	items := []RuntimeItem{
		contextMessage("old", 1, "task-1", "run-old", ItemUserMessage, "old request", now),
		contextMessage("new", 2, "task-1", "run-plan", ItemUserMessage, strings.Repeat("x", 200), now),
	}
	_, err := CompileStageContext(ContextRequest{Stage: StagePlanning, TaskID: "task-1", RunID: "run-plan", Items: items, Runs: []Run{{ID: "run-plan", TaskID: "task-1", Stage: string(StagePlanning)}}, MaxBytes: 100})
	if !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("CompileStageContext() error = %v", err)
	}
	packet, err := CompileStageContext(ContextRequest{Stage: StagePlanning, TaskID: "task-1", RunID: "run-plan", Items: items, Runs: []Run{{ID: "run-plan", TaskID: "task-1", Stage: string(StagePlanning)}}, MaxBytes: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Items) != 1 || packet.Items[0].ID != "new" {
		t.Fatalf("latest objective context = %#v", packet.Items)
	}
}

func TestNewStageCheckpointUsesProviderNeutralFields(t *testing.T) {
	t.Parallel()
	plan, err := NewStageCheckpoint(StagePlanning, "fix", "1. inspect\n2. patch")
	if err != nil || plan.ApprovedPlan == "" || len(plan.Decisions) != 1 {
		t.Fatalf("planning checkpoint = %#v, %v", plan, err)
	}
	validation, err := NewStageCheckpoint(StageValidation, "fix", `{"status":"pass","failures":[],"notes":"ok"}`)
	if err != nil || len(validation.ValidationResults) != 1 || validation.DiffSummary != "" || !validation.Validation.Passed() {
		t.Fatalf("validation checkpoint = %#v, %v", validation, err)
	}
}

func TestNewStageCheckpointRecordsMalformedValidationAsFail(t *testing.T) {
	t.Parallel()
	checkpoint, err := NewStageCheckpoint(StageValidation, "fix", "PASS: tests passed")
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Validation == nil || checkpoint.Validation.Status != ValidationFail || len(checkpoint.Validation.Failures) != 1 {
		t.Fatalf("validation report = %#v", checkpoint.Validation)
	}
	if len(checkpoint.ValidationResults) != 2 || checkpoint.ValidationResults[1] != "PASS: tests passed" {
		t.Fatalf("validation results = %#v", checkpoint.ValidationResults)
	}
}

func contextMessage(id ItemID, sequence uint64, taskID TaskID, runID RunID, kind ItemKind, content string, now time.Time) RuntimeItem {
	return RuntimeItem{ID: id, Sequence: sequence, TaskID: taskID, RunID: runID, Kind: kind, Time: now, Message: &Message{Content: content}}
}

func contextCheckpoint(id ItemID, sequence uint64, taskID TaskID, runID RunID, summary string, now time.Time) RuntimeItem {
	return RuntimeItem{ID: id, Sequence: sequence, TaskID: taskID, RunID: runID, Kind: ItemCheckpoint, Time: now, Checkpoint: &Checkpoint{Objective: "fix checkout", DiffSummary: summary}}
}
