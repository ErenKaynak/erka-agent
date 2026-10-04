package bridges

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/sandbox"
)

var ErrMalformedStream = errors.New("malformed bridge stream")

type Invocation struct {
	Executable       string
	Arguments        []string
	Directory        string
	Environment      map[string]string
	Input            string
	OutputLimitBytes int64
}

type ProcessResult struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
}

type Runner interface {
	LookPath(string) (string, error)
	Run(context.Context, Invocation) (ProcessResult, error)
}

type LocalRunner struct{}

func (LocalRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (LocalRunner) Run(ctx context.Context, invocation Invocation) (ProcessResult, error) {
	if strings.TrimSpace(invocation.Directory) == "" {
		return ProcessResult{}, errors.New("bridge process requires a working directory")
	}
	local, err := sandbox.NewLocal(invocation.Directory, sandbox.Options{Environment: os.Environ()})
	if err != nil {
		return ProcessResult{}, err
	}
	limit := invocation.OutputLimitBytes
	if limit <= 0 {
		return ProcessResult{}, errors.New("bridge process output limit must be positive")
	}
	result, err := local.Run(ctx, erkaruntime.CommandRequest{
		Executable: invocation.Executable, Arguments: invocation.Arguments,
		Directory: ".", Environment: invocation.Environment,
		StandardInput: []byte(invocation.Input), Timeout: 24 * time.Hour,
		OutputLimitBytes: limit,
	})
	return ProcessResult{ExitCode: result.ExitCode, Stdout: result.StandardOutput, Stderr: result.StandardError, Truncated: result.Truncated}, err
}

func New(provider, executable string) (erkaruntime.SubscriptionBridge, error) {
	if _, err := config.RequireCredential(provider, config.CredentialVendorCLIBridge); err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		return NewCodex(CodexOptions{Executable: executable}), nil
	case "opencode":
		return NewOpenCode(OpenCodeOptions{Executable: executable}), nil
	default:
		return nil, fmt.Errorf("unsupported subscription bridge %q", provider)
	}
}

func outputBudget(modelBytes int64) int64 {
	const overhead = int64(256 << 10)
	if modelBytes <= 0 {
		modelBytes = 1 << 20
	}
	if modelBytes > (8<<20-overhead)/4 {
		return 8 << 20
	}
	return modelBytes*4 + overhead
}

func selectedEnvironment(names ...string) map[string]string {
	result := make(map[string]string)
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			result[name] = value
		}
	}
	return result
}

func providerFailure(code, message string, retryable bool) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "subscription bridge failed"
	}
	if len(message) > 4096 {
		message = message[:4096]
	}
	return &erkaruntime.ProviderError{Code: code, Message: message, Retryable: retryable}
}
