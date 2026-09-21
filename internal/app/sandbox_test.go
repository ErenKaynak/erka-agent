package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/sandbox"
)

type approveAll struct{}

func (approveAll) Request(context.Context, erkaruntime.UserRequest) (erkaruntime.UserResponse, error) {
	return erkaruntime.UserResponse{Approved: true}, nil
}

func TestAutoModeSandboxesCommandsAndGodModeDoesNot(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	target := filepath.Join(outside, "escaped.txt")
	arguments, _ := json.Marshal(map[string]any{"command": []string{"/bin/sh", "-c", "echo pwned > " + target}})
	turn := func(id erkaruntime.ToolCallID) [][]erkaruntime.ModelOutput {
		return [][]erkaruntime.ModelOutput{
			{{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: id, Tool: "run_command", ArgumentsFragment: string(arguments)}}, {Kind: erkaruntime.ModelCompleted}},
			{{Kind: erkaruntime.ModelTextDelta, Text: "done"}, {Kind: erkaruntime.ModelCompleted}},
		}
	}
	provider := &scriptedProvider{name: "fixture", responses: append(turn("run-auto"), turn("run-god")...)}
	session, err := OpenSession(context.Background(), SessionOptions{
		WorkingDirectory: root, StateDir: state, Provider: provider, NativeModel: "fixture", IDs: &sequenceIDs{}, Clock: fixedClock,
		Interactor: approveAll{}, ApproveCommands: true,
		Isolation: sandbox.Isolation{Mode: sandbox.IsolationAuto},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if !strings.Contains(session.SandboxStatus(), "writes limited") {
		t.Skipf("OS sandbox unavailable here: %s", session.SandboxStatus())
	}
	taskID, err := session.Submit(context.Background(), "", "try to escape")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.RunNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("auto mode let a command write outside the workspace")
	}
	if tools := provider.Requests()[0].Tools; !strings.Contains(toolDescription(tools, "run_command"), "OS sandbox") {
		t.Fatal("the model was not told commands are sandboxed")
	}

	session.SetFullExecution(true)
	if _, err := session.Submit(context.Background(), taskID, "now in god mode"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.RunNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || strings.TrimSpace(string(data)) != "pwned" {
		t.Fatalf("god mode command did not run unconfined: %q, %v", data, err)
	}
}

func toolDescription(tools []erkaruntime.ToolDefinition, name string) string {
	for _, tool := range tools {
		if tool.Name == name {
			return tool.Description
		}
	}
	return ""
}
