package bridges

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

// ClaudeCodeNotice is shown wherever the experimental Claude Code bridge is
// configured, logged into, or used.
const ClaudeCodeNotice = "Experimental: the Claude Code bridge runs the official `claude` CLI with your own Claude login; erka never reads or forwards Claude credentials. Headless (`claude -p`) use may draw from a separate allowance or be billed at API rates, and is subject to Anthropic's current terms."

// MinimumClaudeCodeVersion is the oldest CLI that supports every flag the
// bridge relies on (--restricted, --json-schema, --permission-prompts).
var MinimumClaudeCodeVersion = [3]int{2, 1, 259}

// claudeCodeEnvironment is the complete set of variables forwarded to the
// CLI on top of the sandbox's base allowlist. It deliberately contains no
// credential variables.
var claudeCodeEnvironment = []string{"CLAUDE_CONFIG_DIR", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"}

// claudeCodeForbiddenEnvironment must never reach the CLI: any of them would
// switch it from the user's subscription login to a credential erka holds.
var claudeCodeForbiddenEnvironment = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

type ClaudeCodeOptions struct {
	Executable string
	Runner     Runner
}

// ClaudeCode is an experimental, opt-in bridge that drives the official
// Claude Code CLI in headless, restricted, read-only mode.
type ClaudeCode struct {
	executable string
	runner     Runner
}

func NewClaudeCode(options ClaudeCodeOptions) *ClaudeCode {
	executable := strings.TrimSpace(options.Executable)
	if executable == "" {
		executable = "claude"
	}
	runner := options.Runner
	if runner == nil {
		runner = LocalRunner{}
	}
	return &ClaudeCode{executable: executable, runner: runner}
}

func (*ClaudeCode) Name() string { return "claude_code" }

// Notice implements the runtime's optional bridge notice.
func (*ClaudeCode) Notice() string { return ClaudeCodeNotice }

func (*ClaudeCode) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true, StructuredOutput: true}, nil
}

func claudeEnvironment() map[string]string {
	environment := selectedEnvironment(claudeCodeEnvironment...)
	for _, name := range claudeCodeForbiddenEnvironment {
		delete(environment, name)
	}
	return environment
}

func (bridge *ClaudeCode) Availability(ctx context.Context) (erkaruntime.BridgeAvailability, error) {
	path, err := bridge.runner.LookPath(bridge.executable)
	if err != nil {
		return erkaruntime.BridgeAvailability{Detail: "Claude Code CLI is not installed"}, nil
	}
	directory, err := os.Getwd()
	if err != nil {
		return erkaruntime.BridgeAvailability{}, err
	}
	version, err := bridge.runner.Run(ctx, Invocation{Executable: path, Arguments: []string{"--version"}, Directory: directory, Environment: claudeEnvironment(), OutputLimitBytes: 8 << 10})
	if err != nil {
		return erkaruntime.BridgeAvailability{}, err
	}
	result := erkaruntime.BridgeAvailability{Version: strings.TrimSpace(version.Stdout)}
	if version.ExitCode != 0 {
		result.Detail = "Claude Code CLI did not report a version"
		return result, nil
	}
	parsed, ok := parseClaudeVersion(version.Stdout)
	if !ok || compareVersion(parsed, MinimumClaudeCodeVersion) < 0 {
		result.Detail = fmt.Sprintf("Claude Code CLI %d.%d.%d or newer is required; run `claude update`", MinimumClaudeCodeVersion[0], MinimumClaudeCodeVersion[1], MinimumClaudeCodeVersion[2])
		return result, nil
	}
	result.Available = true
	status, err := bridge.runner.Run(ctx, Invocation{Executable: path, Arguments: []string{"auth", "status"}, Directory: directory, Environment: claudeEnvironment(), OutputLimitBytes: 16 << 10})
	if err != nil {
		return erkaruntime.BridgeAvailability{}, err
	}
	var auth struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	decodeErr := json.Unmarshal(bytes.TrimSpace([]byte(status.Stdout)), &auth)
	switch {
	case decodeErr != nil:
		result.Detail = "Claude Code authentication status is unreadable; run `erka auth login claude`"
	case status.ExitCode != 0 || auth.AuthMethod == "none" || (auth.LoggedIn != nil && !*auth.LoggedIn):
		result.Detail = "Claude Code is not authenticated; run `erka auth login claude`"
	case auth.AuthMethod != "claude.ai":
		result.Detail = "Claude Code is not using Claude subscription authentication"
	default:
		result.Authenticated = true
		result.Detail = "authenticated with a Claude subscription (experimental)"
	}
	return result, nil
}

func (bridge *ClaudeCode) arguments(request erkaruntime.BridgeRequest) ([]string, error) {
	var schema bytes.Buffer
	if err := json.Compact(&schema, checkpointSchema); err != nil {
		return nil, err
	}
	arguments := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--restricted", "--tools", "Read,Grep,Glob",
		"--disallowedTools", "mcp__*", "--strict-mcp-config",
		"--permission-prompts", "none", "--no-session-persistence",
		"--json-schema", schema.String(),
	}
	if request.Model != "" && request.Model != "default" {
		arguments = append(arguments, "--model", request.Model)
	}
	return arguments, nil
}

