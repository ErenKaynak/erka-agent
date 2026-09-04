package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/sandbox"
)

type WorktreeManager interface {
	Validate(context.Context, string, string) error
	Create(context.Context, string, string, string) error
	Remove(context.Context, string, string) error
	Diff(context.Context, string) (string, error)
}

type GitWorktrees struct{}

func (GitWorktrees) Validate(ctx context.Context, source, ref string) error {
	if !filepath.IsAbs(source) || strings.TrimSpace(ref) == "" {
		return errors.New("worktree source must be absolute and ref must not be empty")
	}
	result, err := runGit(ctx, source, []string{"rev-parse", "--verify", ref + "^{commit}"}, 30*time.Second)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("Git ref %q is unavailable: %s", ref, commandMessage(result))
	}
	return nil
}

func (GitWorktrees) Create(ctx context.Context, source, ref, destination string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(destination) || strings.TrimSpace(ref) == "" {
		return errors.New("worktree creation requires absolute source and destination plus a ref")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("worktree destination already exists: %q", destination)
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create worktree parent: %w", err)
	}
	result, err := runGit(ctx, source, []string{"worktree", "add", "--detach", destination, ref}, 2*time.Minute)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("create Git worktree: %s", commandMessage(result))
	}
	return nil
}

func (GitWorktrees) Remove(ctx context.Context, source, destination string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(destination) {
		return errors.New("worktree removal requires absolute paths")
	}
	result, err := runGit(ctx, source, []string{"worktree", "remove", "--force", destination}, 2*time.Minute)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 && !strings.Contains(strings.ToLower(commandMessage(result)), "not a working tree") {
		return fmt.Errorf("remove Git worktree: %s", commandMessage(result))
	}
	return nil
}

func (GitWorktrees) Diff(ctx context.Context, worktree string) (string, error) {
	if !filepath.IsAbs(worktree) {
		return "", errors.New("worktree diff requires an absolute path")
	}
	result, err := runGit(ctx, worktree, []string{"diff", "--no-ext-diff", "--no-color", "--"}, time.Minute)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("read Git worktree diff: %s", commandMessage(result))
	}
	return result.StandardOutput, nil
}

func runGit(ctx context.Context, directory string, arguments []string, timeout time.Duration) (erkaruntime.CommandResult, error) {
	local, err := sandbox.NewLocal(directory, sandbox.Options{Environment: os.Environ()})
	if err != nil {
		return erkaruntime.CommandResult{}, err
	}
	return local.Run(ctx, erkaruntime.CommandRequest{Executable: "git", Arguments: arguments, Directory: ".", Timeout: timeout, OutputLimitBytes: 256 << 10})
}

func commandMessage(result erkaruntime.CommandResult) string {
	message := strings.TrimSpace(result.StandardError)
	if message == "" {
		message = strings.TrimSpace(result.StandardOutput)
	}
	if message == "" {
		message = fmt.Sprintf("Git exited with code %d", result.ExitCode)
	}
	return message
}

var _ WorktreeManager = GitWorktrees{}
