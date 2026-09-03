package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type TurnStatus string

const (
	TurnQueued    TurnStatus = "queued"
	TurnClaimed   TurnStatus = "claimed"
	TurnRunning   TurnStatus = "running"
	TurnCompleted TurnStatus = "completed"
	TurnFailed    TurnStatus = "failed"
	TurnCancelled TurnStatus = "cancelled"
	TurnTimedOut  TurnStatus = "timed_out"
	TurnWaiting   TurnStatus = "waiting"
)

type Turn struct {
	ID        string
	TaskID    erkaruntime.TaskID
	Position  uint64
	Prompt    string
	RunID     erkaruntime.RunID
	Status    TurnStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Lease struct {
	ProjectID erkaruntime.ProjectID
	Holder    string
	ExpiresAt time.Time
	UpdatedAt time.Time
}

func (store *SQLite) ListTasks(ctx context.Context, projectID erkaruntime.ProjectID) ([]erkaruntime.TaskSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT id FROM tasks WHERE project_id = ? ORDER BY updated_at DESC, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project %q tasks: %w", projectID, err)
	}
	defer rows.Close()
	var ids []erkaruntime.TaskID
	for rows.Next() {
		var id erkaruntime.TaskID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]erkaruntime.TaskSnapshot, 0, len(ids))
	for _, id := range ids {
		task, err := store.LoadTask(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	return result, nil
}

func (store *SQLite) ListTurns(ctx context.Context, taskID erkaruntime.TaskID) ([]Turn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT id, task_id, position, prompt, run_id, status, created_at, updated_at FROM turns WHERE task_id = ? ORDER BY position`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task %q turns: %w", taskID, err)
	}
	defer rows.Close()
	var result []Turn
	for rows.Next() {
		turn, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, turn)
	}
	return result, rows.Err()
}

func (store *SQLite) ListRuns(ctx context.Context, taskID erkaruntime.TaskID) ([]erkaruntime.Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT id FROM runs WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task %q runs: %w", taskID, err)
	}
	defer rows.Close()
	var ids []erkaruntime.RunID
	for rows.Next() {
		var id erkaruntime.RunID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]erkaruntime.Run, 0, len(ids))
	for _, id := range ids {
		run, err := store.LoadRun(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, nil
}

func (store *SQLite) EnqueueTurn(ctx context.Context, turn Turn) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if turn.ID == "" || turn.TaskID == "" || strings.TrimSpace(turn.Prompt) == "" || turn.CreatedAt.IsZero() {
		return errors.New("turn requires IDs, prompt, and timestamps")
	}
	if turn.Status == "" {
		turn.Status = TurnQueued
	}
	if turn.Status != TurnQueued {
		return errors.New("new turn must be queued")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var next uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM turns WHERE task_id = ?`, turn.TaskID).Scan(&next); err != nil {
		return err
	}
	turn.Position = next
	_, err = tx.ExecContext(ctx, `INSERT INTO turns(id, task_id, position, prompt, status, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?)`, turn.ID, turn.TaskID, turn.Position, turn.Prompt, turn.Status, formatTime(turn.CreatedAt), formatTime(turn.CreatedAt))
	if err != nil {
		return mapInsertError(err, "turn", turn.ID)
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, formatTime(turn.CreatedAt), turn.TaskID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (store *SQLite) ClaimNextTurn(ctx context.Context, projectID erkaruntime.ProjectID, run erkaruntime.Run, now time.Time) (Turn, bool, error) {
	if err := ctx.Err(); err != nil {
		return Turn{}, false, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return Turn{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, `SELECT turns.id, turns.task_id, turns.position, turns.prompt, turns.run_id, turns.status, turns.created_at, turns.updated_at FROM turns JOIN tasks ON tasks.id = turns.task_id WHERE tasks.project_id = ? AND turns.status = 'queued' ORDER BY turns.created_at, turns.position LIMIT 1`, projectID)
	turn, err := scanTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, false, nil
	}
	if err != nil {
		return Turn{}, false, err
	}
	if run.ID == "" {
		return Turn{}, false, errors.New("claimed run requires an ID")
	}
	if run.TaskID != "" && run.TaskID != turn.TaskID {
		return Turn{}, false, errors.New("claimed run identifies a different task")
	}
	run.TaskID = turn.TaskID
	if err := erkaruntime.ValidateRun(run); err != nil {
		return Turn{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runs(id, task_id, stage, backend_class, provider, model, status, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, run.ID, run.TaskID, run.Stage, run.BackendClass, run.Provider, run.Model, run.Status, formatTime(run.CreatedAt), formatTime(run.UpdatedAt))
	if err != nil {
		return Turn{}, false, mapInsertError(err, "run", string(run.ID))
	}
	result, err := tx.ExecContext(ctx, `UPDATE turns SET run_id = ?, status = 'claimed', updated_at = ? WHERE id = ? AND status = 'queued'`, run.ID, formatTime(now), turn.ID)
	if err != nil {
		return Turn{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return Turn{}, false, erkaruntime.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, false, err
	}
	turn.RunID, turn.Status, turn.UpdatedAt = run.ID, TurnClaimed, now.UTC()
	return turn, true, nil
}

func (store *SQLite) UpdateTurnStatus(ctx context.Context, id string, from, to TurnStatus, now time.Time) error {
	result, err := store.db.ExecContext(ctx, `UPDATE turns SET status = ?, updated_at = ? WHERE id = ? AND status = ?`, to, formatTime(now), id, from)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return erkaruntime.ErrConflict
	}
	return nil
}

func (store *SQLite) AcquireLease(ctx context.Context, lease Lease) (bool, error) {
	if lease.ProjectID == "" || lease.Holder == "" || lease.ExpiresAt.IsZero() || lease.UpdatedAt.IsZero() {
		return false, errors.New("lease requires project, holder, expiry, and update time")
	}
	result, err := store.db.ExecContext(ctx, `INSERT INTO project_leases(project_id, holder, expires_at, updated_at) VALUES(?, ?, ?, ?) ON CONFLICT(project_id) DO UPDATE SET holder=excluded.holder, expires_at=excluded.expires_at, updated_at=excluded.updated_at WHERE project_leases.holder = excluded.holder OR project_leases.expires_at <= excluded.updated_at`, lease.ProjectID, lease.Holder, formatTime(lease.ExpiresAt), formatTime(lease.UpdatedAt))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (store *SQLite) RenewLease(ctx context.Context, lease Lease) (bool, error) {
	result, err := store.db.ExecContext(ctx, `UPDATE project_leases SET expires_at = ?, updated_at = ? WHERE project_id = ? AND holder = ? AND expires_at > ?`, formatTime(lease.ExpiresAt), formatTime(lease.UpdatedAt), lease.ProjectID, lease.Holder, formatTime(lease.UpdatedAt))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (store *SQLite) ReleaseLease(ctx context.Context, projectID erkaruntime.ProjectID, holder string) error {
	_, err := store.db.ExecContext(ctx, `DELETE FROM project_leases WHERE project_id = ? AND holder = ?`, projectID, holder)
	return err
}

func scanTurn(row rowScanner) (Turn, error) {
	var turn Turn
	var runID sql.NullString
	var status, created, updated string
	if err := row.Scan(&turn.ID, &turn.TaskID, &turn.Position, &turn.Prompt, &runID, &status, &created, &updated); err != nil {
		return Turn{}, err
	}
	if runID.Valid {
		turn.RunID = erkaruntime.RunID(runID.String)
	}
	turn.Status = TurnStatus(status)
	var err error
	turn.CreatedAt, err = parseTime(created)
	if err != nil {
		return Turn{}, err
	}
	turn.UpdatedAt, err = parseTime(updated)
	if err != nil {
		return Turn{}, err
	}
	return turn, nil
}
