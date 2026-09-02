package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestSessionRunsQueuedFollowUpsWithConversationHistory(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	provider := &scriptedProvider{name: "fixture", responses: [][]erkaruntime.ModelOutput{
		{{Kind: erkaruntime.ModelTextDelta, Text: "first answer"}, {Kind: erkaruntime.ModelCompleted}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "second answer"}, {Kind: erkaruntime.ModelCompleted}},
	}}
	session, err := OpenSession(context.Background(), SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, IDs: &sequenceIDs{}, Clock: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	taskID, err := session.Submit(context.Background(), "", "first prompt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Submit(context.Background(), taskID, "second prompt"); err != nil {
		t.Fatal(err)
	}
	first, ok, err := session.RunNext(context.Background())
	if err != nil || !ok || first.Status != erkaruntime.TaskCompleted {
		t.Fatalf("first = %#v, %v, %v", first, ok, err)
	}
	second, ok, err := session.RunNext(context.Background())
	if err != nil || !ok || second.Status != erkaruntime.TaskCompleted || second.RunID == first.RunID {
		t.Fatalf("second = %#v, %v, %v", second, ok, err)
	}
	conversation, err := session.OpenConversation(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversation.Turns) != 2 || len(conversation.Runs) != 2 {
		t.Fatalf("conversation = %#v", conversation)
	}
	requests := provider.Requests()
	if len(requests) != 2 || len(requests[1].Items) < 3 || requests[1].Items[len(requests[1].Items)-1].Message.Content != "second prompt" {
		t.Fatalf("second request history = %#v", requests)
	}
}

func TestSessionResolvesUnknownMutationBeforeResume(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	taskID := seedPendingCall(t, root, state, erkaruntime.ToolCall{CallID: "pending-patch", Tool: "apply_patch", Arguments: json.RawMessage(`{"patch":"--- a/add.go\\n+++ b/add.go\\n"}`), Mutating: true, IdempotencyKey: "task/run/pending-patch"})
	_, err := RunLocal(context.Background(), LocalOptions{WorkingDirectory: root, StateDir: state, ResumeTaskID: taskID, Provider: &scriptedProvider{name: "fixture"}, IDs: &sequenceIDs{}, Clock: fixedClock})
	if err == nil {
		t.Fatal("unknown mutation did not pause")
	}
	provider := &scriptedProvider{name: "fixture", responses: [][]erkaruntime.ModelOutput{{{Kind: erkaruntime.ModelTextDelta, Text: "recovered"}, {Kind: erkaruntime.ModelCompleted}}}}
	session, err := OpenSession(context.Background(), SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, IDs: &sequenceIDs{}, Clock: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call, ok, err := session.UnknownOutcome(context.Background(), taskID)
	if err != nil || !ok || call.CallID != "pending-patch" {
		t.Fatalf("unknown = %#v, %v, %v", call, ok, err)
	}
	if err := session.ResolveUnknown(context.Background(), taskID, true); err != nil {
		t.Fatal(err)
	}
	result, err := session.Resume(context.Background(), taskID)
	if err != nil || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("resume = %#v, %v", result, err)
	}
	if requests := provider.Requests(); len(requests) != 1 || !hasResult(requests[0].Items, "pending-patch") {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestSecondSessionIsReadOnlyWhileLeaseIsHeld(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	options := SessionOptions{WorkingDirectory: filepath.Join(root, "nested"), StateDir: state, IDs: &sequenceIDs{}, Clock: fixedClock}
	first, err := OpenSession(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenSession(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.ReadOnly() || !second.ReadOnly() {
		t.Fatalf("read-only state = %v/%v", first.ReadOnly(), second.ReadOnly())
	}
}
