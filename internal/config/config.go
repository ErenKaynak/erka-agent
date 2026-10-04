package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ErenKaynak/erka-agent/internal/hooks"
	"github.com/ErenKaynak/erka-agent/internal/permissions"
)

const CurrentVersion = 1

const (
	DefaultProvider              = "deepseek"
	DefaultModel                 = "deepseek-v4-flash"
	DefaultDeepSeekURL           = "https://api.deepseek.com"
	DefaultOpenAIURL             = "https://api.openai.com"
	DefaultAnthropicURL          = "https://api.anthropic.com"
	DefaultAPIKeyEnv             = "ERKA_DEEPSEEK_API_KEY"
	DefaultMaxIterations         = 24
	DefaultMaxOutputToken        = int64(8192)
	DefaultStageTimeout          = 30 * time.Minute
	ConnectionNative             = "native"
	ConnectionSubscriptionBridge = "subscription_bridge"
)

// Source identifies where a resolved configuration value originated.
type Source string

const (
	SourceDefault     Source = "default"
	SourceConfigFile  Source = "config_file"
	SourceEnvironment Source = "environment"
	SourceFlag        Source = "flag"
)

// Paths contains platform-standard locations before overrides are applied.
type Paths struct {
	ConfigPath string
	StateDir   string
}

// Resolved is the effective M0 configuration and the source of each value.
type Resolved struct {
	Version          int
	ConfigPath       string
	ConfigPathSource Source
	StateDir         string
	StateDirSource   Source
	Provider         string
	ProviderSource   Source
	Model            string
	ModelSource      Source
	BaseURL          string
	BaseURLSource    Source
	APIKeyEnv        string
	MaxIterations    int
	MaxOutputTokens  int64
	StageTimeout     time.Duration
	// ContextBudgetTokens is the runtime-wide context budget (0 = model window).
	ContextBudgetTokens int64
	AllowFullExecution  bool
	// PermissionRules are allow/ask/deny patterns for tool calls.
	PermissionRules permissions.Rules
	// AutoApprovePermissions answers every "ask" verdict with allow. It is
	// set only by `erka run --yes` and never read from configuration.
	AutoApprovePermissions bool
	// Hooks are user-configured commands run after workspace edits.
	Hooks          hooks.Config
	ActivePipeline string
	Connections    map[string]Connection
	Pipelines      map[string]Pipeline
}

// Connection names either a native provider endpoint or an explicitly labeled
// optional subscription bridge. Native credentials are referenced by
// environment variable name; bridge credentials remain owned by the vendor.
type Connection struct {
	Provider  string                 `yaml:"provider"`
	Class     string                 `yaml:"class,omitempty"`
	Command   string                 `yaml:"command,omitempty"`
	BaseURL   string                 `yaml:"base_url,omitempty"`
	APIKeyEnv string                 `yaml:"api_key_env,omitempty"`
	Models    map[string]ModelConfig `yaml:"models,omitempty"`
	// Credential optionally names the credential kind. When empty it is
	// derived from Class; it must always be allowed by the provider spec.
	Credential CredentialKind `yaml:"credential,omitempty"`
	// Experimental must be true to enable a provider marked experimental.
	Experimental bool `yaml:"experimental,omitempty"`
}

type ModelConfig struct {
	Capabilities                   []string `yaml:"capabilities,omitempty"`
	ContextTokens                  int64    `yaml:"context_tokens,omitempty"`
	InputCostPerMillionMicrounits  int64    `yaml:"input_cost_per_million_microunits,omitempty"`
	OutputCostPerMillionMicrounits int64    `yaml:"output_cost_per_million_microunits,omitempty"`
	// Optional cache pricing. When either is set, cached input tokens are
	// billed at these rates instead of the regular input rate.
	CacheReadCostPerMillionMicrounits  int64  `yaml:"cache_read_cost_per_million_microunits,omitempty"`
	CacheWriteCostPerMillionMicrounits int64  `yaml:"cache_write_cost_per_million_microunits,omitempty"`
	Currency                           string `yaml:"currency,omitempty"`
}

type Pipeline struct {
	Routing           string                `yaml:"routing"`
	MaxRepairAttempts int                   `yaml:"max_repair_attempts,omitempty"`
	Stages            map[string]StageRoute `yaml:"stages"`
}

