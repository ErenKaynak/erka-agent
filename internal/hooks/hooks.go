// Package hooks runs user-configured commands after workspace edits, for
// example `gofmt -w {path}` after a Go file changes.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/permissions"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const (
	// PathPlaceholder is replaced by the edited workspace-relative path. It is
	// substituted into individual argv elements, never into a shell string.
	PathPlaceholder = "{path}"
	DefaultTimeout  = 30 * time.Second
	MaxTimeout      = 10 * time.Minute
	outputLimit     = int64(16 << 10)
)

// PostEdit is one configured post-edit hook.
type PostEdit struct {
	// Match is a glob compared against the edited path and its base name.
	// Empty matches every path.
	Match   string        `yaml:"match,omitempty"`
	Command []string      `yaml:"command"`
	Timeout time.Duration `yaml:"timeout,omitempty"`
}

// Config is the `hooks:` configuration block.
type Config struct {
	PostEdit []PostEdit `yaml:"post_edit,omitempty"`
}

// Empty reports whether no hooks are configured.
func (config Config) Empty() bool { return len(config.PostEdit) == 0 }

// Validate checks hook definitions.
func (config Config) Validate() error {
	for index, hook := range config.PostEdit {
		if len(hook.Command) == 0 || strings.TrimSpace(hook.Command[0]) == "" {
			return fmt.Errorf("hooks.post_edit[%d] requires a command", index)
		}
		if hook.Match != "" {
			if _, err := path.Match(hook.Match, "x"); err != nil {
				return fmt.Errorf("hooks.post_edit[%d] has invalid match %q: %w", index, hook.Match, err)
			}
		}
		if hook.Timeout < 0 || hook.Timeout > MaxTimeout {
			return fmt.Errorf("hooks.post_edit[%d] timeout must be between 0 and %s", index, MaxTimeout)
		}
	}
	return nil
}

func (hook PostEdit) matches(name string) bool {
	if hook.Match == "" {
		return true
	}
	if matched, _ := path.Match(hook.Match, name); matched {
		return true
	}
	matched, _ := path.Match(hook.Match, path.Base(name))
	return matched
}

// Runner executes hooks through the workspace sandbox, so they inherit its
// directory confinement, filtered environment, and output limits.
type Runner struct {
	config  Config
	sandbox erkaruntime.Sandbox
}

func NewRunner(config Config, sandbox erkaruntime.Sandbox) (*Runner, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if sandbox == nil {
		return nil, errors.New("hook runner requires a sandbox")
	}
	return &Runner{config: config, sandbox: sandbox}, nil
}

// editTools are the tools whose successful calls trigger post-edit hooks.
var editTools = map[string]bool{"edit_file": true, "apply_patch": true}

// AfterTool implements runtime.ToolHook.
func (runner *Runner) AfterTool(ctx context.Context, call erkaruntime.ToolCall, result erkaruntime.ToolExecutionResult) []erkaruntime.HookOutcome {
	if runner == nil || result.IsError || !editTools[call.Tool] {
		return nil
	}
	var outcomes []erkaruntime.HookOutcome
	for _, edited := range permissions.Subjects(call.Tool, call.Arguments) {
		if !runner.exists(ctx, edited) {
			continue // deleted by the patch
		}
		for _, hook := range runner.config.PostEdit {
			if !hook.matches(edited) {
				continue
			}
			outcomes = append(outcomes, runner.run(ctx, hook, edited))
		}
	}
	return outcomes
}

func (runner *Runner) run(ctx context.Context, hook PostEdit, edited string) erkaruntime.HookOutcome {
	argv := make([]string, len(hook.Command))
	for index, part := range hook.Command {
		argv[index] = strings.ReplaceAll(part, PathPlaceholder, edited)
	}
	timeout := hook.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	outcome := erkaruntime.HookOutcome{Name: strings.Join(argv, " "), Path: edited}
	commandResult, err := runner.sandbox.Run(ctx, erkaruntime.CommandRequest{Executable: argv[0], Arguments: argv[1:], Directory: ".", Timeout: timeout, OutputLimitBytes: outputLimit})
	if err != nil {
		outcome.Failed, outcome.Output = true, err.Error()
		return outcome
	}
	outcome.Output = strings.TrimSpace(commandResult.StandardOutput + "\n" + commandResult.StandardError)
	if commandResult.ExitCode != 0 {
		outcome.Failed = true
		outcome.Output = strings.TrimSpace(outcome.Output + fmt.Sprintf("\nexit_code=%d", commandResult.ExitCode))
	}
	return outcome
}

func (runner *Runner) exists(ctx context.Context, name string) bool {
	entries, err := runner.sandbox.ListDir(ctx, path.Dir(name), 10_000)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if path.Clean(entry.Path) == name && !entry.Directory {
			return true
		}
	}
	return false
}

var _ erkaruntime.ToolHook = (*Runner)(nil)
