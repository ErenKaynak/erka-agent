package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ErenKaynak/erka-agent/internal/app"
	"github.com/ErenKaynak/erka-agent/internal/buildinfo"
	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/providers/deepseek"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

var (
	// ErrUsage identifies invalid command syntax and arguments.
	ErrUsage = errors.New("invalid command usage")
)

type configLoader func(context.Context, config.Options) (config.Resolved, error)
type taskRunner func(context.Context, config.Resolved, string, erkaruntime.TaskID, io.Writer) error

// Execute runs the erka command and converts command errors to stable exit codes.
func Execute(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	info buildinfo.Info,
) int {
	return execute(ctx, args, stdout, stderr, info, config.Load, runConfigured)
}

func execute(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	info buildinfo.Info,
	loadConfig configLoader,
	runTask taskRunner,
) int {
	command := newRootCommand(info, loadConfig, runTask)
	command.SetArgs(args)
	command.SetOut(stdout)
	command.SetErr(stderr)

	if err := command.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		if errors.Is(err, ErrUsage) {
			return 2
		}
		return 1
	}

	return 0
}

// NewRootCommand constructs the public erka command hierarchy.
func NewRootCommand(info buildinfo.Info) *cobra.Command {
	return newRootCommand(info, config.Load, runConfigured)
}

func newRootCommand(info buildinfo.Info, loadConfig configLoader, runTask taskRunner) *cobra.Command {
	var configPath string
	var stateDir string
	var providerName string
	var model string
	var baseURL string

	root := &cobra.Command{
		Use:           "erka",
		Short:         "A local-first coding-agent runtime",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("%w: unknown command %q", ErrUsage, args[0])
			}
			return nil
		},
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	root.Version = info.Version
	root.SetVersionTemplate("erka {{.Version}}\n")
	root.PersistentFlags().StringVar(
		&configPath,
		"config",
		"",
		"path to the user configuration file (ERKA_CONFIG_PATH)",
	)
	root.PersistentFlags().StringVar(&providerName, "provider", "", "native provider name (ERKA_PROVIDER)")
	root.PersistentFlags().StringVar(&model, "model", "", "model ID (ERKA_MODEL)")
	root.PersistentFlags().StringVar(&baseURL, "provider-base-url", "", "provider API base URL")
	root.PersistentFlags().StringVar(
		&stateDir,
		"state-dir",
		"",
		"path to the local state directory (ERKA_STATE_DIR)",
	)

	root.AddCommand(newVersionCommand(info))
	root.AddCommand(newRunCommand(func(ctx context.Context, prompt string, resume erkaruntime.TaskID, limits runLimits, output io.Writer) error {
		resolved, err := loadConfig(ctx, config.Options{
			ConfigPath:         configPath,
			StateDir:           stateDir,
			Provider:           providerName,
			Model:              model,
			BaseURL:            baseURL,
			MaxIterations:      limits.maxIterations,
			MaxOutputTokens:    limits.maxOutputTokens,
			StageTimeout:       limits.stageTimeout,
			AllowFullExecution: limits.allowFullExecution,
		})
		if err != nil {
			return fmt.Errorf("resolve configuration: %w", err)
		}
		return runTask(ctx, resolved, prompt, resume, output)
	}))

	return root
}

func newVersionCommand(info buildinfo.Info) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(
				command.OutOrStdout(),
				"erka %s\ncommit: %s\nbuilt: %s\n",
				info.Version,
				info.Commit,
				info.Date,
			)
			return err
		},
	}
}

type runLimits struct {
	maxIterations      int
	maxOutputTokens    int64
	stageTimeout       time.Duration
	allowFullExecution bool
}

func newRunCommand(run func(context.Context, string, erkaruntime.TaskID, runLimits, io.Writer) error) *cobra.Command {
	var resume string
	var limits runLimits
	command := &cobra.Command{
		Use:   "run [prompt]",
		Short: "Run a coding task in the current repository",
		Args: func(_ *cobra.Command, args []string) error {
			if resume == "" {
				if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
					return fmt.Errorf("%w: a new task requires exactly one non-empty prompt", ErrUsage)
				}
				return nil
			}
			if len(args) != 0 {
				return fmt.Errorf("%w: --resume cannot be combined with a new prompt", ErrUsage)
			}
			return nil
		},
		RunE: func(command *cobra.Command, args []string) error {
			prompt := ""
			if len(args) == 1 {
				prompt = args[0]
			}
			return run(command.Context(), prompt, erkaruntime.TaskID(resume), limits, command.OutOrStdout())
		},
	}
	command.Flags().StringVar(&resume, "resume", "", "resume a durable task by ID")
	command.Flags().IntVar(&limits.maxIterations, "max-iterations", 0, "maximum model/tool iterations")
	command.Flags().Int64Var(&limits.maxOutputTokens, "max-output-tokens", 0, "maximum output tokens per model request")
	command.Flags().DurationVar(&limits.stageTimeout, "timeout", 0, "overall stage timeout")
	command.Flags().BoolVar(&limits.allowFullExecution, "full", false, "allow arbitrary commands with the current user's access")
	return command
}

func runConfigured(ctx context.Context, resolved config.Resolved, objective string, resume erkaruntime.TaskID, output io.Writer) error {
	key, ok := os.LookupEnv(resolved.APIKeyEnv)
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("DeepSeek credential is missing: set %s", resolved.APIKeyEnv)
	}
	provider, err := deepseek.New(deepseek.Options{APIKey: key, BaseURL: resolved.BaseURL, Model: resolved.Model})
	if err != nil {
		return err
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	_, err = app.RunLocal(ctx, app.LocalOptions{
		WorkingDirectory: workingDirectory, StateDir: resolved.StateDir,
		Objective: objective, ResumeTaskID: resume, Provider: provider, Output: output,
		AllowFullExecution: resolved.AllowFullExecution,
		Limits: erkaruntime.NativeLimits{
			MaxIterations: resolved.MaxIterations, MaxOutputTokens: resolved.MaxOutputTokens,
			MaxModelOutputBytes: 1 << 20, MaxCumulativeToolOutputBytes: 4 << 20,
			StageTimeout: resolved.StageTimeout,
		},
	})
	return err
}

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%w: expected no arguments, received %d", ErrUsage, len(args))
	}
	return nil
}

func exactArgs(want int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != want {
			return fmt.Errorf("%w: expected %d argument, received %d", ErrUsage, want, len(args))
		}
		return nil
	}
}
