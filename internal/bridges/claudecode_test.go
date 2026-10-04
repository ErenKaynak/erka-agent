package bridges

import (
	"context"
	"errors"
	"strings"
	"testing"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const claudeCheckpoint = `{"objective":"fix checkout","constraints":[],"approved_plan":"Patch retry in checkout.go and run its tests.","decisions":["bounded retry"],"relevant_files":["checkout.go"],"diff_summary":"","validation_results":[],"unresolved_problems":[],"suggested_next_action":"implement"}`

func claudeResultLine(structured string) string {
	return `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s","total_cost_usd":0.01,"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000,"output_tokens":50},"structured_output":` + structured + `}`
}

func authenticatedClaudeRunner(final ProcessResult) *scriptedRunner {
	return &scriptedRunner{results: []ProcessResult{
		{ExitCode: 0, Stdout: "2.1.270 (Claude Code)\n"},
		{ExitCode: 0, Stdout: `{"loggedIn":true,"authMethod":"claude.ai","configDirectory":"/home/u/.claude"}`},
		final,
	}}
}

func TestClaudeCodeBridgeUsesRestrictedReadOnlyHeadlessContract(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		t.Setenv(name, "must-not-leak-"+name)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/u/.claude")
	runner := authenticatedClaudeRunner(ProcessResult{ExitCode: 0, Stdout: strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s","tools":["Read","Grep","Glob"]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Looking"}]}}`,
		claudeResultLine(claudeCheckpoint),
	}, "\n") + "\n"})
	bridge := NewClaudeCode(ClaudeCodeOptions{Executable: "claude-fixture", Runner: runner})
	result, err := bridge.Execute(context.Background(), erkaruntime.BridgeRequest{Stage: erkaruntime.StagePlanning, Model: "opus", WorkingDirectory: t.TempDir(), Prompt: "plan the fix", MaxOutputBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if result.Checkpoint.ApprovedPlan == "" || result.Usage.InputTokens != 3210 || result.Usage.CacheReadTokens != 3000 || result.Usage.CacheWriteTokens != 200 || result.Usage.OutputTokens != 50 {
		t.Fatalf("result = %#v", result)
	}
	invocations := runner.Invocations()
	if len(invocations) != 3 {
		t.Fatalf("invocations = %#v", invocations)
	}
	if got := strings.Join(invocations[1].Arguments, " "); got != "auth status" {
		t.Fatalf("auth status arguments = %q", got)
	}
	exec := invocations[2]
	joined := strings.Join(exec.Arguments, " ")
	for _, required := range []string{"-p", "--output-format stream-json", "--verbose", "--restricted", "--tools Read,Grep,Glob", "--disallowedTools mcp__*", "--strict-mcp-config", "--permission-prompts none", "--no-session-persistence", "--json-schema {", "--model opus"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("arguments %q omit %q", joined, required)
		}
	}
	for _, forbidden := range []string{"--dangerously-skip-permissions", "bypassPermissions", "setup-token", "Bash", "Edit", "Write"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("arguments %q contain %q", joined, forbidden)
		}
	}
	if exec.Input != "plan the fix" {
		t.Fatalf("prompt must travel on stdin, got %q", exec.Input)
	}
	for _, invocation := range invocations {
		for key, value := range invocation.Environment {
			if strings.Contains(key, "API_KEY") || strings.Contains(key, "TOKEN") || strings.Contains(value, "must-not-leak") {
				t.Fatalf("credential variable %q reached the Claude Code CLI", key)
			}
		}
	}
	if exec.Environment["CLAUDE_CONFIG_DIR"] != "/home/u/.claude" {
		t.Fatalf("environment = %#v", exec.Environment)
	}
}

func TestClaudeCodeAvailabilityStates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		results       []ProcessResult
		available     bool
		authenticated bool
		detail        string
	}{
		{name: "subscription", results: []ProcessResult{{Stdout: "2.1.259 (Claude Code)"}, {Stdout: `{"loggedIn":true,"authMethod":"claude.ai"}`}}, available: true, authenticated: true},
		{name: "too old", results: []ProcessResult{{Stdout: "2.1.200 (Claude Code)"}}, detail: "or newer is required"},
		{name: "unparseable version", results: []ProcessResult{{Stdout: "dev build"}}, detail: "or newer is required"},
		{name: "logged out", results: []ProcessResult{{Stdout: "2.2.0"}, {ExitCode: 1, Stdout: `{"loggedIn":false,"authMethod":"none"}`}}, available: true, detail: "not authenticated"},
		{name: "api key login", results: []ProcessResult{{Stdout: "2.2.0"}, {Stdout: `{"loggedIn":true,"authMethod":"api_key"}`}}, available: true, detail: "not using Claude subscription"},
		{name: "oauth token login", results: []ProcessResult{{Stdout: "2.2.0"}, {Stdout: `{"loggedIn":true,"authMethod":"oauth_token"}`}}, available: true, detail: "not using Claude subscription"},
		{name: "unreadable status", results: []ProcessResult{{Stdout: "2.2.0"}, {Stdout: "Logged in"}}, available: true, detail: "unreadable"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			bridge := NewClaudeCode(ClaudeCodeOptions{Runner: &scriptedRunner{results: testCase.results}})
			availability, err := bridge.Availability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if availability.Available != testCase.available || availability.Authenticated != testCase.authenticated || !strings.Contains(availability.Detail, testCase.detail) {
				t.Fatalf("availability = %#v", availability)
			}
		})
	}
}

func TestClaudeCodeExecuteRefusesWithoutSubscriptionLogin(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{results: []ProcessResult{{Stdout: "2.2.0"}, {Stdout: `{"loggedIn":true,"authMethod":"api_key"}`}}}
	_, err := NewClaudeCode(ClaudeCodeOptions{Runner: runner}).Execute(context.Background(), erkaruntime.BridgeRequest{Stage: erkaruntime.StagePlanning, WorkingDirectory: t.TempDir(), Prompt: "p", MaxOutputBytes: 1024})
	var providerErr *erkaruntime.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Code != "claude_code_auth" || providerErr.Retryable {
		t.Fatalf("error = %#v", err)
	}
	if len(runner.Invocations()) != 2 {
		t.Fatal("the agent process must not start without subscription authentication")
	}
}

func TestClaudeCodeStreamParsing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		stream  string
		wantErr string
		code    string
	}{
		{name: "falls back to result text", stream: `{"type":"result","subtype":"success","result":` + quoted(claudeCheckpoint) + `,"usage":{}}`},
		{name: "no result", stream: `{"type":"system","subtype":"init"}`, wantErr: "without a result"},
		{name: "not json", stream: "oops", wantErr: "line 1"},
		{name: "error result", stream: `{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["Reached max turns"]}`, code: "claude_code_turn_failed"},
		{name: "rate limited result", stream: `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"429 rate limit"}`, code: "claude_code_turn_failed"},
		{name: "checkpoint missing plan", stream: claudeResultLine(`{"objective":"x","constraints":[],"approved_plan":"","decisions":[],"relevant_files":[],"diff_summary":"","validation_results":[],"unresolved_problems":[],"suggested_next_action":""}`), wantErr: "no approved plan"},
		{name: "empty structured output", stream: `{"type":"result","subtype":"success","structured_output":null,"result":""}`, wantErr: "no structured output"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			result, err := parseClaudeStreamJSON(erkaruntime.StagePlanning, testCase.stream, 8192)
			switch {
			case testCase.code != "":
				var providerErr *erkaruntime.ProviderError
				if !errors.As(err, &providerErr) || providerErr.Code != testCase.code {
					t.Fatalf("error = %#v", err)
				}
			case testCase.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("error = %v, want %q", err, testCase.wantErr)
				}
			default:
				if err != nil || result.Checkpoint.ApprovedPlan == "" {
					t.Fatalf("result = %#v, %v", result, err)
				}
			}
		})
	}
	_, err := parseClaudeStreamJSON(erkaruntime.StagePlanning, `{"type":"result","is_error":true,"result":"429 rate limit"}`, 8192)
	var providerErr *erkaruntime.ProviderError
	if !errors.As(err, &providerErr) || !providerErr.Retryable {
		t.Fatalf("rate limit must allow fallback: %#v", err)
	}
	if _, err := parseClaudeStreamJSON(erkaruntime.StagePlanning, claudeResultLine(claudeCheckpoint), 16); err == nil {
		t.Fatal("oversized output was accepted")
	}
}

func TestClaudeCodeVersionParsing(t *testing.T) {
	t.Parallel()
	for input, want := range map[string][3]int{"2.1.270 (Claude Code)": {2, 1, 270}, "v3.0.1": {3, 0, 1}} {
		got, ok := parseClaudeVersion(input)
		if !ok || got != want {
			t.Errorf("parseClaudeVersion(%q) = %v, %v", input, got, ok)
		}
	}
	for _, input := range []string{"", "2.1", "a.b.c", "2.1.-1"} {
		if _, ok := parseClaudeVersion(input); ok {
			t.Errorf("parseClaudeVersion(%q) succeeded", input)
		}
	}
	if compareVersion([3]int{2, 1, 259}, MinimumClaudeCodeVersion) != 0 || compareVersion([3]int{2, 2, 0}, MinimumClaudeCodeVersion) <= 0 || compareVersion([3]int{1, 9, 999}, MinimumClaudeCodeVersion) >= 0 {
		t.Fatal("compareVersion ordering is wrong")
	}
}

func TestClaudeCodeNoticeAndLoginRouting(t *testing.T) {
	t.Parallel()
	bridge, err := New("claude_code", "")
	if err != nil {
		t.Fatal(err)
	}
	noticer, ok := bridge.(erkaruntime.BridgeNoticer)
	if !ok || !strings.Contains(noticer.Notice(), "Anthropic's current terms") || !strings.Contains(noticer.Notice(), "Experimental") {
		t.Fatalf("bridge notice = %v", bridge)
	}
	if NormalizeBridgeName(" Claude ") != "claude_code" || NormalizeBridgeName("codex") != "codex" {
		t.Fatal("bridge alias normalization failed")
	}
	if err := Login(context.Background(), "claude", "definitely-not-installed-claude", true, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "--device") {
		t.Fatalf("device login error = %v", err)
	}
	if err := Login(context.Background(), "claude", "definitely-not-installed-claude", false, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing CLI error = %v", err)
	}
}
