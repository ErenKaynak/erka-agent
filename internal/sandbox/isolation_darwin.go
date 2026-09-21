//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const sandboxExec = "/usr/bin/sandbox-exec"

func detectBackend() (string, string) {
	if _, err := os.Stat(sandboxExec); err != nil {
		return "", "sandbox-exec is missing"
	}
	return "macOS sandbox", ""
}

// darwinProfile denies writes except to allowed paths, denies reading hidden
// paths, and limits outbound connections to loopback and Unix sockets. Paths
// are passed as parameters so they never need quoting inside the profile.
func darwinProfile(writable, hidden, protected int, network bool) string {
	var profile strings.Builder
	profile.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n")
	profile.WriteString(`(allow file-write* (literal "/dev/null") (literal "/dev/zero") (literal "/dev/tty") (literal "/dev/dtracehelper") (regex #"^/dev/fd/") (regex #"^/dev/ttys"))` + "\n")
	for index := 0; index < hidden; index++ {
		fmt.Fprintf(&profile, "(deny file-read* file-write* (subpath (param \"HIDE%d\")))\n", index)
	}
	profile.WriteString(`(allow file-read* file-write* (subpath (param "WORKSPACE")) (subpath (param "TMP")))` + "\n")
	for index := 0; index < writable; index++ {
		fmt.Fprintf(&profile, "(allow file-write* (subpath (param \"WRITE%d\")))\n", index)
	}
	for index := 0; index < protected; index++ {
		fmt.Fprintf(&profile, "(deny file-write* (subpath (param \"PROTECT%d\")))\n", index)
	}
	if !network {
		profile.WriteString("(deny network-outbound)\n")
		profile.WriteString(`(allow network-outbound (remote unix-socket) (remote ip "localhost:*"))` + "\n")
	}
	return profile.String()
}

func confine(policy Isolation, workspace, temporary, _ string, executable string, arguments []string) (confinedCommand, error) {
	writable := existingCanonical(append(policy.WritablePaths, userCacheDirectory()...))
	hidden := existingCanonical(policy.HiddenPaths)
	protected := protectedMetadata(workspace)
	parameters := []string{"-p", darwinProfile(len(writable), len(hidden), len(protected), policy.Network), "-D", "WORKSPACE=" + workspace, "-D", "TMP=" + temporary}
	for index, path := range protected {
		parameters = append(parameters, "-D", fmt.Sprintf("PROTECT%d=%s", index, path))
	}
	for index, path := range writable {
		parameters = append(parameters, "-D", fmt.Sprintf("WRITE%d=%s", index, path))
	}
	for index, path := range hidden {
		parameters = append(parameters, "-D", fmt.Sprintf("HIDE%d=%s", index, path))
	}
	return confinedCommand{program: sandboxExec, arguments: append(append(parameters, executable), arguments...)}, nil
}

// userCacheDirectory is the per-user Darwin cache directory (the C sibling of
// the temporary directory) that compilers and system tools write to. The
// shared temporary directory itself stays read-only; each command gets a
// private temporary directory instead.
func userCacheDirectory() []string {
	temporary, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil || !strings.HasPrefix(temporary, "/private/var/folders/") {
		return nil
	}
	return []string{filepath.Join(filepath.Dir(filepath.Clean(temporary)), "C")}
}
