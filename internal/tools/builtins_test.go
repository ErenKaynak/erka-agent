package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/sandbox"
)

func TestBuiltinDefinitionsAreCompleteAndValid(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	local, err := sandbox.NewLocal(root, sandbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range Builtins(local, Options{AllowFullExecution: true}) {
		definition := tool.Definition()
		if err := definition.Validate(); err != nil {
			t.Fatalf("%s definition: %v", definition.Name, err)
		}
		names = append(names, definition.Name)
		if (definition.Name == "apply_patch" || definition.Name == "edit_file" || definition.Name == "run_command") != definition.Mutating {
			t.Fatalf("%s mutating = %v", definition.Name, definition.Mutating)
		}
	}
	want := []string{"read_file", "list_files", "search_text", "apply_patch", "edit_file", "run_command", "git_status", "git_diff"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tool names = %v, want %v", names, want)
	}
}

func TestBuiltinCommandsRequireExplicitFullExecution(t *testing.T) {
	t.Parallel()
	local := mustSandbox(t, t.TempDir())
	for _, tool := range Builtins(local, Options{}) {
		if tool.Definition().Name == "run_command" {
			t.Fatal("run_command was exposed without full execution consent")
		}
	}
	if findTool(t, Builtins(local, Options{AllowFullExecution: true}), "run_command") == nil {
		t.Fatal("run_command missing with full execution consent")
	}
}

func TestFileToolsStayScopedAndProduceDeterministicOutput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "Alpha\nneedle one\n")
	mustWrite(t, filepath.Join(root, "nested", "b.txt"), "Needle two\n")
	mustWrite(t, filepath.Join(root, ".git", "ignored"), "needle secret\n")
	local := mustSandbox(t, root)

	read := invoke(t, Builtins(local, Options{}), "read_file", `{"path":"a.txt"}`)
	if read.IsError || read.Output != "Alpha\nneedle one\n" {
		t.Fatalf("read result = %#v", read)
	}
	escape := invoke(t, Builtins(local, Options{}), "read_file", `{"path":"../secret"}`)
	if !escape.IsError || !strings.Contains(escape.Output, "escapes workspace") {
		t.Fatalf("escape result = %#v", escape)
	}

	listed := invoke(t, Builtins(local, Options{}), "list_files", `{}`)
	if listed.IsError || listed.Output != "a.txt\nnested/\nnested/b.txt" {
		t.Fatalf("list output = %q", listed.Output)
	}
	searched := invoke(t, Builtins(local, Options{}), "search_text", `{"query":"needle"}`)
	if searched.IsError || searched.Output != "a.txt:2:needle one\nnested/b.txt:1:Needle two" {
		t.Fatalf("search output = %q", searched.Output)
	}
	caseSensitive := invoke(t, Builtins(local, Options{}), "search_text", `{"query":"Needle","case_sensitive":true}`)
	if caseSensitive.Output != "nested/b.txt:1:Needle two" {
		t.Fatalf("case-sensitive output = %q", caseSensitive.Output)
	}
	unknown := invoke(t, Builtins(local, Options{}), "read_file", `{"path":"a.txt","surprise":true}`)
	if !unknown.IsError || !strings.Contains(unknown.Output, "unknown field") {
		t.Fatalf("unknown argument result = %#v", unknown)
	}
}

func TestApplyPatchAndGitInspection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "hello.txt"), "old\n")
	runGit(t, root, "init", "--initial-branch=main")
	runGit(t, root, "config", "user.email", "fixture@example.invalid")
	runGit(t, root, "config", "user.name", "Fixture")
	runGit(t, root, "config", "core.autocrlf", "false")
	runGit(t, root, "add", "hello.txt")
	runGit(t, root, "commit", "-m", "initial")
	local := mustSandbox(t, root)
	all := Builtins(local, Options{})

	patch := "diff --git a/hello.txt b/hello.txt\n--- a/hello.txt\n+++ b/hello.txt\n@@ -1 +1 @@\n-old\n+new\n"
	result := invoke(t, all, "apply_patch", string(mustJSON(t, map[string]string{"patch": patch})))
	if result.IsError {
		t.Fatalf("apply patch = %#v", result)
	}
	data, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil || string(data) != "new\n" {
		t.Fatalf("patched data = %q, err = %v", data, err)
	}

	status := invoke(t, all, "git_status", `{}`)
	if status.IsError || !strings.Contains(status.Output, " M hello.txt") {
		t.Fatalf("status = %#v", status)
	}
	diff := invoke(t, all, "git_diff", `{"path":"hello.txt"}`)
	if diff.IsError || !strings.Contains(diff.Output, "-old") || !strings.Contains(diff.Output, "+new") {
		t.Fatalf("diff = %#v", diff)
	}

	escapePatch := "--- a/../outside\n+++ b/../outside\n@@ -0,0 +1 @@\n+x\n"
	escape := invoke(t, all, "apply_patch", string(mustJSON(t, map[string]string{"patch": escapePatch})))
	if !escape.IsError || !strings.Contains(escape.Output, "inside the workspace") {
		t.Fatalf("escape patch = %#v", escape)
	}
	binary := invoke(t, all, "apply_patch", `{"patch":"GIT binary patch\\n"}`)
	if !binary.IsError || !strings.Contains(binary.Output, "binary") {
		t.Fatalf("binary patch = %#v", binary)
	}
}