type StageRoute struct {
	Candidates           []string `yaml:"candidates"`
	RequiredCapabilities []string `yaml:"required_capabilities,omitempty"`
	Budget               Budget   `yaml:"budget,omitempty"`
}

type Budget struct {
	MaxInputTokens    int64  `yaml:"max_input_tokens,omitempty"`
	MaxOutputTokens   int64  `yaml:"max_output_tokens,omitempty"`
	MaxIterations     int    `yaml:"max_iterations,omitempty"`
	Timeout           string `yaml:"timeout,omitempty"`
	MaxCostMicrounits int64  `yaml:"max_cost_microunits,omitempty"`
	Currency          string `yaml:"currency,omitempty"`
}

// Options supplies command flags and injectable operating-system dependencies.
// Empty dependency fields use the running process and operating system.
type Options struct {
	ConfigPath string
	StateDir   string
	Provider   string
	Model      string
	BaseURL    string
	APIKeyEnv  string
	Pipeline   string

	MaxIterations      int
	MaxOutputTokens    int64
	StageTimeout       time.Duration
	AllowFullExecution bool

	GOOS      string
	HomeDir   string
	LookupEnv func(string) (string, bool)
	ReadFile  func(string) ([]byte, error)
}

type fileConfig struct {
	Version     int                   `yaml:"version"`
	StateDir    string                `yaml:"state_dir,omitempty"`
	Pipeline    string                `yaml:"pipeline,omitempty"`
	Connections map[string]Connection `yaml:"connections,omitempty"`
	Pipelines   map[string]Pipeline   `yaml:"pipelines,omitempty"`
	Provider    struct {
		Type      string `yaml:"type,omitempty"`
		Model     string `yaml:"model,omitempty"`
		BaseURL   string `yaml:"base_url,omitempty"`
		APIKeyEnv string `yaml:"api_key_env,omitempty"`
	} `yaml:"provider,omitempty"`
	Runtime struct {
		MaxIterations   int    `yaml:"max_iterations,omitempty"`
		MaxOutputTokens int64  `yaml:"max_output_tokens,omitempty"`
		StageTimeout    string `yaml:"stage_timeout,omitempty"`
		// ContextBudgetTokens bounds model context when a stage has no
		// max_input_tokens budget; erka compacts history near this limit.
		ContextBudgetTokens int64 `yaml:"context_budget_tokens,omitempty"`
	} `yaml:"runtime,omitempty"`
	Hooks       hooks.Config `yaml:"hooks,omitempty"`
	Permissions struct {
		AllowFullExecution bool `yaml:"allow_full_execution,omitempty"`
		Rules              struct {
			Allow []string `yaml:"allow,omitempty"`
			Ask   []string `yaml:"ask,omitempty"`
			Deny  []string `yaml:"deny,omitempty"`
		} `yaml:"rules,omitempty"`
	} `yaml:"permissions,omitempty"`
}

