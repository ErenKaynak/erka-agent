package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type (
	TaskID     string
	RunID      string
	ItemID     string
	EventID    string
	ToolCallID string
)

// ItemKind identifies a persisted provider-neutral runtime item.
type ItemKind string

const (
	ItemUserMessage      ItemKind = "user_message"
	ItemAssistantMessage ItemKind = "assistant_message"
	ItemReasoningSummary ItemKind = "reasoning_summary"
	ItemToolCall         ItemKind = "tool_call"
	ItemToolResult       ItemKind = "tool_result"
	ItemUsage            ItemKind = "usage"
	ItemCheckpoint       ItemKind = "checkpoint"
	ItemError            ItemKind = "error"
)

// RuntimeItem is one ordered, persisted element of a task transcript. Exactly
// one payload must be populated and must match Kind.
type RuntimeItem struct {
	ID       ItemID
	Sequence uint64
	TaskID   TaskID
	RunID    RunID
	Kind     ItemKind
	Time     time.Time

	Message    *Message
	ToolCall   *ToolCall
	ToolResult *ToolResult
	Usage      *Usage
	Checkpoint *Checkpoint
	Error      *RuntimeError
}

type Message struct {
	Content string
}

type ToolCall struct {
	CallID         ToolCallID
	Tool           string
	Arguments      json.RawMessage
	Mutating       bool
	IdempotencyKey string
}

type ToolResult struct {
	CallID    ToolCallID
	Output    string
	IsError   bool
	Truncated bool
}

type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostMicrounits   int64
	Currency         string
}

type Checkpoint struct {
	Objective           string   `json:"objective"`
	Constraints         []string `json:"constraints"`
	ApprovedPlan        string   `json:"approved_plan"`
	Decisions           []string `json:"decisions"`
	RelevantFiles       []string `json:"relevant_files"`
	DiffSummary         string   `json:"diff_summary"`
	ValidationResults   []string `json:"validation_results"`
	UnresolvedProblems  []string `json:"unresolved_problems"`
	SuggestedNextAction string   `json:"suggested_next_action"`
}

type RuntimeError struct {
	Code      string
	Message   string
	Retryable bool
}

func (item RuntimeItem) Validate() error {
	if item.ID == "" || item.TaskID == "" || item.RunID == "" {
		return errors.New("runtime item requires item, task, and run IDs")
	}
	if item.Sequence == 0 {
		return errors.New("runtime item sequence must be positive")
	}
	if item.Time.IsZero() {
		return errors.New("runtime item time is required")
	}

	payloads := 0
	for _, present := range []bool{
		item.Message != nil,
		item.ToolCall != nil,
		item.ToolResult != nil,
		item.Usage != nil,
		item.Checkpoint != nil,
		item.Error != nil,
	} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return fmt.Errorf("runtime item must have exactly one payload, got %d", payloads)
	}

	switch item.Kind {
	case ItemUserMessage, ItemAssistantMessage, ItemReasoningSummary:
		if item.Message == nil || strings.TrimSpace(item.Message.Content) == "" {
			return errors.New("message item requires non-empty content")
		}
	case ItemToolCall:
		if item.ToolCall == nil {
			return errors.New("tool_call item requires a tool-call payload")
		}
		if err := item.ToolCall.Validate(); err != nil {
			return err
		}
	case ItemToolResult:
		if item.ToolResult == nil || item.ToolResult.CallID == "" {
			return errors.New("tool_result item requires a call ID")
		}
	case ItemUsage:
		if item.Usage == nil {
			return errors.New("usage item requires a usage payload")
		}
		if err := item.Usage.Validate(); err != nil {
			return err
		}
	case ItemCheckpoint:
		if item.Checkpoint == nil || strings.TrimSpace(item.Checkpoint.Objective) == "" {
			return errors.New("checkpoint item requires an objective")
		}
	case ItemError:
		if item.Error == nil || item.Error.Code == "" || item.Error.Message == "" {
			return errors.New("error item requires a code and message")
		}
	default:
		return fmt.Errorf("unknown runtime item kind %q", item.Kind)
	}

	return nil
}

// ValidateRuntimeItem exposes canonical item validation to store and provider
// implementations.
func ValidateRuntimeItem(item RuntimeItem) error { return item.Validate() }

func (call ToolCall) Validate() error {
	if call.CallID == "" || call.Tool == "" {
		return errors.New("tool call requires a call ID and tool name")
	}
	if !isJSONObject(call.Arguments) {
		return errors.New("tool-call arguments must be one valid JSON object")
	}
	if call.Mutating && call.IdempotencyKey == "" {
		return errors.New("mutating tool call requires an idempotency key")
	}
	return nil
}

func (usage Usage) Validate() error {
	values := []int64{
		usage.InputTokens,
		usage.OutputTokens,
		usage.CacheReadTokens,
		usage.CacheWriteTokens,
		usage.CostMicrounits,
	}
	for _, value := range values {
		if value < 0 {
			return errors.New("usage values must not be negative")
		}
	}
	if usage.CostMicrounits > 0 && usage.Currency == "" {
		return errors.New("usage with cost requires a currency")
	}
	return nil
}

// Event is an append-only user-facing state transition. Data is a bounded,
// provider-neutral JSON object rather than an unrestricted provider payload.
type Event struct {
	ID       EventID
	Sequence uint64
	TaskID   TaskID
	RunID    RunID
	Type     string
	Time     time.Time
	Message  string
	Data     json.RawMessage
}

func (event Event) Validate() error {
	if event.ID == "" || event.TaskID == "" {
		return errors.New("event requires event and task IDs")
	}
	if event.Sequence == 0 {
		return errors.New("event sequence must be positive")
	}
	if event.Time.IsZero() {
		return errors.New("event time is required")
	}
	if !validEventType(event.Type) {
		return fmt.Errorf("invalid event type %q", event.Type)
	}
	if len(event.Data) > 0 && !isJSONObject(event.Data) {
		return errors.New("event data must be one valid JSON object")
	}
	return nil
}

func isJSONObject(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil {
		return false
	}
	var extra any
	return errors.Is(decoder.Decode(&extra), io.EOF)
}

func validEventType(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for index, character := range part {
			if character >= 'a' && character <= 'z' {
				continue
			}
			if index > 0 && ((character >= '0' && character <= '9') || character == '_') {
				continue
			}
			return false
		}
	}
	return true
}
