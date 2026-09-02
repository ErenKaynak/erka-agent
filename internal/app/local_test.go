package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestRunLocalCompletesFixtureAndPersistsState(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)

	patch := "diff --git a/add.go b/add.go\n--- a/add.go\n+++ b/add.go\n@@ -2 +2 @@\n-func Add(a, b int) int { return a - b }\n+func Add(a, b int) int { return a + b }\n"
	provider := &scriptedProvider{name: "fixture", responses: [][]erkaruntime.ModelOutput{
		toolResponse("call-list", "list_files", `{}`),
		toolResponse("call-read", "read_file", `{"path":"add.go"}`),
		toolResponse("call-patch", "apply_patch", string(mustJSON(t, map[string]string{"patch": patch}))),
		toolResponse("call-test", "run_command", `{"command":["go","test","./..."]}`),
		toolResponse("call-diff", "git_diff", `{}`),
		{{Kind: erkaruntime.ModelTextDelta, Text: "Fixed Add and verified the Go test."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	var output strings.Builder
	result, err := RunLocal(context.Background(), LocalOptions{
		WorkingDirectory: filepath.Join(root, "nested"), StateDir: state,
		Objective: "fix Add", Provider: provider, Output: &output,
		AllowFullExecution: true,
		IDs:                &sequenceIDs{}, Clock: func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("RunLocal() error = %v\n%s", err, output.String())
	}
	if result.Status != erkaruntime.TaskCompleted || result.Summary != "Fixed Add and verified the Go test." {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(output.String(), "[task.completed] Task completed") || !strings.Contains(output.String(), "task: task_001") {
		t.Fatalf("output = %q", output.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "add.go"))
	if err != nil || !strings.Contains(string(data), "a + b") {
		t.Fatalf("add.go = %q, %v", data, err)
	}

	database, err := store.OpenSQLite(context.Background(), filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	snapshot, err := database.LoadTask(context.Background(), result.TaskID)
	if err != nil || snapshot.Status != erkaruntime.TaskCompleted {
		t.Fatalf("task = %#v, %v", snapshot, err)
	}
	items, err := database.LoadItems(context.Background(), result.TaskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls, results int
	for _, item := range items {
		switch item.Kind {
		case erkaruntime.ItemToolCall:
			calls++
		case erkaruntime.ItemToolResult:
			results++
		}
	}
	if calls != 5 || results != 5 {
		t.Fatalf("tool calls/results = %d/%d", calls, results)
	}
	requests := provider.Requests()
	if len(requests) != 6 || !hasResult(requests[5].Items, "call-diff") {
		t.Fatalf("provider requests = %d; final request missing durable diff result", len(requests))
	}
}

func TestDiscoverRepositoryRejectsNonRepository(t *testing.T) {
	t.Parallel()
	if _, err := DiscoverRepository(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no Git repository") {
		t.Fatalf("DiscoverRepository() error = %v", err)
	}
}

func writeFixture(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"go.mod":      "module fixture\n\ngo 1.26.0\n",
		"add.go":      "package fixture\nfunc Add(a, b int) int { return a - b }\n",
		"add_test.go": "package fixture\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2, 3) != 5 { t.Fatal(\"wrong\") } }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func initRepository(t *testing.T, root string) {
	t.Helper()
	for _, arguments := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.name", "Fixture"},
		{"config", "user.email", "fixture@example.invalid"},
		{"add", "."}, {"commit", "-m", "initial"},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
}

func toolResponse(callID erkaruntime.ToolCallID, name, arguments string) []erkaruntime.ModelOutput {
	return []erkaruntime.ModelOutput{
		{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: callID, Tool: name, ArgumentsFragment: arguments}},
		{Kind: erkaruntime.ModelCompleted},
	}
}

type scriptedProvider struct {
	name      string
	mu        sync.Mutex
	responses [][]erkaruntime.ModelOutput
	requests  []erkaruntime.ModelRequest
}

func (provider *scriptedProvider) Name() string { return provider.name }
func (provider *scriptedProvider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true}, nil
}
func (provider *scriptedProvider) Stream(_ context.Context, request erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.requests = append(provider.requests, request)
	if len(provider.responses) == 0 {
		return nil, io.EOF
	}
	response := provider.responses[0]
	provider.responses = provider.responses[1:]
	return &sliceStream{outputs: response}, nil
}
func (provider *scriptedProvider) Requests() []erkaruntime.ModelRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]erkaruntime.ModelRequest(nil), provider.requests...)
}

type sliceStream struct{ outputs []erkaruntime.ModelOutput }

func (stream *sliceStream) Next(ctx context.Context) (erkaruntime.ModelOutput, error) {
	if err := ctx.Err(); err != nil {
		return erkaruntime.ModelOutput{}, err
	}
	if len(stream.outputs) == 0 {
		return erkaruntime.ModelOutput{}, io.EOF
	}
	output := stream.outputs[0]
	stream.outputs = stream.outputs[1:]
	return output, nil
}
func (*sliceStream) Close() error { return nil }

type sequenceIDs struct {
	mu   sync.Mutex
	next int
}

func (ids *sequenceIDs) NewID(prefix string) (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	return fmt.Sprintf("%s_%03d", prefix, ids.next), nil
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hasResult(items []erkaruntime.RuntimeItem, callID erkaruntime.ToolCallID) bool {
	for _, item := range items {
		if item.Kind == erkaruntime.ItemToolResult && item.ToolResult.CallID == callID {
			return true
		}
	}
	return false
}
