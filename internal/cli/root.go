package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ErenKaynak/erka-agent/internal/buildinfo"
)

var (
	// ErrUsage identifies invalid command syntax and arguments.
	ErrUsage = errors.New("invalid command usage")
	// ErrRuntimeUnavailable reports the intentional M0 run-command boundary.
	ErrRuntimeUnavailable = errors.New("local runtime is not implemented; it arrives in M1")
)

// Execute runs the erka command and converts command errors to stable exit codes.
func Execute(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	info buildinfo.Info,
) int {
	command := NewRootCommand(info)
	command.SetArgs(args)
	command.SetOut(stdout)
	command.SetErr(stderr)

	if err := command.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		if errors.Is(err, ErrRuntimeUnavailable) {
			return 1
		}
		return 2
	}

	return 0
}

// NewRootCommand constructs the public erka command hierarchy.
func NewRootCommand(info buildinfo.Info) *cobra.Command {
	root := &cobra.Command{
		Use:           "erka",
		Short:         "A local-first coding-agent runtime",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("%w: unknown command %q", ErrUsage, args[0])
			}
			return nil
		},
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	root.Version = info.Version
	root.SetVersionTemplate("erka {{.Version}}\n")

	root.AddCommand(newVersionCommand(info))
	root.AddCommand(newRunCommand())

	return root
}

func newVersionCommand(info buildinfo.Info) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(
				command.OutOrStdout(),
				"erka %s\ncommit: %s\nbuilt: %s\n",
				info.Version,
				info.Commit,
				info.Date,
			)
			return err
		},
	}
}

func newRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "run <prompt>",
		Short: "Run a coding task in the current repository",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, _ []string) error {
			return ErrRuntimeUnavailable
		},
	}
}

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%w: expected no arguments, received %d", ErrUsage, len(args))
	}
	return nil
}

func exactArgs(want int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != want {
			return fmt.Errorf("%w: expected %d argument, received %d", ErrUsage, want, len(args))
		}
		return nil
	}
}
