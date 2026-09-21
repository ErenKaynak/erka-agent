package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ErenKaynak/erka-agent/internal/client"
	"github.com/ErenKaynak/erka-agent/internal/config"
)

func newRemoteCommands(load func(context.Context) (config.Resolved, error), selected func() string) []*cobra.Command {
	return []*cobra.Command{
		newRemoteProjectsCommand(load, selected),
		newRemoteWatchCommand(load, selected),
		newRemoteShowCommand(load, selected),
		newRemoteDiffCommand(load, selected),
		newRemoteApprovalsCommand(load, selected),
		newRemoteActionCommand("cancel", "Cancel a remote task", load, selected, func(ctx context.Context, remote *client.Client, id string) error { return remote.Cancel(ctx, id) }),
		newRemoteActionCommand("retry", "Retry a failed remote task", load, selected, func(ctx context.Context, remote *client.Client, id string) error { return remote.Retry(ctx, id) }),
		newRemoteApprovalCommand("approve", "approved", load, selected),
		newRemoteApprovalCommand("reject", "rejected", load, selected),
		newRemoteAnswerCommand(load, selected),
		newRemotePreviewCommand(load, selected),
		newRemotePreviewsCommand(load, selected),
		newRemoteActionCommand("preview-stop", "Stop a remote preview", load, selected, func(ctx context.Context, remote *client.Client, id string) error { return remote.StopPreview(ctx, id) }),
	}
}

func newRemotePreviewCommand(load func(context.Context) (config.Resolved, error), selected func() string) *cobra.Command {
	return &cobra.Command{Use: "preview <task-id> <preset>", Short: "Start a server preview preset for a task and print its VPN URL", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 2 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "" {
			return fmt.Errorf("%w: preview requires a task ID and a preset name", ErrUsage)
		}
		return nil
	}, RunE: func(command *cobra.Command, args []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		preview, err := remote.StartPreview(command.Context(), args[0], args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", preview.ID, preview.Status, preview.URL)
		return err
	}}
}

func newRemotePreviewsCommand(load func(context.Context) (config.Resolved, error), selected func() string) *cobra.Command {
	return &cobra.Command{Use: "previews <task-id>", Short: "List a remote task's previews", Args: oneNonEmptyArg("previews requires one task ID"), RunE: func(command *cobra.Command, args []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		previews, err := remote.Previews(command.Context(), args[0])
		if err != nil {
			return err
		}
		for _, preview := range previews {
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\t%s\n", preview.ID, preview.Preset, preview.Status, preview.URL); err != nil {
				return err
			}
		}
		return nil
	}}
}

func newRemoteAnswerCommand(load func(context.Context) (config.Resolved, error), selected func() string) *cobra.Command {
	return &cobra.Command{Use: "answer <approval-id> <answer...>", Short: "Answer a remote question and resume its task", Args: func(_ *cobra.Command, args []string) error {
		if len(args) < 2 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(strings.Join(args[1:], " ")) == "" {
			return fmt.Errorf("%w: answer requires an approval ID and answer text", ErrUsage)
		}
		return nil
	}, RunE: func(command *cobra.Command, args []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		approval, err := remote.RespondApproval(command.Context(), args[0], client.ApprovalResponse{Decision: "approved", Response: strings.Join(args[1:], " ")})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\n", approval.ID, approval.Status)
		return err
	}}
}

func newRemoteApprovalsCommand(load func(context.Context) (config.Resolved, error), selected func() string) *cobra.Command {
	var taskID string
	var status string
	command := &cobra.Command{Use: "approvals", Short: "List remote approval requests", Args: noArgs, RunE: func(command *cobra.Command, _ []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		approvals, err := remote.Approvals(command.Context(), taskID, status)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(command.OutOrStdout(), "ID\tTASK\tKIND\tSTATUS\tMESSAGE"); err != nil {
			return err
		}
		for _, approval := range approvals {
			message := approval.Message
			if len(approval.Options) > 0 {
				message += " [options: " + strings.Join(approval.Options, " | ") + "]"
			}
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", approval.ID, approval.TaskID, approval.Kind, approval.Status, message); err != nil {
				return err
			}
		}
		return nil
	}}
	command.Flags().StringVar(&taskID, "task", "", "filter by task ID")
	command.Flags().StringVar(&status, "status", "pending", "filter by approval status")
	return command
}

func runRemoteTask(ctx context.Context, stateDir, profile, project, objective string, output io.Writer) error {
	if strings.TrimSpace(project) == "" {
		return fmt.Errorf("%w: remote run requires --project", ErrUsage)
	}
	remote, err := openRemoteProfile(ctx, stateDir, profile, newAuthManager())
	if err != nil {
		return err
	}
	task, err := remote.Run(ctx, project, objective)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "%s\t%s\n", task.ID, task.Status)
	return err
}

