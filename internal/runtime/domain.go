package runtime

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type (
	ProjectID string
)

// Project identifies one canonical local repository root.
type Project struct {
	ID        ProjectID
	Root      string
	CreatedAt time.Time
}

func (project Project) Validate() error {
	if project.ID == "" {
		return errors.New("project requires an ID")
	}
	if !filepath.IsAbs(project.Root) {
		return fmt.Errorf("project root must be absolute: %q", project.Root)
	}
	if project.CreatedAt.IsZero() {
		return errors.New("project creation time is required")
	}
	return nil
}

// Task is the durable, provider-neutral user objective.
type Task struct {
	ID        TaskID
	ProjectID ProjectID
	Objective string
	Status    TaskStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (task Task) Validate() error {
	if task.ID == "" || task.ProjectID == "" {
		return errors.New("task requires task and project IDs")
	}
	if strings.TrimSpace(task.Objective) == "" {
		return errors.New("task requires an objective")
	}
	if task.Status != TaskQueued {
		return errors.New("new task must be queued")
	}
	if task.CreatedAt.IsZero() || task.UpdatedAt.IsZero() {
		return errors.New("task creation and update times are required")
	}
	if task.UpdatedAt.Before(task.CreatedAt) {
		return errors.New("task update time precedes creation time")
	}
	return nil
}

type RunStatus string

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunWaiting   RunStatus = "waiting"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
	RunTimedOut  RunStatus = "timed_out"
)

// Run is one execution attempt for a task stage.
type Run struct {
	ID           RunID
	TaskID       TaskID
	Stage        string
	BackendClass string
	Provider     string
	Model        string
	Status       RunStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (run Run) Validate() error {
	if run.ID == "" || run.TaskID == "" {
		return errors.New("run requires run and task IDs")
	}
	if strings.TrimSpace(run.Stage) == "" || strings.TrimSpace(run.BackendClass) == "" {
		return errors.New("run requires a stage and backend class")
	}
	if run.Status != RunQueued {
		return errors.New("new run must be queued")
	}
	if run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() {
		return errors.New("run creation and update times are required")
	}
	if run.UpdatedAt.Before(run.CreatedAt) {
		return errors.New("run update time precedes creation time")
	}
	return nil
}

// StoredTask is the durable task record plus its ordered sequence cursors.
type StoredTask struct {
	Task
	LastItemSequence  uint64
	LastEventSequence uint64
}

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrConflict      = errors.New("concurrent state conflict")
)

// ValidateProject exposes the canonical validation used by store
// implementations.
func ValidateProject(project Project) error { return project.Validate() }

// ValidateTask exposes the canonical validation used by store implementations.
func ValidateTask(task Task) error { return task.Validate() }

// ValidateRun exposes the canonical validation used by store implementations.
func ValidateRun(run Run) error { return run.Validate() }
