//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"sync"
)

var (
	bubblewrapOnce   sync.Once
	bubblewrapPath   string
	bubblewrapReason string
)

// detectBackend finds bubblewrap and proves it can create namespaces here;
// hosts may forbid unprivileged user namespaces.
func detectBackend() (string, string) {
	bubblewrapOnce.Do(func() {
		path, err := exec.LookPath("bwrap")
		if err != nil {
			bubblewrapReason = "bubblewrap (bwrap) is not installed"
			return
		}
		probe := exec.Command(path, "--ro-bind", "/", "/", "--dev", "/dev", "--unshare-net", "--die-with-parent", "true")
		if output, err := probe.CombinedOutput(); err != nil {
			bubblewrapReason = "bubblewrap cannot create a sandbox: " + string(output)
			return
		}
		bubblewrapPath = path
	})
	if bubblewrapPath == "" {
		return "", bubblewrapReason
	}
	return "bubblewrap", ""
}

func confine(policy Isolation, workspace, temporary, directory string, executable string, arguments []string) (confinedCommand, error) {
	// The host filesystem is visible read-only, including /tmp so programs
	// stored there still run; TMPDIR points at the private writable directory.
	args := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc"}
	for _, path := range existingCanonical(policy.HiddenPaths) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			args = append(args, "--tmpfs", path)
		} else {
			args = append(args, "--ro-bind", "/dev/null", path)
		}
	}
	for _, path := range existingCanonical(policy.WritablePaths) {
		args = append(args, "--bind", path, path)
	}
	args = append(args, "--bind", workspace, workspace, "--bind", temporary, temporary, "--die-with-parent")
	for _, path := range protectedMetadata(workspace) {
		args = append(args, "--ro-bind", path, path)
	}
	if !policy.Network {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--chdir", directory, "--", executable)
	return confinedCommand{program: bubblewrapPath, arguments: append(args, arguments...)}, nil
}
