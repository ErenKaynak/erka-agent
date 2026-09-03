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
	"golang.org/x/term"

	"github.com/ErenKaynak/erka-agent/internal/app"
	"github.com/ErenKaynak/erka-agent/internal/auth"
	"github.com/ErenKaynak/erka-agent/internal/buildinfo"
	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/providers/deepseek"
	"github.com/ErenKaynak/erka-agent/internal/providers/openai"
	"github.com/ErenKaynak/erka-agent/internal/routing"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/tui"
)

var (
	// ErrUsage identifies invalid command syntax and arguments.
	ErrUsage = errors.New("invalid command usage")
)

type configLoader func(context.Context, config.Options) (config.Resolved, error)
type taskRunner func(context.Context, config.Resolved, string, erkaruntime.TaskID, string, io.Writer) error
type tuiRunner func(context.Context, config.Resolved, tuiLaunchOptions, io.Reader, io.Writer) error

type tuiLaunchOptions struct {
	WorkingDirectory string
	TaskID           erkaruntime.TaskID
	Full             bool
}

// Execute runs the erka command and converts command errors to stable exit codes.
func Execute(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	info buildinfo.Info,
) int {
	interactive := terminalReader(os.Stdin) && terminalWriter(stdout)
	return executeWithTUI(ctx, args, os.Stdin, stdout, stderr, info, config.Load, runConfigured, launchConfigured, interactive)
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
	return executeWithTUI(ctx, args, nil, stdout, stderr, info, loadConfig, runTask, nil, false)
}

func executeWithTUI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, info buildinfo.Info, loadConfig configLoader, runTask taskRunner, launch tuiRunner, interactive bool) int {
	command := newRootCommand(info, loadConfig, runTask, launch, interactive, stdin)
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
	return newRootCommand(info, config.Load, runConfigured, launchConfigured, false, nil)
}

