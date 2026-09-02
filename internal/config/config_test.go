package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		goos        string
		home        string
		environment map[string]string
		want        Paths
	}{
		{
			name: "macOS",
			goos: "darwin",
			home: "/Users/erka",
			want: Paths{
				ConfigPath: "/Users/erka/Library/Application Support/erka/config.yaml",
				StateDir:   "/Users/erka/Library/Application Support/erka/state",
			},
		},
		{
			name: "Linux defaults",
			goos: "linux",
			home: "/home/erka",
			want: Paths{
				ConfigPath: "/home/erka/.config/erka/config.yaml",
				StateDir:   "/home/erka/.local/state/erka",
			},
		},
		{
			name: "Linux XDG",
			goos: "linux",
			home: "/home/erka",
			environment: map[string]string{
				"XDG_CONFIG_HOME": "/config",
				"XDG_STATE_HOME":  "/state",
			},
			want: Paths{
				ConfigPath: "/config/erka/config.yaml",
				StateDir:   "/state/erka",
			},
		},
		{
			name: "Windows",
			goos: "windows",
			home: `C:\Users\erka`,
			environment: map[string]string{
				"APPDATA":      `D:\Roaming`,
				"LOCALAPPDATA": `D:\Local`,
			},
			want: Paths{
				ConfigPath: `D:\Roaming\erka\config.yaml`,
				StateDir:   `D:\Local\erka`,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := DefaultPaths(test.goos, test.home, lookup(test.environment))
			if err != nil {
				t.Fatalf("DefaultPaths() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("DefaultPaths() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestLoadPrecedence(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(
		configPath,
		[]byte("version: 1\nstate_dir: /from-config\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	got, err := Load(context.Background(), Options{
		ConfigPath: configPath,
		StateDir:   "/from-flag",
		GOOS:       "linux",
		HomeDir:    "/home/erka",
		LookupEnv: lookup(map[string]string{
			"ERKA_CONFIG_PATH": "/ignored/config.yaml",
			"ERKA_STATE_DIR":   "/from-environment",
		}),
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.ConfigPath != configPath || got.ConfigPathSource != SourceFlag {
		t.Fatalf("Load() config = %q (%s)", got.ConfigPath, got.ConfigPathSource)
	}
	if got.StateDir != "/from-flag" || got.StateDirSource != SourceFlag {
		t.Fatalf("Load() state = %q (%s)", got.StateDir, got.StateDirSource)
	}
}

func TestLoadUsesConfigAndEnvironment(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(
		configPath,
		[]byte("version: 1\nstate_dir: /from-config\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	got, err := Load(context.Background(), Options{
		GOOS:    "linux",
		HomeDir: "/home/erka",
		LookupEnv: lookup(map[string]string{
			"ERKA_CONFIG_PATH": configPath,
			"ERKA_STATE_DIR":   "/from-environment",
		}),
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.ConfigPathSource != SourceEnvironment {
		t.Fatalf("Load() config source = %s", got.ConfigPathSource)
	}
	if got.StateDir != "/from-environment" || got.StateDirSource != SourceEnvironment {
		t.Fatalf("Load() state = %q (%s)", got.StateDir, got.StateDirSource)
	}
}

func TestLoadAllowsMissingDefaultConfig(t *testing.T) {
	t.Parallel()

	got, err := Load(context.Background(), Options{
		GOOS:      "linux",
		HomeDir:   "/home/erka",
		LookupEnv: lookup(nil),
		ReadFile: func(string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.StateDir != "/home/erka/.local/state/erka" {
		t.Fatalf("Load() state = %q", got.StateDir)
	}
}

func TestLoadRejectsMissingExplicitConfig(t *testing.T) {
	t.Parallel()

	_, err := Load(context.Background(), Options{
		ConfigPath: "/missing/config.yaml",
		GOOS:       "linux",
		HomeDir:    "/home/erka",
		LookupEnv:  lookup(nil),
		ReadFile: func(string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
	})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() error = %v, want not-exist error", err)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "malformed", content: "version: [", want: "did not find expected node content"},
		{name: "unknown field", content: "version: 1\nsecret: value\n", want: "field secret not found"},
		{name: "unsupported version", content: "version: 2\n", want: "unsupported config version"},
		{name: "multiple documents", content: "version: 1\n---\nversion: 1\n", want: "multiple YAML documents"},
		{name: "relative state", content: "version: 1\nstate_dir: relative\n", want: "state directory must be absolute"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			configPath := filepath.Join(directory, "config.yaml")
			if err := os.WriteFile(configPath, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := Load(context.Background(), Options{
				ConfigPath: configPath,
				GOOS:       "linux",
				HomeDir:    "/home/erka",
				LookupEnv:  lookup(nil),
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestLoadHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Load(ctx, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Load() error = %v, want context.Canceled", err)
	}
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
