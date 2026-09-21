package store

import (
	"context"
	"errors"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

// Finalization is a reviewed task whose worktree still needs to be turned
// into a branch or discarded.
type Finalization struct {
	Task     ServerTask
	Decision ApprovalStatus
}

// PendingFinalizations lists completed, unfinalized tasks whose latest review
// was decided and has no newer pending review. Finalization is retried from
// here after a crash, so it must be idempotent.
func (store *SQLite) PendingFinalizations(ctx context.Context, limit int) ([]Finalization, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT `+prefixedServerTaskColumns+`, (
			SELECT a.status FROM approvals a WHERE a.task_id = t.task_id AND a.kind = 'accept_changes' AND a.status IN ('approved', 'rejected')
			ORDER BY a.decided_at DESC, a.id DESC LIMIT 1)
		FROM server_tasks t
		WHERE t.status = 'completed' AND t.finalized_at IS NULL
		  AND EXISTS (SELECT 1 FROM approvals a WHERE a.task_id = t.task_id AND a.kind = 'accept_changes' AND a.status IN ('approved', 'rejected'))
		  AND NOT EXISTS (SELECT 1 FROM approvals a WHERE a.task_id = t.task_id AND a.kind = 'accept_changes' AND a.status = 'pending')
		ORDER BY t.updated_at, t.task_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Finalization, 0)
	for rows.Next() {
		var decision string
		task, err := scanServerTaskWith(rows, "", &decision)
		if err != nil {
			return nil, err
		}
		result = append(result, Finalization{Task: task, Decision: ApprovalStatus(decision)})
	}
	return result, rows.Err()
}

// FinalizeServerTask records a review outcome exactly once.
func (store *SQLite) FinalizeServerTask(ctx context.Context, taskID erkaruntime.TaskID, outcome TaskOutcome, ref, base, commit string, now time.Time) error {
	if outcome != TaskAccepted && outcome != TaskRejected {
		return errors.New("task outcome must be accepted or rejected")
	}
	nullable := func(value string) any {
		if value == "" {
			return nil
		}
		return value
	}
	result, err := store.db.ExecContext(ctx, `UPDATE server_tasks SET outcome = ?, result_ref = ?, result_base = ?, result_commit = ?, finalized_at = ?, updated_at = ? WHERE task_id = ? AND finalized_at IS NULL`, outcome, nullable(ref), nullable(base), nullable(commit), formatTime(now), formatTime(now), taskID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return erkaruntime.ErrConflict
	}
	return nil
}

// SupersedeReviews expires pending change reviews when a follow-up reopens
// the task, so a stale review cannot accept a worktree that is still moving.
func (store *SQLite) SupersedeReviews(ctx context.Context, taskID erkaruntime.TaskID, now time.Time) error {
	_, err := store.db.ExecContext(ctx, `UPDATE approvals SET status = 'expired', decided_at = ? WHERE task_id = ? AND kind = 'accept_changes' AND status = 'pending'`, formatTime(now), taskID)
	return err
}

const prefixedServerTaskColumns = `t.task_id, t.server_project_id, t.worktree_root, t.status, t.claim_holder, t.claim_expires_at, t.created_at, t.updated_at, t.outcome, t.result_ref, t.result_base, t.result_commit, t.finalized_at`
