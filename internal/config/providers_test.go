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