func newRootCommand(info buildinfo.Info, loadConfig configLoader, runTask taskRunner, launch tuiRunner, interactive bool, stdin io.Reader) *cobra.Command {
	var configPath string
	var stateDir string
	var providerName string
	var model string
	var baseURL string
	var workingDirectory string
	var taskID string
	var allowFullExecution bool
	var pipeline string

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
			if interactive && launch != nil {
				resolved, err := loadConfig(command.Context(), config.Options{ConfigPath: configPath, StateDir: stateDir, Provider: providerName, Model: model, BaseURL: baseURL, Pipeline: pipeline, AllowFullExecution: allowFullExecution})
				if err != nil {
					return fmt.Errorf("resolve configuration: %w", err)
				}
				return launch(command.Context(), resolved, tuiLaunchOptions{WorkingDirectory: workingDirectory, TaskID: erkaruntime.TaskID(taskID), Full: resolved.AllowFullExecution}, stdin, command.OutOrStdout())
			}
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
	root.PersistentFlags().StringVar(&workingDirectory, "cwd", "", "repository working directory")
	root.PersistentFlags().StringVar(&taskID, "task", "", "open a conversation by task ID")
	root.PersistentFlags().BoolVar(&allowFullExecution, "full", false, "allow arbitrary commands for this process")
	root.PersistentFlags().StringVar(&providerName, "provider", "", "native provider name (ERKA_PROVIDER)")
	root.PersistentFlags().StringVar(&model, "model", "", "model ID (ERKA_MODEL)")
	root.PersistentFlags().StringVar(&baseURL, "provider-base-url", "", "provider API base URL")
	root.PersistentFlags().StringVar(&pipeline, "pipeline", "", "configured fixed pipeline (ERKA_PIPELINE)")
	root.PersistentFlags().StringVar(
		&stateDir,
		"state-dir",
		"",
		"path to the local state directory (ERKA_STATE_DIR)",
	)

	root.AddCommand(newVersionCommand(info))
	root.AddCommand(newRunCommand(func(ctx context.Context, prompt string, resume erkaruntime.TaskID, limits runLimits, output io.Writer) error {
		limits.allowFullExecution = allowFullExecution
		resolved, err := loadConfig(ctx, config.Options{
			ConfigPath:         configPath,
			StateDir:           stateDir,
			Provider:           providerName,
			Model:              model,
			BaseURL:            baseURL,
			Pipeline:           pipeline,
			MaxIterations:      limits.maxIterations,
			MaxOutputTokens:    limits.maxOutputTokens,
			StageTimeout:       limits.stageTimeout,
			AllowFullExecution: limits.allowFullExecution,
		})
		if err != nil {
			return fmt.Errorf("resolve configuration: %w", err)
		}
		return runTask(ctx, resolved, prompt, resume, workingDirectory, output)
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
	return command
}

func runConfigured(ctx context.Context, resolved config.Resolved, objective string, resume erkaruntime.TaskID, workingDirectory string, output io.Writer) error {
	manager := auth.NewDefaultManager()
	if resolved.ActivePipeline != "" {
		factory := configuredProviderFactory(manager)
		if workingDirectory == "" {
			var err error
			workingDirectory, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
		}
		_, err := app.RunLocal(ctx, app.LocalOptions{WorkingDirectory: workingDirectory, StateDir: resolved.StateDir, Objective: objective, ResumeTaskID: resume, Output: output, AllowFullExecution: resolved.AllowFullExecution, Pipeline: resolved, ProviderFactory: factory, Limits: nativeLimits(resolved)})
		return err
	}
	providerName, baseURL := resolved.Provider, resolved.BaseURL
	if providerName == "" {
		providerName = "deepseek"
	}
	if baseURL == "" {
		baseURL = deepseek.DefaultBaseURL
	}
	key, _, err := manager.Resolve(ctx, providerName, baseURL, resolved.APIKeyEnv)
	if errors.Is(err, auth.ErrCredentialNotFound) {
		return fmt.Errorf("%s credential is missing: set %s or launch interactive erka", providerName, resolved.APIKeyEnv)
	}
	if err != nil {
		return fmt.Errorf("%s credential is missing: set %s or launch interactive erka (OS keyring unavailable: %w)", providerName, resolved.APIKeyEnv, err)
	}
	provider, err := openNativeProvider(providerName, baseURL, resolved.Model, key)
	if err != nil {
		return err
	}
	if workingDirectory == "" {
		workingDirectory, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
	}
	_, err = app.RunLocal(ctx, app.LocalOptions{
		WorkingDirectory: workingDirectory, StateDir: resolved.StateDir,
		Objective: objective, ResumeTaskID: resume, Provider: provider, Output: output,
		AllowFullExecution: resolved.AllowFullExecution,
		Limits:             nativeLimits(resolved),
	})
	return err
}

func launchConfigured(ctx context.Context, resolved config.Resolved, options tuiLaunchOptions, input io.Reader, output io.Writer) error {
	manager := auth.NewDefaultManager()
	if resolved.ActivePipeline != "" {
		if err := preparePipelineCredentials(ctx, resolved, manager, input, output); err != nil {
			return err
		}
		workingDirectory := options.WorkingDirectory
		var err error
		if workingDirectory == "" {
			workingDirectory, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		live := make(chan erkaruntime.LiveUpdate, 128)
		observer := erkaruntime.LiveObserverFunc(func(_ context.Context, update erkaruntime.LiveUpdate) {
			select {
			case live <- update:
			default:
			}
		})
		session, err := app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: workingDirectory, StateDir: resolved.StateDir, Pipeline: resolved, ProviderFactory: configuredProviderFactory(manager), Observer: observer, Limits: nativeLimits(resolved), AllowFullExecution: options.Full})
		if err != nil {
			return err
		}
		defer session.Close()
		model := tui.NewModel(tui.Options{Context: ctx, Controller: session, TaskID: options.TaskID, Provider: "pipeline", Model: resolved.ActivePipeline, Branch: session.Branch(ctx), Full: options.Full, Live: live})
		return tui.Run(model, input, output)
	}
	key, _, credentialErr := manager.Resolve(ctx, resolved.Provider, resolved.BaseURL, resolved.APIKeyEnv)
	if credentialErr != nil {
		key, credentialErr = tui.PromptCredential(ctx, input, output, resolved.Provider)
		if credentialErr != nil {
			return credentialErr
		}
		if source, saveErr := manager.Save(ctx, resolved.Provider, resolved.BaseURL, key); saveErr != nil {
			_, _ = fmt.Fprintf(output, "Warning: OS keyring unavailable; credential is in memory for this session only (%s).\n", source)
		}
	}
	provider, err := openNativeProvider(resolved.Provider, resolved.BaseURL, resolved.Model, key)
	if err != nil {
		return err
	}
	workingDirectory := options.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	live := make(chan erkaruntime.LiveUpdate, 128)
	observer := erkaruntime.LiveObserverFunc(func(_ context.Context, update erkaruntime.LiveUpdate) {
		select {
		case live <- update:
		default:
		}
	})
	session, err := app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: workingDirectory, StateDir: resolved.StateDir, Provider: provider, Observer: observer, Limits: nativeLimits(resolved), AllowFullExecution: options.Full})
	if err != nil {
		return err
	}
	defer session.Close()
	model := tui.NewModel(tui.Options{Context: ctx, Controller: session, TaskID: options.TaskID, Provider: resolved.Provider, Model: resolved.Model, Branch: session.Branch(ctx), Full: options.Full, Live: live})
	return tui.Run(model, input, output)
}

