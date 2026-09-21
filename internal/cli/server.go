package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ErenKaynak/erka-agent/internal/app"
	"github.com/ErenKaynak/erka-agent/internal/auth"
	"github.com/ErenKaynak/erka-agent/internal/catalog"
	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	erkaserver "github.com/ErenKaynak/erka-agent/internal/server"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

type serveOptions struct {
	listen       string
	trustedCIDRs []string
	tlsCert      string
	tlsKey       string
	workerID     string
	poll         time.Duration
	claim        time.Duration
	heartbeat    time.Duration
}

var (
	isRootProcess = runningAsRoot
	runServe      = serveConfigured
)

func newServeCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	options := serveOptions{}
	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the optional authenticated personal server",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			resolved, err := load(command.Context())
			if err != nil {
				return fmt.Errorf("resolve configuration: %w", err)
			}
			return runServe(command.Context(), resolved, options, command.OutOrStdout())
		},
	}
	command.Flags().StringVar(&options.listen, "listen", "127.0.0.1:7432", "HTTP listen address")
	command.Flags().StringVar(&options.tlsCert, "tls-cert", "", "TLS certificate file")
	command.Flags().StringVar(&options.tlsKey, "tls-key", "", "TLS private-key file")
	command.Flags().StringSliceVar(&options.trustedCIDRs, "trusted-cidr", nil, "accept clients only from this network and allow plain HTTP on an address inside it (repeatable; for example a VPN range)")
	command.Flags().StringVar(&options.workerID, "worker-id", "", "durable worker identity")
	command.Flags().DurationVar(&options.poll, "worker-poll", time.Second, "queued-task polling interval")
	command.Flags().DurationVar(&options.claim, "claim-duration", 30*time.Second, "durable worker claim duration")
	command.Flags().DurationVar(&options.heartbeat, "sse-heartbeat", 15*time.Second, "SSE heartbeat interval")
	return command
}

func newServerCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	command := &cobra.Command{Use: "server", Short: "Manage optional server state", Args: noArgs}
	token := &cobra.Command{Use: "token", Short: "Manage scoped server API tokens", Args: noArgs}
	token.AddCommand(newServerTokenCreateCommand(load))
	token.AddCommand(newServerTokenRevokeCommand(load))
	command.AddCommand(token)
	return command
}

func newServerTokenCreateCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	var scopes []string
	command := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an API token and print its secret once",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("%w: token creation requires one non-empty name", ErrUsage)
			}
			return nil
		},
		RunE: func(command *cobra.Command, args []string) error {
			resolved, err := load(command.Context())
			if err != nil {
				return fmt.Errorf("resolve configuration: %w", err)
			}
			id, secret, normalized, err := createServerToken(command.Context(), resolved.StateDir, args[0], scopes)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "ID: %s\nToken: %s\nScopes: %s\n", id, secret, strings.Join(normalized, ","))
			return err
		},
	}
	command.Flags().StringSliceVar(&scopes, "scope", []string{"read", "write"}, "token scope (read, write, approve, or admin)")
	return command
}

func newServerTokenRevokeCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke a server API token",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("%w: token revocation requires one token ID", ErrUsage)
			}
			return nil
		},
		RunE: func(command *cobra.Command, args []string) error {
			resolved, err := load(command.Context())
			if err != nil {
				return fmt.Errorf("resolve configuration: %w", err)
			}
			database, err := store.OpenSQLite(command.Context(), filepath.Join(resolved.StateDir, "erka.db"))
			if err != nil {
				return err
			}
			defer database.Close()
			if err := database.RevokeAPIToken(command.Context(), args[0], time.Now().UTC()); err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Revoked %s\n", args[0])
			return err
		},
	}
}

func createServerToken(ctx context.Context, stateDir, name string, scopes []string) (string, string, []string, error) {
	if !filepath.IsAbs(stateDir) {
		return "", "", nil, errors.New("server token storage requires an absolute state directory")
	}
	normalized, err := normalizeServerScopes(scopes)
	if err != nil {
		return "", "", nil, err
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", "", nil, fmt.Errorf("generate server token: %w", err)
	}
	secret := "erka_" + base64.RawURLEncoding.EncodeToString(random)
	id, err := (erkaruntime.RandomIDGenerator{}).NewID("token")
	if err != nil {
		return "", "", nil, err
	}
	database, err := store.OpenSQLite(ctx, filepath.Join(stateDir, "erka.db"))
	if err != nil {
		return "", "", nil, err
	}
	defer database.Close()
	hash := sha256.Sum256([]byte(secret))
	if err := database.CreateAPIToken(ctx, store.APIToken{ID: id, Name: strings.TrimSpace(name), Hash: hash, Scopes: normalized, CreatedAt: time.Now().UTC()}); err != nil {
		return "", "", nil, err
	}
	return id, secret, normalized, nil
}

