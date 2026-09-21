package cli

import (
	"path/filepath"

	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/sandbox"
)

// commandIsolation builds the OS sandbox policy for commands the model runs.
// erka's own state and configuration are hidden from commands, alongside the
// standard credential stores.
func commandIsolation(resolved config.Resolved) sandbox.Isolation {
	mode := sandbox.IsolationAuto
	switch resolved.Sandbox.Mode {
	case config.SandboxRequired:
		mode = sandbox.IsolationRequired
	case config.SandboxOff:
		mode = sandbox.IsolationOff
	}
	private := []string{resolved.StateDir}
	if resolved.ConfigPath != "" {
		private = append(private, filepath.Dir(resolved.ConfigPath))
	}
	return sandbox.DefaultIsolation(mode, resolved.Sandbox.Network, resolved.Sandbox.WritablePaths, private)
}