func nativeLimits(resolved config.Resolved) erkaruntime.NativeLimits {
	return erkaruntime.NativeLimits{MaxIterations: resolved.MaxIterations, MaxOutputTokens: resolved.MaxOutputTokens, MaxModelOutputBytes: 1 << 20, MaxCumulativeToolOutputBytes: 4 << 20, StageTimeout: resolved.StageTimeout}
}

func configuredProviderFactory(manager *auth.Manager) app.ProviderFactory {
	return func(ctx context.Context, candidate routing.Candidate) (erkaruntime.ModelProvider, error) {
		key, _, err := manager.Resolve(ctx, candidate.Provider, candidate.BaseURL, candidate.APIKeyEnv)
		if err != nil {
			kind := routing.FailureUnavailable
			if !errors.Is(err, auth.ErrCredentialNotFound) {
				kind = routing.FailureConnection
			}
			return nil, &routing.AttemptError{Kind: kind, Err: fmt.Errorf("connection %q credential: %w", candidate.Connection, err)}
		}
		provider, err := openNativeProvider(candidate.Provider, candidate.BaseURL, candidate.Model, key)
		if err != nil {
			return nil, &routing.AttemptError{Kind: routing.FailureInvalid, Err: err}
		}
		return provider, nil
	}
}

func openNativeProvider(providerName, baseURL, model, key string) (erkaruntime.ModelProvider, error) {
	switch providerName {
	case "deepseek":
		return deepseek.New(deepseek.Options{APIKey: key, BaseURL: baseURL, Model: model})
	case "openai":
		return openai.New(openai.Options{APIKey: key, BaseURL: baseURL, Model: model})
	default:
		return nil, fmt.Errorf("unsupported native provider %q", providerName)
	}
}

func preparePipelineCredentials(ctx context.Context, resolved config.Resolved, manager *auth.Manager, input io.Reader, output io.Writer) error {
	pipeline := resolved.Pipelines[resolved.ActivePipeline]
	seen := make(map[string]bool)
	for _, stage := range []routing.Stage{routing.StagePlanning, routing.StageCoding, routing.StageValidation, routing.StageRepair, routing.StageReview} {
		if _, configured := pipeline.Stages[string(stage)]; !configured {
			continue
		}
		route, err := routing.ResolveFixed(resolved, stage, nativeLimits(resolved))
		if err != nil {
			return err
		}
		for _, candidate := range route.Candidates {
			account, err := auth.Account(candidate.Provider, candidate.BaseURL)
			if err != nil {
				return err
			}
			if seen[account] {
				continue
			}
			seen[account] = true
			if _, _, err = manager.Resolve(ctx, candidate.Provider, candidate.BaseURL, candidate.APIKeyEnv); err == nil {
				continue
			}
			key, promptErr := tui.PromptCredential(ctx, input, output, candidate.Connection+" ("+candidate.Provider+")")
			if promptErr != nil {
				return promptErr
			}
			if source, saveErr := manager.Save(ctx, candidate.Provider, candidate.BaseURL, key); saveErr != nil {
				_, _ = fmt.Fprintf(output, "Warning: OS keyring unavailable; credential is in memory for this session only (%s).\n", source)
			}
		}
	}
	return nil
}

func terminalReader(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}
func terminalWriter(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
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
