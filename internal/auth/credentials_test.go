package auth

import (
	"context"
	"errors"
	"os"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

type fakeRing struct {
	value                     string
	getErr, setErr, deleteErr error
}

func (ring *fakeRing) Get(_, _ string) (string, error) { return ring.value, ring.getErr }
func (ring *fakeRing) Set(_, _, value string) error {
	if ring.setErr == nil {
		ring.value = value
		ring.getErr = nil
	}
	return ring.setErr
}
func (ring *fakeRing) Delete(_, _ string) error { ring.value = ""; return ring.deleteErr }

func TestCredentialResolutionPrefersEnvironmentThenKeyring(t *testing.T) {
	ring := &fakeRing{value: "keyring-secret"}
	manager := NewManager(ring)
	t.Setenv("ERKA_TEST_KEY", "environment-secret")
	value, source, err := manager.Resolve(context.Background(), "DeepSeek", "https://API.DeepSeek.com/", "ERKA_TEST_KEY")
	if err != nil || value != "environment-secret" || source != SourceEnvironment {
		t.Fatalf("resolve = %q, %q, %v", value, source, err)
	}
	if err := os.Unsetenv("ERKA_TEST_KEY"); err != nil {
		t.Fatal(err)
	}
	value, source, err = manager.Resolve(context.Background(), "deepseek", "https://api.deepseek.com", "ERKA_TEST_KEY")
	if err != nil || value != "keyring-secret" || source != SourceKeyring {
		t.Fatalf("resolve = %q, %q, %v", value, source, err)
	}
}

func TestUnavailableKeyringFallsBackToMemoryWithoutLeakingSecret(t *testing.T) {
	ring := &fakeRing{getErr: keyring.ErrNotFound, setErr: errors.New("unavailable")}
	manager := NewManager(ring)
	source, err := manager.Save(context.Background(), "deepseek", "https://api.deepseek.com", "top-secret")
	if source != SourceMemory || err == nil || contains(err.Error(), "top-secret") {
		t.Fatalf("save = %q, %v", source, err)
	}
	value, source, err := manager.Resolve(context.Background(), "deepseek", "https://api.deepseek.com/", "")
	if err != nil || value != "top-secret" || source != SourceMemory {
		t.Fatalf("resolve = %q, %q, %v", value, source, err)
	}
}

func TestCredentialReplaceDeleteAndNormalizeAccount(t *testing.T) {
	ring := &fakeRing{getErr: keyring.ErrNotFound}
	manager := NewManager(ring)
	if _, err := manager.Save(context.Background(), "DeepSeek", "HTTPS://API.DeepSeek.com///", "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Save(context.Background(), "deepseek", "https://api.deepseek.com", "two"); err != nil {
		t.Fatal(err)
	}
	value, _, err := manager.Resolve(context.Background(), "deepseek", "https://api.deepseek.com/", "")
	if err != nil || value != "two" {
		t.Fatalf("value = %q, %v", value, err)
	}
	if err := manager.Delete(context.Background(), "deepseek", "https://api.deepseek.com"); err != nil {
		t.Fatal(err)
	}
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}
