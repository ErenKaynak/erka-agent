package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ErenKaynak/erka-agent/internal/app"
	"github.com/ErenKaynak/erka-agent/internal/auth"
	"github.com/ErenKaynak/erka-agent/internal/bridges"
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

	// newAuthManager is overridden by tests that must not depend on the
	// operating-system keyring.
	newAuthManager = auth.NewDefaultManager
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
	var apiKeyEnv string
	var workingDirectory string
	var taskID string
	var allowFullExecution bool
	var pipeline string
	var remoteProfile string

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
				resolved, err := loadConfig(command.Context(), config.Options{ConfigPath: configPath, StateDir: stateDir, Provider: providerName, Model: model, BaseURL: baseURL, APIKeyEnv: apiKeyEnv, Pipeline: pipeline, AllowFullExecution: allowFullExecution})
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
	root.PersistentFlags().StringVarP(&taskID, "session", "s", "", "resume a session by ID; a unique prefix is enough")
	root.PersistentFlags().BoolVar(&allowFullExecution, "full", false, "allow arbitrary commands for this process")
	root.PersistentFlags().StringVar(&providerName, "provider", "", "native provider name (ERKA_PROVIDER)")
	root.PersistentFlags().StringVar(&model, "model", "", "model ID (ERKA_MODEL)")
	root.PersistentFlags().StringVar(&baseURL, "provider-base-url", "", "provider API base URL")
	root.PersistentFlags().StringVar(&apiKeyEnv, "api-key-env", "", "provider API key environment name (ERKA_API_KEY_ENV)")
	root.PersistentFlags().StringVar(&pipeline, "pipeline", "", "configured fixed pipeline (ERKA_PIPELINE)")
	root.PersistentFlags().StringVar(&remoteProfile, "profile", "", "remote server profile")
	root.PersistentFlags().StringVar(
		&stateDir,
		"state-dir",
		"",
		"path to the local state directory (ERKA_STATE_DIR)",
	)

	root.AddCommand(newVersionCommand(info))
	root.AddCommand(newAuthCommand(stdin))
	root.AddCommand(newProvidersCommand(func(ctx context.Context) (config.Resolved, error) {
		return loadConfig(ctx, config.Options{ConfigPath: configPath, StateDir: stateDir, Provider: providerName, Model: model, BaseURL: baseURL, APIKeyEnv: apiKeyEnv, Pipeline: pipeline, AllowFullExecution: allowFullExecution})
	}))
	root.AddCommand(newModelsCommand(func(ctx context.Context) (config.Resolved, error) {
		return loadConfig(ctx, config.Options{ConfigPath: configPath, StateDir: stateDir, Provider: providerName, Model: model, BaseURL: baseURL, APIKeyEnv: apiKeyEnv, Pipeline: pipeline, AllowFullExecution: allowFullExecution})
	}))
	loadServerConfig := func(ctx context.Context) (config.Resolved, error) {
		return loadConfig(ctx, config.Options{ConfigPath: configPath, StateDir: stateDir, Provider: providerName, Model: model, BaseURL: baseURL, APIKeyEnv: apiKeyEnv, Pipeline: pipeline, AllowFullExecution: allowFullExecution})
	}
	root.AddCommand(newMemoryCommand(loadServerConfig, func() string { return workingDirectory }))
	root.AddCommand(newServeCommand(loadServerConfig))
	root.AddCommand(newServerCommand(loadServerConfig))
	root.AddCommand(newProfileCommand(loadServerConfig))
	for _, command := range newRemoteCommands(loadServerConfig, func() string { return remoteProfile }) {
		root.AddCommand(command)
	}
	root.AddCommand(newRunCommand(func(ctx context.Context, prompt string, resume erkaruntime.TaskID, limits runLimits, output io.Writer) error {
		limits.allowFullExecution = allowFullExecution
		resolved, err := loadConfig(ctx, config.Options{
			ConfigPath:         configPath,
			StateDir:           stateDir,
			Provider:           providerName,
			Model:              model,
			BaseURL:            baseURL,
			APIKeyEnv:          apiKeyEnv,
			Pipeline:           pipeline,
			MaxIterations:      limits.maxIterations,
			MaxOutputTokens:    limits.maxOutputTokens,
			StageTimeout:       limits.stageTimeout,
			AllowFullExecution: limits.allowFullExecution,
		})
		if err != nil {
			return fmt.Errorf("resolve configuration: %w", err)
		}
		if remoteProfile != "" {
			if resume != "" {
				return fmt.Errorf("%w: remote run does not accept --resume; use watch or retry", ErrUsage)
			}
			return runRemoteTask(ctx, resolved.StateDir, remoteProfile, limits.project, prompt, output)
		}
		if limits.project != "" {
			return fmt.Errorf("%w: --project requires --profile", ErrUsage)
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
	project            string
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
	command.Flags().DurationVar(&limits.stageTimeout, "stage-timeout", 0, "overall stage timeout (ERKA_STAGE_TIMEOUT)")
	command.Flags().DurationVar(&limits.stageTimeout, "timeout", 0, "overall stage timeout (alias for --stage-timeout)")
	command.Flags().StringVar(&limits.project, "project", "", "remote server project ID")
	return command
}

func runConfigured(ctx context.Context, resolved config.Resolved, objective string, resume erkaruntime.TaskID, workingDirectory string, output io.Writer) error {
	manager := newAuthManager()
	if resolved.ActivePipeline != "" {
		factory := configuredProviderFactory(manager)
		if workingDirectory == "" {
			var err error
			workingDirectory, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
		}
		_, err := app.RunLocal(ctx, app.LocalOptions{WorkingDirectory: workingDirectory, StateDir: resolved.StateDir, Objective: objective, ResumeTaskID: resume, Output: output, AllowFullExecution: resolved.AllowFullExecution, Isolation: commandIsolation(resolved), Pipeline: resolved, ProviderFactory: factory, BridgeFactory: configuredBridgeFactory(), Limits: nativeLimits(resolved)})
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
		Objective: objective, ResumeTaskID: resume, Provider: provider, NativeModel: resolved.Model, Output: output,
		AllowFullExecution: resolved.AllowFullExecution,
		Isolation:          commandIsolation(resolved),
		Limits:             nativeLimits(resolved),
	})
	return err
}

func launchConfigured(ctx context.Context, resolved config.Resolved, options tuiLaunchOptions, input io.Reader, output io.Writer) error {
	manager := newAuthManager()
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
		prompter := tui.NewPrompter()
		session, err := app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: workingDirectory, StateDir: resolved.StateDir, Pipeline: resolved, ProviderFactory: configuredProviderFactory(manager), BridgeFactory: configuredBridgeFactory(), Observer: observer, Limits: nativeLimits(resolved), AllowFullExecution: options.Full, Interactor: prompter, ApproveCommands: true, Isolation: commandIsolation(resolved)})
		if err != nil {
			return err
		}
		defer session.Close()
		taskID, err := resolveSession(ctx, session, string(options.TaskID))
		if err != nil {
			return err
		}
		model := tui.NewModel(tui.Options{Context: ctx, Controller: session, TaskID: taskID, Provider: "pipeline", Model: resolved.ActivePipeline, Branch: session.Branch(ctx), Full: options.Full, Live: live, Providers: providerStatusFunc(manager, resolved), Models: modelCatalogFunc(manager, resolved), Prompter: prompter})
		runErr := tui.Run(model, input, output)
		printResumeHint(context.Background(), output, session, model.TaskID())
		return runErr
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
	rebuildProvider := func(model string) (erkaruntime.ModelProvider, error) {
		key, _, err := manager.Resolve(context.Background(), resolved.Provider, resolved.BaseURL, resolved.APIKeyEnv)
		if err != nil {
			return nil, fmt.Errorf("resolve %s credential: %w", resolved.Provider, err)
		}
		return openNativeProvider(resolved.Provider, resolved.BaseURL, model, key)
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
	// An interactive session has a person present, so commands are offered
	// with per-call consent instead of being hidden until full access.
	prompter := tui.NewPrompter()
	session, err := app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: workingDirectory, StateDir: resolved.StateDir, Provider: provider, NativeModel: resolved.Model, Observer: observer, Limits: nativeLimits(resolved), AllowFullExecution: options.Full, RebuildProvider: rebuildProvider, Interactor: prompter, ApproveCommands: true, Isolation: commandIsolation(resolved)})
	if err != nil {
		return err
	}
	defer session.Close()
	taskID, err := resolveSession(ctx, session, string(options.TaskID))
	if err != nil {
		return err
	}
	model := tui.NewModel(tui.Options{Context: ctx, Controller: session, TaskID: taskID, Provider: resolved.Provider, Model: resolved.Model, Branch: session.Branch(ctx), Full: options.Full, Live: live, Providers: providerStatusFunc(manager, resolved), Models: modelCatalogFunc(manager, resolved), Prompter: prompter})
	runErr := tui.Run(model, input, output)
	printResumeHint(context.Background(), output, session, model.TaskID())
	return runErr
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