// Load resolves defaults, a user configuration file, environment variables,
// and command flags in ascending precedence order.
func Load(ctx context.Context, options Options) (Resolved, error) {
	if err := ctx.Err(); err != nil {
		return Resolved{}, err
	}

	options = withDefaults(options)
	paths, err := DefaultPaths(options.GOOS, options.HomeDir, options.LookupEnv)
	if err != nil {
		return Resolved{}, err
	}

	resolved := Resolved{
		Version:          CurrentVersion,
		ConfigPath:       paths.ConfigPath,
		ConfigPathSource: SourceDefault,
		StateDir:         paths.StateDir,
		StateDirSource:   SourceDefault,
		Provider:         DefaultProvider,
		ProviderSource:   SourceDefault,
		Model:            DefaultModel,
		ModelSource:      SourceDefault,
		BaseURL:          DefaultDeepSeekURL,
		BaseURLSource:    SourceDefault,
		APIKeyEnv:        DefaultAPIKeyEnv,
		MaxIterations:    DefaultMaxIterations,
		MaxOutputTokens:  DefaultMaxOutputToken,
		StageTimeout:     DefaultStageTimeout,
	}

	explicitConfig := false
	if value, ok := nonEmptyEnv(options.LookupEnv, "ERKA_CONFIG_PATH"); ok {
		resolved.ConfigPath = value
		resolved.ConfigPathSource = SourceEnvironment
		explicitConfig = true
	}
	if options.ConfigPath != "" {
		resolved.ConfigPath = options.ConfigPath
		resolved.ConfigPathSource = SourceFlag
		explicitConfig = true
	}

	data, readErr := options.ReadFile(resolved.ConfigPath)
	switch {
	case readErr == nil:
		loaded, parseErr := parse(data)
		if parseErr != nil {
			return Resolved{}, fmt.Errorf("load config %q: %w", resolved.ConfigPath, parseErr)
		}
		resolved.Version = loaded.Version
		if loaded.StateDir != "" {
			resolved.StateDir = loaded.StateDir
			resolved.StateDirSource = SourceConfigFile
		}
		if loaded.Provider.Type != "" {
			resolved.Provider, resolved.ProviderSource = loaded.Provider.Type, SourceConfigFile
		}
		if loaded.Provider.Model != "" {
			resolved.Model, resolved.ModelSource = loaded.Provider.Model, SourceConfigFile
		}
		if loaded.Provider.BaseURL != "" {
			resolved.BaseURL, resolved.BaseURLSource = loaded.Provider.BaseURL, SourceConfigFile
		}
		if loaded.Provider.APIKeyEnv != "" {
			resolved.APIKeyEnv = loaded.Provider.APIKeyEnv
		}
		if loaded.Runtime.MaxIterations != 0 {
			resolved.MaxIterations = loaded.Runtime.MaxIterations
		}
		if loaded.Runtime.MaxOutputTokens != 0 {
			resolved.MaxOutputTokens = loaded.Runtime.MaxOutputTokens
		}
		if loaded.Runtime.StageTimeout != "" {
			resolved.StageTimeout, parseErr = time.ParseDuration(loaded.Runtime.StageTimeout)
			if parseErr != nil {
				return Resolved{}, fmt.Errorf("load config %q: parse runtime.stage_timeout: %w", resolved.ConfigPath, parseErr)
			}
		}
		resolved.AllowFullExecution = loaded.Permissions.AllowFullExecution
		resolved.ContextBudgetTokens = loaded.Runtime.ContextBudgetTokens
		resolved.Hooks = loaded.Hooks
		resolved.PermissionRules = permissions.Rules{Allow: loaded.Permissions.Rules.Allow, Ask: loaded.Permissions.Rules.Ask, Deny: loaded.Permissions.Rules.Deny}
		resolved.ActivePipeline = loaded.Pipeline
		resolved.Connections = loaded.Connections
		resolved.Pipelines = loaded.Pipelines
	case errors.Is(readErr, fs.ErrNotExist) && !explicitConfig:
		// A default config file is optional.
	case readErr != nil:
		return Resolved{}, fmt.Errorf("read config %q: %w", resolved.ConfigPath, readErr)
	}

	if value, ok := nonEmptyEnv(options.LookupEnv, "ERKA_STATE_DIR"); ok {
		resolved.StateDir = value
		resolved.StateDirSource = SourceEnvironment
	}
	applyStringEnv(options.LookupEnv, "ERKA_PROVIDER", &resolved.Provider, &resolved.ProviderSource)
	applyStringEnv(options.LookupEnv, "ERKA_MODEL", &resolved.Model, &resolved.ModelSource)
	applyStringEnv(options.LookupEnv, "ERKA_DEEPSEEK_BASE_URL", &resolved.BaseURL, &resolved.BaseURLSource)
	applyStringEnv(options.LookupEnv, "ERKA_PROVIDER_BASE_URL", &resolved.BaseURL, &resolved.BaseURLSource)
	if value, ok := nonEmptyEnv(options.LookupEnv, "ERKA_API_KEY_ENV"); ok {
		resolved.APIKeyEnv = value
	}
	if value, ok := nonEmptyEnv(options.LookupEnv, "ERKA_PIPELINE"); ok {
		resolved.ActivePipeline = value
	}
	if err := applyIntEnv(options.LookupEnv, "ERKA_MAX_ITERATIONS", &resolved.MaxIterations); err != nil {
		return Resolved{}, err
	}
	if err := applyInt64Env(options.LookupEnv, "ERKA_MAX_OUTPUT_TOKENS", &resolved.MaxOutputTokens); err != nil {
		return Resolved{}, err
	}
	if value, ok := nonEmptyEnv(options.LookupEnv, "ERKA_STAGE_TIMEOUT"); ok {
		resolved.StageTimeout, err = time.ParseDuration(value)
		if err != nil {
			return Resolved{}, fmt.Errorf("parse ERKA_STAGE_TIMEOUT: %w", err)
		}
	}
	if value, ok := nonEmptyEnv(options.LookupEnv, "ERKA_ALLOW_FULL_EXECUTION"); ok {
		resolved.AllowFullExecution, err = strconv.ParseBool(value)
		if err != nil {
			return Resolved{}, fmt.Errorf("parse ERKA_ALLOW_FULL_EXECUTION: %w", err)
		}
	}
	if options.StateDir != "" {
		resolved.StateDir = options.StateDir
		resolved.StateDirSource = SourceFlag
	}
	if options.Provider != "" {
		resolved.Provider, resolved.ProviderSource = options.Provider, SourceFlag
	}
	if options.Model != "" {
		resolved.Model, resolved.ModelSource = options.Model, SourceFlag
	}
	if options.BaseURL != "" {
		resolved.BaseURL, resolved.BaseURLSource = options.BaseURL, SourceFlag
	}
	if options.APIKeyEnv != "" {
		resolved.APIKeyEnv = options.APIKeyEnv
	}
	if options.Pipeline != "" {
		resolved.ActivePipeline = options.Pipeline
	}
	if options.MaxIterations != 0 {
		resolved.MaxIterations = options.MaxIterations
	}
	if options.MaxOutputTokens != 0 {
		resolved.MaxOutputTokens = options.MaxOutputTokens
	}
	if options.StageTimeout != 0 {
		resolved.StageTimeout = options.StageTimeout
	}
	if options.AllowFullExecution {
		resolved.AllowFullExecution = true
	}

	if !isPlatformAbs(options.GOOS, resolved.StateDir) {
		return Resolved{}, fmt.Errorf("state directory must be absolute: %q", resolved.StateDir)
	}
	spec, ok := LookupProvider(resolved.Provider)
	if !ok || spec.Class != ConnectionNative || spec.Experimental {
		return Resolved{}, fmt.Errorf("unsupported native provider %q", resolved.Provider)
	}
	if !spec.Allows(CredentialAPIKey) {
		return Resolved{}, fmt.Errorf("provider %q does not allow credential kind %q", resolved.Provider, CredentialAPIKey)
	}
	if spec.Name != DefaultProvider {
		if resolved.BaseURLSource == SourceDefault {
			if spec.DefaultBaseURL == "" {
				return Resolved{}, fmt.Errorf("provider %q requires an explicit base URL", spec.Name)
			}
			resolved.BaseURL = spec.DefaultBaseURL
		}
		if resolved.APIKeyEnv == DefaultAPIKeyEnv {
			resolved.APIKeyEnv = spec.DefaultAPIKeyEnv
		}
		if spec.RequiresModel && resolved.ModelSource == SourceDefault {
			return Resolved{}, fmt.Errorf("provider %q requires an explicit model ID", spec.Name)
		}
	}
	if strings.TrimSpace(resolved.Model) == "" || (strings.TrimSpace(resolved.APIKeyEnv) == "" && !spec.APIKeyOptional) {
		return Resolved{}, errors.New("provider model and API key environment name must not be empty")
	}
	if resolved.APIKeyEnv != "" && !validEnvironmentName(resolved.APIKeyEnv) {
		return Resolved{}, fmt.Errorf("invalid API key environment name %q", resolved.APIKeyEnv)
	}
	if credentialEnvironmentConflicts(resolved.APIKeyEnv) {
		return Resolved{}, fmt.Errorf("API key environment %q is also a general subprocess variable", resolved.APIKeyEnv)
	}
	if resolved.MaxIterations <= 0 || resolved.MaxOutputTokens <= 0 || resolved.StageTimeout <= 0 {
		return Resolved{}, errors.New("runtime limits must be positive")
	}
	if err := resolved.PermissionRules.Validate(); err != nil {
		return Resolved{}, err
	}
	if resolved.ContextBudgetTokens < 0 {
		return Resolved{}, errors.New("runtime.context_budget_tokens must not be negative")
	}
	if err := resolved.Hooks.Validate(); err != nil {
		return Resolved{}, err
	}
	if err := validateRouting(&resolved); err != nil {
		return Resolved{}, err
	}
	if err := ctx.Err(); err != nil {
		return Resolved{}, err
	}

	return resolved, nil
}

