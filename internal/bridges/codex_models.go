package bridges

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const (
	codexCatalogOutputLimit = 4 << 20
	codexCatalogPageLimit   = 32
	codexCatalogModelLimit  = 4096
)

type CodexCatalogRunner func(context.Context, string, string, map[string]string) ([]erkaruntime.ModelInfo, error)

type codexRPCEnvelope struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type codexModelListResult struct {
	Data []struct {
		ID          string `json:"id"`
		Model       string `json:"model"`
		DisplayName string `json:"displayName"`
		Description string `json:"description"`
		Hidden      bool   `json:"hidden"`
		Default     bool   `json:"isDefault"`
	} `json:"data"`
	NextCursor *string `json:"nextCursor"`
}

func runCodexModelCatalog(ctx context.Context, executable, directory string, environment map[string]string) ([]erkaruntime.ModelInfo, error) {
	command := exec.CommandContext(ctx, executable, "app-server", "--listen", "stdio://")
	command.Dir = directory
	command.Env = sortedEnvironment(environment)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Codex app-server stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open Codex app-server stdout: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open Codex app-server stderr: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	var stderrCopy cappedOutput
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderrCopy, stderr)
		close(stderrDone)
	}()
	defer func() {
		_ = stdin.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
		<-stderrDone
	}()

	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), codexCatalogOutputLimit)
	readBytes := 0
	requestID := 1
	if err := encoder.Encode(map[string]any{
		"id": requestID, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]string{"name": "erka", "version": "1"}},
	}); err != nil {
		return nil, fmt.Errorf("initialize Codex app-server: %w", err)
	}
	if _, err := readCodexRPCResponse(ctx, scanner, requestID, &readBytes); err != nil {
		return nil, catalogProtocolError("initialize", err, stderrCopy.String())
	}
	if err := encoder.Encode(map[string]any{"method": "initialized"}); err != nil {
		return nil, fmt.Errorf("acknowledge Codex app-server initialization: %w", err)
	}

	models := make([]erkaruntime.ModelInfo, 0)
	seen := make(map[string]bool)
	var cursor *string
	for page := 0; page < codexCatalogPageLimit; page++ {
		requestID++
		params := map[string]any{"limit": 200, "includeHidden": false}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		if err := encoder.Encode(map[string]any{"id": requestID, "method": "model/list", "params": params}); err != nil {
			return nil, fmt.Errorf("request Codex model list: %w", err)
		}
		result, err := readCodexRPCResponse(ctx, scanner, requestID, &readBytes)
		if err != nil {
			return nil, catalogProtocolError("model/list", err, stderrCopy.String())
		}
		var pageResult codexModelListResult
		if err := json.Unmarshal(result, &pageResult); err != nil {
			return nil, fmt.Errorf("decode Codex model list: %w", err)
		}
		for _, item := range pageResult.Data {
			id := strings.TrimSpace(item.Model)
			if id == "" {
				id = strings.TrimSpace(item.ID)
			}
			model := erkaruntime.ModelInfo{ID: id, DisplayName: strings.TrimSpace(item.DisplayName), Description: strings.TrimSpace(item.Description), Default: item.Default}
			if item.Hidden || model.Validate() != nil || seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, model)
			if len(models) > codexCatalogModelLimit {
				return nil, errors.New("Codex model catalog exceeded 4096 models")
			}
		}
		cursor = pageResult.NextCursor
		if cursor == nil || strings.TrimSpace(*cursor) == "" {
			break
		}
		if page == codexCatalogPageLimit-1 {
			return nil, errors.New("Codex model catalog exceeded 32 pages")
		}
	}
	if len(models) == 0 {
		return nil, errors.New("Codex model catalog contained no valid models")
	}
	sort.SliceStable(models, func(left, right int) bool {
		if models[left].Default != models[right].Default {
			return models[left].Default
		}
		return models[left].ID < models[right].ID
	})
	return models, nil
}

func readCodexRPCResponse(ctx context.Context, scanner *bufio.Scanner, requestID int, readBytes *int) (json.RawMessage, error) {
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := scanner.Bytes()
		*readBytes += len(line) + 1
		if *readBytes > codexCatalogOutputLimit {
			return nil, errors.New("Codex model catalog output exceeded 4 MiB")
		}
		var envelope codexRPCEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			return nil, fmt.Errorf("malformed Codex app-server message: %w", err)
		}
		if envelope.ID != requestID {
			continue
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			return nil, fmt.Errorf("Codex app-server error: %s", strings.TrimSpace(string(envelope.Error)))
		}
		if len(envelope.Result) == 0 {
			return nil, errors.New("Codex app-server response omitted its result")
		}
		return envelope.Result, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Codex app-server output: %w", err)
	}
	return nil, errors.New("Codex app-server exited before responding")
}

func catalogProtocolError(method string, err error, stderr string) error {
	if value := strings.TrimSpace(stderr); value != "" {
		return fmt.Errorf("Codex app-server %s failed: %w (%s)", method, err, value)
	}
	return fmt.Errorf("Codex app-server %s failed: %w", method, err)
}

func sortedEnvironment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

type cappedOutput struct {
	mu     sync.Mutex
	buffer strings.Builder
}

func (output *cappedOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	originalLength := len(data)
	remaining := (64 << 10) - output.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = output.buffer.Write(data)
	}
	return originalLength, nil
}

func (output *cappedOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.String()
}