func newRemoteProjectsCommand(load func(context.Context) (config.Resolved, error), selected func() string) *cobra.Command {
	command := &cobra.Command{Use: "projects", Short: "List projects from a remote profile", Args: noArgs, RunE: func(command *cobra.Command, _ []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		projects, err := remote.Projects(command.Context())
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(command.OutOrStdout(), "ID\tNAME\tDEFAULT_REF"); err != nil {
			return err
		}
		for _, project := range projects {
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", project.ID, project.Name, project.DefaultRef); err != nil {
				return err
			}
		}
		return nil
	}}
	command.AddCommand(newRemoteProjectAddCommand(load, selected))
	return command
}

func newRemoteProjectAddCommand(load func(context.Context) (config.Resolved, error), selected func() string) *cobra.Command {
	var defaultRef string
	command := &cobra.Command{Use: "add <name> <server-repository-root>", Short: "Register a repository already present on the server", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 2 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "" {
			return fmt.Errorf("%w: projects add requires a name and server repository root", ErrUsage)
		}
		return nil
	}, RunE: func(command *cobra.Command, args []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		project, err := remote.CreateProject(command.Context(), args[0], args[1], defaultRef)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\n", project.ID, project.Name)
		return err
	}}
	command.Flags().StringVar(&defaultRef, "ref", "HEAD", "default Git ref")
	return command
}

func newRemoteWatchCommand(load func(context.Context) (config.Resolved, error), selected func() string) *cobra.Command {
	var after string
	command := &cobra.Command{Use: "watch <task-id>", Short: "Watch committed remote task events", Args: oneNonEmptyArg("watch requires one task ID"), RunE: func(command *cobra.Command, args []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		return remote.Watch(command.Context(), args[0], after, func(event client.Event) error {
			_, err := fmt.Fprintf(command.OutOrStdout(), "%d\t%s\t%s\n", event.Sequence, event.Type, event.Message)
			return err
		})
	}}
	command.Flags().StringVar(&after, "after", "", "last received event ID or sequence")
	return command
}

func newRemoteShowCommand(load func(context.Context) (config.Resolved, error), selected func() string) *cobra.Command {
	return &cobra.Command{Use: "show <task-id>", Short: "Show remote task state", Args: oneNonEmptyArg("show requires one task ID"), RunE: func(command *cobra.Command, args []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		task, err := remote.Task(command.Context(), args[0])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "ID: %s\nProject: %s\nStatus: %s\nRuntime: %s\nObjective: %s\n", task.ID, task.ProjectID, task.Status, task.RuntimeStatus, task.Objective)
		return err
	}}
}

func newRemoteDiffCommand(load func(context.Context) (config.Resolved, error), selected func() string) *cobra.Command {
	return &cobra.Command{Use: "diff <task-id>", Short: "Show a remote task worktree diff", Args: oneNonEmptyArg("diff requires one task ID"), RunE: func(command *cobra.Command, args []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		diff, err := remote.Diff(command.Context(), args[0])
		if err != nil {
			return err
		}
		_, err = io.WriteString(command.OutOrStdout(), diff)
		return err
	}}
}

func newRemoteActionCommand(use, short string, load func(context.Context) (config.Resolved, error), selected func() string, action func(context.Context, *client.Client, string) error) *cobra.Command {
	return &cobra.Command{Use: use + " <task-id>", Short: short, Args: oneNonEmptyArg(use + " requires one task ID"), RunE: func(command *cobra.Command, args []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		if err := action(command.Context(), remote, args[0]); err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "%s requested for %s\n", use, args[0])
		return err
	}}
}

func newRemoteApprovalCommand(use, decision string, load func(context.Context) (config.Resolved, error), selected func() string) *cobra.Command {
	short := strings.ToUpper(use[:1]) + use[1:] + " a remote approval"
	var note string
	var always bool
	command := &cobra.Command{Use: use + " <approval-id>", Short: short, Args: oneNonEmptyArg(use + " requires one approval ID"), RunE: func(command *cobra.Command, args []string) error {
		remote, err := loadRemoteClient(command.Context(), load, selected())
		if err != nil {
			return err
		}
		response := client.ApprovalResponse{Decision: decision, Response: note}
		if always {
			response.Scope = "task"
		}
		approval, err := remote.RespondApproval(command.Context(), args[0], response)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\n", approval.ID, approval.Status)
		return err
	}}
	command.Flags().StringVar(&note, "note", "", "note passed to the model with the decision")
	if decision == "approved" {
		command.Flags().BoolVar(&always, "always", false, "approve this tool for the rest of the task")
	}
	return command
}

func loadRemoteClient(ctx context.Context, load func(context.Context) (config.Resolved, error), profile string) (*client.Client, error) {
	if strings.TrimSpace(profile) == "" {
		return nil, fmt.Errorf("%w: this command requires --profile", ErrUsage)
	}
	resolved, err := load(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve configuration: %w", err)
	}
	return openRemoteProfile(ctx, resolved.StateDir, profile, newAuthManager())
}

func oneNonEmptyArg(message string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return fmt.Errorf("%w: %s", ErrUsage, message)
		}
		return nil
	}
}
