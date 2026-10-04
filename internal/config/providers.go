package config

import (
	"fmt"
	"sort"
	"strings"
)

// CredentialKind names how erka obtains access to a provider. The kinds a
// provider accepts are fixed in code; configuration can only select among them.
type CredentialKind string

const (
	// CredentialAPIKey is a provider API key that erka resolves from an
	// environment variable or the OS keyring and sends to a native API.
	CredentialAPIKey CredentialKind = "api_key"
	// CredentialVendorCLIBridge delegates authentication to the vendor's own
	// official CLI. erka never reads, copies, or forwards those credentials.
	CredentialVendorCLIBridge CredentialKind = "vendor_cli_bridge"
)

// ProviderSpec is the code-owned contract for one provider name.
type ProviderSpec struct {
	Name string
	// Class is ConnectionNative or ConnectionSubscriptionBridge.
	Class string
	// Credentials lists every credential kind this provider may use.
	Credentials []CredentialKind
	// DefaultBaseURL is used when a native connection omits base_url. Empty
	// means the base URL must be configured explicitly.
	DefaultBaseURL string
	// DefaultAPIKeyEnv is used when a native connection omits api_key_env.
	DefaultAPIKeyEnv string
	// APIKeyOptional allows a native connection without any API key, for
	// local servers that do not authenticate.
	APIKeyOptional bool
	// RequiresModel means the provider has no default model ID.
	RequiresModel bool
	// DefaultCommand is the official CLI executable for a bridge.
	DefaultCommand string
	// Experimental providers must be enabled with `experimental: true`.
	Experimental bool
}

// Allows reports whether the provider accepts a credential kind.
func (spec ProviderSpec) Allows(kind CredentialKind) bool {
	for _, allowed := range spec.Credentials {
		if allowed == kind {
			return true
		}
	}
	return false
}

var providerSpecs = map[string]ProviderSpec{
	"deepseek": {
		Name: "deepseek", Class: ConnectionNative, Credentials: []CredentialKind{CredentialAPIKey},
		DefaultBaseURL: DefaultDeepSeekURL, DefaultAPIKeyEnv: DefaultAPIKeyEnv,
	},
	"openai": {
		Name: "openai", Class: ConnectionNative, Credentials: []CredentialKind{CredentialAPIKey},
		DefaultBaseURL: DefaultOpenAIURL, DefaultAPIKeyEnv: "OPENAI_API_KEY", RequiresModel: true,
	},
	"anthropic": {
		Name: "anthropic", Class: ConnectionNative, Credentials: []CredentialKind{CredentialAPIKey},
		DefaultBaseURL: DefaultAnthropicURL, DefaultAPIKeyEnv: "ANTHROPIC_API_KEY", RequiresModel: true,
	},
	"openai_compatible": {
		Name: "openai_compatible", Class: ConnectionNative, Credentials: []CredentialKind{CredentialAPIKey},
		APIKeyOptional: true, RequiresModel: true,
	},
	"codex": {
		Name: "codex", Class: ConnectionSubscriptionBridge, Credentials: []CredentialKind{CredentialVendorCLIBridge},
		DefaultCommand: "codex",
	},
	"opencode": {
		Name: "opencode", Class: ConnectionSubscriptionBridge, Credentials: []CredentialKind{CredentialVendorCLIBridge},
		DefaultCommand: "opencode",
	},
}

// LookupProvider returns the code-owned specification for a provider name.
func LookupProvider(name string) (ProviderSpec, bool) {
	spec, ok := providerSpecs[strings.ToLower(strings.TrimSpace(name))]
	return spec, ok
}

// ProviderNames lists providers of one connection class in sorted order.
func ProviderNames(class string) []string {
	names := make([]string, 0, len(providerSpecs))
	for name, spec := range providerSpecs {
		if spec.Class == class {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// CredentialForClass is the credential kind implied by a connection class
// when a connection does not name one explicitly.
func CredentialForClass(class string) CredentialKind {
	if class == ConnectionSubscriptionBridge {
		return CredentialVendorCLIBridge
	}
	return CredentialAPIKey
}

// RequireCredential enforces the provider allowlist at runtime boundaries.
func RequireCredential(provider string, kind CredentialKind) (ProviderSpec, error) {
	spec, ok := LookupProvider(provider)
	if !ok {
		return ProviderSpec{}, fmt.Errorf("unsupported provider %q", provider)
	}
	if !spec.Allows(kind) {
		return ProviderSpec{}, fmt.Errorf("provider %q does not allow credential kind %q", provider, kind)
	}
	return spec, nil
}
