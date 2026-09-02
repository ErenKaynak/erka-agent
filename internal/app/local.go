package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/sandbox"
	"github.com/ErenKaynak/erka-agent/internal/store"
	"github.com/ErenKaynak/erka-agent/internal/tools"
)

const nativeInstructions = `You are erka's coding stage. Work only inside the provided repository. Inspect relevant files, make the smallest correct change, run focused validation, inspect the final diff, and finish with a concise summary. Use tools for repository facts; never invent tool results.`

type LocalOptions struct {
	WorkingDirectory   string
	StateDir           string
	Objective          string
	ResumeTaskID       erkaruntime.TaskID
	Provider           erkaruntime.ModelProvider
	Output             io.Writer
	IDs                erkaruntime.IDGenerator
	Clock              func() time.Time
	Limits             erkaruntime.NativeLimits
	AllowFullExecution bool
}

type LocalResult struct {
	ProjectID erkaruntime.ProjectID
	TaskID    erkaruntime.TaskID
	RunID     erkaruntime.RunID
	Status    erkaruntime.TaskStatus
	Summary   string
}

// RunLocal creates or resumes one durable task and executes it directly in the
// current repository. It starts no daemon or listener.
func RunLocal(ctx context.Context, options LocalOptions) (LocalResult, error) {
	if err := ctx.Err(); err != nil {
		return LocalResult{}, err
	}
	if options.Provider == nil {
		return LocalResult{}, errors.New("local run requires a model provider")
	}
	if !filepath.IsAbs(options.StateDir) {
		return LocalResult{}, errors.New("local run requires an absolute state directory")
	}
	root, err := DiscoverRepository(options.WorkingDirectory)
	if err != nil {
		return LocalResult{}, err
	}
	localSandbox, err := sandbox.NewLocal(root, sandbox.Options{})
	if err != nil {
		return LocalResult{}, fmt.Errorf("initialize workspace sandbox: %w", err)
	}
	database, err := store.OpenSQLite(ctx, filepath.Join(options.StateDir, "erka.db"))
	if err != nil {
		return LocalResult{}, err
	}
	defer database.Close()

	ids := options.IDs
	if ids == nil {
		ids = erkaruntime.RandomIDGenerator{}
	}
	clock := options.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}

	projectID := projectID(root)
	if err := ensureProject(ctx, database, erkaruntime.Project{ID: projectID, Root: root, CreatedAt: clock()}); err != nil {
		return LocalResult{}, err
	}
	task, run, err := prepareTask(ctx, database, options, projectID, ids, clock)
	if err != nil {
		return LocalResult{}, err
	}
	if options.Output != nil {
		if _, err := fmt.Fprintf(options.Output, "task: %s\n", task.ID); err != nil {
			return LocalResult{}, err
		}
	}

	sink := &writerSink{writer: options.Output}
	backend := &erkaruntime.NativeBackend{
		Provider: options.Provider, Tools: tools.Builtins(localSandbox, tools.Options{AllowFullExecution: options.AllowFullExecution}), Sink: sink,
		Instructions: nativeInstructions, Limits: options.Limits, IDs: ids, Clock: clock,
	}
	stage, err := backend.RunStage(ctx, erkaruntime.StageRequest{TaskID: task.ID, RunID: run.ID, Objective: task.Objective}, database)
	result := LocalResult{ProjectID: projectID, TaskID: task.ID, RunID: run.ID, Status: stage.Status}
	if len(stage.Checkpoint.Decisions) > 0 {
		result.Summary = stage.Checkpoint.Decisions[len(stage.Checkpoint.Decisions)-1]
	}
	if err != nil {
		return result, err
	}
	if options.Output != nil {
		if _, err := fmt.Fprintf(options.Output, "\n%s\n", result.Summary); err != nil {
			return result, err
		}
	}
	return result, nil
}

func DiscoverRepository(start string) (string, error) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
	}
	absolute, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	current, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve working directory symlinks: %w", err)
	}
	for {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return current, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect repository marker: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no Git repository found from %q", start)
		}
		current = parent
	}
}

func ensureProject(ctx context.Context, database erkaruntime.Store, project erkaruntime.Project) error {
	err := database.CreateProject(ctx, project)
	if err == nil {
		return nil
	}
	if !errors.Is(err, erkaruntime.ErrAlreadyExists) {
		return err
	}
	existing, loadErr := database.LoadProject(ctx, project.ID)
	if loadErr != nil {
		return loadErr
	}
	if existing.Root != project.Root {
		return fmt.Errorf("project ID collision between %q and %q", existing.Root, project.Root)
	}
	return nil
}

func prepareTask(ctx context.Context, database erkaruntime.Store, options LocalOptions, projectID erkaruntime.ProjectID, ids erkaruntime.IDGenerator, clock func() time.Time) (erkaruntime.Task, erkaruntime.Run, error) {
	if options.ResumeTaskID != "" {
		snapshot, err := database.LoadTask(ctx, options.ResumeTaskID)
		if err != nil {
			return erkaruntime.Task{}, erkaruntime.Run{}, err
		}
		if snapshot.ProjectID != projectID {
			return erkaruntime.Task{}, erkaruntime.Run{}, errors.New("resumed task belongs to another repository")
		}
		run, err := database.LoadRun(ctx, runIDForTask(snapshot.ID))
		return snapshot.Task, run, err
	}
	if strings.TrimSpace(options.Objective) == "" {
		return erkaruntime.Task{}, erkaruntime.Run{}, errors.New("new local task requires an objective")
	}
	id, err := ids.NewID("task")
	if err != nil {
		return erkaruntime.Task{}, erkaruntime.Run{}, err
	}
	now := clock().UTC()
	task := erkaruntime.Task{ID: erkaruntime.TaskID(id), ProjectID: projectID, Objective: options.Objective, Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now}
	run := erkaruntime.Run{ID: runIDForTask(task.ID), TaskID: task.ID, Stage: "coding", BackendClass: "native", Provider: options.Provider.Name(), Model: options.Provider.Name(), Status: erkaruntime.RunQueued, CreatedAt: now, UpdatedAt: now}
	if err := database.CreateTask(ctx, task); err != nil {
		return erkaruntime.Task{}, erkaruntime.Run{}, err
	}
	if err := database.CreateRun(ctx, run); err != nil {
		return erkaruntime.Task{}, erkaruntime.Run{}, err
	}
	return task, run, nil
}

func projectID(root string) erkaruntime.ProjectID {
	digest := sha256.Sum256([]byte(root))
	return erkaruntime.ProjectID(fmt.Sprintf("project_%x", digest[:16]))
}

func runIDForTask(taskID erkaruntime.TaskID) erkaruntime.RunID {
	value := strings.TrimPrefix(string(taskID), "task_")
	return erkaruntime.RunID("run_" + value)
}

type writerSink struct{ writer io.Writer }

func (sink *writerSink) Publish(ctx context.Context, batch erkaruntime.CommittedBatch) error {
	if sink.writer == nil {
		return nil
	}
	for _, event := range batch.Events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(sink.writer, "[%s] %s\n", event.Type, event.Message); err != nil {
			return err
		}
	}
	return nil
}

var _ erkaruntime.EventSink = (*writerSink)(nil)