func normalizeServerScopes(scopes []string) ([]string, error) {
	valid := map[string]bool{"read": true, "write": true, "approve": true, "admin": true}
	seen := make(map[string]bool)
	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if !valid[scope] {
			return nil, fmt.Errorf("unsupported server token scope %q", scope)
		}
		if !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("server token requires at least one scope")
	}
	sort.Strings(result)
	return result, nil
}

func serveConfigured(ctx context.Context, resolved config.Resolved, options serveOptions, output io.Writer) error {
	if isRootProcess() {
		return errors.New("erka serve refuses to run as the root OS user")
	}
	options.trustedCIDRs = append(append([]string(nil), resolved.Server.TrustedCIDRs...), options.trustedCIDRs...)
	if err := validateServerListen(options); err != nil {
		return err
	}
	trusted, err := erkaserver.ParseTrustedNetworks(options.trustedCIDRs)
	if err != nil {
		return err
	}
	targets, err := erkaserver.NotificationTargets(resolved.Server, os.LookupEnv, nil)
	if err != nil {
		return err
	}
	database, err := store.OpenSQLite(ctx, filepath.Join(resolved.StateDir, "erka.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	manager := newAuthManager()
	interactor, err := erkaserver.NewInteractor(database, nil, 0)
	if err != nil {
		return err
	}
	dispatcher, err := erkaserver.NewDispatcher(database, targets, nil)
	if err != nil {
		return err
	}
	openSession := configuredServerSessionFactory(resolved, manager, interactor)
	service, err := erkaserver.NewService(erkaserver.ServiceOptions{
		StateDir: resolved.StateDir, Store: database, RuntimeStore: database,
		OpenSession: openSession, WorkerPoll: options.poll, ClaimDuration: options.claim,
	})
	if err != nil {
		return err
	}
	previews := erkaserver.NewPreviewManager(resolved.Server.Previews, resolved.StateDir)
	defer previews.StopAll()
	api, err := erkaserver.NewAPI(erkaserver.APIOptions{
		Service: service, Store: database, SSEHeartbeat: options.heartbeat,
		Connections: serverConnections(resolved), Models: serverModels(resolved, manager), Pipelines: serverPipelines(resolved),
		Previews: previews, Memories: database,
	})
	if err != nil {
		return err
	}
	holder := strings.TrimSpace(options.workerID)
	if holder == "" {
		holder, err = (erkaruntime.RandomIDGenerator{}).NewID("server")
		if err != nil {
			return err
		}
	}
	server := &http.Server{
		Addr: options.listen, Handler: erkaserver.RestrictNetworks(api.Handler(), trusted), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	workerResult := make(chan error, 1)
	go func() { workerResult <- service.RunWorker(workerCtx, holder) }()
	go func() {
		if err := dispatcher.Run(workerCtx); err != nil && workerCtx.Err() == nil {
			_, _ = fmt.Fprintf(output, "erka notifications stopped: %v\n", err)
		}
	}()
	if len(targets) > 0 {
		_, _ = fmt.Fprintf(output, "erka notifications enabled for %d target(s)\n", len(targets))
	}
	listenResult := make(chan error, 1)
	go func() {
		_, _ = fmt.Fprintf(output, "erka server listening on %s\n", options.listen)
		if options.tlsCert != "" {
			listenResult <- server.ListenAndServeTLS(options.tlsCert, options.tlsKey)
			return
		}
		listenResult <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-workerResult:
		_ = server.Close()
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("server worker: %w", err)
	case err := <-listenResult:
		cancelWorker()
		if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
			return nil
		}
		return err
	}
}

func validateServerListen(options serveOptions) error {
	if (options.tlsCert == "") != (options.tlsKey == "") {
		return errors.New("server TLS requires both --tls-cert and --tls-key")
	}
	if options.poll <= 0 || options.claim <= 0 || options.heartbeat <= 0 {
		return errors.New("server worker and heartbeat durations must be positive")
	}
	host, _, err := net.SplitHostPort(options.listen)
	if err != nil {
		return fmt.Errorf("invalid server listen address: %w", err)
	}
	if options.tlsCert != "" || host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	if len(options.trustedCIDRs) > 0 {
		networks, err := erkaserver.ParseTrustedNetworks(options.trustedCIDRs)
		if err != nil {
			return err
		}
		if ip != nil && !ip.IsUnspecified() && erkaserver.InTrustedNetwork(ip, networks) {
			return nil
		}
		return errors.New("plain HTTP with trusted networks must listen on a specific address inside a trusted network, such as the server's VPN address")
	}
	return errors.New("plain HTTP server must listen on a loopback address; configure TLS or --trusted-cidr for remote access")
}

func configuredServerSessionFactory(resolved config.Resolved, manager *auth.Manager, interactor erkaruntime.Interactor) erkaserver.SessionFactory {
	return func(ctx context.Context, root string) (erkaserver.TaskSession, error) {
		options := app.SessionOptions{
			WorkingDirectory: root, StateDir: resolved.StateDir, Pipeline: resolved,
			ProviderFactory: configuredProviderFactory(manager), BridgeFactory: configuredBridgeFactory(),
			Limits: nativeLimits(resolved), AllowFullExecution: resolved.AllowFullExecution,
			Interactor: interactor, ApproveCommands: resolved.ApproveCommands,
		}
		if resolved.ActivePipeline == "" {
			key, _, err := manager.Resolve(ctx, resolved.Provider, resolved.BaseURL, resolved.APIKeyEnv)
			if err != nil {
				return nil, fmt.Errorf("resolve %s credential: %w", resolved.Provider, err)
			}
			provider, err := openNativeProvider(resolved.Provider, resolved.BaseURL, resolved.Model, key)
			if err != nil {
				return nil, err
			}
			options.Pipeline = config.Resolved{}
			options.Provider = provider
			options.NativeModel = resolved.Model
			options.ProviderFactory = nil
			options.BridgeFactory = nil
		}
		return app.OpenSession(ctx, options)
	}
}

func serverConnections(resolved config.Resolved) erkaserver.ResourceLoader {
	return func(context.Context) (any, error) {
		if len(resolved.Connections) == 0 {
			return map[string]any{"connections": []map[string]any{{"name": "default", "class": config.ConnectionNative, "provider": resolved.Provider, "baseUrl": resolved.BaseURL}}}, nil
		}
		result := make([]map[string]any, 0, len(resolved.Connections))
		for _, name := range sortedConnectionKeys(resolved.Connections) {
			connection := resolved.Connections[name]
			class := connection.Class
			if class == "" {
				class = config.ConnectionNative
			}
			result = append(result, map[string]any{"name": name, "class": class, "provider": connection.Provider, "baseUrl": connection.BaseURL})
		}
		return map[string]any{"connections": result}, nil
	}
}

func serverModels(resolved config.Resolved, manager *auth.Manager) erkaserver.ResourceLoader {
	return func(ctx context.Context) (any, error) {
		loaded := catalog.Load(ctx, resolved, configuredModelCatalogFactory(manager))
		models := make([]map[string]any, 0, len(loaded.Entries))
		for _, entry := range loaded.Entries {
			models = append(models, map[string]any{"stage": entry.Stage, "connection": entry.Connection, "class": entry.Class, "provider": entry.Provider, "id": entry.Model.ID, "source": entry.Source, "state": entry.State, "current": entry.Current, "selectable": entry.Selectable})
		}
		warnings := make([]string, 0, len(loaded.Issues))
		for _, warning := range loaded.Issues {
			warnings = append(warnings, fmt.Sprintf("%s: %v", warning.Connection, warning.Err))
		}
		return map[string]any{"models": models, "warnings": warnings}, nil
	}
}

func serverPipelines(resolved config.Resolved) erkaserver.ResourceLoader {
	return func(context.Context) (any, error) {
		names := make([]string, 0, len(resolved.Pipelines))
		for name := range resolved.Pipelines {
			names = append(names, name)
		}
		sort.Strings(names)
		result := make([]map[string]any, 0, len(names))
		for _, name := range names {
			pipeline := resolved.Pipelines[name]
			result = append(result, map[string]any{"name": name, "routing": pipeline.Routing, "maxRepairAttempts": pipeline.MaxRepairAttempts, "active": name == resolved.ActivePipeline})
		}
		return map[string]any{"pipelines": result}, nil
	}
}

func sortedConnectionKeys(connections map[string]config.Connection) []string {
	result := make([]string, 0, len(connections))
	for name := range connections {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}
