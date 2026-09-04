package openai

import (
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

func TestResponsesStreamTranslatesTextToolsUsageAndCompletion(t *testing.T) {
	t.Parallel()
	const secret = "openai-secret"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://example.test/v1/responses" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer "+secret {
			t.Fatal("authorization header missing")
		}
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatal("API secret appeared in request body")
		}
		var body struct {
			Model             string `json:"model"`
			Store             bool   `json:"store"`
			Stream            bool   `json:"stream"`
			ParallelToolCalls bool   `json:"parallel_tool_calls"`
			Input             []struct {
				Type   string `json:"type"`
				Role   string `json:"role"`
				CallID string `json:"call_id"`
			} `json:"input"`
			Tools []struct {
				Type   string `json:"type"`
				Name   string `json:"name"`
				Strict bool   `json:"strict"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "gpt-fixture" || body.Store || !body.Stream || !body.ParallelToolCalls || len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Name != "read_file" || body.Tools[0].Strict {
			t.Fatalf("request body = %s", data)
		}
		if len(body.Input) != 6 || body.Input[2].Type != "function_call" || body.Input[2].CallID != "old-call" || body.Input[3].Type != "function_call_output" || body.Input[4].Role != "user" {
			t.Fatalf("input = %#v", body.Input)
		}
		return sseResponse(strings.Join([]string{
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","item_id":"msg-1","delta":"Working "}`,
			``,
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","item":{"id":"fc-1","type":"function_call","call_id":"call-1","name":"read_file"}}`,
			``,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc-1","delta":"{\"pa"}`,
			``,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc-1","delta":"th\":\"x\"}"}`,
			``,
			`data: {"type":"response.function_call_arguments.done","item_id":"fc-1","name":"read_file","arguments":"{\"path\":\"x\"}"}`,
			``,
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":12,"output_tokens":4,"input_tokens_details":{"cached_tokens":3}}}}`,
			``, ``,
		}, "\n")), nil
	})
	provider, err := New(Options{APIKey: secret, BaseURL: "https://example.test", Model: "gpt-fixture", Client: &http.Client{Transport: transport}})
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
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, output)
	}
	want := []erkaruntime.ModelOutputKind{erkaruntime.ModelTextDelta, erkaruntime.ModelToolCallDelta, erkaruntime.ModelToolCallDelta, erkaruntime.ModelToolCallDelta, erkaruntime.ModelUsage, erkaruntime.ModelCompleted}
	if len(outputs) != len(want) {
		t.Fatalf("outputs = %#v", outputs)
	}
	for index := range want {
		if outputs[index].Kind != want[index] {
			t.Fatalf("output %d = %#v", index, outputs[index])
		}
	}
	if outputs[1].ToolCall.CallID != "call-1" || outputs[1].ToolCall.Tool != "read_file" || outputs[2].ToolCall.ArgumentsFragment+outputs[3].ToolCall.ArgumentsFragment != `{"path":"x"}` {
		t.Fatalf("tool outputs = %#v", outputs)
	}
	if outputs[4].Usage.InputTokens != 12 || outputs[4].Usage.OutputTokens != 4 || outputs[4].Usage.CacheReadTokens != 3 {
		t.Fatalf("usage = %#v", outputs[4].Usage)
	}
}

func TestResponsesStreamUsesDoneArgumentsWhenNoDeltasArrive(t *testing.T) {
	t.Parallel()
	provider, err := New(Options{APIKey: "x", BaseURL: "https://example.test/v1", Model: "gpt-fixture", Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://example.test/v1/responses" {
			t.Fatalf("URL = %s", request.URL)
		}
		return sseResponse("data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc-1\",\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"read_file\"}}\n\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc-1\",\"arguments\":\"{}\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), modelRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, _ := stream.Next(context.Background())
	second, _ := stream.Next(context.Background())
	if first.ToolCall.Tool != "read_file" || second.ToolCall.ArgumentsFragment != "{}" {
		t.Fatalf("outputs = %#v / %#v", first, second)
	}
}

func TestResponsesErrorsAreBoundedAndClassified(t *testing.T) {
	t.Parallel()
	provider, err := New(Options{APIKey: "secret", BaseURL: "https://example.test", Model: "gpt-fixture", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusTooManyRequests, "application/json", `{"error":{"message":"slow down"}}`), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Stream(context.Background(), modelRequest())
	var failure *erkaruntime.ProviderError
	if !errors.As(err, &failure) || !failure.Retryable || failure.Code != "http_429" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %#v", err)
	}
}

func TestResponsesRejectsMalformedStreamAndEndpoint(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{APIKey: "x", Model: "m", BaseURL: "http://remote.test"}); err == nil {
		t.Fatal("insecure endpoint error = nil")
	}
	provider, err := New(Options{APIKey: "x", Model: "m", BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return sseResponse("data: {bad}\n\n"), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), modelRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "decode OpenAI") {
		t.Fatalf("stream error = %v", err)
	}
}

func TestListModelsUsesV1EndpointAndDeduplicates(t *testing.T) {
	t.Parallel()
	provider, err := New(Options{APIKey: "secret", BaseURL: "https://example.test", Model: "gpt-b", Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != "https://example.test/v1/models" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("authorization header missing")
		}
		return response(http.StatusOK, "application/json", `{"data":[{"id":"gpt-b"},{"id":"gpt-a"},{"id":"gpt-a"}]}`), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "gpt-a" || models[1].ID != "gpt-b" || !models[1].Default {
		t.Fatalf("models = %#v", models)
	}
}

func TestListModelsRejectsMalformedOversizedAndCancelledResponses(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{bad}`, strings.Repeat("x", (1<<20)+1)} {
		provider, err := New(Options{APIKey: "secret", BaseURL: "https://example.test/v1", Model: "gpt-a", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(http.StatusOK, "application/json", body), nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.ListModels(context.Background()); err == nil {
			t.Fatalf("ListModels() accepted %d-byte response", len(body))
		}
	}
	provider, err := New(Options{APIKey: "secret", BaseURL: "https://example.test", Model: "gpt-a", Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.ListModels(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ListModels() error = %v", err)
	}
}

func modelRequest() erkaruntime.ModelRequest {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	return erkaruntime.ModelRequest{Instructions: "system", MaxOutputTokens: 100, Items: []erkaruntime.RuntimeItem{
		{ID: "item-1", Sequence: 1, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemUserMessage, Time: now, Message: &erkaruntime.Message{Content: "fix it"}},
		{ID: "item-2", Sequence: 2, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemAssistantMessage, Time: now, Message: &erkaruntime.Message{Content: "checking"}},
		{ID: "item-3", Sequence: 3, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemToolCall, Time: now, ToolCall: &erkaruntime.ToolCall{CallID: "old-call", Tool: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}},
		{ID: "item-4", Sequence: 4, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemToolResult, Time: now, ToolResult: &erkaruntime.ToolResult{CallID: "old-call", Output: "contents"}},
		{ID: "item-5", Sequence: 5, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemCheckpoint, Time: now, Checkpoint: &erkaruntime.Checkpoint{Objective: "fix", ApprovedPlan: "plan"}},
		{ID: "item-6", Sequence: 6, TaskID: "task", RunID: "run", Kind: erkaruntime.ItemReasoningSummary, Time: now, Message: &erkaruntime.Message{Content: "summary"}},
	}, Tools: []erkaruntime.ToolDefinition{{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`), OutputLimitBytes: 100}}}
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
