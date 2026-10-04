package openaicompat

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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func sseResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func modelRequest() erkaruntime.ModelRequest {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	item := func(sequence uint64, kind erkaruntime.ItemKind) erkaruntime.RuntimeItem {
		return erkaruntime.RuntimeItem{ID: erkaruntime.ItemID("item-" + string(rune('a'+sequence))), Sequence: sequence, TaskID: "task", RunID: "run", Kind: kind, Time: now}
	}
	user := item(1, erkaruntime.ItemUserMessage)
	user.Message = &erkaruntime.Message{Content: "fix the tests"}
	assistant := item(2, erkaruntime.ItemAssistantMessage)
	assistant.Message = &erkaruntime.Message{Content: "Reading files."}
	callA := item(3, erkaruntime.ItemToolCall)
	callA.ToolCall = &erkaruntime.ToolCall{CallID: "call-a", Tool: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)}
	callB := item(4, erkaruntime.ItemToolCall)
	callB.ToolCall = &erkaruntime.ToolCall{CallID: "call-b", Tool: "read_file", Arguments: json.RawMessage(`{"path":"b.go"}`)}
	resultA := item(5, erkaruntime.ItemToolResult)
	resultA.ToolResult = &erkaruntime.ToolResult{CallID: "call-a", Output: "package a"}
	resultB := item(6, erkaruntime.ItemToolResult)
	resultB.ToolResult = &erkaruntime.ToolResult{CallID: "call-b", Output: "package b"}
	checkpoint := item(7, erkaruntime.ItemCheckpoint)
	checkpoint.Checkpoint = &erkaruntime.Checkpoint{Objective: "fix", ApprovedPlan: "approved plan"}
	return erkaruntime.ModelRequest{
		Instructions: "be careful", MaxOutputTokens: 1024,
		Items: []erkaruntime.RuntimeItem{user, assistant, callA, callB, resultA, resultB, checkpoint},
		Tools: []erkaruntime.ToolDefinition{{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`), OutputLimitBytes: 1024}},
	}
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

func TestStreamGroupsToolCallsAndTranslatesOutput(t *testing.T) {
	t.Parallel()
	const secret = "sk-compat-secret"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://openrouter.example/api/v1/chat/completions" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer "+secret {
			t.Fatal("authorization header missing")
		}
		data, _ := io.ReadAll(request.Body)
		if strings.Contains(string(data), secret) {
			t.Fatal("API key appeared in request body")
		}
		var body struct {
			Model         string `json:"model"`
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
			Messages []struct {
				Role       string  `json:"role"`
				Content    *string `json:"content"`
				ToolCallID string  `json:"tool_call_id"`
				ToolCalls  []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "vendor/model" || !body.StreamOptions.IncludeUsage {
			t.Fatalf("body = %s", data)
		}
		// system, user, assistant(text + 2 calls), tool, tool, checkpoint
		if len(body.Messages) != 6 {
			t.Fatalf("messages = %s", data)
		}
		assistant := body.Messages[2]
		if assistant.Role != "assistant" || assistant.Content == nil || *assistant.Content != "Reading files." || len(assistant.ToolCalls) != 2 {
			t.Fatalf("assistant turn = %#v", assistant)
		}
		if body.Messages[3].ToolCallID != "call-a" || body.Messages[4].ToolCallID != "call-b" {
			t.Fatalf("tool results = %#v %#v", body.Messages[3], body.Messages[4])
		}
		if body.Messages[5].Role != "user" || !strings.Contains(*body.Messages[5].Content, "approved plan") {
			t.Fatalf("checkpoint = %#v", body.Messages[5])
		}
		return sseResponse(strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{"content":"Done "},"finish_reason":null}]}`, "",
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`, "",
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`, "",
			`data: {"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":12}}}`, "",
			"data: [DONE]", "",
		}, "\n")), nil
	})
	provider, err := New(Options{APIKey: secret, BaseURL: "https://openrouter.example/api/v1/", Model: "vendor/model", Client: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), modelRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	outputs := drain(t, stream)
	if len(outputs) != 5 {
		t.Fatalf("outputs = %#v", outputs)
	}
	if outputs[0].Text != "Done " || outputs[1].ToolCall.CallID != "call-1" || outputs[1].ToolCall.Tool != "read_file" || outputs[2].ToolCall.CallID != "call-1" {
		t.Fatalf("outputs = %#v", outputs)
	}
	usage := outputs[3].Usage
	if outputs[3].Kind != erkaruntime.ModelUsage || usage.InputTokens != 20 || usage.OutputTokens != 5 || usage.CacheReadTokens != 12 {
		t.Fatalf("usage = %#v", outputs[3])
	}
	if outputs[4].Kind != erkaruntime.ModelCompleted {
		t.Fatalf("last output = %#v", outputs[4])
	}
}

