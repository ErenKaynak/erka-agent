package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	keyring "github.com/zalando/go-keyring"

	"github.com/ErenKaynak/erka-agent/internal/auth"
	"github.com/ErenKaynak/erka-agent/internal/buildinfo"
	"github.com/ErenKaynak/erka-agent/internal/catalog"
	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
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
	for _, fragment := range []string{"Usage:", "auth", "models", "providers", "run", "version", "--config", "--state-dir", "--pipeline"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("Execute() help missing %q:\n%s", fragment, stdout)
		}
	}
}

func TestAuthAndProviderCommandsExposeM3Surface(t *testing.T) {
	t.Parallel()
	stdout, stderr, code := executeForTest(t, "auth", "--help")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "login") || !strings.Contains(stdout, "status") {
		t.Fatalf("auth help = %d, %q, %q", code, stdout, stderr)
	}
	_, stderr, code = executeForTest(t, "auth", "login")
	if code != 2 || !strings.Contains(stderr, "exactly one bridge") {
		t.Fatalf("auth login usage = %d, %q", code, stderr)
	}
	_, stderr, code = executeForTest(t, "auth", "login", "--device", "opencode")
	if code != 2 || !strings.Contains(stderr, "only supported for codex") {
		t.Fatalf("auth login --device opencode = %d, %q", code, stderr)
	}
	stdout, stderr, code = executeForTest(t, "providers")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "native") {
		t.Fatalf("providers = %d, %q, %q", code, stdout, stderr)
	}
}

func TestBareNonTTYPrintsHelpAndInteractiveLaunchesTUI(t *testing.T) {
	t.Parallel()
	stdout, stderr, code := executeForTest(t)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Usage:") {
		t.Fatalf("non-TTY = %d, %q, %q", code, stdout, stderr)
	}
	var launched tuiLaunchOptions
	var output bytes.Buffer
	command := executeWithTUI(context.Background(), []string{"--cwd", "/repo", "--task", "task-7", "--full"}, strings.NewReader(""), &output, &output, buildinfo.Info{}, func(context.Context, config.Options) (config.Resolved, error) {
		return config.Resolved{AllowFullExecution: true}, nil
	}, fakeRunner, func(_ context.Context, _ config.Resolved, options tuiLaunchOptions, _ io.Reader, _ io.Writer) error {
		launched = options
		return nil
	}, true)
	if command != 0 || launched.WorkingDirectory != "/repo" || launched.TaskID != "task-7" || !launched.Full {
		t.Fatalf("interactive launch = %d, %#v, %q", command, launched, output.String())
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

func TestExecuteRunInvokesLocalRuntime(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := executeForTest(t, "run", "fix the tests")
	if code != 0 {
		t.Fatalf("Execute() code = %d, want 0", code)
	}
	if stdout != "ran: fix the tests\n" || stderr != "" {
		t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
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
		"--provider", "deepseek",
		"--model", "deepseek-v4-pro",
		"--provider-base-url", "https://deepseek.example",
		"--api-key-env", "MY_KEY",
		"--pipeline", "alternate",
		"run", "--max-iterations", "9", "--max-output-tokens", "2048", "--timeout", "2m", "--full",
		"fix the tests",
	)
	if code != 0 || stderr != "" {
		t.Fatalf("Execute() code = %d, stderr = %q", code, stderr)
	}
	if got.ConfigPath != "/config.yaml" || got.StateDir != "/state" || got.Provider != "deepseek" || got.Model != "deepseek-v4-pro" || got.BaseURL != "https://deepseek.example" || got.APIKeyEnv != "MY_KEY" || got.Pipeline != "alternate" || got.MaxIterations != 9 || got.MaxOutputTokens != 2048 || got.StageTimeout != 2*time.Minute || !got.AllowFullExecution {
		t.Fatalf("config options = %#v", got)
	}
}

func TestExecuteRunResumeDoesNotRequirePrompt(t *testing.T) {
	t.Parallel()
	stdout, stderr, code := executeForTest(t, "run", "--resume", "task_123")
	if code != 0 || stderr != "" || stdout != "resumed: task_123\n" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	_, stderr, code = executeForTest(t, "run", "--resume", "task_123", "new prompt")
	if code != 2 || !strings.Contains(stderr, "cannot be combined") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
}

func TestExecuteRunStageTimeoutAlias(t *testing.T) {
	t.Parallel()

	var got config.Options
	_, stderr, code := executeForTestWithLoader(
		t,
		func(_ context.Context, options config.Options) (config.Resolved, error) {
			got = options
			return config.Resolved{}, nil
		},
		"run", "--stage-timeout", "90s", "fix the tests",
	)
	if code != 0 || stderr != "" {
		t.Fatalf("Execute() code = %d, stderr = %q", code, stderr)
	}
	if got.StageTimeout != 90*time.Second {
		t.Fatalf("config options = %#v", got)
	}
}

func TestRunConfiguredRequiresCredential(t *testing.T) {
	previous := newAuthManager
	newAuthManager = func() *auth.Manager { return auth.NewManager(emptyKeyring{}) }
	t.Cleanup(func() { newAuthManager = previous })

	err := runConfigured(context.Background(), config.Resolved{APIKeyEnv: "TEST_ERKA_MISSING_KEY"}, "fix", "", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "TEST_ERKA_MISSING_KEY") {
		t.Fatalf("runConfigured() error = %v", err)
	}
}

type emptyKeyring struct{}

func (emptyKeyring) Get(string, string) (string, error) { return "", keyring.ErrNotFound }
func (emptyKeyring) Set(string, string, string) error   { return nil }
func (emptyKeyring) Delete(string, string) error        { return nil }

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
		fakeRunner,
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
	code := execute(context.Background(), args, &stdout, &stderr, info, loader, fakeRunner)
	return stdout.String(), stderr.String(), code
}