func configuredBridgeFactory() app.BridgeFactory {
	return func(_ context.Context, candidate routing.Candidate) (erkaruntime.SubscriptionBridge, error) {
		bridge, err := bridges.New(candidate.Provider, candidate.Command)
		if err != nil {
			return nil, &routing.AttemptError{Kind: routing.FailureInvalid, Err: err}
		}
		return bridge, nil
	}
}

func providerStatusFunc(manager *auth.Manager, resolved config.Resolved) tui.ProviderStatusFunc {
	return func(ctx context.Context) ([]tui.ProviderInfo, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return collectProviderStatus(ctx, manager, resolved)
	}
}

func collectProviderStatus(ctx context.Context, manager *auth.Manager, resolved config.Resolved) ([]tui.ProviderInfo, error) {
	if len(resolved.Connections) == 0 {
		models := []string{resolved.Model}
		if strings.TrimSpace(resolved.Model) == "" {
			models = []string{config.DefaultModel}
		}
		return []tui.ProviderInfo{nativeStatus(ctx, manager, "default", resolved.Provider, resolved.BaseURL, resolved.APIKeyEnv, models)}, nil
	}
	names := make([]string, 0, len(resolved.Connections))
	for name := range resolved.Connections {
		names = append(names, name)
	}
	sort.Strings(names)
	infos := make([]tui.ProviderInfo, 0, len(names))
	for _, name := range names {
		connection := resolved.Connections[name]
		if connection.Class == config.ConnectionSubscriptionBridge {
			infos = append(infos, bridgeStatus(ctx, name, connection))
			continue
		}
		infos = append(infos, nativeStatus(ctx, manager, name, connection.Provider, connection.BaseURL, connection.APIKeyEnv, sortedModelKeys(connection.Models)))
	}
	return infos, nil
}