func TestLocalServerWithoutAPIKeySendsNoAuthorization(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "http://localhost:11434/v1/chat/completions" {
			t.Fatalf("url = %s", request.URL)
		}
		if _, present := request.Header["Authorization"]; present {
			t.Fatal("authorization header sent without a key")
		}
		return sseResponse("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
	})
	provider, err := New(Options{BaseURL: "http://localhost:11434/v1", Model: "qwen3-coder", Client: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), modelRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if outputs := drain(t, stream); len(outputs) != 2 || outputs[0].Text != "hi" {
		t.Fatalf("outputs = %#v", outputs)
	}
}

func TestNewValidatesBaseURL(t *testing.T) {
	t.Parallel()
	valid := []string{"https://openrouter.ai/api/v1", "http://localhost:11434/v1", "http://127.0.0.1:8000/v1", "http://[::1]:8080/v1"}
	for _, base := range valid {
		if _, err := New(Options{BaseURL: base, Model: "m"}); err != nil {
			t.Errorf("New(%q) = %v", base, err)
		}
	}
	invalid := []string{"", "http://192.168.1.10:11434/v1", "http://ollama.lan/v1", "ftp://example.test", "https://user:pass@example.test/v1", "not a url"}
	for _, base := range invalid {
		if _, err := New(Options{BaseURL: base, Model: "m"}); err == nil {
			t.Errorf("New(%q) succeeded", base)
		}
	}
	if _, err := New(Options{BaseURL: "https://example.test/v1"}); err == nil {
		t.Error("New without model succeeded")
	}
}

func TestStreamMapsHTTPAndStreamErrors(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`))}, nil
	})
	provider, _ := New(Options{APIKey: "k", BaseURL: "https://example.test/v1", Model: "m", Client: &http.Client{Transport: transport}})
	_, err := provider.Stream(context.Background(), modelRequest())
	var providerErr *erkaruntime.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Code != "http_429" || !providerErr.Retryable || providerErr.Message != "slow down" {
		t.Fatalf("error = %#v", err)
	}

	transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return sseResponse("data: {\"error\":{\"code\":\"server_error\",\"message\":\"upstream failed\"}}\n\n"), nil
	})
	provider, _ = New(Options{BaseURL: "https://example.test/v1", Model: "m", Client: &http.Client{Transport: transport}})
	stream, err := provider.Stream(context.Background(), modelRequest())
	if err != nil {
		t.Fatal(err)
	}
	outputs := drain(t, stream)
	if len(outputs) != 1 || outputs[0].Kind != erkaruntime.ModelFailed || outputs[0].Error.Code != "server_error" || !outputs[0].Error.Retryable {
		t.Fatalf("outputs = %#v", outputs)
	}

	transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return sseResponse("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"), nil
	})
	provider, _ = New(Options{BaseURL: "https://example.test/v1", Model: "m", Client: &http.Client{Transport: transport}})
	stream, _ = provider.Stream(context.Background(), modelRequest())
	if outputs := drain(t, stream); outputs[0].Kind != erkaruntime.ModelFailed || outputs[0].Error.Code != "incomplete" {
		t.Fatalf("length outputs = %#v", outputs)
	}
}

func TestListModels(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://example.test/v1/models" {
			t.Fatalf("url = %s", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"b"},{"id":"a"},{"id":"a"},{"id":""}]}`))}, nil
	})
	provider, _ := New(Options{APIKey: "k", BaseURL: "https://example.test/v1", Model: "a", Client: &http.Client{Transport: transport}})
	models, err := provider.ListModels(context.Background())
	if err != nil || len(models) != 2 || models[0].ID != "a" || !models[0].Default || models[1].ID != "b" {
		t.Fatalf("models = %#v, %v", models, err)
	}
}
