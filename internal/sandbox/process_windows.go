//go:build windows

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
)

func configureProcess(*exec.Cmd) {}

func terminateProcess(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	// taskkill /T follows the Windows parent-process tree. Process.Kill remains
	// the fallback when taskkill is unavailable or the process exits meanwhile.
	treeErr := exec.Command(
		"taskkill", "/T", "/F", "/PID", strconv.Itoa(command.Process.Pid),
	).Run()
	if treeErr == nil {
		return nil
	}
	killErr := command.Process.Kill()
	if errors.Is(killErr, os.ErrProcessDone) {
		return nil
	}
	return errors.Join(treeErr, killErr)
}
