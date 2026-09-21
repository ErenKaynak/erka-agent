package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrAwaitingUser reports that a stage parked durably until a person answers
// a question or decides a permission request. It is not a failure.
var ErrAwaitingUser = errors.New("task is waiting for a user response")

// AskUserToolName is the built-in tool native backends expose when an
// Interactor is configured.
const AskUserToolName = "ask_user"

type UserRequestKind string

const (
	UserQuestion   UserRequestKind = "question"
	UserPermission UserRequestKind = "permission"
)

// UserRequest is one question or permission request raised by a tool call.
// Key is stable for the task and call so repeated requests after a restart or
// resume resolve to the same durable record.
type UserRequest struct {
	Key     string
	TaskID  TaskID
	RunID   RunID
	CallID  ToolCallID
	Kind    UserRequestKind
	Tool    string
	Message string
	Options []string
	Details json.RawMessage
}

func (request UserRequest) Validate() error {
	if request.Key == "" || request.TaskID == "" || request.CallID == "" {
		return errors.New("user request requires key, task, and call IDs")
	}
	if request.Kind != UserQuestion && request.Kind != UserPermission {
		return fmt.Errorf("unknown user request kind %q", request.Kind)
	}
	if strings.TrimSpace(request.Message) == "" {
		return errors.New("user request requires a message")
	}
	return nil
}

type UserResponse struct {
	Approved bool
	Answer   string
}

// Interactor delivers questions and permission requests to a person.
// Request must be idempotent by key: the first call durably records the
// request and returns ErrAwaitingUser, later calls keep returning
// ErrAwaitingUser until a person responds, then return that response.
type Interactor interface {
	Request(context.Context, UserRequest) (UserResponse, error)
}

// UserRequestKey derives the stable durable key for one tool call's request.
func UserRequestKey(taskID TaskID, callID ToolCallID, kind UserRequestKind) string {
	sum := sha256.Sum256([]byte(string(taskID) + "\x00" + string(callID) + "\x00" + string(kind)))
	return hex.EncodeToString(sum[:16])
}

type askUserTool struct{}

func (askUserTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:         AskUserToolName,
		Description:  "Ask the user a question and pause until they answer. Use only when a decision is genuinely theirs and cannot be resolved from the repository or the request.",
		InputSchema:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["question"],"properties":{"question":{"type":"string","minLength":1,"maxLength":4000},"options":{"type":"array","maxItems":8,"items":{"type":"string","minLength":1,"maxLength":200}}}}`),
		ParallelSafe: false, OutputLimitBytes: 64 << 10,
	}
}

func (askUserTool) Execute(context.Context, ToolInvocation) (ToolExecutionResult, error) {
	return ToolExecutionResult{}, errors.New("ask_user is handled by the native runtime")
}

func (backend *preparedBackend) requiresApproval(tool string) bool {
	if backend.Interactor == nil {
		return false
	}
	for _, name := range backend.ApprovalTools {
		if name == tool {
			return true
		}
	}
	return false
}

func (backend *preparedBackend) askUser(ctx context.Context, request StageRequest, call ToolCall) (ToolExecutionResult, error) {
	var arguments struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil || strings.TrimSpace(arguments.Question) == "" {
		return ToolExecutionResult{Output: "ask_user requires a non-empty question", IsError: true}, nil
	}
	response, err := backend.Interactor.Request(ctx, UserRequest{
		Key: UserRequestKey(request.TaskID, call.CallID, UserQuestion), TaskID: request.TaskID, RunID: request.RunID,
		CallID: call.CallID, Kind: UserQuestion, Tool: AskUserToolName, Message: strings.TrimSpace(arguments.Question),
		Options: arguments.Options,
	})
	if err != nil {
		return ToolExecutionResult{}, err
	}
	answer := strings.TrimSpace(response.Answer)
	if answer == "" {
		return ToolExecutionResult{Output: "The user did not answer. Proceed with your best judgment and state the assumption you made."}, nil
	}
	return ToolExecutionResult{Output: "User answer: " + answer}, nil
}

// park moves the task to waiting_approval and records which pending calls have
// not started, so a later resume can safely run them after the user responds.
func (backend *preparedBackend) park(ctx context.Context, appender *durableAppender, remaining []ToolCall) error {
	if len(remaining) == 0 {
		return errors.New("cannot park a stage without a pending tool call")
	}
	unstarted := make([]ToolCallID, len(remaining))
	for index, call := range remaining {
		unstarted[index] = call.CallID
	}
	first := remaining[0]
	return appender.transition(ctx, TaskWaitingApproval, []pendingEvent{{
		Type: "task.awaiting_user", Message: fmt.Sprintf("%s is waiting for the user", first.Tool),
		Data: eventData(map[string]any{"tool": first.Tool, "call_id": first.CallID, "unstarted_call_ids": unstarted}),
	}})
}

// unstartedCalls replays durable events to find calls that were parked before
// execution and have not begun executing since.
func unstartedCalls(ctx context.Context, appender *durableAppender) (map[ToolCallID]bool, error) {
	events, err := appender.store.LoadEvents(ctx, appender.taskID, 0)
	if err != nil {
		return nil, err
	}
	result := make(map[ToolCallID]bool)
	for _, event := range events {
		switch event.Type {
		case "task.awaiting_user":
			var data struct {
				Unstarted []ToolCallID `json:"unstarted_call_ids"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				for _, id := range data.Unstarted {
					result[id] = true
				}
			}
		case "tool.executing":
			var data struct {
				CallID ToolCallID `json:"call_id"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				delete(result, data.CallID)
			}
		}
	}
	return result, nil
}

const maxPermissionMessageBytes = 1000

func permissionMessage(call ToolCall) string {
	var arguments struct {
		Command   []string `json:"command"`
		Directory string   `json:"directory"`
	}
	message := ""
	if json.Unmarshal(call.Arguments, &arguments) == nil && len(arguments.Command) > 0 {
		message = "Run command: " + strings.Join(arguments.Command, " ")
		if arguments.Directory != "" && arguments.Directory != "." {
			message += " (in " + arguments.Directory + ")"
		}
	} else {
		message = fmt.Sprintf("Allow %s with arguments %s", call.Tool, string(call.Arguments))
	}
	if len(message) > maxPermissionMessageBytes {
		cut := message[:maxPermissionMessageBytes]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		message = cut + "…"
	}
	return message
}
