//go:build !windows

package sandbox

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLocalTimeoutKillsProcessGroup(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	local, err := NewLocal(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pidFile := root + string(os.PathSeparator) + "child.pid"
	request := helperRequest("tree", 1024, 200*time.Millisecond)
	request.Environment["ERKA_HELPER_PID_FILE"] = pidFile
	if _, err := local.Run(context.Background(), request); !errors.Is(err, ErrCommandTimedOut) {
		t.Fatalf("Run() error = %v, want ErrCommandTimedOut", err)
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse child PID: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d still exists after process-group timeout", pid)
}
