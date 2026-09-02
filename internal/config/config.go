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
)

const CurrentVersion = 1

const (
	DefaultProvider       = "deepseek"
	DefaultModel          = "deepseek-v4-flash"
	DefaultDeepSeekURL    = "https://api.deepseek.com"
	DefaultAPIKeyEnv      = "ERKA_DEEPSEEK_API_KEY"
	DefaultMaxIterations  = 24
	DefaultMaxOutputToken = int64(8192)
	DefaultStageTimeout   = 30 * time.Minute
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
	Version            int
	ConfigPath         string
	ConfigPathSource   Source
	StateDir           string
	StateDirSource     Source
	Provider           string
	ProviderSource     Source
	Model              string
	ModelSource        Source
	BaseURL            string
	BaseURLSource      Source
	APIKeyEnv          string
	MaxIterations      int
	MaxOutputTokens    int64
	StageTimeout       time.Duration
	AllowFullExecution bool
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
	Version  int    `yaml:"version"`
	StateDir string `yaml:"state_dir,omitempty"`
	Provider struct {
		Type      string `yaml:"type,omitempty"`
		Model     string `yaml:"model,omitempty"`
		BaseURL   string `yaml:"base_url,omitempty"`
		APIKeyEnv string `yaml:"api_key_env,omitempty"`
	} `yaml:"provider,omitempty"`
	Runtime struct {
		MaxIterations   int    `yaml:"max_iterations,omitempty"`
		MaxOutputTokens int64  `yaml:"max_output_tokens,omitempty"`
		StageTimeout    string `yaml:"stage_timeout,omitempty"`
	} `yaml:"runtime,omitempty"`
	Permissions struct {
		AllowFullExecution bool `yaml:"allow_full_execution,omitempty"`
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
			resolved.AllowFullExecution = loaded.Permissions.AllowFullExecution
		}
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
	if value, ok := nonEmptyEnv(options.LookupEnv, "ERKA_API_KEY_ENV"); ok {
		resolved.APIKeyEnv = value
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
	if resolved.Provider != "deepseek" {
		return Resolved{}, fmt.Errorf("unsupported native provider %q", resolved.Provider)
	}
	if strings.TrimSpace(resolved.Model) == "" || strings.TrimSpace(resolved.APIKeyEnv) == "" {
		return Resolved{}, errors.New("provider model and API key environment name must not be empty")
	}
	if !validEnvironmentName(resolved.APIKeyEnv) {
		return Resolved{}, fmt.Errorf("invalid API key environment name %q", resolved.APIKeyEnv)
	}
	if credentialEnvironmentConflicts(resolved.APIKeyEnv) {
		return Resolved{}, fmt.Errorf("API key environment %q is also a general subprocess variable", resolved.APIKeyEnv)
	}
	if resolved.MaxIterations <= 0 || resolved.MaxOutputTokens <= 0 || resolved.StageTimeout <= 0 {
		return Resolved{}, errors.New("runtime limits must be positive")
	}
	if err := ctx.Err(); err != nil {
		return Resolved{}, err
	}

	return resolved, nil
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