var supportedStages = map[string]bool{
	"planning": true, "coding": true, "validation": true, "repair": true, "review": true,
}

var supportedCapabilities = map[string]bool{
	"tools": true, "parallel_tools": true, "structured_output": true, "vision": true,
}

func validateRouting(resolved *Resolved) error {
	for name, connection := range resolved.Connections {
		if strings.TrimSpace(name) == "" || strings.Contains(name, "/") {
			return fmt.Errorf("invalid connection name %q", name)
		}
		connection.Provider = strings.ToLower(strings.TrimSpace(connection.Provider))
		connection.Class = strings.ToLower(strings.TrimSpace(connection.Class))
		if connection.Class == "" {
			connection.Class = ConnectionNative
		}
		if err := validateConnection(name, &connection); err != nil {
			return err
		}
		for model, metadata := range connection.Models {
			if strings.TrimSpace(model) == "" || metadata.ContextTokens < 0 || metadata.InputCostPerMillionMicrounits < 0 || metadata.OutputCostPerMillionMicrounits < 0 || metadata.CacheReadCostPerMillionMicrounits < 0 || metadata.CacheWriteCostPerMillionMicrounits < 0 {
				return fmt.Errorf("connection %q has invalid model metadata for %q", name, model)
			}
			if (metadata.InputCostPerMillionMicrounits > 0 || metadata.OutputCostPerMillionMicrounits > 0 || metadata.CacheReadCostPerMillionMicrounits > 0 || metadata.CacheWriteCostPerMillionMicrounits > 0) && strings.TrimSpace(metadata.Currency) == "" {
				return fmt.Errorf("connection %q model %q pricing requires a currency", name, model)
			}
			for _, capability := range metadata.Capabilities {
				if !supportedCapabilities[capability] {
					return fmt.Errorf("connection %q model %q has unsupported capability %q", name, model, capability)
				}
			}
		}
		resolved.Connections[name] = connection
	}
	if len(resolved.Pipelines) == 0 {
		if resolved.ActivePipeline != "" {
			return fmt.Errorf("pipeline %q is not configured", resolved.ActivePipeline)
		}
		return nil
	}
	if resolved.ActivePipeline == "" {
		if _, ok := resolved.Pipelines["default"]; !ok {
			return errors.New("pipeline must be selected when no default pipeline is configured")
		}
		resolved.ActivePipeline = "default"
	}
	if _, ok := resolved.Pipelines[resolved.ActivePipeline]; !ok {
		return fmt.Errorf("pipeline %q is not configured", resolved.ActivePipeline)
	}
	for name, pipeline := range resolved.Pipelines {
		if pipeline.Routing != "fixed" {
			return fmt.Errorf("pipeline %q uses unsupported routing mode %q", name, pipeline.Routing)
		}
		if pipeline.MaxRepairAttempts < 0 {
			return fmt.Errorf("pipeline %q has a negative repair-attempt limit", name)
		}
		for stage, route := range pipeline.Stages {
			if !supportedStages[stage] {
				return fmt.Errorf("pipeline %q has unsupported stage %q", name, stage)
			}
			if len(route.Candidates) == 0 {
				return fmt.Errorf("pipeline %q stage %q has no candidates", name, stage)
			}
			for _, capability := range route.RequiredCapabilities {
				if !supportedCapabilities[capability] {
					return fmt.Errorf("pipeline %q stage %q requires unsupported capability %q", name, stage, capability)
				}
			}
			if route.Budget.MaxInputTokens < 0 || route.Budget.MaxOutputTokens < 0 || route.Budget.MaxIterations < 0 || route.Budget.MaxCostMicrounits < 0 {
				return fmt.Errorf("pipeline %q stage %q has a negative budget", name, stage)
			}
			if route.Budget.Timeout != "" {
				value, err := time.ParseDuration(route.Budget.Timeout)
				if err != nil || value <= 0 {
					return fmt.Errorf("pipeline %q stage %q has invalid timeout %q", name, stage, route.Budget.Timeout)
				}
			}
			if route.Budget.MaxCostMicrounits > 0 && strings.TrimSpace(route.Budget.Currency) == "" {
				return fmt.Errorf("pipeline %q stage %q cost budget requires a currency", name, stage)
			}
			for _, candidate := range route.Candidates {
				connectionName, model, ok := strings.Cut(candidate, "/")
				if !ok || connectionName == "" || model == "" {
					return fmt.Errorf("pipeline %q stage %q has invalid candidate %q", name, stage, candidate)
				}
				connection, exists := resolved.Connections[connectionName]
				if !exists {
					return fmt.Errorf("pipeline %q stage %q references unknown connection %q", name, stage, connectionName)
				}
				if len(connection.Models) > 0 {
					if _, exists := connection.Models[model]; !exists {
						return fmt.Errorf("pipeline %q stage %q references unknown model %q on connection %q", name, stage, model, connectionName)
					}
				}
				if connection.Class == ConnectionSubscriptionBridge && stage != "planning" && stage != "review" {
					return fmt.Errorf("pipeline %q stage %q cannot use subscription bridge %q", name, stage, connectionName)
				}
			}
		}
		if _, ok := pipeline.Stages["planning"]; !ok {
			return fmt.Errorf("pipeline %q requires a planning stage", name)
		}
		if _, ok := pipeline.Stages["coding"]; !ok {
			return fmt.Errorf("pipeline %q requires a coding stage", name)
		}
	}
	return nil
}

