package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/auth"
)

func TestNativeProviderBoundaryRequiresAPIKeyCredentialKind(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"codex", "opencode", "unknown"} {
		if _, err := openNativeProvider(provider, "https://example.test", "model", "key"); err == nil {
			t.Fatalf("openNativeProvider(%q) succeeded", provider)
		}
		if _, _, err := resolveNativeKey(context.Background(), auth.NewManager(emptyKeyring{}), provider, "https://example.test", "SOME_KEY"); err == nil || !strings.Contains(err.Error(), "provider") {
			t.Fatalf("resolveNativeKey(%q) error = %v", provider, err)
		}
	}
}

func TestOptionalKeyProviderSkipsCredentialLookup(t *testing.T) {
	t.Parallel()
	key, _, err := resolveNativeKey(context.Background(), auth.NewManager(emptyKeyring{}), "openai_compatible", "http://localhost:11434/v1", "")
	if err != nil || key != "" {
		t.Fatalf("resolveNativeKey() = %q, %v", key, err)
	}
	if _, _, err := resolveNativeKey(context.Background(), auth.NewManager(emptyKeyring{}), "openai_compatible", "https://openrouter.ai/api/v1", "ERKA_TEST_UNSET_OPENROUTER_KEY"); !errors.Is(err, auth.ErrCredentialNotFound) {
		t.Fatalf("configured key must be required, got %v", err)
	}
	provider, err := openNativeProvider("openai_compatible", "http://localhost:11434/v1", "qwen3-coder", "")
	if err != nil || provider.Name() != "openai_compatible" {
		t.Fatalf("openNativeProvider() = %v, %v", provider, err)
	}
}