func (bridge *ClaudeCode) Execute(ctx context.Context, request erkaruntime.BridgeRequest) (erkaruntime.BridgeResult, error) {
	availability, err := bridge.Availability(ctx)
	if err != nil {
		return erkaruntime.BridgeResult{}, err
	}
	if !availability.Available {
		return erkaruntime.BridgeResult{}, providerFailure("claude_code_unavailable", availability.Detail, true)
	}
	if !availability.Authenticated {
		return erkaruntime.BridgeResult{}, providerFailure("claude_code_auth", availability.Detail, false)
	}
	path, err := bridge.runner.LookPath(bridge.executable)
	if err != nil {
		return erkaruntime.BridgeResult{}, providerFailure("claude_code_unavailable", err.Error(), true)
	}
	arguments, err := bridge.arguments(request)
	if err != nil {
		return erkaruntime.BridgeResult{}, err
	}
	process, err := bridge.runner.Run(ctx, Invocation{
		Executable: path, Arguments: arguments, Directory: request.WorkingDirectory,
		Environment: claudeEnvironment(), Input: request.Prompt, OutputLimitBytes: outputBudget(request.MaxOutputBytes),
	})
	if err != nil {
		return erkaruntime.BridgeResult{}, err
	}
	if process.Truncated {
		return erkaruntime.BridgeResult{}, providerFailure("claude_code_output_limit", "Claude Code bridge process output exceeded its limit", false)
	}
	result, parseErr := parseClaudeStreamJSON(request.Stage, process.Stdout, request.MaxOutputBytes)
	if parseErr == nil {
		return result, nil
	}
	if process.ExitCode != 0 {
		return erkaruntime.BridgeResult{}, providerFailure("claude_code_failed", process.Stderr, retryableProcessFailure(process.Stderr))
	}
	return erkaruntime.BridgeResult{}, parseErr
}

type claudeStreamEvent struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Errors           []string        `json:"errors"`
	Usage            struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func parseClaudeStreamJSON(stage erkaruntime.Stage, stream string, maxOutputBytes int64) (erkaruntime.BridgeResult, error) {
	scanner := bufio.NewScanner(strings.NewReader(stream))
	limit := int(maxOutputBytes + (256 << 10))
	if limit < 512<<10 {
		limit = 512 << 10
	}
	scanner.Buffer(make([]byte, 64<<10), limit)
	var final *claudeStreamEvent
	line := 0
	for scanner.Scan() {
		line++
		data := bytes.TrimSpace(scanner.Bytes())
		if len(data) == 0 {
			continue
		}
		var event claudeStreamEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return erkaruntime.BridgeResult{}, fmt.Errorf("%w: Claude Code stream line %d: %v", ErrMalformedStream, line, err)
		}
		if event.Type == "result" {
			copy := event
			final = &copy
		}
	}
	if err := scanner.Err(); err != nil {
		return erkaruntime.BridgeResult{}, fmt.Errorf("%w: read Claude Code stream: %v", ErrMalformedStream, err)
	}
	if final == nil {
		return erkaruntime.BridgeResult{}, fmt.Errorf("%w: Claude Code stream ended without a result", ErrMalformedStream)
	}
	if final.IsError || (final.Subtype != "" && final.Subtype != "success") {
		message := strings.Join(final.Errors, "; ")
		if message == "" {
			message = final.Result
		}
		if message == "" {
			message = "Claude Code run failed (" + final.Subtype + ")"
		}
		return erkaruntime.BridgeResult{}, providerFailure("claude_code_turn_failed", message, retryableProcessFailure(message))
	}
	output := strings.TrimSpace(string(final.StructuredOutput))
	if output == "" || output == "null" {
		output = strings.TrimSpace(final.Result)
	}
	if output == "" {
		return erkaruntime.BridgeResult{}, fmt.Errorf("%w: Claude Code result has no structured output", ErrMalformedStream)
	}
	if maxOutputBytes > 0 && int64(len(output)) > maxOutputBytes {
		return erkaruntime.BridgeResult{}, providerFailure("claude_code_output_limit", "Claude Code final message exceeded its limit", false)
	}
	checkpoint, err := decodeCheckpoint(stage, output)
	if err != nil {
		return erkaruntime.BridgeResult{}, err
	}
	usage := final.Usage
	return erkaruntime.BridgeResult{Output: output, Checkpoint: checkpoint, Usage: erkaruntime.Usage{
		InputTokens:      usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadInputTokens,
		CacheWriteTokens: usage.CacheCreationInputTokens,
	}}, nil
}

// parseClaudeVersion reads the leading semantic version from output such as
// "2.1.270 (Claude Code)".
func parseClaudeVersion(output string) ([3]int, bool) {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return [3]int{}, false
	}
	parts := strings.Split(strings.TrimPrefix(fields[0], "v"), ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var version [3]int
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return [3]int{}, false
		}
		version[index] = value
	}
	return version, true
}

func compareVersion(left, right [3]int) int {
	for index := range left {
		if left[index] != right[index] {
			if left[index] < right[index] {
				return -1
			}
			return 1
		}
	}
	return 0
}

var _ erkaruntime.SubscriptionBridge = (*ClaudeCode)(nil)