func TestRunCommandUsesArgumentsWithoutShellAndReportsFailures(t *testing.T) {
	t.Parallel()
	local := mustSandbox(t, t.TempDir())
	all := Builtins(local, Options{AllowFullExecution: true})

	result := invoke(t, all, "run_command", `{"command":["go","version"],"timeout_seconds":10}`)
	if result.IsError || !strings.Contains(result.Output, "go version") {
		t.Fatalf("go version = %#v", result)
	}
	failure := invoke(t, all, "run_command", `{"command":["go","definitely-not-a-command"],"timeout_seconds":10}`)
	if !failure.IsError || !strings.Contains(failure.Output, "exit_code=") {
		t.Fatalf("failure = %#v", failure)
	}
	shellText := invoke(t, all, "run_command", `{"command":["go version"],"timeout_seconds":10}`)
	if !shellText.IsError || !strings.Contains(shellText.Output, "start command") {
		t.Fatalf("shell text = %#v", shellText)
	}
}

func TestToolCancellationPropagates(t *testing.T) {
	t.Parallel()
	local := mustSandbox(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := findTool(t, Builtins(local, Options{}), "list_files")
	_, err := tool.Execute(ctx, erkaruntime.ToolInvocation{CallID: "call", Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
}

func invoke(t *testing.T, all []erkaruntime.Tool, name, arguments string) erkaruntime.ToolExecutionResult {
	t.Helper()
	tool := findTool(t, all, name)
	invocation := erkaruntime.ToolInvocation{CallID: "call", Arguments: json.RawMessage(arguments)}
	if tool.Definition().Mutating {
		invocation.IdempotencyKey = "idempotency"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := tool.Execute(ctx, invocation)
	if err != nil {
		t.Fatalf("execute %s: %v", name, err)
	}
	return result
}

func findTool(t *testing.T, all []erkaruntime.Tool, name string) erkaruntime.Tool {
	t.Helper()
	for _, tool := range all {
		if tool.Definition().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func mustSandbox(t *testing.T, root string) *sandbox.Local {
	t.Helper()
	local, err := sandbox.NewLocal(root, sandbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return local
}

func mustWrite(t *testing.T, filename, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEditFileReplacesExactlyOneMatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "pkg", "main.go")
	mustWrite(t, target, "package main\n\nfunc a() {}\nfunc b() {}\nfunc b() {}\n")
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	all := Builtins(mustSandbox(t, root), Options{})
	edit := func(oldStr, newStr string) erkaruntime.ToolExecutionResult {
		return invoke(t, all, "edit_file", string(mustJSON(t, map[string]string{"path": "pkg/main.go", "old_str": oldStr, "new_str": newStr})))
	}
	result := edit("func a() {}", "func a() { println(1) }")
	if result.IsError || result.Output != "edited pkg/main.go at line 3" {
		t.Fatalf("edit = %#v", result)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "package main\n\nfunc a() { println(1) }\nfunc b() {}\nfunc b() {}\n" {
		t.Fatalf("content = %q", data)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode changed to %v", info.Mode().Perm())
	}
	for _, testCase := range []struct{ oldStr, newStr, want string }{
		{"func b() {}", "x", "matches 2 times"},
		{"func c() {}", "x", "was not found"},
		{"package main", "package main", "identical"},
	} {
		if result := edit(testCase.oldStr, testCase.newStr); !result.IsError || !strings.Contains(result.Output, testCase.want) {
			t.Fatalf("edit(%q) = %#v, want %q", testCase.oldStr, result, testCase.want)
		}
	}
	after, _ := os.ReadFile(target)
	if string(after) != string(data) {
		t.Fatal("a failed edit modified the file")
	}
	for _, escape := range []string{"../outside.go", "/etc/passwd"} {
		result := invoke(t, all, "edit_file", string(mustJSON(t, map[string]string{"path": escape, "old_str": "a", "new_str": "b"})))
		if !result.IsError {
			t.Fatalf("edit outside the workspace succeeded: %s", escape)
		}
	}
}
