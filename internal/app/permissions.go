package app

import (
	"context"
	"errors"
	"sync"
	"time"

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
			approver.observer.Observe(ctx, erkaruntime.LiveUpdate{TaskID: request.TaskID, RunID: request.RunID, Kind: LivePermissionRequested, Tool: request.Tool, CallID: request.CallID, Text: request.Summary()})
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

// DecidePermission answers the pending permission request for callID.
func (session *Session) DecidePermission(callID erkaruntime.ToolCallID, allowed bool) error {
	return session.interactive.decide(callID, allowed)
}
