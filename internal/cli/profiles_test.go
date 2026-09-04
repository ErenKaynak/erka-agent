package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	keyring "github.com/zalando/go-keyring"

	"github.com/ErenKaynak/erka-agent/internal/auth"
)

func TestRemoteProfilePersistsMetadataWithoutToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := t.TempDir()
	ring := &profileKeyring{values: make(map[string]string)}
	manager := auth.NewManager(ring)
	if err := addRemoteProfile(ctx, state, "home", "https://erka.example.com/", "secret-token", manager); err != nil {
		t.Fatal(err)
	}
	profiles, err := loadRemoteProfiles(state)
	if err != nil || profiles.Profiles["home"].URL != "https://erka.example.com" {
		t.Fatalf("profiles = %#v, %v", profiles, err)
	}
	data, err := os.ReadFile(filepath.Join(state, "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-token") {
		t.Fatal("profile metadata contains bearer token")
	}
	info, err := os.Stat(filepath.Join(state, "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("profile file mode = %v", info.Mode().Perm())
	}
	if _, err := openRemoteProfile(ctx, state, "home", manager); err != nil {
		t.Fatal(err)
	}
	if err := removeRemoteProfile(ctx, state, "home", manager); err != nil {
		t.Fatal(err)
	}
	profiles, err = loadRemoteProfiles(state)
	if err != nil || len(profiles.Profiles) != 0 {
		t.Fatalf("profiles after removal = %#v, %v", profiles, err)
	}
	if _, _, err := manager.Resolve(ctx, profileCredentialName("home"), "https://erka.example.com", ""); !errors.Is(err, auth.ErrCredentialNotFound) {
		t.Fatalf("removed token resolution = %v", err)
	}
}

func TestRemoteProfileRejectsUnsafeNamesAndURLs(t *testing.T) {
	t.Parallel()
	manager := auth.NewManager(&profileKeyring{values: make(map[string]string)})
	for _, test := range []struct{ name, endpoint string }{
		{"../home", "https://erka.example.com"},
		{"home", "http://erka.example.com"},
		{"home", "https://user:secret@erka.example.com"},
	} {
		if err := addRemoteProfile(context.Background(), t.TempDir(), test.name, test.endpoint, "secret", manager); err == nil {
			t.Fatalf("unsafe profile accepted: %#v", test)
		}
	}
}

type profileKeyring struct {
	mu     sync.Mutex
	values map[string]string
}

func (ring *profileKeyring) Get(service, user string) (string, error) {
	ring.mu.Lock()
	defer ring.mu.Unlock()
	value, ok := ring.values[service+"|"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}

func (ring *profileKeyring) Set(service, user, password string) error {
	ring.mu.Lock()
	defer ring.mu.Unlock()
	ring.values[service+"|"+user] = password
	return nil
}

func (ring *profileKeyring) Delete(service, user string) error {
	ring.mu.Lock()
	defer ring.mu.Unlock()
	key := service + "|" + user
	if _, ok := ring.values[key]; !ok {
		return keyring.ErrNotFound
	}
	delete(ring.values, key)
	return nil
}
