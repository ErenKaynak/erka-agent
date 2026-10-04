package cli

import (
	"context"
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
