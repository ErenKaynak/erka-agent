package bridges

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestMain(testingMain *testing.M) {
	if mode := os.Getenv("GO_WANT_CODEX_CATALOG_HELPER"); mode != "" {
		runCodexCatalogHelper(mode)
		return
	}
	os.Exit(testingMain.Run())
}

func TestCodexBridgeUsesOfficialReadOnlyJSONContract(t *testing.T) {
	t.Parallel()
	checkpoint := `{"objective":"fix checkout","constraints":[],"approved_plan":"Inspect checkout.go, patch the retry, and run focused tests.","decisions":["keep retry bounded"],"relevant_files":["checkout.go"],"diff_summary":"","validation_results":[],"unresolved_problems":[],"suggested_next_action":"implement"}`
	runner := &scriptedRunner{results: []ProcessResult{
		{ExitCode: 0, Stdout: "codex-cli 1.0\n"},
		{ExitCode: 0, Stdout: "Logged in using ChatGPT\n"},
		{ExitCode: 0, Stdout: strings.Join([]string{
			`{"type":"thread.started","thread_id":"thread-1"}`,
			`{"type":"turn.started"}`,
			`{"type":"item.completed","item":{"id":"item-1","type":"agent_message","text":` + quoted(checkpoint) + `}}`,
			`{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":20,"output_tokens":40}}`,
		}, "\n") + "\n"},
	}}
	bridge := NewCodex(CodexOptions{Executable: "codex-fixture", Runner: runner})
	result, err := bridge.Execute(context.Background(), erkaruntime.BridgeRequest{Stage: erkaruntime.StagePlanning, Model: "default", WorkingDirectory: t.TempDir(), Prompt: "plan", MaxOutputBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if result.Checkpoint.ApprovedPlan == "" || result.Usage.InputTokens != 120 || result.Usage.CacheReadTokens != 20 || result.Usage.OutputTokens != 40 {
		t.Fatalf("result = %#v", result)
	}
	invocations := runner.Invocations()
	if len(invocations) != 3 {
		t.Fatalf("invocations = %#v", invocations)
	}
	execCall := invocations[2]
	joined := strings.Join(execCall.Arguments, " ")
	for _, required := range []string{"exec", "--json", "--ephemeral", "--sandbox read-only", "--ignore-user-config", "--ignore-rules", "--output-schema"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("Codex arguments %q omit %q", joined, required)
		}
	}
	for key := range execCall.Environment {
		if strings.Contains(key, "API_KEY") || strings.Contains(key, "TOKEN") {
			t.Fatalf("credential variable %q entered bridge environment", key)
		}
	}
	schemaPath := argumentAfter(execCall.Arguments, "--output-schema")
	if schemaPath == "" {
		t.Fatal("schema path is empty")
	}
	if _, err := os.Stat(schemaPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary schema still exists: %v", err)
	}
}

func TestOpenCodeBridgeUsesDenyByDefaultPlanProfile(t *testing.T) {
	t.Parallel()
	checkpoint := `{"objective":"review","constraints":[],"approved_plan":"","decisions":["PASS: no defects"],"relevant_files":[],"diff_summary":"clean","validation_results":["PASS"],"unresolved_problems":[],"suggested_next_action":"complete"}`
	runner := &scriptedRunner{results: []ProcessResult{
		{ExitCode: 0, Stdout: "1.18.15\n"},
		{ExitCode: 0, Stdout: "GitHub Copilot oauth\n1 credentials\n"},
		{ExitCode: 0, Stdout: strings.Join([]string{
			`{"type":"step_start","part":{"type":"step-start"}}`,
			`{"type":"text","part":{"type":"text","text":` + quoted(checkpoint) + `}}`,
			`{"type":"step_finish","part":{"type":"step-finish","tokens":{"input":30,"output":10,"cache":{"read":4,"write":2}}}}`,
		}, "\n") + "\n"},
	}}
	bridge := NewOpenCode(OpenCodeOptions{Executable: "opencode-fixture", Runner: runner})
	result, err := bridge.Execute(context.Background(), erkaruntime.BridgeRequest{Stage: erkaruntime.StageReview, Model: "github-copilot/gpt", WorkingDirectory: t.TempDir(), Prompt: "review", MaxOutputBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Checkpoint.Decisions) != 1 || result.Usage.InputTokens != 30 || result.Usage.CacheReadTokens != 4 {
		t.Fatalf("result = %#v", result)
	}
	call := runner.Invocations()[2]
	if !strings.Contains(strings.Join(call.Arguments, " "), "--agent plan") || !strings.Contains(strings.Join(call.Arguments, " "), "--format json") {
		t.Fatalf("OpenCode arguments = %#v", call.Arguments)
	}
	config := call.Environment["OPENCODE_CONFIG_CONTENT"]
	for _, denied := range []string{`"edit": "deny"`, `"bash": "deny"`, `"task": "deny"`, `"external_directory": "deny"`} {
		if !strings.Contains(config, denied) {
			t.Fatalf("OpenCode bridge config omits %s: %s", denied, config)
		}
	}
	if call.Environment["OPENCODE_DISABLE_PROJECT_CONFIG"] != "true" {
		t.Fatal("project OpenCode configuration was not disabled")
	}
}

func TestOpenCodeModelCatalogKeepsProviderQualifiedIDs(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{results: []ProcessResult{
		{ExitCode: 0, Stdout: "1.2.3\n"},
		{ExitCode: 0, Stdout: "1 credentials\n"},
		{ExitCode: 0, Stdout: "openai/gpt-5.6-sol\ndeepseek/deepseek-v4-pro\nopenai/gpt-5.6-sol\ninvalid\n"},
	}}
	models, err := NewOpenCode(OpenCodeOptions{Executable: "opencode-fixture", Runner: runner}).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "deepseek/deepseek-v4-pro" || models[1].ID != "openai/gpt-5.6-sol" {
		t.Fatalf("models = %#v", models)
	}
	call := runner.Invocations()[2]
	if strings.Join(call.Arguments, " ") != "models --pure" || call.OutputLimitBytes != 4<<20 {
		t.Fatalf("invocation = %#v", call)
	}
}

func TestOpenCodeModelCatalogHandlesLargeCatalog(t *testing.T) {
	t.Parallel()
	const modelCount = 10_000
	var output strings.Builder
	for index := modelCount - 1; index >= 0; index-- {
		_, _ = fmt.Fprintf(&output, "provider/model-%05d\n", index)
	}
	runner := &scriptedRunner{results: []ProcessResult{
		{ExitCode: 0, Stdout: "1.2.3\n"},
		{ExitCode: 0, Stdout: "1 credentials\n"},
		{ExitCode: 0, Stdout: output.String()},
	}}
	models, err := NewOpenCode(OpenCodeOptions{Executable: "opencode-fixture", Runner: runner}).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != modelCount || models[0].ID != "provider/model-00000" || models[modelCount-1].ID != "provider/model-09999" {
		t.Fatalf("large catalog boundaries = %d, %q, %q", len(models), models[0].ID, models[len(models)-1].ID)
	}
}

func TestOpenCodeModelCatalogClassifiesNonzeroProcessFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		stderr    string
		retryable bool
	}{
		{name: "temporary", stderr: "503 temporarily unavailable", retryable: true},
		{name: "permanent", stderr: "provider configuration is invalid", retryable: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner := &scriptedRunner{results: []ProcessResult{
				{ExitCode: 0, Stdout: "1.2.3\n"},
				{ExitCode: 0, Stdout: "1 credentials\n"},
				{ExitCode: 7, Stderr: test.stderr},
			}}
			_, err := NewOpenCode(OpenCodeOptions{Executable: "opencode-fixture", Runner: runner}).ListModels(context.Background())
			var failure *erkaruntime.ProviderError
			if !errors.As(err, &failure) || failure.Code != "opencode_models_failed" || failure.Retryable != test.retryable || !strings.Contains(failure.Message, test.stderr) {
				t.Fatalf("failure = %#v, %v", failure, err)
			}
		})
	}
}

