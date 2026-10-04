package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

type approvalFixture struct {
	mu        sync.Mutex
	approvals map[string]store.Approval
	decideTo  store.ApprovalStatus
	loads     int
}

func (fixture *approvalFixture) CreateApproval(_ context.Context, approval store.Approval) error {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.approvals[approval.ID] = approval
	return nil
}

func (fixture *approvalFixture) LoadApproval(_ context.Context, id string) (store.Approval, error) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.loads++
	approval, ok := fixture.approvals[id]
	if !ok {
		return store.Approval{}, errors.New("missing")
	}
	if fixture.loads >= 2 && fixture.decideTo != "" {
		approval.Status = fixture.decideTo
	}
	return approval, nil
}

func TestPermissionApproverWaitsForRemoteDecision(t *testing.T) {
	t.Parallel()
	request := erkaruntime.PermissionRequest{TaskID: "task-1", CallID: "call-1", Tool: "run_command", Arguments: []byte(`{"command":["make","deploy"]}`), Reason: "not allow-listed"}
	for status, want := range map[store.ApprovalStatus]bool{store.ApprovalApproved: true, store.ApprovalRejected: false, store.ApprovalExpired: false} {
		fixture := &approvalFixture{approvals: map[string]store.Approval{}, decideTo: status}
		approver := NewPermissionApprover(fixture, PermissionApproverOptions{Poll: time.Millisecond})
		allowed, err := approver.RequestPermission(context.Background(), request)
		if err != nil || allowed != want {
			t.Fatalf("%s: allowed = %v, %v", status, allowed, err)
		}
		for _, approval := range fixture.approvals {
			if approval.Kind != PermissionApprovalKind || approval.TaskID != "task-1" || !strings.Contains(approval.Message, "make") || !strings.Contains(approval.Message, "not allow-listed") {
				t.Fatalf("approval = %#v", approval)
			}
		}
	}
}

func TestPermissionApproverDeniesOnExpiryAndHonorsCancellation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(30 * time.Minute)
		return now
	}
	approver := NewPermissionApprover(&approvalFixture{approvals: map[string]store.Approval{}}, PermissionApproverOptions{Poll: time.Millisecond, Clock: clock, Expiry: time.Hour})
	allowed, err := approver.RequestPermission(context.Background(), erkaruntime.PermissionRequest{TaskID: "t", CallID: "c", Tool: "x"})
	if err != nil || allowed {
		t.Fatalf("expired request = %v, %v", allowed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pending := NewPermissionApprover(&approvalFixture{approvals: map[string]store.Approval{}}, PermissionApproverOptions{Poll: time.Millisecond})
	if _, err := pending.RequestPermission(ctx, erkaruntime.PermissionRequest{TaskID: "t", CallID: "c", Tool: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request error = %v", err)
	}
}
