package deepseek

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestStreamTranslatesTextFragmentedToolsUsageAndCompletion(t *testing.T) {
	t.Parallel()
	const secret = "top-secret-key"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://example.test/chat/completions" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer "+secret {
			t.Fatalf("authorization header missing")
		}
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatal("API secret appeared in model request body")
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
			Tools []any `json:"tools"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "deepseek-v4-pro" || len(body.Messages) != 6 || len(body.Tools) != 1 {
			t.Fatalf("request body = %s", data)
		}
		if body.Messages[4].Role != "tool" || body.Messages[4].ToolCallID != "old-call" {
			t.Fatalf("tool result message = %#v", body.Messages[4])
		}
		if body.Messages[5].Role != "user" || !strings.Contains(body.Messages[5].Content, "approved plan") {
			t.Fatalf("checkpoint message = %#v", body.Messages[5])
		}
		return sseResponse(strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{"content":"Working "},"finish_reason":null}]}`,
			"",
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read_file","arguments":"{\"pa"}}]},"finish_reason":null}]}`,
			"",
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
			"",
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"prompt_cache_hit_tokens":3}}`,
			"",
			"data: [DONE]", "", "",
		}, "\n")), nil
	})
	provider, err := New(Options{APIKey: secret, BaseURL: "https://example.test", Model: "deepseek-v4-pro", Client: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), modelRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var outputs []erkaruntime.ModelOutput
	for {
		output, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, output)
	}
	wantKinds := []erkaruntime.ModelOutputKind{erkaruntime.ModelTextDelta, erkaruntime.ModelToolCallDelta, erkaruntime.ModelToolCallDelta, erkaruntime.ModelUsage, erkaruntime.ModelCompleted}
	if len(outputs) != len(wantKinds) {
		t.Fatalf("outputs = %#v", outputs)
	}
	for index, want := range wantKinds {
		if outputs[index].Kind != want {
			t.Fatalf("output %d kind = %q, want %q", index, outputs[index].Kind, want)
		}
	}
	if outputs[1].ToolCall.CallID != "call-1" || outputs[2].ToolCall.CallID != "call-1" || outputs[1].ToolCall.Tool != "read_file" || outputs[1].ToolCall.ArgumentsFragment+outputs[2].ToolCall.ArgumentsFragment != `{"path":"x"}` {
		t.Fatalf("tool deltas = %#v / %#v", outputs[1], outputs[2])
	}
	if outputs[3].Usage.InputTokens != 12 || outputs[3].Usage.CacheReadTokens != 3 {
		t.Fatalf("usage = %#v", outputs[3].Usage)
	}
}

func TestStreamRejectsHTTPAndMalformedSSEWithoutLeakingSecret(t *testing.T) {
	t.Parallel()
	const secret = "never-print-me"
	for _, test := range []struct {
		name      string
		response  *http.Response
		streamErr bool
		want      string
	}{
		{name: "API error", response: response(http.StatusUnauthorized, "application/json", `{"error":{"message":"invalid key"}}`), want: "invalid key"},
		{name: "content type", response: response(http.StatusOK, "application/json", `{}`), want: "content type"},
		{name: "malformed event", response: sseResponse("data: {bad}\n\n"), streamErr: true, want: "decode DeepSeek"},
		{name: "missing call ID", response: sseResponse(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}` + "\n\n"), streamErr: true, want: "no call ID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, err := New(Options{APIKey: secret, BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return test.response, nil })}})
			if err != nil {
				t.Fatal(err)
			}
			stream, err := provider.Stream(context.Background(), modelRequest())
			if test.streamErr && err == nil {
				_, err = stream.Next(context.Background())
				stream.Close()
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("API secret leaked in error")
			}
		})
	}
}

func TestNewValidatesCredentialAndEndpoint(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{}); err == nil || !strings.Contains(err.Error(), "key is empty") {
		t.Fatalf("empty key error = %v", err)
	}
	if _, err := New(Options{APIKey: "x", BaseURL: "http://remote.test"}); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("HTTP endpoint error = %v", err)
	}
}

func TestNextHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	provider, err := New(Options{APIKey: "x", BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return sseResponse("data: [DONE]\n\n"), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), modelRequest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stream.Next(ctx); err != context.Canceled {
		t.Fatalf("Next() error = %v", err)
	}
}

func modelRequest() erkaruntime.ModelRequest {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	return erkaruntime.ModelRequest{
		Instructions: "system", MaxOutputTokens: 100,
		Items: []erkaruntime.RuntimeItem{
			{ID: "item-1", Sequence: 1, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemUserMessage, Time: now, Message: &erkaruntime.Message{Content: "fix it"}},
			{ID: "item-2", Sequence: 2, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemAssistantMessage, Time: now, Message: &erkaruntime.Message{Content: "checking"}},
			{ID: "item-3", Sequence: 3, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemToolCall, Time: now, ToolCall: &erkaruntime.ToolCall{CallID: "old-call", Tool: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}},
			{ID: "item-4", Sequence: 4, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemToolResult, Time: now, ToolResult: &erkaruntime.ToolResult{CallID: "old-call", Output: "contents"}},
			{ID: "item-5", Sequence: 5, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemCheckpoint, Time: now, Checkpoint: &erkaruntime.Checkpoint{Objective: "fix it", ApprovedPlan: "approved plan"}},
		},
		Tools: []erkaruntime.ToolDefinition{{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`), OutputLimitBytes: 100}},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func sseResponse(body string) *http.Response {
	return response(http.StatusOK, "text/event-stream; charset=utf-8", body)
}

func response(status int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}