func TestCodexModelCatalogInitializesPaginatesAndIgnoresNotifications(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	models, err := runCodexModelCatalog(context.Background(), executable, t.TempDir(), map[string]string{"GO_WANT_CODEX_CATALOG_HELPER": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "gpt-default" || !models[0].Default || models[1].ID != "gpt-other" || models[1].DisplayName != "Other" {
		t.Fatalf("models = %#v", models)
	}
}

func TestCodexModelCatalogRejectsMalformedProtocol(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = runCodexModelCatalog(context.Background(), executable, t.TempDir(), map[string]string{"GO_WANT_CODEX_CATALOG_HELPER": "malformed"})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v", err)
	}
}

func TestCodexModelCatalogEnforcesCumulativeOutputLimit(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = runCodexModelCatalog(context.Background(), executable, t.TempDir(), map[string]string{"GO_WANT_CODEX_CATALOG_HELPER": "output-limit"})
	if err == nil || !strings.Contains(err.Error(), "output exceeded 4 MiB") {
		t.Fatalf("error = %v", err)
	}
}

func TestCodexModelCatalogPropagatesCancellation(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = runCodexModelCatalog(ctx, executable, t.TempDir(), map[string]string{"GO_WANT_CODEX_CATALOG_HELPER": "cancel"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func runCodexCatalogHelper(mode string) {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     int `json:"id"`
			Method string
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		switch request.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{"userAgent": "fixture"}})
		case "initialized":
			if mode == "malformed" {
				_, _ = os.Stdout.WriteString("not-json\n")
			}
		case "model/list":
			if mode == "cancel" {
				time.Sleep(time.Hour)
			}
			if mode == "output-limit" {
				payload := strings.Repeat("x", 32<<10)
				for index := 0; index < 129; index++ {
					_ = encoder.Encode(map[string]any{"method": "catalog/progress", "params": payload})
				}
				continue
			}
			_ = encoder.Encode(map[string]any{"method": "account/updated", "params": map[string]any{}})
			if request.Params.Cursor == "" {
				_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{"data": []map[string]any{{"id": "id-other", "model": "gpt-other", "displayName": "Other", "description": "fixture", "hidden": false, "isDefault": false}}, "nextCursor": "page-2"}})
			} else {
				_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{"data": []map[string]any{{"id": "id-default", "model": "gpt-default", "displayName": "Default", "hidden": false, "isDefault": true}}, "nextCursor": nil}})
			}
		}
	}
	os.Exit(0)
}

