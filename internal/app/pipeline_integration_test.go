package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/providers/deepseek"
	"github.com/ErenKaynak/erka-agent/internal/providers/openai"
	"github.com/ErenKaynak/erka-agent/internal/routing"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestNativePipelineProtocolAdaptersPersistAndRestoreWorkflow(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	const openAISecret = "fixture-openai-secret"
	const deepSeekSecret = "fixture-deepseek-secret"

	var openAIRequest string
	openAIClient := &http.Client{Transport: pipelineRoundTripper(func(request *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		openAIRequest = string(data)
		if request.URL.String() != "http://127.0.0.1:17001/v1/responses" || request.Header.Get("Authorization") != "Bearer "+openAISecret {
			t.Fatalf("OpenAI request = %s, authorization = %q", request.URL, request.Header.Get("Authorization"))
		}
		return pipelineSSEResponse(strings.Join([]string{
			`data: {"type":"response.output_text.delta","item_id":"plan-1","delta":"Inspect add.go, replace subtraction with addition, then inspect the diff."}`,
			``,
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}`,
			``, ``,
		}, "\n")), nil
	})}

	patch := "diff --git a/add.go b/add.go\n--- a/add.go\n+++ b/add.go\n@@ -1,2 +1,2 @@\n package fixture\n-func Add(a, b int) int { return a - b }\n+func Add(a, b int) int { return a + b }\n"
	arguments := string(mustJSON(t, map[string]string{"patch": patch}))
	var deepSeekMu sync.Mutex
	var deepSeekRequests []string
	deepSeekClient := &http.Client{Transport: pipelineRoundTripper(func(request *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		deepSeekMu.Lock()
		deepSeekRequests = append(deepSeekRequests, string(data))
		requestNumber := len(deepSeekRequests)
		deepSeekMu.Unlock()
		if request.URL.String() != "http://127.0.0.1:17002/chat/completions" || request.Header.Get("Authorization") != "Bearer "+deepSeekSecret {
			t.Fatalf("DeepSeek request = %s, authorization = %q", request.URL, request.Header.Get("Authorization"))
		}
		if requestNumber == 1 {
			event := map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": 0, "id": "patch-1", "function": map[string]any{"name": "apply_patch", "arguments": arguments},
					}}},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]any{"prompt_tokens": 8, "completion_tokens": 4},
			}
			return pipelineSSEResponse("data: " + string(mustJSON(t, event)) + "\n\ndata: [DONE]\n\n"), nil
		}
		if requestNumber == 2 {
			return pipelineSSEResponse("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Implemented the approved arithmetic fix.\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":6,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"), nil
		}
		return nil, fmt.Errorf("unexpected DeepSeek request %d", requestNumber)
	})}

	resolved := config.Resolved{
		ActivePipeline: "default",
		Connections: map[string]config.Connection{
			"planner": {Provider: "openai", BaseURL: "http://127.0.0.1:17001/v1", Models: map[string]config.ModelConfig{
				"gpt-fixture": {InputCostPerMillionMicrounits: 1_000_000, OutputCostPerMillionMicrounits: 2_000_000, Currency: "USD"},
			}},
			"coder": {Provider: "deepseek", BaseURL: "http://127.0.0.1:17002", Models: map[string]config.ModelConfig{
				"deepseek-fixture": {InputCostPerMillionMicrounits: 3_000_000, OutputCostPerMillionMicrounits: 4_000_000, Currency: "USD"},
			}},
		},
		Pipelines: map[string]config.Pipeline{"default": {Routing: "fixed", Stages: map[string]config.StageRoute{
			"planning": {Candidates: []string{"planner/gpt-fixture"}},
			"coding":   {Candidates: []string{"coder/deepseek-fixture"}},
		}}},
	}
	factory := func(_ context.Context, candidate routing.Candidate) (erkaruntime.ModelProvider, error) {
		switch candidate.Provider {
		case "openai":
			return openai.New(openai.Options{APIKey: openAISecret, BaseURL: candidate.BaseURL, Model: candidate.Model, Client: openAIClient})
		case "deepseek":
			return deepseek.New(deepseek.Options{APIKey: deepSeekSecret, BaseURL: candidate.BaseURL, Model: candidate.Model, Client: deepSeekClient})
		default:
			return nil, fmt.Errorf("unexpected provider %q", candidate.Provider)
		}
	}
	result, err := RunLocal(context.Background(), LocalOptions{
		WorkingDirectory: root,
		StateDir:         state,
		Objective:        "Fix Add with the smallest correct change.",
		Pipeline:         resolved,
		ProviderFactory:  factory,
		Limits:           erkaruntime.DefaultNativeLimits(),
		IDs:              &sequenceIDs{},
		Clock:            fixedClock,
	})
	if err != nil || result.Status != erkaruntime.TaskCompleted || result.Summary != "Implemented the approved arithmetic fix." {
		t.Fatalf("RunLocal() = %#v, %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "add.go"))
	if err != nil || !strings.Contains(string(data), "return a + b") {
		t.Fatalf("patched file = %q, %v", data, err)
	}
	deepSeekMu.Lock()
	requests := append([]string(nil), deepSeekRequests...)
	deepSeekMu.Unlock()
	if len(requests) != 2 || !strings.Contains(requests[0], "Durable stage checkpoint") || !strings.Contains(requests[1], "patch-1") {
		t.Fatalf("DeepSeek requests = %#v", requests)
	}
	for _, body := range append([]string{openAIRequest}, requests...) {
		if strings.Contains(body, openAISecret) || strings.Contains(body, deepSeekSecret) {
			t.Fatal("provider credential leaked into a model request body")
		}
	}

	restored, err := OpenSession(context.Background(), SessionOptions{
		WorkingDirectory: root,
		StateDir:         state,
		Pipeline:         resolved,
		ProviderFactory:  factory,
		IDs:              &sequenceIDs{},
		Clock:            fixedClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	conversation, err := restored.OpenConversation(context.Background(), result.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Task.Status != erkaruntime.TaskCompleted || len(conversation.Runs) != 2 {
		t.Fatalf("restored conversation = %#v", conversation)
	}
	if conversation.Runs[0].Provider != "openai" || conversation.Runs[0].Usage.InputTokens != 10 || conversation.Runs[0].Usage.OutputTokens != 5 || conversation.Runs[0].Usage.CostMicrounits != 20 {
		t.Fatalf("planning accounting = %#v", conversation.Runs[0])
	}
	if conversation.Runs[1].Provider != "deepseek" || conversation.Runs[1].Usage.InputTokens != 14 || conversation.Runs[1].Usage.OutputTokens != 7 || conversation.Runs[1].Usage.CostMicrounits != 70 {
		t.Fatalf("coding accounting = %#v", conversation.Runs[1])
	}
	var checkpoints, toolResults int
	for _, item := range conversation.Items {
		switch item.Kind {
		case erkaruntime.ItemCheckpoint:
			checkpoints++
		case erkaruntime.ItemToolResult:
			toolResults++
		}
	}
	if checkpoints != 2 || toolResults != 1 {
		t.Fatalf("restored items: checkpoints=%d toolResults=%d", checkpoints, toolResults)
	}
	databaseBytes, err := os.ReadFile(filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(databaseBytes), openAISecret) || strings.Contains(string(databaseBytes), deepSeekSecret) {
		t.Fatal("provider credential leaked into SQLite")
	}
}

type pipelineRoundTripper func(*http.Request) (*http.Response, error)

func (function pipelineRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func pipelineSSEResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
