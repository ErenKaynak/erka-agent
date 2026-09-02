package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	keyring "github.com/zalando/go-keyring"
)

const ServiceName = "erka-agent"

var ErrCredentialNotFound = errors.New("provider credential not found")

type Source string

const (
	SourceEnvironment Source = "environment"
	SourceKeyring     Source = "keyring"
	SourceMemory      Source = "memory"
)

type Keyring interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (osKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (osKeyring) Delete(service, user string) error { return keyring.Delete(service, user) }

type Manager struct {
	keyring Keyring
	mu      sync.RWMutex
	memory  map[string]string
}

func NewManager(ring Keyring) *Manager {
	if ring == nil {
		ring = osKeyring{}
	}
	return &Manager{keyring: ring, memory: make(map[string]string)}
}

func NewDefaultManager() *Manager { return NewManager(nil) }

func (manager *Manager) Resolve(ctx context.Context, provider, endpoint, environmentName string) (string, Source, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if environmentName != "" {
		if value, ok := os.LookupEnv(environmentName); ok && strings.TrimSpace(value) != "" {
			return value, SourceEnvironment, nil
		}
	}
	account, err := Account(provider, endpoint)
	if err != nil {
		return "", "", err
	}
	manager.mu.RLock()
	value := manager.memory[account]
	manager.mu.RUnlock()
	if strings.TrimSpace(value) != "" {
		return value, SourceMemory, nil
	}
	value, err = manager.keyring.Get(ServiceName, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", "", ErrCredentialNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read OS keyring: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return "", "", ErrCredentialNotFound
	}
	return value, SourceKeyring, nil
}

// Save always retains a valid entered credential for this process. If the OS
// keyring is unavailable, SourceMemory and the keyring error are both returned.
func (manager *Manager) Save(ctx context.Context, provider, endpoint, secret string) (Source, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("credential is empty")
	}
	account, err := Account(provider, endpoint)
	if err != nil {
		return "", err
	}
	manager.mu.Lock()
	manager.memory[account] = secret
	manager.mu.Unlock()
	if err := manager.keyring.Set(ServiceName, account, secret); err != nil {
		return SourceMemory, fmt.Errorf("save to OS keyring: %w", err)
	}
	manager.mu.Lock()
	delete(manager.memory, account)
	manager.mu.Unlock()
	return SourceKeyring, nil
}

func (manager *Manager) Delete(ctx context.Context, provider, endpoint string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	account, err := Account(provider, endpoint)
	if err != nil {
		return err
	}
	manager.mu.Lock()
	delete(manager.memory, account)
	manager.mu.Unlock()
	if err := manager.keyring.Delete(ServiceName, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete OS keyring credential: %w", err)
	}
	return nil
}

func Account(provider, endpoint string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return "", errors.New("provider name is empty")
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid provider endpoint %q", endpoint)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery, parsed.Fragment = "", ""
	return provider + "|" + parsed.String(), nil
}
