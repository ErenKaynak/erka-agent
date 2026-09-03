package bridges

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type CodexOptions struct {
	Executable string
	Runner     Runner
}

type Codex struct {
	executable string
	runner     Runner
}

func NewCodex(options CodexOptions) *Codex {
	executable := strings.TrimSpace(options.Executable)
	if executable == "" {
		executable = "codex"
	}
	runner := options.Runner
	if runner == nil {
		runner = LocalRunner{}
	}
	return &Codex{executable: executable, runner: runner}
}

func (*Codex) Name() string { return "codex" }

func (*Codex) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true, StructuredOutput: true}, nil
}

func (bridge *Codex) Availability(ctx context.Context) (erkaruntime.BridgeAvailability, error) {
	path, err := bridge.runner.LookPath(bridge.executable)
	if err != nil {
		return erkaruntime.BridgeAvailability{Detail: "Codex CLI is not installed"}, nil
	}
	directory, err := os.Getwd()
	if err != nil {
		return erkaruntime.BridgeAvailability{}, err
	}
	version, err := bridge.runner.Run(ctx, Invocation{Executable: path, Arguments: []string{"--version"}, Directory: directory, OutputLimitBytes: 8 << 10})
	if err != nil {
		return erkaruntime.BridgeAvailability{}, err
	}
	result := erkaruntime.BridgeAvailability{Available: version.ExitCode == 0, Version: strings.TrimSpace(version.Stdout)}
	if !result.Available {
		result.Detail = strings.TrimSpace(version.Stderr)
		return result, nil
	}
	status, err := bridge.runner.Run(ctx, Invocation{
		Executable: path, Arguments: []string{"login", "status"}, Directory: directory,
		Environment: selectedEnvironment("CODEX_HOME", "CODEX_CA_CERTIFICATE", "SSL_CERT_FILE"), OutputLimitBytes: 16 << 10,
	})
	if err != nil {
		return erkaruntime.BridgeAvailability{}, err
	}
	message := strings.TrimSpace(status.Stdout + "\n" + status.Stderr)
	result.Authenticated = status.ExitCode == 0 && strings.Contains(strings.ToLower(message), "logged in using chatgpt")
	if result.Authenticated {
		result.Detail = "authenticated with ChatGPT"
	} else if status.ExitCode == 0 {
		result.Detail = "Codex is not using ChatGPT subscription authentication"
	} else {
		result.Detail = "Codex is not authenticated"
	}
	return result, nil
}

func (bridge *Codex) Execute(ctx context.Context, request erkaruntime.BridgeRequest) (erkaruntime.BridgeResult, error) {
	availability, err := bridge.Availability(ctx)
	if err != nil {
		return erkaruntime.BridgeResult{}, err
	}
	if !availability.Available {
		return erkaruntime.BridgeResult{}, providerFailure("codex_unavailable", availability.Detail, true)
	}
	if !availability.Authenticated {
		return erkaruntime.BridgeResult{}, providerFailure("codex_auth", availability.Detail, false)
	}
	schema, err := os.CreateTemp("", "erka-codex-checkpoint-*.json")
	if err != nil {
		return erkaruntime.BridgeResult{}, fmt.Errorf("create Codex checkpoint schema: %w", err)
	}
	schemaName := schema.Name()
	defer os.Remove(schemaName)
	if err := schema.Chmod(0o600); err == nil {
		_, err = schema.Write(checkpointSchema)
	}
	if closeErr := schema.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return erkaruntime.BridgeResult{}, fmt.Errorf("write Codex checkpoint schema: %w", err)
	}
	path, err := bridge.runner.LookPath(bridge.executable)
	if err != nil {
		return erkaruntime.BridgeResult{}, providerFailure("codex_unavailable", err.Error(), true)
	}
	arguments := []string{
		"exec", "--json", "--ephemeral", "--sandbox", "read-only",
		"--ignore-user-config", "--ignore-rules", "--color", "never",
		"--output-schema", schemaName, "-C", request.WorkingDirectory,
	}
	if request.Model != "" && request.Model != "default" {
		arguments = append(arguments, "--model", request.Model)
	}
	arguments = append(arguments, "-")
	process, err := bridge.runner.Run(ctx, Invocation{
		Executable: path, Arguments: arguments, Directory: request.WorkingDirectory,
		Environment: selectedEnvironment("CODEX_HOME", "CODEX_CA_CERTIFICATE", "SSL_CERT_FILE"),
		Input:       request.Prompt, OutputLimitBytes: outputBudget(request.MaxOutputBytes),
	})
	if err != nil {
		return erkaruntime.BridgeResult{}, err
	}
	if process.Truncated {
		return erkaruntime.BridgeResult{}, providerFailure("codex_output_limit", "Codex bridge process output exceeded its limit", false)
	}
	if process.ExitCode != 0 {
		return erkaruntime.BridgeResult{}, providerFailure("codex_failed", process.Stderr, retryableProcessFailure(process.Stderr))
	}
	return parseCodexJSONL(request.Stage, process.Stdout, request.MaxOutputBytes)
}