func TestBridgeParsersRejectMalformedOrIncompleteStreams(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "Codex malformed JSONL", run: func() error { _, err := parseCodexJSONL(erkaruntime.StagePlanning, "not-json\n", 1024); return err }},
		{name: "Codex missing completion", run: func() error {
			_, err := parseCodexJSONL(erkaruntime.StagePlanning, `{"type":"turn.started"}`+"\n", 1024)
			return err
		}},
		{name: "OpenCode malformed checkpoint", run: func() error {
			_, err := parseOpenCodeJSONL(erkaruntime.StagePlanning, `{"type":"text","part":{"type":"text","text":"{}"}}`+"\n"+`{"type":"step_finish","part":{"type":"step-finish"}}`+"\n", 1024)
			return err
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.run(); !errors.Is(err, ErrMalformedStream) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCodexBridgePropagatesCancellation(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{results: []ProcessResult{{ExitCode: 0, Stdout: "codex-cli 1\n"}, {ExitCode: 0, Stdout: "Logged in using ChatGPT\n"}}, blockAt: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := NewCodex(CodexOptions{Runner: runner}).Execute(ctx, erkaruntime.BridgeRequest{Stage: erkaruntime.StagePlanning, WorkingDirectory: t.TempDir(), Prompt: "plan", MaxOutputBytes: 1024})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestLoginRejectsUnsupportedBridgeBeforeProcessLookup(t *testing.T) {
	t.Parallel()
	err := Login(context.Background(), "unknown", "definitely-not-installed", false, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported subscription bridge") {
		t.Fatalf("error = %v", err)
	}
}

type scriptedRunner struct {
	mu          sync.Mutex
	results     []ProcessResult
	invocations []Invocation
	blockAt     int
}

func (*scriptedRunner) LookPath(name string) (string, error) { return "/fixture/" + name, nil }

func (runner *scriptedRunner) Run(ctx context.Context, invocation Invocation) (ProcessResult, error) {
	runner.mu.Lock()
	index := len(runner.invocations)
	runner.invocations = append(runner.invocations, invocation)
	block := runner.blockAt > 0 && index == runner.blockAt
	var result ProcessResult
	if index < len(runner.results) {
		result = runner.results[index]
	}
	runner.mu.Unlock()
	if block {
		<-ctx.Done()
		return ProcessResult{}, ctx.Err()
	}
	return result, nil
}

func (runner *scriptedRunner) Invocations() []Invocation {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return append([]Invocation(nil), runner.invocations...)
}

func quoted(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func argumentAfter(arguments []string, name string) string {
	for index := range arguments {
		if arguments[index] == name && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	return ""
}
