package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/routing"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

// PermissionMode selects who answers "ask" permission verdicts when no
// explicit Approver is supplied.
type PermissionMode string

const (
	// PermissionModeDeny refuses every "ask" verdict (headless default).
	PermissionModeDeny PermissionMode = "deny"
	// PermissionModeAutoApprove allows every "ask" verdict (`erka run --yes`).
	PermissionModeAutoApprove PermissionMode = "auto_approve"
	// PermissionModeInteractive asks the attached UI through
	// PendingPermission and DecidePermission.
	PermissionModeInteractive PermissionMode = "interactive"
)

// LivePermissionRequested tells an attached UI that a tool call is waiting
// for approval. It is re-sent periodically until decided, because live
// updates are best-effort.
const LivePermissionRequested erkaruntime.LiveUpdateKind = "permission_requested"

// LivePlanRequested tells an attached UI that a pipeline plan is waiting for
// approval; Text carries the plan.
const LivePlanRequested erkaruntime.LiveUpdateKind = "plan_requested"

const permissionReminderInterval = time.Second

var ErrNoPendingPermission = errors.New("no permission request is pending for that call")

type pendingPermission struct {
	request  erkaruntime.PermissionRequest
	decision chan bool
}

// interactiveApprover parks the runtime until the UI decides.
type interactiveApprover struct {
	mu       sync.Mutex
	pending  *pendingPermission
	observer erkaruntime.LiveObserver
	kind     erkaruntime.LiveUpdateKind
}

func (approver *interactiveApprover) RequestPermission(ctx context.Context, request erkaruntime.PermissionRequest) (bool, error) {
	pending := &pendingPermission{request: request, decision: make(chan bool, 1)}
	approver.mu.Lock()
	approver.pending = pending
	approver.mu.Unlock()
	defer func() {
		approver.mu.Lock()
		if approver.pending == pending {
			approver.pending = nil
		}
		approver.mu.Unlock()
	}()
	notify := func() {
		if approver.observer != nil {
			kind := approver.kind
			if kind == "" {
				kind = LivePermissionRequested
			}
			approver.observer.Observe(ctx, erkaruntime.LiveUpdate{TaskID: request.TaskID, RunID: request.RunID, Kind: kind, Tool: request.Tool, CallID: request.CallID, Text: request.Summary()})
		}
	}
	notify()
	ticker := time.NewTicker(permissionReminderInterval)
	defer ticker.Stop()
	for {
		select {
		case allowed := <-pending.decision:
			return allowed, nil
		case <-ticker.C:
			notify()
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func (approver *interactiveApprover) current() (erkaruntime.PermissionRequest, bool) {
	approver.mu.Lock()
	defer approver.mu.Unlock()
	if approver.pending == nil {
		return erkaruntime.PermissionRequest{}, false
	}
	return approver.pending.request, true
}

func (approver *interactiveApprover) decide(callID erkaruntime.ToolCallID, allowed bool) error {
	approver.mu.Lock()
	defer approver.mu.Unlock()
	if approver.pending == nil || approver.pending.request.CallID != callID {
		return ErrNoPendingPermission
	}
	select {
	case approver.pending.decision <- allowed:
	default:
	}
	return nil
}

// approver returns the runtime approver for this session's permission mode.
func (session *Session) approver() erkaruntime.PermissionApprover {
	if session.options.Approver != nil {
		return session.options.Approver
	}
	switch session.options.PermissionMode {
	case PermissionModeAutoApprove:
		return erkaruntime.PermissionApproverFunc(func(context.Context, erkaruntime.PermissionRequest) (bool, error) { return true, nil })
	case PermissionModeInteractive:
		return session.interactive
	default:
		return nil
	}
}

// PendingPermission returns the tool call currently waiting for approval.
func (session *Session) PendingPermission() (erkaruntime.PermissionRequest, bool) {
	return session.interactive.current()
}

// DecidePermission answers the pending permission or plan request for
// callID.
func (session *Session) DecidePermission(callID erkaruntime.ToolCallID, allowed bool) error {
	if err := session.interactive.decide(callID, allowed); !errors.Is(err, ErrNoPendingPermission) {
		return err
	}
	return session.interactivePlan.decide(callID, allowed)
}

const planInstructions = `You are erka in plan mode. Investigate the repository with read-only tools and produce a concrete implementation plan: the goal, the files to change and why, ordered steps, risks, and how to validate the change. Mutating tools are unavailable; do not claim to have changed anything. Finish with the plan.`

// SetPlanMode switches subsequent turns between planning (read-only tools,
// plan instructions, or a plan approval gate in pipelines) and normal work.
// Like --full it lasts for this process only.
func (session *Session) SetPlanMode(enabled bool) {
	session.mu.Lock()
	session.planMode = enabled
	session.mu.Unlock()
}

// PlanMode reports whether plan mode is active.
func (session *Session) PlanMode() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.planMode
}

// planApprover returns the pipeline plan gate for this session's mode.
func (session *Session) planApprover() routing.PlanApprover {
	var approver erkaruntime.PermissionApprover
	switch session.options.PermissionMode {
	case PermissionModeAutoApprove:
		return func(context.Context, routing.PlanApproval) (bool, error) { return true, nil }
	case PermissionModeInteractive:
		approver = session.interactivePlan
	default:
		if session.options.Approver == nil {
			return func(_ context.Context, approval routing.PlanApproval) (bool, error) {
				if session.options.Output != nil {
					_, _ = fmt.Fprintf(session.options.Output, "Proposed plan:\n%s\n\nThe plan was not approved; rerun with --yes to continue automatically.\n", approval.Plan)
				}
				return false, nil
			}
		}
		approver = session.options.Approver
	}
	return func(ctx context.Context, approval routing.PlanApproval) (bool, error) {
		return approver.RequestPermission(ctx, erkaruntime.PermissionRequest{
			TaskID: approval.TaskID, RunID: approval.RunID, CallID: erkaruntime.ToolCallID("plan:" + string(approval.RunID)),
			Tool: "plan", Reason: "plan mode requires approval", Display: approval.Plan,
		})
	}
}
