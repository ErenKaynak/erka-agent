package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ErenKaynak/erka-agent/internal/app"
	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/memory"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func newMemoryCommand(load func(context.Context) (config.Resolved, error), workingDirectory func() string) *cobra.Command {
	command := &cobra.Command{Use: "memory", Short: "Manage long-term memory shared by every model", Args: noArgs}
	command.AddCommand(&cobra.Command{Use: "list", Short: "List global and current-project memories", Args: noArgs, RunE: func(command *cobra.Command, _ []string) error {
		return withMemoryStore(command.Context(), load, workingDirectory(), func(ctx context.Context, database *store.SQLite, projectKey string) error {
			notes, err := database.ListMemories(ctx, projectKey, 500)
			if err != nil {
				return err
			}
			return printMemories(command.OutOrStdout(), notes)
		})
	}})
	command.AddCommand(&cobra.Command{Use: "search <query...>", Short: "Search memories by relevance", Args: cobra.MinimumNArgs(1), RunE: func(command *cobra.Command, args []string) error {
		return withMemoryStore(command.Context(), load, workingDirectory(), func(ctx context.Context, database *store.SQLite, projectKey string) error {
			notes, err := database.SearchMemories(ctx, projectKey, strings.Join(args, " "), 50)
			if err != nil {
				return err
			}
			return printMemories(command.OutOrStdout(), notes)
		})
	}})
	var global bool
	var kind string
	add := &cobra.Command{Use: "add <text...>", Short: "Add a memory for this project, or for everything with --global", Args: cobra.MinimumNArgs(1), RunE: func(command *cobra.Command, args []string) error {
		return withMemoryStore(command.Context(), load, workingDirectory(), func(ctx context.Context, database *store.SQLite, projectKey string) error {
			note, err := newUserMemory(strings.Join(args, " "), kind, global, projectKey)
			if err != nil {
				return err
			}
			saved, err := database.SaveMemory(ctx, note)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\n", saved.ID, memory.Describe(saved))
			return err
		})
	}}
	add.Flags().BoolVar(&global, "global", false, "apply to every project and model")
	add.Flags().StringVar(&kind, "kind", "", "fact, preference, or decision (default: preference with --global, otherwise fact)")
	command.AddCommand(add)
	command.AddCommand(&cobra.Command{Use: "rm <memory-id>", Short: "Delete a memory", Args: oneNonEmptyArg("rm requires one memory ID"), RunE: func(command *cobra.Command, args []string) error {
		return withMemoryStore(command.Context(), load, workingDirectory(), func(ctx context.Context, database *store.SQLite, _ string) error {
			if err := database.DeleteMemory(ctx, args[0]); err != nil {
				return err
			}
			_, err := fmt.Fprintf(command.OutOrStdout(), "deleted %s\n", args[0])
			return err
		})
	}})
	return command
}

func newUserMemory(text, kind string, global bool, projectKey string) (store.MemoryNote, error) {
	if kind == "" {
		kind = "fact"
		if global {
			kind = "preference"
		}
	}
	scope := store.MemoryProject
	if global {
		scope, projectKey = store.MemoryGlobal, ""
	} else if projectKey == "" {
		return store.MemoryNote{}, fmt.Errorf("%w: project memory requires a Git repository; use --global or --cwd", ErrUsage)
	}
	id, err := (erkaruntime.RandomIDGenerator{}).NewID("memory")
	if err != nil {
		return store.MemoryNote{}, err
	}
	now := time.Now().UTC()
	return store.MemoryNote{ID: id, Scope: scope, ProjectKey: projectKey, Kind: kind, Origin: "user", Text: text, CreatedAt: now, UpdatedAt: now}, nil
}

func withMemoryStore(ctx context.Context, load func(context.Context) (config.Resolved, error), workingDirectory string, run func(context.Context, *store.SQLite, string) error) error {
	resolved, err := load(ctx)
	if err != nil {
		return fmt.Errorf("resolve configuration: %w", err)
	}
	projectKey := ""
	if root, err := app.DiscoverRepository(workingDirectory); err == nil {
		projectKey, _ = memory.ProjectKey(root)
	}
	database, err := store.OpenSQLite(ctx, filepath.Join(resolved.StateDir, "erka.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	return run(ctx, database, projectKey)
}

func printMemories(output io.Writer, notes []store.MemoryNote) error {
	if len(notes) == 0 {
		_, err := fmt.Fprintln(output, "No memories.")
		return err
	}
	for _, note := range notes {
		if _, err := fmt.Fprintf(output, "%s\t%s\n", note.ID, memory.Describe(note)); err != nil {
			return err
		}
	}
	return nil
}
