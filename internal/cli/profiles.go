package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/ErenKaynak/erka-agent/internal/auth"
	"github.com/ErenKaynak/erka-agent/internal/client"
	"github.com/ErenKaynak/erka-agent/internal/config"
)

const profilesVersion = 1

type remoteProfile struct {
	URL string `json:"url"`
}

type remoteProfileFile struct {
	Version  int                      `json:"version"`
	Profiles map[string]remoteProfile `json:"profiles"`
}

func newProfileCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	command := &cobra.Command{Use: "profile", Short: "Manage remote server profiles", Args: noArgs}
	command.AddCommand(newProfileAddCommand(load))
	command.AddCommand(newProfileListCommand(load))
	command.AddCommand(newProfileRemoveCommand(load))
	return command
}

func newProfileAddCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	var tokenEnvironment string
	command := &cobra.Command{
		Use:   "add <name> <server-url>",
		Short: "Save remote server metadata and a keyring-backed token",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 2 {
				return fmt.Errorf("%w: profile add requires a name and server URL", ErrUsage)
			}
			return nil
		},
		RunE: func(command *cobra.Command, args []string) error {
			resolved, err := load(command.Context())
			if err != nil {
				return fmt.Errorf("resolve configuration: %w", err)
			}
			secret := strings.TrimSpace(os.Getenv(tokenEnvironment))
			if secret == "" {
				return fmt.Errorf("remote token is missing: set %s", tokenEnvironment)
			}
			if err := addRemoteProfile(command.Context(), resolved.StateDir, args[0], args[1], secret, newAuthManager()); err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Saved profile %s\n", args[0])
			return err
		},
	}
	command.Flags().StringVar(&tokenEnvironment, "token-env", "ERKA_SERVER_TOKEN", "environment variable containing the server token")
	return command
}

func newProfileListCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List remote server profiles", Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			resolved, err := load(command.Context())
			if err != nil {
				return fmt.Errorf("resolve configuration: %w", err)
			}
			profiles, err := loadRemoteProfiles(resolved.StateDir)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(command.OutOrStdout(), "NAME\tURL"); err != nil {
				return err
			}
			for _, name := range sortedProfileNames(profiles.Profiles) {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\n", name, profiles.Profiles[name].URL); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newProfileRemoveCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	return &cobra.Command{
		Use: "remove <name>", Short: "Remove remote server metadata and its token", Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: profile remove requires one name", ErrUsage)
			}
			return nil
		},
		RunE: func(command *cobra.Command, args []string) error {
			resolved, err := load(command.Context())
			if err != nil {
				return fmt.Errorf("resolve configuration: %w", err)
			}
			if err := removeRemoteProfile(command.Context(), resolved.StateDir, args[0], newAuthManager()); err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Removed profile %s\n", args[0])
			return err
		},
	}
}

func addRemoteProfile(ctx context.Context, stateDir, name, serverURL, token string, manager *auth.Manager) error {
	name = strings.TrimSpace(name)
	if !validProfileName(name) {
		return errors.New("profile name must use 1-64 letters, digits, hyphens, or underscores")
	}
	remote, err := client.New(serverURL, token, nil)
	if err != nil {
		return err
	}
	_ = remote
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	profiles, err := loadRemoteProfiles(stateDir)
	if err != nil {
		return err
	}
	if _, exists := profiles.Profiles[name]; exists {
		return fmt.Errorf("profile %q already exists", name)
	}
	if _, err := manager.Save(ctx, profileCredentialName(name), serverURL, token); err != nil {
		return fmt.Errorf("save remote token to OS keyring: %w", err)
	}
	profiles.Profiles[name] = remoteProfile{URL: serverURL}
	if err := saveRemoteProfiles(stateDir, profiles); err != nil {
		_ = manager.Delete(context.Background(), profileCredentialName(name), serverURL)
		return err
	}
	return nil
}

func removeRemoteProfile(ctx context.Context, stateDir, name string, manager *auth.Manager) error {
	profiles, err := loadRemoteProfiles(stateDir)
	if err != nil {
		return err
	}
	profile, exists := profiles.Profiles[name]
	if !exists {
		return fmt.Errorf("profile %q not found", name)
	}
	delete(profiles.Profiles, name)
	if err := saveRemoteProfiles(stateDir, profiles); err != nil {
		return err
	}
	if err := manager.Delete(ctx, profileCredentialName(name), profile.URL); err != nil {
		return fmt.Errorf("profile metadata removed but token deletion failed: %w", err)
	}
	return nil
}

func openRemoteProfile(ctx context.Context, stateDir, name string, manager *auth.Manager) (*client.Client, error) {
	profiles, err := loadRemoteProfiles(stateDir)
	if err != nil {
		return nil, err
	}
	profile, exists := profiles.Profiles[name]
	if !exists {
		return nil, fmt.Errorf("remote profile %q not found", name)
	}
	token, _, err := manager.Resolve(ctx, profileCredentialName(name), profile.URL, "")
	if err != nil {
		return nil, fmt.Errorf("resolve token for profile %q: %w", name, err)
	}
	return client.New(profile.URL, token, nil)
}

func loadRemoteProfiles(stateDir string) (remoteProfileFile, error) {
	result := remoteProfileFile{Version: profilesVersion, Profiles: make(map[string]remoteProfile)}
	if !filepath.IsAbs(stateDir) {
		return result, errors.New("remote profiles require an absolute state directory")
	}
	file, err := os.Open(filepath.Join(stateDir, "profiles.json"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("load remote profiles: %w", err)
	}
	if result.Version != profilesVersion || result.Profiles == nil {
		return result, fmt.Errorf("unsupported remote profile file version %d", result.Version)
	}
	for name, profile := range result.Profiles {
		if !validProfileName(name) {
			return result, fmt.Errorf("invalid stored remote profile name %q", name)
		}
		if _, err := client.New(profile.URL, "validation", nil); err != nil {
			return result, fmt.Errorf("invalid stored remote profile %q: %w", name, err)
		}
	}
	return result, nil
}

func saveRemoteProfiles(stateDir string, profiles remoteProfileFile) error {
	if !filepath.IsAbs(stateDir) {
		return errors.New("remote profiles require an absolute state directory")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(stateDir, ".profiles-*.json")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(profiles); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, filepath.Join(stateDir, "profiles.json"))
}

func validProfileName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for index, character := range name {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || (index > 0 && (character == '-' || character == '_')) {
			continue
		}
		return false
	}
	return true
}

func profileCredentialName(name string) string { return "server-profile-" + name }

func sortedProfileNames(profiles map[string]remoteProfile) []string {
	result := make([]string, 0, len(profiles))
	for name := range profiles {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}
