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
	"path/filepath"
	"runtime"
	"strings"

	"go.yaml.in/yaml/v3"
)

const CurrentVersion = 1

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
}

// Options supplies command flags and injectable operating-system dependencies.
// Empty dependency fields use the running process and operating system.
type Options struct {
	ConfigPath string
	StateDir   string

	GOOS      string
	HomeDir   string
	LookupEnv func(string) (string, bool)
	ReadFile  func(string) ([]byte, error)
}

type fileConfig struct {
	Version  int    `yaml:"version"`
	StateDir string `yaml:"state_dir,omitempty"`
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
	case errors.Is(readErr, fs.ErrNotExist) && !explicitConfig:
		// A default config file is optional.
	case readErr != nil:
		return Resolved{}, fmt.Errorf("read config %q: %w", resolved.ConfigPath, readErr)
	}

	if value, ok := nonEmptyEnv(options.LookupEnv, "ERKA_STATE_DIR"); ok {
		resolved.StateDir = value
		resolved.StateDirSource = SourceEnvironment
	}
	if options.StateDir != "" {
		resolved.StateDir = options.StateDir
		resolved.StateDirSource = SourceFlag
	}

	if !isPlatformAbs(options.GOOS, resolved.StateDir) {
		return Resolved{}, fmt.Errorf("state directory must be absolute: %q", resolved.StateDir)
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

func platformJoin(goos string, elements ...string) string {
	if goos != "windows" {
		return filepath.Join(elements...)
	}

	normalized := make([]string, 0, len(elements))
	for _, element := range elements {
		normalized = append(normalized, strings.ReplaceAll(element, `\`, "/"))
	}
	return strings.ReplaceAll(path.Join(normalized...), "/", `\`)
}

func isPlatformAbs(goos string, value string) bool {
	if goos != "windows" {
		return filepath.IsAbs(value)
	}
	value = strings.ReplaceAll(value, "/", `\`)
	return strings.HasPrefix(value, `\\`) ||
		(len(value) >= 3 && value[1] == ':' && value[2] == '\\')
}
