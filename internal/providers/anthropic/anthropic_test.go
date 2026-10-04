package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func sseResponse(events ...string) *http.Response {
	var body strings.Builder
	for _, event := range events {
		body.WriteString("event: x\ndata: " + event + "\n\n")
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body.String()))}
}

var fixtureTime = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func runtimeItem(sequence uint64, kind erkaruntime.ItemKind) erkaruntime.RuntimeItem {
	return erkaruntime.RuntimeItem{ID: erkaruntime.ItemID(strings.Repeat("i", int(sequence))), Sequence: sequence, TaskID: "task", RunID: "run", Kind: kind, Time: fixtureTime}
}

func userItem(sequence uint64, text string) erkaruntime.RuntimeItem {
	item := runtimeItem(sequence, erkaruntime.ItemUserMessage)
	item.Message = &erkaruntime.Message{Content: text}
	return item
}

func toolCallItem(sequence uint64, id, arguments string) erkaruntime.RuntimeItem {
	item := runtimeItem(sequence, erkaruntime.ItemToolCall)
	item.ToolCall = &erkaruntime.ToolCall{CallID: erkaruntime.ToolCallID(id), Tool: "read_file", Arguments: json.RawMessage(arguments)}
	return item
}

func toolResultItem(sequence uint64, id, output string) erkaruntime.RuntimeItem {
	item := runtimeItem(sequence, erkaruntime.ItemToolResult)
	item.ToolResult = &erkaruntime.ToolResult{CallID: erkaruntime.ToolCallID(id), Output: output}
	return item
}

func tools() []erkaruntime.ToolDefinition {
	return []erkaruntime.ToolDefinition{
		{Name: "read_file", Description: "Read a file", InputSchema: json.RawMessage("{\n  \"type\": \"object\",\n  \"properties\": {\"path\": {\"type\": \"string\"}}\n}"), OutputLimitBytes: 1024},
		{Name: "search", Description: "Search files", InputSchema: json.RawMessage(`{"type":"object"}`), OutputLimitBytes: 1024},
	}
}

