package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRuntimeItemValidate(t *testing.T) {
	t.Parallel()

	item := messageItem(1, "item-1")
	if err := item.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	tests := []struct {
		name string
		edit func(*RuntimeItem)
		want string
	}{
		{name: "missing ID", edit: func(item *RuntimeItem) { item.ID = "" }, want: "requires item"},
		{name: "zero sequence", edit: func(item *RuntimeItem) { item.Sequence = 0 }, want: "sequence"},
		{name: "zero time", edit: func(item *RuntimeItem) { item.Time = time.Time{} }, want: "time"},
		{name: "multiple payloads", edit: func(item *RuntimeItem) { item.Usage = &Usage{} }, want: "exactly one"},
		{name: "wrong payload", edit: func(item *RuntimeItem) { item.Kind = ItemUsage }, want: "usage payload"},
		{name: "unknown kind", edit: func(item *RuntimeItem) { item.Kind = "future" }, want: "unknown"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := item
			test.edit(&candidate)
			err := candidate.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestToolCallValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call ToolCall
		want string
	}{
		{
			name: "valid read-only",
			call: ToolCall{CallID: "call-1", Tool: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)},
		},
		{
			name: "malformed arguments",
			call: ToolCall{CallID: "call-1", Tool: "read_file", Arguments: json.RawMessage(`{"path":`)},
			want: "valid JSON object",
		},
		{
			name: "scalar arguments",
			call: ToolCall{CallID: "call-1", Tool: "read_file", Arguments: json.RawMessage(`"x"`)},
			want: "valid JSON object",
		},
		{
			name: "mutation without idempotency",
			call: ToolCall{CallID: "call-1", Tool: "write_file", Arguments: json.RawMessage(`{}`), Mutating: true},
			want: "idempotency key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.call.Validate()
			if test.want == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestEventValidate(t *testing.T) {
	t.Parallel()

	event := Event{
		ID: "event-1", Sequence: 1, TaskID: "task-1", RunID: "run-1",
		Type: "tool.completed", Time: time.Now(), Data: json.RawMessage(`{"tool":"test"}`),
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	event.Type = "Tool Completed"
	if err := event.Validate(); err == nil {
		t.Fatal("Validate() error = nil for invalid event type")
	}
}

func messageItem(sequence uint64, id ItemID) RuntimeItem {
	return RuntimeItem{
		ID: id, Sequence: sequence, TaskID: "task-1", RunID: "run-1",
		Kind: ItemUserMessage, Time: time.Now(), Message: &Message{Content: "test"},
	}
}
