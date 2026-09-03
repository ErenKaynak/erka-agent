package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if got.Provider != DefaultProvider || got.Model != DefaultModel || got.BaseURL != DefaultDeepSeekURL || got.APIKeyEnv != DefaultAPIKeyEnv || got.MaxIterations != DefaultMaxIterations || got.MaxOutputTokens != DefaultMaxOutputToken || got.StageTimeout != DefaultStageTimeout {
		t.Fatalf("Load() provider/runtime defaults = %#v", got)
	}
}

func TestLoadProviderAndRuntimePrecedence(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	content := `version: 1
provider:
  type: deepseek
  model: config-model
  base_url: https://config.example
  api_key_env: CONFIG_KEY
runtime:
  max_iterations: 4
  max_output_tokens: 1000
  stage_timeout: 1m
permissions:
  allow_full_execution: false
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(context.Background(), Options{
		ConfigPath: configPath, GOOS: "linux", HomeDir: "/home/erka",
		Model: "flag-model", MaxIterations: 8,
		LookupEnv: lookup(map[string]string{
			"ERKA_MODEL": "env-model", "ERKA_DEEPSEEK_BASE_URL": "https://env.example",
			"ERKA_API_KEY_ENV": "ENV_KEY", "ERKA_MAX_OUTPUT_TOKENS": "2000", "ERKA_STAGE_TIMEOUT": "2m",
			"ERKA_ALLOW_FULL_EXECUTION": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "flag-model" || got.ModelSource != SourceFlag || got.BaseURL != "https://env.example" || got.BaseURLSource != SourceEnvironment || got.APIKeyEnv != "ENV_KEY" || got.MaxIterations != 8 || got.MaxOutputTokens != 2000 || got.StageTimeout != 2*time.Minute || !got.AllowFullExecution {
		t.Fatalf("resolved = %#v", got)
	}
}

func TestLoadPipelineConnectionsAndBudgets(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	content := `version: 1
pipeline: default
connections:
  planner:
    provider: openai
    models:
      gpt-test:
        capabilities: [tools, structured_output]
        context_tokens: 128000
        input_cost_per_million_microunits: 1000000
        output_cost_per_million_microunits: 2000000
        currency: USD
  coder:
    provider: deepseek
    base_url: https://deepseek.example
    api_key_env: CODER_KEY
pipelines:
  default:
    routing: fixed
    max_repair_attempts: 2
    stages:
      planning:
        candidates: [planner/gpt-test]
        required_capabilities: [structured_output]
        budget:
          max_input_tokens: 20000
          max_output_tokens: 4000
          max_iterations: 3
          timeout: 5m
          max_cost_microunits: 500000
          currency: USD
      coding:
        candidates: [coder/deepseek-test]
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(context.Background(), Options{ConfigPath: configPath, GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got.ActivePipeline != "default" || len(got.Connections) != 2 || len(got.Pipelines) != 1 {
		t.Fatalf("resolved routing = %#v", got)
	}
	if got.Connections["planner"].BaseURL != DefaultOpenAIURL || got.Connections["planner"].APIKeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("planner defaults = %#v", got.Connections["planner"])
	}
	budget := got.Pipelines["default"].Stages["planning"].Budget
	if budget.MaxInputTokens != 20000 || budget.MaxOutputTokens != 4000 || budget.MaxIterations != 3 || budget.Timeout != "5m" || budget.MaxCostMicrounits != 500000 || budget.Currency != "USD" {
		t.Fatalf("planning budget = %#v", budget)
	}
}

func TestLoadPipelineSelectionPrecedence(t *testing.T) {
	t.Parallel()
	content := []byte(`version: 1
connections:
  native:
    provider: deepseek
pipelines:
  default:
    routing: fixed
    stages:
      planning: {candidates: [native/planner]}
      coding: {candidates: [native/coder]}
  alternate:
    routing: fixed
    stages:
      planning: {candidates: [native/planner]}
      coding: {candidates: [native/coder]}
`)
	got, err := Load(context.Background(), Options{
		Pipeline: "alternate", GOOS: "linux", HomeDir: "/home/erka",
		LookupEnv: lookup(map[string]string{"ERKA_PIPELINE": "default"}),
		ReadFile:  func(string) ([]byte, error) { return content, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ActivePipeline != "alternate" {
		t.Fatalf("active pipeline = %q", got.ActivePipeline)
	}
}

func TestLoadDefaultsLegacyOpenAIProvider(t *testing.T) {
	t.Parallel()
	got, err := Load(context.Background(), Options{Provider: "openai", GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil), ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist }})
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != DefaultOpenAIURL || got.APIKeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("OpenAI defaults = %#v", got)
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
		{name: "unsupported provider", content: "version: 1\nprovider:\n  type: other\n", want: "unsupported native provider"},
		{name: "invalid timeout", content: "version: 1\nruntime:\n  stage_timeout: tomorrow\n", want: "runtime.stage_timeout"},
		{name: "negative limit", content: "version: 1\nruntime:\n  max_iterations: -1\n", want: "runtime limits must be positive"},
		{name: "invalid key environment", content: "version: 1\nprovider:\n  api_key_env: BAD-NAME\n", want: "invalid API key environment"},
		{name: "leaky key environment", content: "version: 1\nprovider:\n  api_key_env: PATH\n", want: "general subprocess variable"},
		{name: "missing pipeline connection", content: "version: 1\npipelines:\n  default:\n    routing: fixed\n    stages:\n      planning: {candidates: [missing/model]}\n      coding: {candidates: [missing/model]}\n", want: "unknown connection"},
		{name: "unsupported routing", content: "version: 1\nconnections:\n  native: {provider: deepseek}\npipelines:\n  default:\n    routing: llm\n    stages:\n      planning: {candidates: [native/model]}\n      coding: {candidates: [native/model]}\n", want: "unsupported routing mode"},
		{name: "invalid stage budget", content: "version: 1\nconnections:\n  native: {provider: deepseek}\npipelines:\n  default:\n    routing: fixed\n    stages:\n      planning: {candidates: [native/model], budget: {timeout: never}}\n      coding: {candidates: [native/model]}\n", want: "invalid timeout"},
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
