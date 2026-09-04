package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ErenKaynak/erka-agent/internal/auth"
	"github.com/ErenKaynak/erka-agent/internal/bridges"
	"github.com/ErenKaynak/erka-agent/internal/catalog"
	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/tui"
)

var discoverConfiguredModels = func(ctx context.Context, resolved config.Resolved, manager *auth.Manager) catalog.Result {
	return catalog.Load(ctx, resolved, configuredModelCatalogFactory(manager))
}

func configuredModelCatalogFactory(manager *auth.Manager) catalog.Factory {
	return func(ctx context.Context, _ string, connection config.Connection, model string) (erkaruntime.ModelCatalog, error) {
		if connection.Class == config.ConnectionSubscriptionBridge {
			bridge, err := bridges.New(connection.Provider, connection.Command)
			if err != nil {
				return nil, err
			}
			modelCatalog, ok := bridge.(erkaruntime.ModelCatalog)
			if !ok {
				return nil, fmt.Errorf("%s bridge does not support model discovery", connection.Provider)
			}
			return modelCatalog, nil
		}
		key, _, err := manager.Resolve(ctx, connection.Provider, connection.BaseURL, connection.APIKeyEnv)
		if err != nil {
			return nil, fmt.Errorf("resolve %s credential: %w", connection.Provider, err)
		}
		provider, err := openNativeProvider(connection.Provider, connection.BaseURL, model, key)
		if err != nil {
			return nil, err
		}
		modelCatalog, ok := provider.(erkaruntime.ModelCatalog)
		if !ok {
			return nil, fmt.Errorf("%s provider does not support model discovery", connection.Provider)
		}
		return modelCatalog, nil
	}
}

func modelCatalogFunc(manager *auth.Manager, resolved config.Resolved) tui.ModelCatalogFunc {
	return func(ctx context.Context) ([]tui.ModelOption, []string, error) {
		result := discoverConfiguredModels(ctx, resolved, manager)
		options := make([]tui.ModelOption, 0, len(result.Entries))
		for _, entry := range result.Entries {
			if entry.Stage == "" {
				continue
			}
			options = append(options, tui.ModelOption{
				Stage: entry.Stage, Connection: entry.Connection, Class: entry.Class, Provider: entry.Provider,
				ID: entry.Model.ID, DisplayName: entry.Model.DisplayName, Source: entry.Source, State: entry.State,
				Detail: entry.Detail, Default: entry.Model.Default, Current: entry.Current, Selectable: entry.Selectable,
			})
		}
		issues := make([]string, 0, len(result.Issues))
		for _, issue := range result.Issues {
			issues = append(issues, fmt.Sprintf("%s: %v", issue.Connection, issue.Err))
		}
		return options, issues, result.Error()
	}
}

func newModelsCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "List configured and currently available models",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			resolved, err := load(command.Context())
			if err != nil {
				return err
			}
			result := discoverConfiguredModels(command.Context(), resolved, newAuthManager())
			if err := result.Error(); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(command.OutOrStdout(), "STAGE\tCONNECTION\tCLASS\tPROVIDER\tMODEL\tSOURCE\tSTATE"); err != nil {
				return err
			}
			for _, entry := range result.Entries {
				stage := string(entry.Stage)
				if stage == "" {
					stage = "-"
				}
				state := entry.State
				if entry.Current {
					state = appendState(state, "current")
				}
				if entry.Model.Default {
					state = appendState(state, "default")
				}
				if !entry.Selectable {
					state = appendState(state, "disabled")
				}
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", tableCell(stage), tableCell(entry.Connection), tableCell(entry.Class), tableCell(entry.Provider), tableCell(entry.Model.ID), tableCell(entry.Source), tableCell(state)); err != nil {
					return err
				}
			}
			for _, issue := range result.Issues {
				if _, err := fmt.Fprintf(command.ErrOrStderr(), "Warning: %s model discovery failed: %s\n", tableCell(issue.Connection), tableCell(issue.Err.Error())); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func appendState(state, value string) string {
	if strings.TrimSpace(state) == "" {
		return value
	}
	return state + "," + value
}

func tableCell(value string) string {
	value = tui.Sanitize(value)
	value = strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(value)
	characters := []rune(value)
	if len(characters) > 4096 {
		value = string(characters[:4096])
	}
	return value
}
