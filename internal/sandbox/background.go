package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// BackgroundRequest starts a long-running workspace process such as a dev
// server. It receives the same filtered environment as tool commands.
type BackgroundRequest struct {
	Executable  string
	Arguments   []string
	Directory   string
	Environment map[string]string
	Output      io.Writer
	// Unconfined skips OS isolation.
	Unconfined bool
}

// BackgroundProcess is a running process group that Stop terminates.
type BackgroundProcess struct {
	command *exec.Cmd
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	err     error
}

func (local *Local) StartBackground(request BackgroundRequest) (*BackgroundProcess, error) {
	if strings.TrimSpace(request.Executable) == "" {
		return nil, errors.New("background executable is empty")
	}
	directory, err := local.resolveExisting(request.Directory)
	if err != nil {
		return nil, fmt.Errorf("resolve background directory: %w", err)
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("background directory %q is not a directory", request.Directory)
	}
	command, cleanup, err := local.newCommand(request.Executable, request.Arguments, directory, request.Unconfined, request.Environment)
	if err != nil {
		return nil, err
	}
	command.Stdout, command.Stderr = request.Output, request.Output
	configureProcess(command)
	if err := command.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("start %q: %w", request.Executable, err)
	}
	process := &BackgroundProcess{command: command, done: make(chan struct{})}
	go func() {
		waitErr := command.Wait()
		cleanup()
		process.mu.Lock()
		process.err = waitErr
		process.mu.Unlock()
		close(process.done)
	}()
	return process, nil
}

func (process *BackgroundProcess) Done() <-chan struct{} { return process.done }

func (process *BackgroundProcess) Err() error {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.err
}

// Stop terminates the whole process group and waits for it to exit.
func (process *BackgroundProcess) Stop() error {
	var err error
	process.once.Do(func() {
		select {
		case <-process.done:
			return
		default:
		}
		err = terminateProcess(process.command)
		<-process.done
	})
	return err
}
