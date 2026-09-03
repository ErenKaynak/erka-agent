package bridges

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Login delegates authentication to the official vendor CLI. erka never reads
// or copies the resulting credentials.
func Login(ctx context.Context, provider, executable string, device bool, input io.Reader, output, errorOutput io.Writer) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	var arguments []string
	switch provider {
	case "codex":
		arguments = []string{"login"}
		if device {
			arguments = append(arguments, "--device-auth")
		}
	case "opencode":
		if device {
			return errors.New("OpenCode login does not support erka's --device flag")
		}
		arguments = []string{"auth", "login"}
	default:
		return fmt.Errorf("unsupported subscription bridge %q", provider)
	}
	if executable == "" {
		executable = provider
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return fmt.Errorf("%s CLI is not installed: %w", provider, err)
	}
	command := exec.CommandContext(ctx, path, arguments...)
	command.Stdin = input
	command.Stdout = output
	command.Stderr = errorOutput
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s login failed: %w", provider, err)
	}
	return nil
}
