package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderSpecsAllowOnlyTheirCredentialKinds(t *testing.T) {
	t.Parallel()
	kinds := []CredentialKind{CredentialAPIKey, CredentialVendorCLIBridge}
	for name, spec := range providerSpecs {
		if spec.Name != name {
			t.Fatalf("spec %q has name %q", name, spec.Name)
		}
		if len(spec.Credentials) == 0 {
			t.Fatalf("provider %q allows no credential kinds", name)
		}
		for _, kind := range kinds {
			want := kind == CredentialForClass(spec.Class)
			if got := spec.Allows(kind); got != want {
				t.Errorf("%s.Allows(%s) = %v, want %v", name, kind, got, want)
			}
			_, err := RequireCredential(name, kind)
			if (err == nil) != want {
				t.Errorf("RequireCredential(%s, %s) error = %v", name, kind, err)
			}
		}
	}
	if _, err := RequireCredential("nonexistent", CredentialAPIKey); err == nil {
		t.Fatal("unknown provider was allowed")
	}
	if _, err := RequireCredential("codex", CredentialAPIKey); err == nil || !strings.Contains(err.Error(), "does not allow") {
		t.Fatalf("codex api_key error = %v", err)
	}
}

func TestProviderNamesByClass(t *testing.T) {
	t.Parallel()
	native := ProviderNames(ConnectionNative)
	bridges := ProviderNames(ConnectionSubscriptionBridge)
	if !containsString(native, "deepseek") || !containsString(native, "openai") || containsString(native, "codex") {
		t.Fatalf("native providers = %v", native)
	}
	if !containsString(bridges, "codex") || !containsString(bridges, "opencode") || containsString(bridges, "openai") {
		t.Fatalf("bridge providers = %v", bridges)
	}
}

func TestConnectionCredentialKindIsDerivedAndEnforced(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		yaml    string
		wantErr string
		check   func(*testing.T, Resolved)
	}{
		{
			name: "derived from class",
			yaml: `
  coder:
    provider: deepseek
  chatgpt:
    provider: codex
    class: subscription_bridge`,
			check: func(t *testing.T, got Resolved) {
				if got.Connections["coder"].Credential != CredentialAPIKey || got.Connections["chatgpt"].Credential != CredentialVendorCLIBridge {
					t.Fatalf("credentials = %#v", got.Connections)
				}
			},
		},
		{
			name: "explicit matching kind",
			yaml: `
  coder:
    provider: deepseek
    credential: API_KEY`,
			check: func(t *testing.T, got Resolved) {
				if got.Connections["coder"].Credential != CredentialAPIKey {
					t.Fatalf("credential = %q", got.Connections["coder"].Credential)
				}
			},
		},
		{
			name: "api key provider cannot use a bridge credential",
			yaml: `
  coder:
    provider: deepseek
    credential: vendor_cli_bridge`,
			wantErr: "does not match class",
		},
		{
			name: "bridge cannot use an api key credential",
			yaml: `
  chatgpt:
    provider: codex
    class: subscription_bridge
    credential: api_key`,
			wantErr: "does not match class",
		},
		{
			name: "unknown credential kind",
			yaml: `
  coder:
    provider: deepseek
    credential: oauth_token`,
			wantErr: "does not match class",
		},
		{
			name: "bridge provider declared native",
			yaml: `
  chatgpt:
    provider: codex`,
			wantErr: "unsupported native provider",
		},
		{
			name: "native provider declared as bridge",
			yaml: `
  coder:
    provider: openai
    class: subscription_bridge`,
			wantErr: "unsupported subscription bridge",
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := loadConnections(t, testCase.yaml)
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("Load() error = %v, want %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			testCase.check(t, got)
		})
	}
}

func loadConnections(t *testing.T, connections string) (Resolved, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nconnections:"+connections+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(context.Background(), Options{ConfigPath: path, GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil)})
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestOpenAICompatibleConnections(t *testing.T) {
	t.Parallel()
	got, err := loadConnections(t, `
  local:
    provider: openai_compatible
    base_url: http://localhost:11434/v1
  router:
    provider: openai_compatible
    base_url: https://openrouter.ai/api/v1
    api_key_env: OPENROUTER_API_KEY`)
	if err != nil {
		t.Fatal(err)
	}
	if local := got.Connections["local"]; local.APIKeyEnv != "" || local.Credential != CredentialAPIKey {
		t.Fatalf("local = %#v", local)
	}
	if router := got.Connections["router"]; router.APIKeyEnv != "OPENROUTER_API_KEY" {
		t.Fatalf("router = %#v", router)
	}
	if _, err := loadConnections(t, `
  broken:
    provider: openai_compatible`); err == nil || !strings.Contains(err.Error(), "requires base_url") {
		t.Fatalf("missing base_url error = %v", err)
	}
}

func TestTopLevelOpenAICompatibleProvider(t *testing.T) {
	t.Parallel()
	options := Options{GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil), Provider: "openai_compatible", Model: "qwen3-coder", BaseURL: "http://localhost:11434/v1"}
	got, err := Load(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKeyEnv != "" || got.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("resolved = %#v", got)
	}
	options.BaseURL = ""
	if _, err := Load(context.Background(), options); err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("missing base URL error = %v", err)
	}
	options.BaseURL, options.Model = "http://localhost:11434/v1", ""
	if _, err := Load(context.Background(), options); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("missing model error = %v", err)
	}
}