func nativeStatus(ctx context.Context, manager *auth.Manager, name, provider, baseURL, apiKeyEnv string, models []string) tui.ProviderInfo {
	info := tui.ProviderInfo{Name: name, Class: config.ConnectionNative, Detail: provider, Models: models}
	_, _, err := manager.Resolve(ctx, provider, baseURL, apiKeyEnv)
	switch {
	case err == nil:
		info.State = "ready"
	case errors.Is(err, auth.ErrCredentialNotFound):
		info.State = "credential required"
	default:
		info.State = "unavailable"
		info.Detail = provider + " (" + err.Error() + ")"
	}
	return info
}

func sortedModelKeys(models map[string]config.ModelConfig) []string {
	keys := make([]string, 0, len(models))
	for model := range models {
		keys = append(keys, model)
	}
	sort.Strings(keys)
	return keys
}

func bridgeStatus(ctx context.Context, name string, connection config.Connection) tui.ProviderInfo {
	info := tui.ProviderInfo{Name: name, Class: config.ConnectionSubscriptionBridge, Detail: connection.Provider, Models: sortedModelKeys(connection.Models)}
	bridge, err := bridges.New(connection.Provider, connection.Command)
	if err != nil {
		info.State = "unavailable"
		info.Detail = connection.Provider + " (" + err.Error() + ")"
		return info
	}
	availability, err := bridge.Availability(ctx)
	if err != nil {
		info.State = "unavailable"
		info.Detail = connection.Provider + " (" + err.Error() + ")"
		return info
	}
	switch {
	case !availability.Available:
		info.State = "unavailable"
	case !availability.Authenticated:
		info.State = "login required"
	default:
		info.State = "authenticated"
	}
	if strings.TrimSpace(availability.Detail) != "" {
		info.Detail = availability.Detail
	}
	return info
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
			if candidate.BackendClass == config.ConnectionSubscriptionBridge {
				continue
			}
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

func newAuthCommand(input io.Reader) *cobra.Command {
	command := &cobra.Command{Use: "auth", Short: "Manage subscription bridge authentication", Args: noArgs}
	var device bool
	login := &cobra.Command{
		Use: "login <codex|opencode>", Short: "Delegate login to an official bridge CLI",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: auth login requires exactly one bridge name", ErrUsage)
			}
			return nil
		},
		RunE: func(command *cobra.Command, args []string) error {
			if device && strings.ToLower(strings.TrimSpace(args[0])) != "codex" {
				return fmt.Errorf("%w: --device is only supported for codex", ErrUsage)
			}
			return bridges.Login(command.Context(), args[0], "", device, input, command.OutOrStdout(), command.ErrOrStderr())
		},
	}
	login.Flags().BoolVar(&device, "device", false, "use the Codex device-code login flow")
	status := &cobra.Command{
		Use: "status", Short: "Report subscription bridge authentication", Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			for _, bridge := range []erkaruntime.SubscriptionBridge{bridges.NewCodex(bridges.CodexOptions{}), bridges.NewOpenCode(bridges.OpenCodeOptions{})} {
				availability, err := bridge.Availability(command.Context())
				if err != nil {
					return fmt.Errorf("inspect %s bridge: %w", bridge.Name(), err)
				}
				state := "unavailable"
				if availability.Available {
					state = "login required"
				}
				if availability.Authenticated {
					state = "authenticated"
				}
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", bridge.Name(), state, availability.Detail); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.AddCommand(login, status)
	return command
}

func newProvidersCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	return &cobra.Command{
		Use: "providers", Short: "Report configured native providers and subscription bridges", Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			resolved, err := load(command.Context())
			if err != nil {
				return fmt.Errorf("resolve configuration: %w", err)
			}
			if _, err := fmt.Fprintln(command.OutOrStdout(), "NAME\tCLASS\tPROVIDER\tSTATE\tCAPABILITIES"); err != nil {
				return err
			}
			if len(resolved.Connections) == 0 {
				_, err = fmt.Fprintf(command.OutOrStdout(), "default\t%s\t%s\tconfigured\tprovider-defined\n", config.ConnectionNative, resolved.Provider)
				return err
			}
			names := make([]string, 0, len(resolved.Connections))
			for name := range resolved.Connections {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				connection := resolved.Connections[name]
				state := "configured"
				capabilities := "provider-defined"
				if connection.Class == config.ConnectionSubscriptionBridge {
					bridge, openErr := bridges.New(connection.Provider, connection.Command)
					if openErr != nil {
						return openErr
					}
					available, inspectErr := bridge.Availability(command.Context())
					if inspectErr != nil {
						return inspectErr
					}
					state = "unavailable"
					if available.Available {
						state = "login required"
					}
					if available.Authenticated {
						state = "available"
					}
					caps, _ := bridge.Capabilities(command.Context())
					capabilities = fmt.Sprintf("tools=%t structured_output=%t", caps.Tools, caps.StructuredOutput)
				}
				if models := sortedModelKeys(connection.Models); len(models) > 0 {
					capabilities += " configured_models=" + strings.Join(models, ",")
				}
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", name, connection.Class, connection.Provider, state, capabilities); err != nil {
					return err
				}
			}
			return nil
		},
	}
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