func fakeRunner(_ context.Context, _ config.Resolved, objective string, resume erkaruntime.TaskID, _ string, output io.Writer) error {
	if resume != "" {
		_, _ = fmt.Fprintf(output, "resumed: %s\n", resume)
		return nil
	}
	_, _ = fmt.Fprintf(output, "ran: %s\n", objective)
	return nil
}

func TestModelsCommandListsLiveAndConfiguredModels(t *testing.T) {
	previous := discoverConfiguredModels
	discoverConfiguredModels = func(context.Context, config.Resolved, *auth.Manager) catalog.Result {
		return catalog.Result{Entries: []catalog.Entry{
			{Stage: erkaruntime.StageReview, Connection: "planner", Class: config.ConnectionNative, Provider: "openai", Model: erkaruntime.ModelInfo{ID: "gpt-review"}, Source: "live", State: "available", Selectable: true},
			{Stage: erkaruntime.StageCoding, Connection: "coder", Class: config.ConnectionNative, Provider: "deepseek", Model: erkaruntime.ModelInfo{ID: "deepseek-v4-pro"}, Source: "live", State: "available", Selectable: true},
			{Stage: erkaruntime.StagePlanning, Connection: "planner", Class: config.ConnectionNative, Provider: "openai", Model: erkaruntime.ModelInfo{ID: "gpt-plan"}, Source: "configured+live", State: "available", Current: true, Selectable: true},
			{Stage: erkaruntime.StagePlanning, Connection: "coder", Class: config.ConnectionNative, Provider: "deepseek", Model: erkaruntime.ModelInfo{ID: "deepseek-v4-flash"}, Source: "configured+live", State: "available", Current: true, Selectable: true},
		}}
	}
	t.Cleanup(func() { discoverConfiguredModels = previous })
	resolved := config.Resolved{}
	stdout, stderr, code := executeForTestWithLoader(t, func(context.Context, config.Options) (config.Resolved, error) { return resolved, nil }, "models")
	if code != 0 || stderr != "" {
		t.Fatalf("models command = code %d, stderr %q", code, stderr)
	}
	for _, fragment := range []string{"STAGE\tCONNECTION", "planning\tcoder\tnative\tdeepseek\tdeepseek-v4-flash\tconfigured+live", "coding\tcoder\tnative\tdeepseek\tdeepseek-v4-pro\tlive"} {
		if !strings.Contains(stdout, fragment) {
			t.Fatalf("models output missing %q:\n%s", fragment, stdout)
		}
	}
	orderedRows := []string{
		"planning\tcoder\t", "coding\tcoder\t", "planning\tplanner\t", "review\tplanner\t",
	}
	position := -1
	for _, row := range orderedRows {
		next := strings.Index(stdout[position+1:], row)
		if next < 0 {
			t.Fatalf("models output missing ordered row %q:\n%s", row, stdout)
		}
		position += next + 1
	}
}
