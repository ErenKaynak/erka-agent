package permissions

import (
	"encoding/json"
	"strings"
	"testing"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func command(argv ...string) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"command": argv})
	return data
}

func file(tool, path string) (string, json.RawMessage) {
	data, _ := json.Marshal(map[string]string{"path": path, "old_str": "a", "new_str": "b"})
	return tool, data
}

func patch(paths ...string) json.RawMessage {
	var builder strings.Builder
	for _, name := range paths {
		builder.WriteString("--- a/" + name + "\n+++ b/" + name + "\n@@ -1 +1 @@\n-a\n+b\n")
	}
	data, _ := json.Marshal(map[string]string{"patch": builder.String()})
	return data
}

func TestParseRule(t *testing.T) {
	t.Parallel()
	for _, valid := range []string{"apply_patch", "run_command(go test *)", "mcp__github__*", "edit_file(internal/**)", " read_file "} {
		if _, err := ParseRule(valid); err != nil {
			t.Errorf("ParseRule(%q) = %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "run_command(", "run_command()", "bad name", "run command(x)", "x(y"} {
		if _, err := ParseRule(invalid); err == nil {
			t.Errorf("ParseRule(%q) succeeded", invalid)
		}
	}
	if _, err := New(Rules{Deny: []string{"run_command("}}, false); err == nil || !strings.Contains(err.Error(), "permissions.rules.deny") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestDecidePrecedenceAndDefaults(t *testing.T) {
	t.Parallel()
	policy, err := New(Rules{
		Allow: []string{"run_command(go test *)", "run_command(go vet *)", "edit_file(internal/*)", "apply_patch(internal/*)"},
		Ask:   []string{"edit_file(internal/secret/*)", "mcp__github__create_*"},
		Deny:  []string{"run_command(rm -rf *)", "run_command(go test -exec *)", "edit_file(*.env)"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	editTool, editSecret := file("edit_file", "internal/secret/key.go")
	_, editEnv := file("edit_file", "internal/.env")
	_, editOutside := file("edit_file", "cmd/main.go")
	_, editInternal := file("edit_file", "./internal/app/../app/x.go")
	cases := []struct {
		name string
		tool string
		args json.RawMessage
		want erkaruntime.PermissionAction
		rule string
	}{
		{"allow-listed command", "run_command", command("go", "test", "./..."), erkaruntime.PermissionAllow, "run_command(go test *)"},
		{"deny wins over allow", "run_command", command("go", "test", "-exec", "sh", "./..."), erkaruntime.PermissionDeny, "run_command(go test -exec *)"},
		{"denied command", "run_command", command("rm", "-rf", "/"), erkaruntime.PermissionDeny, "run_command(rm -rf *)"},
		{"unlisted command asks without --full", "run_command", command("make", "deploy"), erkaruntime.PermissionAsk, ""},
		{"ask wins over allow", editTool, editSecret, erkaruntime.PermissionAsk, "edit_file(internal/secret/*)"},
		{"deny on path", "edit_file", editEnv, erkaruntime.PermissionDeny, "edit_file(*.env)"},
		{"path normalized before matching", "edit_file", editInternal, erkaruntime.PermissionAllow, "edit_file(internal/*)"},
		{"unmatched edit falls back to allow", "edit_file", editOutside, erkaruntime.PermissionAllow, ""},
		{"patch inside allowed paths", "apply_patch", patch("internal/a.go", "internal/b.go"), erkaruntime.PermissionAllow, "apply_patch(internal/*)"},
		{"tool-name wildcard", "mcp__github__create_issue", json.RawMessage(`{}`), erkaruntime.PermissionAsk, "mcp__github__create_*"},
		{"tool without subjects", "git_status", json.RawMessage(`{}`), erkaruntime.PermissionAllow, ""},
	}
	for _, testCase := range cases {
		got := policy.Decide(testCase.tool, testCase.args)
		if got.Action != testCase.want || got.Rule != testCase.rule {
			t.Errorf("%s: Decide() = %#v, want %s %q", testCase.name, got, testCase.want, testCase.rule)
		}
	}
	// A patch that also touches a file outside the allowed paths is not
	// covered by the allow rule.
	partial := policy.Decide("apply_patch", patch("internal/a.go", "cmd/main.go"))
	if partial.Rule == "apply_patch(internal/*)" {
		t.Fatalf("partial patch matched allow rule: %#v", partial)
	}
}

func TestFullExecutionAllowsUnlistedCommandsButKeepsDeny(t *testing.T) {
	t.Parallel()
	policy, _ := New(Rules{Deny: []string{"run_command(rm *)"}}, true)
	if got := policy.Decide("run_command", command("make", "build")); got.Action != erkaruntime.PermissionAllow {
		t.Fatalf("full unlisted = %#v", got)
	}
	if got := policy.Decide("run_command", command("rm", "x")); got.Action != erkaruntime.PermissionDeny {
		t.Fatalf("full denied = %#v", got)
	}
	var nilPolicy *Policy
	if got := nilPolicy.Decide("run_command", command("rm", "x")); got.Action != erkaruntime.PermissionAllow {
		t.Fatalf("nil policy = %#v", got)
	}
}

func TestExposesCommand(t *testing.T) {
	t.Parallel()
	for rules, want := range map[*Rules]bool{
		{Allow: []string{"run_command(go test *)"}}: true,
		{Ask: []string{"run_command"}}:              true,
		{Deny: []string{"run_command"}}:             false,
		{Allow: []string{"*"}}:                      false,
		{Allow: []string{"edit_file"}}:              false,
	} {
		policy, _ := New(*rules, false)
		if got := policy.ExposesCommand(); got != want {
			t.Errorf("ExposesCommand(%#v) = %v, want %v", *rules, got, want)
		}
	}
}

func TestPatchPathsIncludeRenamesAndSkipDevNull(t *testing.T) {
	t.Parallel()
	text := "diff --git a/old.go b/new.go\nrename from old.go\nrename to new.go\n--- /dev/null\n+++ b/created.go\t2026-10-04\n"
	data, _ := json.Marshal(map[string]string{"patch": text})
	got := strings.Join(Subjects("apply_patch", data), ",")
	if got != "old.go,new.go,created.go" {
		t.Fatalf("paths = %s", got)
	}
}
