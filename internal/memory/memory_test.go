package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProjectKeyIsSharedByLinkedWorktrees(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	source := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	run(source, "init", "-q")
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(source, "add", "a.txt")
	run(source, "commit", "-q", "-m", "init")
	worktree := filepath.Join(t.TempDir(), "task")
	run(source, "worktree", "add", "-q", "--detach", worktree)

	sourceKey, err := ProjectKey(source)
	if err != nil {
		t.Fatal(err)
	}
	worktreeKey, err := ProjectKey(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if sourceKey != worktreeKey || sourceKey == "" {
		t.Fatalf("keys differ: source %q worktree %q", sourceKey, worktreeKey)
	}
	otherKey, err := ProjectKey(t.TempDir())
	if err == nil && otherKey == sourceKey {
		t.Fatal("unrelated directory shared the project key")
	}
}
