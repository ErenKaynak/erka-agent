package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/buildinfo"
	"github.com/ErenKaynak/erka-agent/internal/config"
)

func TestExecuteVersion(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := executeForTest(t, "version")
	if code != 0 {
		t.Fatalf("Execute() code = %d, want 0; stderr = %q", code, stderr)
	}
	const want = "erka v0.1.0-test\ncommit: abc123\nbuilt: 2026-09-02T12:00:00Z\n"
	if stdout != want {
		t.Fatalf("Execute() stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("Execute() stderr = %q, want empty", stderr)
	}
}

func TestExecuteVersionFlag(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := executeForTest(t, "--version")
	if code != 0 {
		t.Fatalf("Execute() code = %d, want 0; stderr = %q", code, stderr)
	}
	if stdout != "erka v0.1.0-test\n" {
		t.Fatalf("Execute() stdout = %q", stdout)
	}
}

func TestExecuteHelp(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := executeForTest(t, "help")
	if code != 0 {
		t.Fatalf("Execute() code = %d, want 0; stderr = %q", code, stderr)
	}
	for _, fragment := range []string{"Usage:", "run", "version", "--config", "--state-dir"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("Execute() help missing %q:\n%s", fragment, stdout)
		}
	}
}

func TestExecuteRunRequiresPrompt(t *testing.T) {
	t.Parallel()

	_, stderr, code := executeForTest(t, "run")
	if code != 2 {
		t.Fatalf("Execute() code = %d, want 2", code)
	}
	if !strings.Contains(stderr, ErrUsage.Error()) {
		t.Fatalf("Execute() stderr = %q, want usage error", stderr)
	}
}

func TestExecuteRunReportsM1Boundary(t *testing.T) {
	t.Parallel()

	_, stderr, code := executeForTest(t, "run", "fix the tests")
	if code != 1 {
		t.Fatalf("Execute() code = %d, want 1", code)
	}
	if !strings.Contains(stderr, ErrRuntimeUnavailable.Error()) {
		t.Fatalf("Execute() stderr = %q, want runtime boundary", stderr)
	}
}

func TestExecuteRejectsUnknownCommand(t *testing.T) {
	t.Parallel()

	_, stderr, code := executeForTest(t, "unknown")
	if code != 2 {
		t.Fatalf("Execute() code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Fatalf("Execute() stderr = %q, want unknown command error", stderr)
	}
}

func TestExecuteRunPassesConfigFlagsToLoader(t *testing.T) {
	t.Parallel()

	var got config.Options
	_, stderr, code := executeForTestWithLoader(
		t,
		func(_ context.Context, options config.Options) (config.Resolved, error) {
			got = options
			return config.Resolved{}, nil
		},
		"--config", "/config.yaml",
		"--state-dir", "/state",
		"run", "fix the tests",
	)
	if code != 1 || !strings.Contains(stderr, ErrRuntimeUnavailable.Error()) {
		t.Fatalf("Execute() code = %d, stderr = %q", code, stderr)
	}
	if got.ConfigPath != "/config.yaml" || got.StateDir != "/state" {
		t.Fatalf("config options = %#v", got)
	}
}

func TestExecuteRunReportsConfigFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("broken config")
	_, stderr, code := executeForTestWithLoader(
		t,
		func(context.Context, config.Options) (config.Resolved, error) {
			return config.Resolved{}, wantErr
		},
		"run", "fix the tests",
	)
	if code != 1 {
		t.Fatalf("Execute() code = %d, want 1", code)
	}
	if !strings.Contains(stderr, wantErr.Error()) {
		t.Fatalf("Execute() stderr = %q, want config error", stderr)
	}
}

func executeForTest(t *testing.T, args ...string) (string, string, int) {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	info := buildinfo.Info{
		Version: "v0.1.0-test",
		Commit:  "abc123",
		Date:    "2026-09-02T12:00:00Z",
	}
	code := execute(
		context.Background(),
		args,
		&stdout,
		&stderr,
		info,
		func(context.Context, config.Options) (config.Resolved, error) {
			return config.Resolved{}, nil
		},
	)
	return stdout.String(), stderr.String(), code
}

func executeForTestWithLoader(
	t *testing.T,
	loader configLoader,
	args ...string,
) (string, string, int) {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	info := buildinfo.Info{
		Version: "v0.1.0-test",
		Commit:  "abc123",
		Date:    "2026-09-02T12:00:00Z",
	}
	code := execute(context.Background(), args, &stdout, &stderr, info, loader)
	return stdout.String(), stderr.String(), code
}
