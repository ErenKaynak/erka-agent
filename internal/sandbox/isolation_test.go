package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

// TestIsolationHelperProcess performs one action from inside the sandbox.
func TestIsolationHelperProcess(t *testing.T) {
	arguments := os.Args
	marker := -1
	for index, argument := range arguments {
		if argument == "--sandbox-helper" {
			marker = index
		}
	}
	if marker < 0 || marker+2 >= len(arguments) {
		return
	}
	action, target := arguments[marker+1], arguments[marker+2]
	var err error
	switch action {
	case "write":
		err = os.WriteFile(target, []byte("written"), 0o600)
	case "read":
		_, err = os.ReadFile(target)
	case "dial":
		var connection net.Conn
		connection, err = net.DialTimeout("tcp", target, 2*time.Second)
		if err == nil {
			_ = connection.Close()
		}
	case "selfloop":
		var listener net.Listener
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			go acceptAll(listener)
			var connection net.Conn
			connection, err = net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
			if err == nil {
				_ = connection.Close()
			}
			_ = listener.Close()
		}
	case "tempfile":
		var file *os.File
		file, err = os.CreateTemp("", "helper-")
		if err == nil {
			fmt.Print(file.Name())
			_ = file.Close()
		}
	}
	if err != nil {
		fmt.Print("error: ", err)
		os.Exit(3)
	}
	os.Exit(0)
}

type isolationFixture struct {
	local     *Local
	workspace string
	outside   string
}