// validateConnection normalizes one connection against its code-owned
// provider spec and rejects any credential kind the provider does not allow.
func validateConnection(name string, connection *Connection) error {
	switch connection.Class {
	case ConnectionNative, ConnectionSubscriptionBridge:
	default:
		return fmt.Errorf("connection %q uses unsupported class %q", name, connection.Class)
	}
	spec, ok := LookupProvider(connection.Provider)
	if !ok || spec.Class != connection.Class {
		if connection.Class == ConnectionSubscriptionBridge {
			return fmt.Errorf("connection %q uses unsupported subscription bridge %q", name, connection.Provider)
		}
		return fmt.Errorf("connection %q uses unsupported native provider %q", name, connection.Provider)
	}
	connection.Credential = CredentialKind(strings.ToLower(strings.TrimSpace(string(connection.Credential))))
	implied := CredentialForClass(connection.Class)
	if connection.Credential == "" {
		connection.Credential = implied
	}
	if connection.Credential != implied {
		return fmt.Errorf("connection %q credential %q does not match class %q", name, connection.Credential, connection.Class)
	}
	if !spec.Allows(connection.Credential) {
		return fmt.Errorf("connection %q: provider %q does not allow credential kind %q", name, spec.Name, connection.Credential)
	}
	if spec.Experimental && !connection.Experimental {
		return fmt.Errorf("connection %q uses experimental provider %q; set `experimental: true` to opt in", name, spec.Name)
	}
	switch connection.Class {
	case ConnectionNative:
		if connection.Command != "" {
			return fmt.Errorf("native connection %q cannot configure a bridge command", name)
		}
		if connection.BaseURL == "" {
			if spec.DefaultBaseURL == "" {
				return fmt.Errorf("connection %q (%s) requires base_url", name, spec.Name)
			}
			connection.BaseURL = spec.DefaultBaseURL
		}
		if connection.APIKeyEnv == "" && !spec.APIKeyOptional {
			connection.APIKeyEnv = spec.DefaultAPIKeyEnv
		}
		if connection.APIKeyEnv != "" && (!validEnvironmentName(connection.APIKeyEnv) || credentialEnvironmentConflicts(connection.APIKeyEnv)) {
			return fmt.Errorf("connection %q has invalid API key environment %q", name, connection.APIKeyEnv)
		}
	case ConnectionSubscriptionBridge:
		if connection.BaseURL != "" || connection.APIKeyEnv != "" {
			return fmt.Errorf("subscription bridge %q must not configure a base URL or API key environment", name)
		}
		if connection.Command == "" {
			connection.Command = spec.DefaultCommand
		}
	}
	return nil
}

