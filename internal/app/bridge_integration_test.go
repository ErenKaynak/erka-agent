package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/routing"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestSubscriptionBridgePlanHandsDurableCheckpointToNativeCoding(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	planner := &bridgeFixture{result: erkaruntime.BridgeResult{
		Output:     `{"objective":"fix arithmetic","approved_plan":"Inspect add.go and make the smallest correction."}`,
		Checkpoint: erkaruntime.Checkpoint{Objective: "fix arithmetic", ApprovedPlan: "Inspect add.go and make the smallest correction.", Decisions: []string{"change only add.go"}, RelevantFiles: []string{"add.go"}},
		Usage:      erkaruntime.Usage{InputTokens: 50, OutputTokens: 20},
	}}
	coder := &scriptedProvider{name: "deepseek", responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "Implemented the bridge-approved plan."},
		{Kind: erkaruntime.ModelCompleted},
	}}}
	resolved := config.Resolved{
		ActivePipeline: "default",
		Connections: map[string]config.Connection{
			"chatgpt": {Class: config.ConnectionSubscriptionBridge, Provider: "codex", Command: "codex"},
			"coder":   {Class: config.ConnectionNative, Provider: "deepseek", BaseURL: "https://deepseek.example", APIKeyEnv: "DEEPSEEK_KEY"},
		},
		Pipelines: map[string]config.Pipeline{"default": {Routing: "fixed", Stages: map[string]config.StageRoute{
			"planning": {Candidates: []string{"chatgpt/default"}, RequiredCapabilities: []string{"structured_output"}},
			"coding":   {Candidates: []string{"coder/deepseek-fixture"}},
		}}},
	}
	session, err := OpenSession(context.Background(), SessionOptions{
		WorkingDirectory: root, StateDir: state, Pipeline: resolved,
		ProviderFactory: func(_ context.Context, candidate routing.Candidate) (erkaruntime.ModelProvider, error) {
			if candidate.Provider != "deepseek" {
				return nil, errors.New("bridge candidate reached native factory")
			}
			return coder, nil
		},
		BridgeFactory: func(_ context.Context, candidate routing.Candidate) (erkaruntime.SubscriptionBridge, error) {
			if candidate.Provider != "codex" || candidate.BackendClass != config.ConnectionSubscriptionBridge {
				return nil, errors.New("unexpected bridge candidate")
			}
			return planner, nil
		},
		IDs: &sequenceIDs{}, Clock: fixedClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	taskID, err := session.Submit(context.Background(), "", "fix arithmetic")
	if err != nil {
		t.Fatal(err)
	}
	result, ok, err := session.RunNext(context.Background())
	if err != nil || !ok || result.Status != erkaruntime.TaskCompleted {
		t.Fatalf("RunNext() = %#v, %v, %v", result, ok, err)
	}
	conversation, err := session.OpenConversation(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversation.Runs) != 2 || conversation.Runs[0].BackendClass != erkaruntime.BackendSubscriptionBridge || conversation.Runs[1].BackendClass != erkaruntime.BackendNative {
		t.Fatalf("runs = %#v", conversation.Runs)
	}
	requests := coder.Requests()
	if len(requests) != 1 {
		t.Fatalf("coding requests = %#v", requests)
	}
	var durablePlan, leakedBridgeTranscript bool
	for _, item := range requests[0].Items {
		if item.Kind == erkaruntime.ItemCheckpoint && item.Checkpoint.ApprovedPlan == planner.result.Checkpoint.ApprovedPlan {
			durablePlan = true
		}
		if item.Kind == erkaruntime.ItemAssistantMessage && strings.Contains(item.Message.Content, "smallest correction") {
			leakedBridgeTranscript = true
		}
	}
	if !durablePlan || leakedBridgeTranscript {
		t.Fatalf("coding handoff durablePlan=%v leaked=%v: %#v", durablePlan, leakedBridgeTranscript, requests[0].Items)
	}
}

type bridgeFixture struct {
	request erkaruntime.BridgeRequest
	result  erkaruntime.BridgeResult
}

func (*bridgeFixture) Name() string { return "codex" }
func (*bridgeFixture) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true, StructuredOutput: true}, nil
}
func (*bridgeFixture) Availability(context.Context) (erkaruntime.BridgeAvailability, error) {
	return erkaruntime.BridgeAvailability{Available: true, Authenticated: true}, nil
}
func (bridge *bridgeFixture) Execute(_ context.Context, request erkaruntime.BridgeRequest) (erkaruntime.BridgeResult, error) {
	bridge.request = request
	return bridge.result, nil
}

var _ erkaruntime.SubscriptionBridge = (*bridgeFixture)(nil)
