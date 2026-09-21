package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// IsolationMode selects whether commands run inside an OS sandbox.
type IsolationMode string

const (
	// IsolationOff runs commands with the user's full access.
	IsolationOff IsolationMode = "off"
	// IsolationAuto sandboxes commands when the platform supports it.
	IsolationAuto IsolationMode = "auto"
	// IsolationRequired refuses to run commands without a sandbox.
	IsolationRequired IsolationMode = "required"
)

var ErrIsolationUnavailable = errors.New("OS sandbox is required but unavailable")

// Isolation is the OS-level policy for commands. Inside it, writes are limited
// to the workspace, a private temporary directory, and WritablePaths; the
// network is limited to loopback unless Network is set; and HiddenPaths, such
// as credential directories, cannot be read at all.
type Isolation struct {
	Mode          IsolationMode
	Network       bool
	WritablePaths []string
	HiddenPaths   []string
}

// DefaultIsolation builds the standard policy: common build caches are
// writable, and well-known credential stores plus the given private paths
// (such as erka's state directory) are hidden.
func DefaultIsolation(mode IsolationMode, network bool, writable, private []string) Isolation {
	home, _ := os.UserHomeDir()
	policy := Isolation{Mode: mode, Network: network}
	if home != "" {
		for _, relative := range []string{"Library/Caches", ".cache", ".npm", ".yarn", ".pnpm-store", ".cargo/registry", ".gradle/caches", ".m2/repository"} {
			policy.WritablePaths = append(policy.WritablePaths, filepath.Join(home, relative))
		}
		for _, relative := range []string{".ssh", ".aws", ".azure", ".gnupg", ".kube", ".docker", ".config/gh", ".config/gcloud", ".config/op", ".netrc", ".npmrc", ".pypirc", ".git-credentials", ".terraform.d", "Library/Keychains"} {
			policy.HiddenPaths = append(policy.HiddenPaths, filepath.Join(home, relative))
		}
	}
	policy.WritablePaths = append(policy.WritablePaths, writable...)
	policy.HiddenPaths = append(policy.HiddenPaths, private...)
	return policy
}

func (policy Isolation) enabled() bool {
	return policy.Mode == IsolationAuto || policy.Mode == IsolationRequired
}

// confinedCommand is a command rewritten to start inside the sandbox.
type confinedCommand struct {
	program   string
	arguments []string
	env       map[string]string
	cleanup   func()
}

// IsolationStatus describes how commands in this workspace are confined.
func (local *Local) IsolationStatus() string {
	if !local.isolation.enabled() {
		return "off"
	}
	if local.backend == "" {
		return "unavailable (" + local.unavailable + ")"
	}
	network := "no network"
	if local.isolation.Network {
		network = "network allowed"
	} else if local.backend == "bubblewrap" {
		network = "no network (isolated, host services unreachable)"
	}
	return local.backend + ", writes limited to the workspace, " + network
}

// newCommand builds the process for a request, inside the sandbox unless it is
// unconfined or isolation is off. The cleanup must run after the process exits.
func (local *Local) newCommand(executable string, arguments []string, directory string, unconfined bool, overrides map[string]string) (*exec.Cmd, func(), error) {
	noop := func() {}
	env := make(map[string]string, len(overrides)+3)
	for key, value := range overrides {
		env[key] = value
	}
	if unconfined || !local.isolation.enabled() {
		command := exec.Command(executable, arguments...)
		command.Dir = directory
		command.Env = mergeEnvironment(local.environment, env)
		return command, noop, nil
	}
	if local.backend == "" {
		if local.isolation.Mode == IsolationRequired {
			return nil, noop, fmt.Errorf("%w: %s", ErrIsolationUnavailable, local.unavailable)
		}
		command := exec.Command(executable, arguments...)
		command.Dir = directory
		command.Env = mergeEnvironment(local.environment, env)
		return command, noop, nil
	}
	resolved, err := resolveExecutable(executable, directory, local.environment)
	if err != nil {
		return nil, noop, err
	}
	temporary, err := os.MkdirTemp("", "erka-sandbox-")
	if err != nil {
		return nil, noop, fmt.Errorf("create sandbox temporary directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(temporary) }
	if canonical, err := filepath.EvalSymlinks(temporary); err == nil {
		temporary = canonical
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		env[key] = temporary
	}
	confined, err := confine(local.isolation, local.root, temporary, directory, resolved, arguments)
	if err != nil {
		cleanup()
		return nil, noop, err
	}
	command := exec.Command(confined.program, confined.arguments...)
	command.Dir = directory
	command.Env = mergeEnvironment(local.environment, env)
	return command, cleanup, nil
}

// resolveExecutable finds the program the sandbox should start, using the
// command's own PATH so confinement does not change which program runs.
func resolveExecutable(executable, directory string, environment []string) (string, error) {
	if filepath.IsAbs(executable) {
		return executable, nil
	}
	if strings.ContainsRune(executable, filepath.Separator) || strings.ContainsRune(executable, '/') {
		return filepath.Join(directory, executable), nil
	}
	path := ""
	for _, entry := range environment {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = value
		}
	}
	for _, folder := range filepath.SplitList(path) {
		if folder == "" {
			continue
		}
		candidate := filepath.Join(folder, executable)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q was not found on PATH", executable)
}

// protectedMetadata lists repository files that commands may read but never
// write: a hostile hook or config entry (such as core.fsmonitor) would run
// unsandboxed the next time the user runs Git.
func protectedMetadata(workspace string) []string {
	return existingCanonical([]string{
		filepath.Join(workspace, ".git", "config"),
		filepath.Join(workspace, ".git", "config.worktree"),
		filepath.Join(workspace, ".git", "hooks"),
		filepath.Join(workspace, ".git", "info", "attributes"),
	})
}

// existingCanonical keeps paths that exist, resolved through symlinks, because
// sandboxes match the real path a process opens.
func existingCanonical(paths []string) []string {
	result := make([]string, 0, len(paths))
	seen := make(map[string]bool)
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			continue
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil || seen[canonical] {
			continue
		}
		seen[canonical] = true
		result = append(result, canonical)
	}
	return result
}
