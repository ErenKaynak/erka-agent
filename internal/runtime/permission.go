package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PermissionAction is the outcome of evaluating a tool call against the
// configured permission rules.
type PermissionAction string

const (
	PermissionAllow PermissionAction = "allow"
	PermissionAsk   PermissionAction = "ask"
	PermissionDeny  PermissionAction = "deny"
)

// PermissionDecision is a policy verdict for one tool call.
type PermissionDecision struct {
	Action PermissionAction
	// Rule is the configured rule that matched, if any.
	Rule string
	// Reason is a short human-readable explanation.
	Reason string
}

// PermissionPolicy evaluates tool calls. Implementations must be pure and
// deterministic: the same tool and arguments always yield the same decision.
type PermissionPolicy interface {
	Decide(tool string, arguments json.RawMessage) PermissionDecision
}

// PermissionRequest asks a person to approve one tool call.
type PermissionRequest struct {
	TaskID    TaskID
	RunID     RunID
	CallID    ToolCallID
	Tool      string
	Arguments json.RawMessage
	Rule      string
	Reason    string
	// Display, when set, replaces the default summary (used for plans).
	Display string
}

// Summary renders the request for prompts and approval records. Arguments
// are tool input produced by the model and never contain provider secrets.
func (request PermissionRequest) Summary() string {
	if request.Display != "" {
		return request.Display
	}
	arguments := strings.TrimSpace(string(request.Arguments))
	if len(arguments) > 512 {
		arguments = arguments[:512] + "…"
	}
	return fmt.Sprintf("%s %s", request.Tool, arguments)
}

// PermissionApprover asks a person to decide an "ask" verdict. It returns
// true to allow the call. A nil approver denies every "ask" verdict.
type PermissionApprover interface {
	RequestPermission(context.Context, PermissionRequest) (bool, error)
}

// PermissionApproverFunc adapts a function to PermissionApprover.
type PermissionApproverFunc func(context.Context, PermissionRequest) (bool, error)

func (function PermissionApproverFunc) RequestPermission(ctx context.Context, request PermissionRequest) (bool, error) {
	return function(ctx, request)
}

// PermissionRecord is the durable outcome of a non-trivial permission check.
// It is committed before the tool runs so that resume replays the recorded
// decision instead of asking again.
type PermissionRecord struct {
	CallID ToolCallID       `json:"call_id"`
	Tool   string           `json:"tool"`
	Action PermissionAction `json:"action"`
	// DecidedBy is "rule" for configured rules, "user" for an interactive
	// decision, or "policy" when no approver was available.
	DecidedBy string `json:"decided_by"`
	Rule      string `json:"rule,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (record PermissionRecord) Validate() error {
	if record.CallID == "" || strings.TrimSpace(record.Tool) == "" {
		return fmt.Errorf("permission record requires a call ID and tool")
	}
	switch record.Action {
	case PermissionAllow, PermissionDeny:
	default:
		return fmt.Errorf("permission record has unsupported action %q", record.Action)
	}
	return nil
}

// HookOutcome is the result of one hook command run after a tool call.
type HookOutcome struct {
	Name   string
	Path   string
	Output string
	Failed bool
}

// ToolHook runs after every successful tool call (for example post-edit
// formatters). Hooks are user-configured and their outcomes are appended to
// the tool result and recorded as hook events.
type ToolHook interface {
	AfterTool(context.Context, ToolCall, ToolExecutionResult) []HookOutcome
}
