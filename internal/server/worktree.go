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
	// Finalize preserves a reviewed worktree's changes under a ref and
	// removes the worktree. It must be safe to repeat after a crash.
	Finalize(context.Context, FinalizeRequest) (FinalizeResult, error)
	// CommitDiff shows the changes a finalized task recorded.
	CommitDiff(ctx context.Context, source, base, commit string) (string, error)
}

// FinalizeRequest turns a worktree into a commit on Ref. Accepted work uses a
// branch; rejected work uses a hidden ref so it stays recoverable.
type FinalizeRequest struct {
	Source   string
	Worktree string
	Ref      string
	Message  string
	// BaseRef is the ref the task started from; the recorded base is its
	// merge base with the result, so agent-made commits are included.
	BaseRef string
}

// FinalizeResult is empty when the worktree was already gone or unchanged.
type FinalizeResult struct {
	Ref    string
	Base   string
	Commit string
}

type GitWorktrees struct{}

const maxWorktreeDiffBytes = 1 << 20

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
	result, err := runGit(ctx, worktree, []string{"diff", "--no-ext-diff", "--no-color", "HEAD", "--"}, time.Minute)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("read Git worktree diff: %s", commandMessage(result))
	}
	combined := result.StandardOutput
	status, err := runGit(ctx, worktree, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}, time.Minute)
	if err != nil {
		return "", err
	}
	if status.ExitCode != 0 {
		return "", fmt.Errorf("read Git worktree status: %s", commandMessage(status))
	}
	for _, entry := range strings.Split(status.StandardOutput, "\x00") {
		if !strings.HasPrefix(entry, "?? ") {
			continue
		}
		path := strings.TrimPrefix(entry, "?? ")
		untracked, runErr := runGit(ctx, worktree, []string{"diff", "--no-index", "--no-color", "--", os.DevNull, path}, time.Minute)
		if runErr != nil {
			return "", runErr
		}
		if untracked.ExitCode != 0 && untracked.ExitCode != 1 {
			return "", fmt.Errorf("read untracked worktree diff: %s", commandMessage(untracked))
		}
		combined += untracked.StandardOutput
		if len(combined) >= maxWorktreeDiffBytes {
			return combined[:maxWorktreeDiffBytes] + "\n[diff truncated]\n", nil
		}
	}
	return combined, nil
}

// Finalize commits every change in the worktree, points Ref at that commit
// in the shared repository, and removes the worktree. Repository hooks never
// run: a task worktree may contain hostile code.
func (manager GitWorktrees) Finalize(ctx context.Context, request FinalizeRequest) (FinalizeResult, error) {
	if !filepath.IsAbs(request.Source) || !filepath.IsAbs(request.Worktree) || !strings.HasPrefix(request.Ref, "refs/") {
		return FinalizeResult{}, errors.New("finalize requires absolute paths and a full ref name")
	}
	if _, err := os.Stat(request.Worktree); errors.Is(err, os.ErrNotExist) {
		// A previous attempt removed the worktree; recover what it recorded.
		_, _ = runGit(ctx, request.Source, []string{"worktree", "prune"}, time.Minute)
		existing, err := runGit(ctx, request.Source, []string{"rev-parse", "--verify", "--quiet", request.Ref}, time.Minute)
		if err != nil || existing.ExitCode != 0 {
			return FinalizeResult{}, err
		}
		commit := strings.TrimSpace(existing.StandardOutput)
		base, err := runGit(ctx, request.Source, []string{"merge-base", commit, request.BaseRef}, time.Minute)
		if err != nil || base.ExitCode != 0 {
			return FinalizeResult{Ref: request.Ref, Commit: commit}, err
		}
		return FinalizeResult{Ref: request.Ref, Base: strings.TrimSpace(base.StandardOutput), Commit: commit}, nil
	} else if err != nil {
		return FinalizeResult{}, err
	}
	git := func(arguments ...string) (erkaruntime.CommandResult, error) {
		safe := append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgSign=false"}, arguments...)
		return runGit(ctx, request.Worktree, safe, 2*time.Minute)
	}
	output := func(arguments ...string) (string, error) {
		result, err := git(arguments...)
		if err != nil {
			return "", err
		}
		if result.ExitCode != 0 {
			return "", fmt.Errorf("git %s: %s", arguments[0], commandMessage(result))
		}
		return strings.TrimSpace(result.StandardOutput), nil
	}
	// Resolve the base in the source repository: inside the worktree, HEAD
	// names the worktree's own, possibly advanced, commit.
	baseRef := request.BaseRef
	if baseRef == "" {
		baseRef = "HEAD"
	}
	resolved, err := runGit(ctx, request.Source, []string{"rev-parse", "--verify", baseRef + "^{commit}"}, time.Minute)
	if err != nil {
		return FinalizeResult{}, err
	}
	if resolved.ExitCode != 0 {
		return FinalizeResult{}, fmt.Errorf("resolve base %q: %s", baseRef, commandMessage(resolved))
	}
	base, err := output("merge-base", "HEAD", strings.TrimSpace(resolved.StandardOutput))
	if err != nil {
		return FinalizeResult{}, err
	}
	if _, err := output("add", "--all"); err != nil {
		return FinalizeResult{}, err
	}
	staged, err := git("diff", "--cached", "--quiet")
	if err != nil {
		return FinalizeResult{}, err
	}
	if staged.ExitCode == 1 {
		identity := []string{}
		if email, _ := output("config", "user.email"); email == "" {
			identity = []string{"-c", "user.name=erka", "-c", "user.email=erka@localhost"}
		}
		commit := append(identity, "commit", "--no-verify", "--quiet", "-m", request.Message)
		if _, err := output(commit...); err != nil {
			return FinalizeResult{}, err
		}
	} else if staged.ExitCode != 0 {
		return FinalizeResult{}, fmt.Errorf("git diff: %s", commandMessage(staged))
	}
	head, err := output("rev-parse", "HEAD")
	if err != nil {
		return FinalizeResult{}, err
	}
	result := FinalizeResult{}
	if head != base {
		existing, _ := output("rev-parse", "--verify", "--quiet", request.Ref)
		if existing != "" && existing != head {
			return FinalizeResult{}, fmt.Errorf("ref %s already points at another commit", request.Ref)
		}
		if existing == "" {
			if _, err := output("update-ref", request.Ref, head); err != nil {
				return FinalizeResult{}, err
			}
		}
		result = FinalizeResult{Ref: request.Ref, Base: base, Commit: head}
	}
	if err := manager.Remove(ctx, request.Source, request.Worktree); err != nil {
		return FinalizeResult{}, err
	}
	return result, nil
}

func (GitWorktrees) CommitDiff(ctx context.Context, source, base, commit string) (string, error) {
	if !filepath.IsAbs(source) || base == "" || commit == "" {
		return "", errors.New("commit diff requires an absolute source and both commits")
	}
	result, err := runGit(ctx, source, []string{"diff", "--no-ext-diff", "--no-color", base, commit, "--"}, time.Minute)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("read recorded diff: %s", commandMessage(result))
	}
	if len(result.StandardOutput) > maxWorktreeDiffBytes {
		return result.StandardOutput[:maxWorktreeDiffBytes] + "\n[diff truncated]\n", nil
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