type codexEvent struct {
	Type string `json:"type"`
	Item struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		CachedInput  int64 `json:"cached_input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Message string          `json:"message"`
	Error   json.RawMessage `json:"error"`
}

func parseCodexJSONL(stage erkaruntime.Stage, stream string, maxOutputBytes int64) (erkaruntime.BridgeResult, error) {
	scanner := bufio.NewScanner(strings.NewReader(stream))
	max := int(maxOutputBytes + (64 << 10))
	if max < 128<<10 {
		max = 128 << 10
	}
	scanner.Buffer(make([]byte, 64<<10), max)
	var output string
	var usage erkaruntime.Usage
	completed := false
	line := 0
	for scanner.Scan() {
		line++
		data := strings.TrimSpace(scanner.Text())
		if data == "" {
			continue
		}
		var event codexEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return erkaruntime.BridgeResult{}, fmt.Errorf("%w: Codex JSONL line %d: %v", ErrMalformedStream, line, err)
		}
		switch event.Type {
		case "item.completed":
			if event.Item.Type == "agent_message" {
				output = event.Item.Text
			}
		case "turn.completed":
			completed = true
			usage.InputTokens += event.Usage.InputTokens
			usage.CacheReadTokens += event.Usage.CachedInput
			usage.OutputTokens += event.Usage.OutputTokens
		case "turn.failed", "error":
			message := event.Message
			if message == "" {
				message = string(event.Error)
			}
			return erkaruntime.BridgeResult{}, providerFailure("codex_turn_failed", message, retryableProcessFailure(message))
		}
	}
	if err := scanner.Err(); err != nil {
		return erkaruntime.BridgeResult{}, fmt.Errorf("%w: read Codex JSONL: %v", ErrMalformedStream, err)
	}
	if !completed || strings.TrimSpace(output) == "" {
		return erkaruntime.BridgeResult{}, fmt.Errorf("%w: Codex stream ended without a completed turn and final message", ErrMalformedStream)
	}
	if maxOutputBytes > 0 && int64(len(output)) > maxOutputBytes {
		return erkaruntime.BridgeResult{}, providerFailure("codex_output_limit", "Codex final message exceeded its limit", false)
	}
	checkpoint, err := decodeCheckpoint(stage, output)
	if err != nil {
		return erkaruntime.BridgeResult{}, err
	}
	return erkaruntime.BridgeResult{Output: output, Checkpoint: checkpoint, Usage: usage}, nil
}

func retryableProcessFailure(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{"timeout", "timed out", "rate limit", "temporarily unavailable", "connection reset", "connection refused", "503", "429"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

var _ erkaruntime.SubscriptionBridge = (*Codex)(nil)
