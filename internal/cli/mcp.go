package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ErenKaynak/erka-agent/internal/app"
	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/mcp"
)

var startMCP = mcp.Start

func newMCPCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	command := &cobra.Command{Use: "mcp", Short: "Inspect configured MCP tool servers", Args: noArgs}
	command.AddCommand(&cobra.Command{
		Use: "tools", Short: "Connect to configured MCP servers and list their tools", Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			resolved, err := load(command.Context())
			if err != nil {
				return fmt.Errorf("resolve configuration: %w", err)
			}
			if len(resolved.MCPServers) == 0 {
				_, err := fmt.Fprintln(command.OutOrStdout(), "No MCP servers are configured (see mcp_servers in the README).")
				return err
			}
			directory, err := os.Getwd()
			if err != nil {
				return err
			}
			if root, discoverErr := app.DiscoverRepository(directory); discoverErr == nil {
				directory = root
			}
			manager := startMCP(command.Context(), resolved.MCPServers, directory, nil)
			defer manager.Close()
			output := command.OutOrStdout()
			if _, err := fmt.Fprintln(output, "TOOL\tMUTATING\tDESCRIPTION"); err != nil {
				return err
			}
			for _, tool := range manager.Tools() {
				definition := tool.Definition()
				description := strings.ReplaceAll(definition.Description, "\n", " ")
				if len(description) > 100 {
					description = description[:100] + "…"
				}
				if _, err := fmt.Fprintf(output, "%s\t%t\t%s\n", definition.Name, definition.Mutating, description); err != nil {
					return err
				}
			}
			for _, warning := range manager.Warnings() {
				if _, err := fmt.Fprintf(command.ErrOrStderr(), "warning: %s\n", warning); err != nil {
					return err
				}
			}
			return nil
		},
	})
	return command
}