func newTestProvider(t *testing.T, transport roundTripFunc) *Provider {
	t.Helper()
	provider, err := New(Options{APIKey: "sk-ant-secret", BaseURL: "https://anthropic.example", Model: "claude-test", Client: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func drain(t *testing.T, stream erkaruntime.ModelStream) []erkaruntime.ModelOutput {
	t.Helper()
	var outputs []erkaruntime.ModelOutput
	for {
		output, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return outputs
		}
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, output)
	}
}

func TestRequestPlacesCacheBreakpointsAndKeepsPrefixStable(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, nil)
	first := erkaruntime.ModelRequest{Instructions: "You are erka.", MaxOutputTokens: 2048, Tools: tools(), Items: []erkaruntime.RuntimeItem{userItem(1, "fix the bug")}}
	second := first
	second.Items = append(append([]erkaruntime.RuntimeItem(nil), first.Items...),
		toolCallItem(2, "toolu_1", `{"path": "a.go"}`),
		toolResultItem(3, "toolu_1", "package a"),
	)
	firstBody, err := provider.requestBody(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := provider.requestBody(second)
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Tools []struct {
			Name         string          `json:"name"`
			InputSchema  json.RawMessage `json:"input_schema"`
			CacheControl *cacheControl   `json:"cache_control"`
		} `json:"tools"`
		System []struct {
			CacheControl *cacheControl `json:"cache_control"`
		} `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type         string          `json:"type"`
				Input        json.RawMessage `json:"input"`
				ToolUseID    string          `json:"tool_use_id"`
				CacheControl *cacheControl   `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(secondBody, &decoded); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(secondBody, []byte(`"cache_control"`)) != 3 {
		t.Fatalf("want exactly three cache breakpoints: %s", secondBody)
	}
	if decoded.Tools[0].CacheControl != nil || decoded.Tools[1].CacheControl == nil {
		t.Fatalf("tool breakpoint must be on the last tool only: %s", secondBody)
	}
	if string(decoded.Tools[0].InputSchema) != `{"type":"object","properties":{"path":{"type":"string"}}}` {
		t.Fatalf("schema not compacted deterministically: %s", decoded.Tools[0].InputSchema)
	}
	if decoded.System[0].CacheControl == nil {
		t.Fatal("system prompt has no cache breakpoint")
	}
	if len(decoded.Messages) != 3 || decoded.Messages[1].Role != "assistant" || decoded.Messages[2].Role != "user" {
		t.Fatalf("messages = %s", secondBody)
	}
	if string(decoded.Messages[1].Content[0].Input) != `{"path":"a.go"}` {
		t.Fatalf("tool_use input = %s", decoded.Messages[1].Content[0].Input)
	}
	last := decoded.Messages[2].Content[0]
	if last.ToolUseID != "toolu_1" || last.CacheControl == nil {
		t.Fatalf("conversation breakpoint must be on the final block: %#v", last)
	}
	if decoded.Messages[0].Content[0].CacheControl != nil {
		t.Fatal("an earlier message kept a breakpoint")
	}

	// Everything up to and including the system prompt must be byte-identical
	// across iterations so the cached prefix is reused.
	prefix := func(body []byte) []byte {
		index := bytes.Index(body, []byte(`"messages":`))
		if index < 0 {
			t.Fatalf("no messages in %s", body)
		}
		return body[:index]
	}
	if !bytes.Equal(prefix(firstBody), prefix(secondBody)) {
		t.Fatalf("cached prefix changed between iterations:\n%s\n%s", prefix(firstBody), prefix(secondBody))
	}
	if !bytes.Contains(prefix(firstBody), []byte(`"tools":`)) || bytes.Index(firstBody, []byte(`"tools":`)) > bytes.Index(firstBody, []byte(`"system":`)) {
		t.Fatalf("tools must precede system: %s", firstBody)
	}
}

func TestBuildMessagesAlternatesRolesAndMergesTurns(t *testing.T) {
	t.Parallel()
	assistant := runtimeItem(2, erkaruntime.ItemAssistantMessage)
	assistant.Message = &erkaruntime.Message{Content: "Looking."}
	checkpoint := runtimeItem(7, erkaruntime.ItemCheckpoint)
	checkpoint.Checkpoint = &erkaruntime.Checkpoint{Objective: "fix", ApprovedPlan: "plan"}
	messages, err := buildMessages([]erkaruntime.RuntimeItem{
		checkpoint, userItem(1, "go"), assistant,
		toolCallItem(3, "a", `{"path":"a"}`), toolCallItem(4, "b", ``),
		toolResultItem(5, "a", "A"), toolResultItem(6, "b", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	roles := []string{"user", "assistant", "user"}
	for index, message := range messages {
		if message.Role != roles[index] {
			t.Fatalf("message %d role = %s", index, message.Role)
		}
	}
	if len(messages[0].Content) != 2 || len(messages[1].Content) != 3 || len(messages[2].Content) != 2 {
		t.Fatalf("merged content = %#v", messages)
	}
	if string(messages[1].Content[2].Input) != `{}` || messages[2].Content[1].Content != "(no output)" {
		t.Fatalf("empty tool input/output handling = %#v", messages)
	}
	onlyAssistant, _ := buildMessages([]erkaruntime.RuntimeItem{assistant})
	if onlyAssistant[0].Role != "user" {
		t.Fatalf("conversation must start with a user turn: %#v", onlyAssistant)
	}
}

func TestStreamTranslatesTextToolsAndCacheUsage(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://anthropic.example/v1/messages" {
			t.Fatalf("url = %s", request.URL)
		}
		if request.Header.Get("x-api-key") != "sk-ant-secret" || request.Header.Get("anthropic-version") != APIVersion {
			t.Fatalf("headers = %#v", request.Header)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatal("Anthropic requests must not send a bearer token")
		}
		body, _ := io.ReadAll(request.Body)
		if bytes.Contains(body, []byte("sk-ant-secret")) {
			t.Fatal("API key leaked into the request body")
		}
		return sseResponse(
			`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":2000,"output_tokens":1}}}`,
			`{"type":"ping"}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Reading "}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_2","name":"search","input":{}}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
			`{"type":"message_stop"}`,
		), nil
	})
	provider := newTestProvider(t, transport)
	stream, err := provider.Stream(context.Background(), erkaruntime.ModelRequest{Instructions: "x", MaxOutputTokens: 100, Tools: tools(), Items: []erkaruntime.RuntimeItem{userItem(1, "go")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	outputs := drain(t, stream)
	if len(outputs) != 8 {
		t.Fatalf("outputs = %#v", outputs)
	}
	if outputs[0].Text != "Reading " || outputs[1].ToolCall.Tool != "read_file" || outputs[2].ToolCall.ArgumentsFragment != `{"path":` || outputs[3].ToolCall.ArgumentsFragment != `"a.go"}` {
		t.Fatalf("text/tool outputs = %#v", outputs[:4])
	}
	if outputs[4].ToolCall.CallID != "toolu_2" || outputs[5].ToolCall.ArgumentsFragment != "{}" {
		t.Fatalf("argument-less tool = %#v %#v", outputs[4], outputs[5])
	}
	usage := outputs[6].Usage
	if outputs[6].Kind != erkaruntime.ModelUsage || usage.InputTokens != 2112 || usage.CacheReadTokens != 2000 || usage.CacheWriteTokens != 100 || usage.OutputTokens != 42 {
		t.Fatalf("usage = %#v", usage)
	}
	if outputs[7].Kind != erkaruntime.ModelCompleted {
		t.Fatalf("final = %#v", outputs[7])
	}
}

func TestStreamFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		response  *http.Response
		openErr   string
		retryable bool
		failCode  string
	}{
		{name: "overloaded http", response: &http.Response{StatusCode: 529, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))}, openErr: "http_529", retryable: true},
		{name: "bad request", response: &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))}, openErr: "http_400"},
		{name: "stream error", response: sseResponse(`{"type":"message_start","message":{"usage":{}}}`, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`), failCode: "overloaded_error", retryable: true},
		{name: "max tokens", response: sseResponse(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`, `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":5}}`, `{"type":"message_stop"}`), failCode: "incomplete"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			provider := newTestProvider(t, func(*http.Request) (*http.Response, error) { return testCase.response, nil })
			stream, err := provider.Stream(context.Background(), erkaruntime.ModelRequest{Instructions: "x", MaxOutputTokens: 10, Items: []erkaruntime.RuntimeItem{userItem(1, "go")}})
			if testCase.openErr != "" {
				var providerErr *erkaruntime.ProviderError
				if !errors.As(err, &providerErr) || providerErr.Code != testCase.openErr || providerErr.Retryable != testCase.retryable {
					t.Fatalf("error = %#v", err)
				}
				if strings.Contains(providerErr.Message, "sk-ant-secret") {
					t.Fatal("error leaked the API key")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			outputs := drain(t, stream)
			final := outputs[len(outputs)-1]
			if final.Kind != erkaruntime.ModelFailed || final.Error.Code != testCase.failCode || final.Error.Retryable != testCase.retryable {
				t.Fatalf("outputs = %#v", outputs)
			}
		})
	}
}

func TestStreamWithoutMessageStopIsMalformed(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(*http.Request) (*http.Response, error) {
		return sseResponse(`{"type":"message_start","message":{"usage":{}}}`), nil
	})
	stream, err := provider.Stream(context.Background(), erkaruntime.ModelRequest{Instructions: "x", MaxOutputTokens: 10, Items: []erkaruntime.RuntimeItem{userItem(1, "go")}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = stream.Next(context.Background())
		if err != nil {
			break
		}
	}
	if !errors.Is(err, erkaruntime.ErrMalformedStream) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewValidatesOptionsAndListModels(t *testing.T) {
	t.Parallel()
	for _, options := range []Options{{Model: "m"}, {APIKey: "k"}, {APIKey: "k", Model: "m", BaseURL: "http://anthropic.example"}, {APIKey: "k", Model: "m", BaseURL: "https://u:p@anthropic.example"}} {
		if _, err := New(options); err == nil {
			t.Errorf("New(%#v) succeeded", options)
		}
	}
	provider := newTestProvider(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://anthropic.example/v1/models?limit=1000" || request.Header.Get("x-api-key") == "" {
			t.Fatalf("request = %s %#v", request.URL, request.Header)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"claude-b","display_name":"B"},{"id":"claude-test","display_name":"Test"}]}`))}, nil
	})
	models, err := provider.ListModels(context.Background())
	if err != nil || len(models) != 2 || models[1].ID != "claude-test" || !models[1].Default || models[0].DisplayName != "B" {
		t.Fatalf("models = %#v, %v", models, err)
	}
}
