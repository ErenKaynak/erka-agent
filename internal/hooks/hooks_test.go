package hooks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/sandbox"
)

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	if err := (Config{PostEdit: []PostEdit{{Match: "*.go", Command: []string{"gofmt", "-w", "{path}"}}}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []Config{
		{PostEdit: []PostEdit{{Match: "*.go"}}},
		{PostEdit: []PostEdit{{Command: []string{" "}}}},
		{PostEdit: []PostEdit{{Match: "[", Command: []string{"x"}}}},
		{PostEdit: []PostEdit{{Command: []string{"x"}, Timeout: MaxTimeout + 1}}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("Validate(%#v) succeeded", invalid)
		}
	}
}

func TestRunnerRunsMatchingHooksForEditedFiles(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	root := t.TempDir()
	for _, name := range []string{"pkg/a.go", "README.md"} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	local, err := sandbox.NewLocal(root, sandbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(Config{PostEdit: []PostEdit{
		{Match: "*.go", Command: []string{"sh", "-c", `printf 'formatted %s' "$1"`, "hook", "{path}"}},
		{Match: "*.md", Command: []string{"sh", "-c", "echo lint failed >&2; exit 3"}},
	}}, local)
	if err != nil {
		t.Fatal(err)
	}
	edit, _ := json.Marshal(map[string]string{"path": "pkg/a.go", "old_str": "x", "new_str": "y"})
	outcomes := runner.AfterTool(context.Background(), erkaruntime.ToolCall{Tool: "edit_file", Arguments: edit}, erkaruntime.ToolExecutionResult{Output: "edited"})
	if len(outcomes) != 1 || outcomes[0].Failed || outcomes[0].Output != "formatted pkg/a.go" || outcomes[0].Path != "pkg/a.go" {
		t.Fatalf("outcomes = %#v", outcomes)
	}
	patch, _ := json.Marshal(map[string]string{"patch": "--- a/README.md\n+++ b/README.md\n@@\n--- a/gone.go\n+++ /dev/null\n"})
	outcomes = runner.AfterTool(context.Background(), erkaruntime.ToolCall{Tool: "apply_patch", Arguments: patch}, erkaruntime.ToolExecutionResult{})
	if len(outcomes) != 1 || !outcomes[0].Failed || !strings.Contains(outcomes[0].Output, "lint failed") || !strings.Contains(outcomes[0].Output, "exit_code=3") {
		t.Fatalf("patch outcomes = %#v", outcomes)
	}
	if got := runner.AfterTool(context.Background(), erkaruntime.ToolCall{Tool: "edit_file", Arguments: edit}, erkaruntime.ToolExecutionResult{IsError: true}); got != nil {
		t.Fatalf("hooks ran after a failed edit: %#v", got)
	}
	if got := runner.AfterTool(context.Background(), erkaruntime.ToolCall{Tool: "read_file", Arguments: edit}, erkaruntime.ToolExecutionResult{}); got != nil {
		t.Fatalf("hooks ran after a read: %#v", got)
	}
}

func TestPathPlaceholderIsNeverShellInterpreted(t *testing.T) {
	t.Parallel()
	hook := PostEdit{Command: []string{"echo", "{path}"}}
	if !hook.matches("anything") {
		t.Fatal("empty match must match every path")
	}
	// The placeholder replaces text inside one argv element; a path with
	// shell metacharacters stays a single literal argument.
	argv := strings.ReplaceAll(hook.Command[1], PathPlaceholder, "a;rm -rf /")
	if argv != "a;rm -rf /" {
		t.Fatalf("argv = %q", argv)
	}
}