func newIsolationFixture(t *testing.T, network bool) isolationFixture {
	t.Helper()
	workspace, outside := canonicalTemp(t), canonicalTemp(t)
	secret := filepath.Join(outside, "secret")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "id_rsa"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	local, err := NewLocal(workspace, Options{Isolation: Isolation{Mode: IsolationAuto, Network: network, HiddenPaths: []string{secret}}})
	if err != nil {
		t.Fatal(err)
	}
	if local.backend == "" {
		t.Skipf("OS sandbox unavailable: %s", local.unavailable)
	}
	return isolationFixture{local: local, workspace: workspace, outside: outside}
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func (fixture isolationFixture) run(t *testing.T, unconfined bool, action, target string) erkaruntime.CommandResult {
	t.Helper()
	result, err := fixture.local.Run(context.Background(), erkaruntime.CommandRequest{
		Executable: os.Args[0], Arguments: []string{"-test.run=TestIsolationHelperProcess", "--", "--sandbox-helper", action, target},
		Directory: ".", Timeout: 30 * time.Second, OutputLimitBytes: 1 << 16, Unconfined: unconfined,
	})
	if err != nil {
		t.Fatalf("Run(%s %s) error = %v", action, target, err)
	}
	return result
}

func TestSandboxLimitsWritesToWorkspace(t *testing.T) {
	t.Parallel()
	fixture := newIsolationFixture(t, false)
	inside := filepath.Join(fixture.workspace, "inside.txt")
	if result := fixture.run(t, false, "write", inside); result.ExitCode != 0 {
		t.Fatalf("write inside workspace failed: %s", result.StandardOutput)
	}
	outside := filepath.Join(fixture.outside, "escaped.txt")
	if result := fixture.run(t, false, "write", outside); result.ExitCode == 0 {
		t.Fatal("sandboxed command wrote outside the workspace")
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("escaped file exists")
	}
	if result := fixture.run(t, true, "write", outside); result.ExitCode != 0 {
		t.Fatalf("unconfined write failed: %s", result.StandardOutput)
	}
}

func TestSandboxHidesCredentialPaths(t *testing.T) {
	t.Parallel()
	fixture := newIsolationFixture(t, false)
	secret := filepath.Join(fixture.outside, "secret", "id_rsa")
	if result := fixture.run(t, false, "read", secret); result.ExitCode == 0 {
		t.Fatal("sandboxed command read a hidden credential file")
	}
	if result := fixture.run(t, false, "read", filepath.Join(fixture.workspace, "..", filepath.Base(fixture.workspace))); result.ExitCode != 0 && !strings.Contains(result.StandardOutput, "directory") {
		t.Fatalf("workspace became unreadable: %s", result.StandardOutput)
	}
}

func TestSandboxBlocksNetworkButAllowsLoopback(t *testing.T) {
	t.Parallel()
	loopback, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer loopback.Close()
	go acceptAll(loopback)
	fixture := newIsolationFixture(t, false)
	if result := fixture.run(t, false, "selfloop", "-"); result.ExitCode != 0 {
		t.Fatalf("a sandboxed command could not use its own loopback: %s", result.StandardOutput)
	}
	// macOS keeps host loopback reachable; Linux isolates the network
	// namespace entirely, so host services are unreachable too.
	hostLoopback := fixture.run(t, false, "dial", loopback.Addr().String())
	if runtime.GOOS == "darwin" && hostLoopback.ExitCode != 0 {
		t.Fatalf("host loopback was blocked on macOS: %s", hostLoopback.StandardOutput)
	}
	if runtime.GOOS == "linux" && hostLoopback.ExitCode == 0 {
		t.Fatal("isolated Linux sandbox reached a host loopback service")
	}
	// 192.0.2.1 is TEST-NET-1: never routable, so an unconfined dial times
	// out while the sandbox must refuse it immediately.
	started := time.Now()
	result := fixture.run(t, false, "dial", "192.0.2.1:9")
	if result.ExitCode == 0 {
		t.Fatal("sandboxed command connected to an external address")
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond || strings.Contains(result.StandardOutput, "timeout") {
		t.Fatalf("external dial was not refused by policy (%s): %s", elapsed, result.StandardOutput)
	}
	external := nonLoopbackListener(t)
	defer external.Close()
	go acceptAll(external)
	allowed := newIsolationFixture(t, true)
	if result := allowed.run(t, false, "dial", external.Addr().String()); result.ExitCode != 0 {
		t.Fatalf("network-allowed sandbox was blocked: %s", result.StandardOutput)
	}
}

func TestSandboxGivesEachCommandAPrivateTemporaryDirectory(t *testing.T) {
	t.Parallel()
	fixture := newIsolationFixture(t, false)
	result := fixture.run(t, false, "tempfile", "-")
	if result.ExitCode != 0 {
		t.Fatalf("temporary file creation failed: %s", result.StandardOutput)
	}
	created := strings.TrimSpace(result.StandardOutput)
	if !strings.Contains(created, "erka-sandbox-") {
		t.Fatalf("temporary file %q is outside the private directory", created)
	}
	if _, err := os.Stat(filepath.Dir(created)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private temporary directory was not removed")
	}
}

func TestRequiredIsolationFailsClosedWithoutBackend(t *testing.T) {
	t.Parallel()
	local := &Local{root: canonicalTemp(t), isolation: Isolation{Mode: IsolationRequired}, unavailable: "test backend missing"}
	_, err := local.Run(context.Background(), erkaruntime.CommandRequest{Executable: "true", Directory: ".", Timeout: time.Second, OutputLimitBytes: 1024})
	if !errors.Is(err, ErrIsolationUnavailable) {
		t.Fatalf("Run() error = %v, want isolation unavailable", err)
	}
	auto := &Local{root: t.TempDir(), isolation: Isolation{Mode: IsolationAuto}, unavailable: "test backend missing"}
	if status := auto.IsolationStatus(); !strings.Contains(status, "unavailable") {
		t.Fatalf("status = %q", status)
	}
}

func acceptAll(listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		_ = connection.Close()
	}
}

func nonLoopbackListener(t *testing.T) net.Listener {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok || network.IP.IsLoopback() || network.IP.To4() == nil || network.IP.IsLinkLocalUnicast() {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(network.IP.String(), "0"))
		if err == nil {
			return listener
		}
	}
	t.Skip("no non-loopback IPv4 address to test against")
	return nil
}

func TestSandboxProtectsRepositoryHooksAndConfig(t *testing.T) {
	t.Parallel()
	fixture := newIsolationFixture(t, false)
	for _, path := range []string{".git/hooks", ".git/objects"} {
		if err := os.MkdirAll(filepath.Join(fixture.workspace, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fixture.workspace, ".git", "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Reload so the protected paths that now exist are part of the policy.
	local, err := NewLocal(fixture.workspace, Options{Isolation: fixture.local.isolation})
	if err != nil {
		t.Fatal(err)
	}
	fixture.local = local
	for _, target := range []string{".git/config", ".git/hooks/pre-commit"} {
		if result := fixture.run(t, false, "write", filepath.Join(fixture.workspace, target)); result.ExitCode == 0 {
			t.Fatalf("sandboxed command wrote %s", target)
		}
	}
	if result := fixture.run(t, false, "write", filepath.Join(fixture.workspace, ".git", "objects", "blob")); result.ExitCode != 0 {
		t.Fatalf("ordinary Git object write was blocked: %s", result.StandardOutput)
	}
	for _, name := range []string{".git/config", ".git/hooks/post-checkout", "sub/.GIT/config"} {
		if err := local.WriteFile(context.Background(), name, []byte("x"), 0o600); !errors.Is(err, ErrProtectedPath) && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("WriteFile(%q) error = %v, want protected", name, err)
		}
	}
}