func TestAnthropicConnectionDefaultsAndCachePricing(t *testing.T) {
	t.Parallel()
	got, err := loadConnections(t, `
  claude:
    provider: anthropic
    models:
      claude-test:
        input_cost_per_million_microunits: 3000000
        output_cost_per_million_microunits: 15000000
        cache_read_cost_per_million_microunits: 300000
        cache_write_cost_per_million_microunits: 3750000
        currency: USD`)
	if err != nil {
		t.Fatal(err)
	}
	connection := got.Connections["claude"]
	if connection.BaseURL != DefaultAnthropicURL || connection.APIKeyEnv != "ANTHROPIC_API_KEY" || connection.Credential != CredentialAPIKey {
		t.Fatalf("connection = %#v", connection)
	}
	if connection.Models["claude-test"].CacheReadCostPerMillionMicrounits != 300000 {
		t.Fatalf("model = %#v", connection.Models["claude-test"])
	}
	for _, broken := range []string{
		"\n  claude:\n    provider: anthropic\n    models:\n      m:\n        cache_read_cost_per_million_microunits: 1",
		"\n  claude:\n    provider: anthropic\n    models:\n      m:\n        cache_write_cost_per_million_microunits: -1\n        currency: USD",
	} {
		if _, err := loadConnections(t, broken); err == nil {
			t.Fatalf("accepted invalid cache pricing: %s", broken)
		}
	}
}

func TestClaudeCodeBridgeIsExperimentalAndReadOnly(t *testing.T) {
	t.Parallel()
	if _, err := loadConnections(t, `
  claude:
    provider: claude_code
    class: subscription_bridge`); err == nil || !strings.Contains(err.Error(), "experimental: true") {
		t.Fatalf("missing opt-in error = %v", err)
	}
	got, err := loadConnections(t, `
  claude:
    provider: claude_code
    class: subscription_bridge
    experimental: true`)
	if err != nil {
		t.Fatal(err)
	}
	if connection := got.Connections["claude"]; connection.Command != "claude" || connection.Credential != CredentialVendorCLIBridge {
		t.Fatalf("connection = %#v", connection)
	}
	if _, err := loadConnections(t, `
  claude:
    provider: claude_code
    class: subscription_bridge
    experimental: true
    api_key_env: ANTHROPIC_API_KEY`); err == nil {
		t.Fatal("a vendor CLI bridge accepted an API key environment variable")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
pipeline: default
connections:
  claude:
    provider: claude_code
    class: subscription_bridge
    experimental: true
  coder:
    provider: deepseek
pipelines:
  default:
    routing: fixed
    stages:
      planning:
        candidates: [claude/default]
      coding:
        candidates: [claude/default]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), Options{ConfigPath: path, GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil)}); err == nil || !strings.Contains(err.Error(), "cannot use subscription bridge") {
		t.Fatalf("coding-stage bridge error = %v", err)
	}
}

func TestPermissionRulesLoadAndValidate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(rules string) (Resolved, error) {
		if err := os.WriteFile(path, []byte("version: 1\npermissions:\n  rules:\n"+rules), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(context.Background(), Options{ConfigPath: path, GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil)})
	}
	got, err := write("    allow: [\"run_command(go test *)\"]\n    ask: [apply_patch]\n    deny: [\"run_command(rm -rf *)\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PermissionRules.Allow) != 1 || got.PermissionRules.Ask[0] != "apply_patch" || got.PermissionRules.Deny[0] != "run_command(rm -rf *)" {
		t.Fatalf("rules = %#v", got.PermissionRules)
	}
	if _, err := write("    deny: [\"run_command(\"]\n"); err == nil || !strings.Contains(err.Error(), "permissions.rules.deny") {
		t.Fatalf("invalid rule error = %v", err)
	}
}

func TestHooksLoadAndValidate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\nhooks:\n  post_edit:\n    - match: \"*.go\"\n      command: [gofmt, -w, \"{path}\"]\n      timeout: 45s\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(context.Background(), Options{ConfigPath: path, GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hooks.PostEdit) != 1 || got.Hooks.PostEdit[0].Command[2] != "{path}" || got.Hooks.PostEdit[0].Timeout.Seconds() != 45 {
		t.Fatalf("hooks = %#v", got.Hooks)
	}
	if err := os.WriteFile(path, []byte("version: 1\nhooks:\n  post_edit:\n    - match: \"*.go\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), Options{ConfigPath: path, GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil)}); err == nil {
		t.Fatal("hook without a command was accepted")
	}
}

func TestRuntimeContextBudgetTokens(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nruntime:\n  context_budget_tokens: 120000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(context.Background(), Options{ConfigPath: path, GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil)})
	if err != nil || got.ContextBudgetTokens != 120000 {
		t.Fatalf("resolved = %d, %v", got.ContextBudgetTokens, err)
	}
	if err := os.WriteFile(path, []byte("version: 1\nruntime:\n  context_budget_tokens: -1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), Options{ConfigPath: path, GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil)}); err == nil {
		t.Fatal("negative budget accepted")
	}
}
