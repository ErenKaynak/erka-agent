package bridges

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/ErenKaynak/erka-agent/internal/config"
)

// NormalizeBridgeName maps user-facing aliases ("claude") to provider names.
func NormalizeBridgeName(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "claude" {
		return "claude_code"
	}
	return provider
}

// Login delegates authentication to the official vendor CLI. erka never reads
// or copies the resulting credentials.
func Login(ctx context.Context, provider, executable string, device bool, input io.Reader, output, errorOutput io.Writer) error {
	provider = NormalizeBridgeName(provider)
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
	case "claude_code":
		if device {
			return errors.New("Claude Code login does not support erka's --device flag")
		}
		// `claude auth login` signs in with a Claude subscription by default.
		// erka never runs `claude setup-token` or handles the resulting token.
		arguments = []string{"auth", "login"}
	default:
		return fmt.Errorf("unsupported subscription bridge %q", provider)
	}
	if executable == "" {
		executable = provider
		if spec, ok := config.LookupProvider(provider); ok && spec.DefaultCommand != "" {
			executable = spec.DefaultCommand
		}
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