// DefaultPaths returns platform-standard config and state locations.
func DefaultPaths(
	goos string,
	homeDir string,
	lookupEnv func(string) (string, bool),
) (Paths, error) {
	if homeDir == "" {
		return Paths{}, errors.New("home directory is empty")
	}
	if lookupEnv == nil {
		lookupEnv = func(string) (string, bool) { return "", false }
	}

	switch goos {
	case "darwin":
		root := platformJoin(goos, homeDir, "Library", "Application Support", "erka")
		return Paths{
			ConfigPath: platformJoin(goos, root, "config.yaml"),
			StateDir:   platformJoin(goos, root, "state"),
		}, nil
	case "windows":
		configRoot := platformJoin(goos, homeDir, "AppData", "Roaming")
		if value, ok := nonEmptyEnv(lookupEnv, "APPDATA"); ok {
			configRoot = value
		}
		stateRoot := platformJoin(goos, homeDir, "AppData", "Local")
		if value, ok := nonEmptyEnv(lookupEnv, "LOCALAPPDATA"); ok {
			stateRoot = value
		}
		return Paths{
			ConfigPath: platformJoin(goos, configRoot, "erka", "config.yaml"),
			StateDir:   platformJoin(goos, stateRoot, "erka"),
		}, nil
	default:
		configRoot := platformJoin(goos, homeDir, ".config")
		if value, ok := nonEmptyEnv(lookupEnv, "XDG_CONFIG_HOME"); ok && isPlatformAbs(goos, value) {
			configRoot = value
		}
		stateRoot := platformJoin(goos, homeDir, ".local", "state")
		if value, ok := nonEmptyEnv(lookupEnv, "XDG_STATE_HOME"); ok && isPlatformAbs(goos, value) {
			stateRoot = value
		}
		return Paths{
			ConfigPath: platformJoin(goos, configRoot, "erka", "config.yaml"),
			StateDir:   platformJoin(goos, stateRoot, "erka"),
		}, nil
	}
}

