package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestLocalReadWriteAndList(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "b.txt"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := NewLocal(root, Options{})
	if err != nil {
		t.Fatal(err)
	}

	data, err := local.ReadFile(context.Background(), "src/a.txt", 16)
	if err != nil || string(data) != "old" {
		t.Fatalf("ReadFile() = %q, %v", data, err)
	}
	if err := local.WriteFile(context.Background(), "src/a.txt", []byte("new"), 0o640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "src", "a.txt"))
	if string(data) != "new" {
		t.Fatalf("written data = %q", data)
	}
	entries, err := local.ListDir(context.Background(), "src", 10)
	if err != nil || len(entries) != 2 || entries[0].Path != "src/a.txt" || entries[1].Path != "src/b.txt" {
		t.Fatalf("ListDir() = %#v, %v", entries, err)
	}
}

func TestLocalRejectsTraversalAndAbsolutePaths(t *testing.T) {
	t.Parallel()
	local, err := NewLocal(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"../outside", `..\outside`, "/absolute", `C:\absolute`, `\\server\share`}
	for _, name := range paths {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := local.ReadFile(context.Background(), name, 10); !errors.Is(err, ErrPathEscape) {
				t.Fatalf("ReadFile(%q) error = %v, want ErrPathEscape", name, err)
			}
		})
	}
}

func TestLocalRejectsSymlinkEscapeAndSymlinkWrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	local, err := NewLocal(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.ReadFile(context.Background(), "link.txt", 64); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("ReadFile() error = %v, want ErrPathEscape", err)
	}
	if err := local.WriteFile(context.Background(), "link.txt", []byte("overwrite"), 0o600); err == nil {
		t.Fatal("WriteFile() error = nil for symlink")
	}
	data, _ := os.ReadFile(outside)
	if string(data) != "secret" {
		t.Fatalf("outside file changed to %q", data)
	}
}

func TestLocalRejectsOversizedAndSpecialReads(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "second.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	local, err := NewLocal(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.ReadFile(context.Background(), "large.txt", 4); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("ReadFile() error = %v, want ErrFileTooLarge", err)
	}
	if _, err := local.ReadFile(context.Background(), ".", 100); !errors.Is(err, ErrSpecialFile) {
		t.Fatalf("ReadFile(directory) error = %v, want ErrSpecialFile", err)
	}
	if _, err := local.ListDir(context.Background(), ".", 0); err == nil {
		t.Fatal("ListDir() error = nil for zero limit")
	}
	if _, err := local.ListDir(context.Background(), ".", 1); !errors.Is(err, ErrDirectoryLimit) {
		t.Fatalf("ListDir() error = %v, want ErrDirectoryLimit", err)
	}
}

func TestLocalRunCapturesExitAndFiltersSecrets(t *testing.T) {
	t.Parallel()
	environment := append([]string(nil), os.Environ()...)
	environment = append(environment, "ERKA_DEEPSEEK_API_KEY=must-not-leak")
	local, err := NewLocal(t.TempDir(), Options{Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	request := helperRequest("output", 1024, 5*time.Second)
	result, err := local.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 || result.StandardOutput != "stdout:" || result.StandardError != "stderr" {
		t.Fatalf("Run() result = %#v", result)
	}
	request = helperRequest("nonzero", 1024, 5*time.Second)
	result, err = local.Run(context.Background(), request)
	if err != nil || result.ExitCode != 3 {
		t.Fatalf("Run(nonzero) = %#v, %v", result, err)
	}
}

func TestLocalRunCapsCombinedOutput(t *testing.T) {
	t.Parallel()
	local, err := NewLocal(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := local.Run(context.Background(), helperRequest("large", 16, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.StandardOutput)+len(result.StandardError) > 16 {
		t.Fatalf("Run() result = %#v", result)
	}
}

func TestLocalRunTimeoutAndCancellation(t *testing.T) {
	t.Parallel()
	local, err := NewLocal(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = local.Run(context.Background(), helperRequest("sleep", 1024, 50*time.Millisecond))
	if !errors.Is(err, ErrCommandTimedOut) || time.Since(started) > 2*time.Second {
		t.Fatalf("Run(timeout) error = %v, duration = %v", err, time.Since(started))
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err = local.Run(ctx, helperRequest("sleep", 1024, time.Second))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(cancel) error = %v, want context.Canceled", err)
	}
}

func TestLocalHonorsCancelledFilesystemContext(t *testing.T) {
	t.Parallel()
	local, err := NewLocal(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := local.ListDir(ctx, ".", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListDir() error = %v", err)
	}
	if err := local.WriteFile(ctx, "x", []byte("x"), 0o600); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func helperRequest(mode string, outputLimit int64, timeout time.Duration) erkaruntime.CommandRequest {
	return erkaruntime.CommandRequest{
		Executable: os.Args[0],
		Arguments:  []string{"-test.run=^TestSandboxHelperProcess$"},
		Directory:  ".",
		Environment: map[string]string{
			"ERKA_SANDBOX_HELPER": mode,
		},
		Timeout: timeout, OutputLimitBytes: outputLimit,
	}
}

func TestSandboxHelperProcess(t *testing.T) {
	mode := os.Getenv("ERKA_SANDBOX_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "output":
		fmt.Printf("stdout:%s", os.Getenv("ERKA_DEEPSEEK_API_KEY"))
		_, _ = fmt.Fprint(os.Stderr, "stderr")
		os.Exit(0)
	case "nonzero":
		os.Exit(3)
	case "large":
		fmt.Print(strings.Repeat("x", 128))
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("y", 128))
		os.Exit(0)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "tree":
		command := exec.Command(os.Args[0], "-test.run=^TestSandboxHelperProcess$")
		command.Env = replaceEnvironment(os.Environ(), "ERKA_SANDBOX_HELPER", "child-sleep")
		if err := command.Start(); err != nil {
			os.Exit(5)
		}
		if err := os.WriteFile(os.Getenv("ERKA_HELPER_PID_FILE"), []byte(fmt.Sprint(command.Process.Pid)), 0o600); err != nil {
			os.Exit(6)
		}
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "child-sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	default:
		os.Exit(4)
	}
}

func replaceEnvironment(environment []string, key, value string) []string {
	result := make([]string, 0, len(environment)+1)
	prefix := key + "="
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}