func withDefaults(options Options) Options {
	if options.GOOS == "" {
		options.GOOS = runtime.GOOS
	}
	if options.HomeDir == "" {
		options.HomeDir, _ = os.UserHomeDir()
	}
	if options.LookupEnv == nil {
		options.LookupEnv = os.LookupEnv
	}
	if options.ReadFile == nil {
		options.ReadFile = os.ReadFile
	}
	return options
}

func parse(data []byte) (fileConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var result fileConfig
	if err := decoder.Decode(&result); err != nil {
		return fileConfig{}, err
	}
	if result.Version != CurrentVersion {
		return fileConfig{}, fmt.Errorf(
			"unsupported config version %d; want %d",
			result.Version,
			CurrentVersion,
		)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fileConfig{}, errors.New("multiple YAML documents are not allowed")
		}
		return fileConfig{}, err
	}

	return result, nil
}

func nonEmptyEnv(lookupEnv func(string) (string, bool), key string) (string, bool) {
	value, ok := lookupEnv(key)
	return value, ok && value != ""
}

func applyStringEnv(lookup func(string) (string, bool), key string, target *string, source *Source) {
	if value, ok := nonEmptyEnv(lookup, key); ok {
		*target, *source = value, SourceEnvironment
	}
}

func applyIntEnv(lookup func(string) (string, bool), key string, target *int) error {
	value, ok := nonEmptyEnv(lookup, key)
	if !ok {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("parse %s: %w", key, err)
	}
	*target = parsed
	return nil
}

func applyInt64Env(lookup func(string) (string, bool), key string, target *int64) error {
	value, ok := nonEmptyEnv(lookup, key)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("parse %s: %w", key, err)
	}
	*target = parsed
	return nil
}

func validEnvironmentName(value string) bool {
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' || (index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return value != ""
}

func credentialEnvironmentConflicts(value string) bool {
	switch strings.ToUpper(value) {
	case "COMSPEC", "HOME", "LANG", "LC_ALL", "PATH", "PATHEXT", "SYSTEMROOT", "TEMP", "TMP", "TMPDIR", "USERPROFILE", "WINDIR":
		return true
	default:
		return false
	}
}

func platformJoin(goos string, elements ...string) string {
	if goos != "windows" {
		return path.Join(elements...)
	}

	normalized := make([]string, 0, len(elements))
	for _, element := range elements {
		normalized = append(normalized, strings.ReplaceAll(element, `\`, "/"))
	}
	return strings.ReplaceAll(path.Join(normalized...), "/", `\`)
}

func isPlatformAbs(goos string, value string) bool {
	if goos != "windows" {
		return path.IsAbs(value)
	}
	value = strings.ReplaceAll(value, "/", `\`)
	return strings.HasPrefix(value, `\\`) ||
		(len(value) >= 3 && value[1] == ':' && value[2] == '\\')
}
