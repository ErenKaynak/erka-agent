package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const (
	defaultFileLimit    = int64(256 << 10)
	defaultOutputLimit  = int64(256 << 10)
	defaultCommandLimit = int64(1 << 20)
	defaultTimeout      = 30 * time.Second
	maxTimeout          = 10 * time.Minute
	maxEntries          = 10_000
)

// Builtins returns the complete M1 tool set. The returned order is stable so
// model requests and fixture tests remain deterministic.
func Builtins(sandbox erkaruntime.Sandbox) []erkaruntime.Tool {
	return []erkaruntime.Tool{
		&readFileTool{sandbox}, &listFilesTool{sandbox}, &searchTextTool{sandbox},
		&applyPatchTool{sandbox}, &runCommandTool{sandbox},
		&gitStatusTool{sandbox}, &gitDiffTool{sandbox},
	}
}

type readFileTool struct{ sandbox erkaruntime.Sandbox }

func (tool *readFileTool) Definition() erkaruntime.ToolDefinition {
	return definition("read_file", "Read one UTF-8 text file inside the workspace.",
		`{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string","minLength":1}}}`,
		false, true, defaultOutputLimit)
}

func (tool *readFileTool) Execute(ctx context.Context, invocation erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	var arguments struct {
		Path string `json:"path"`
	}
	if err := decode(invocation.Arguments, &arguments); err != nil {
		return toolError(err), nil
	}
	data, err := tool.sandbox.ReadFile(ctx, arguments.Path, defaultFileLimit)
	if err != nil {
		return resultFromError(err)
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return toolError(errors.New("file is not UTF-8 text")), nil
	}
	return bounded(string(data), false, defaultOutputLimit), nil
}

type listFilesTool struct{ sandbox erkaruntime.Sandbox }

func (tool *listFilesTool) Definition() erkaruntime.ToolDefinition {
	return definition("list_files", "List workspace files recursively without following symlinks.",
		`{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string"},"max_depth":{"type":"integer","minimum":1,"maximum":20}}}`,
		false, true, defaultOutputLimit)
}

func (tool *listFilesTool) Execute(ctx context.Context, invocation erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	arguments := struct {
		Path     string `json:"path"`
		MaxDepth int    `json:"max_depth"`
	}{Path: ".", MaxDepth: 8}
	if err := decode(invocation.Arguments, &arguments); err != nil {
		return toolError(err), nil
	}
	if arguments.Path == "" {
		arguments.Path = "."
	}
	if arguments.MaxDepth == 0 {
		arguments.MaxDepth = 8
	}
	entries, err := walk(ctx, tool.sandbox, arguments.Path, arguments.MaxDepth)
	if err != nil {
		return resultFromError(err)
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		suffix := ""
		if entry.Directory {
			suffix = "/"
		} else if entry.Symlink {
			suffix = "@"
		}
		lines = append(lines, entry.Path+suffix)
	}
	return bounded(strings.Join(lines, "\n"), false, defaultOutputLimit), nil
}

type searchTextTool struct{ sandbox erkaruntime.Sandbox }

func (tool *searchTextTool) Definition() erkaruntime.ToolDefinition {
	return definition("search_text", "Search UTF-8 workspace files for a literal string.",
		`{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string","minLength":1},"path":{"type":"string"},"case_sensitive":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"maximum":1000}}}`,
		false, true, defaultOutputLimit)
}

func (tool *searchTextTool) Execute(ctx context.Context, invocation erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	arguments := struct {
		Query         string `json:"query"`
		Path          string `json:"path"`
		CaseSensitive bool   `json:"case_sensitive"`
		MaxResults    int    `json:"max_results"`
	}{Path: ".", MaxResults: 100}
	if err := decode(invocation.Arguments, &arguments); err != nil {
		return toolError(err), nil
	}
	if arguments.Path == "" {
		arguments.Path = "."
	}
	if arguments.MaxResults == 0 {
		arguments.MaxResults = 100
	}
	entries, err := walk(ctx, tool.sandbox, arguments.Path, 20)
	if err != nil {
		return resultFromError(err)
	}
	query := arguments.Query
	if !arguments.CaseSensitive {
		query = strings.ToLower(query)
	}
	var matches []string
	for _, entry := range entries {
		if entry.Directory || entry.Symlink || entry.Size > defaultFileLimit {
			continue
		}
		data, readErr := tool.sandbox.ReadFile(ctx, entry.Path, defaultFileLimit)
		if readErr != nil {
			if ctx.Err() != nil {
				return erkaruntime.ToolExecutionResult{}, ctx.Err()
			}
			continue
		}
		if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
			continue
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for line := 1; scanner.Scan(); line++ {
			text := scanner.Text()
			haystack := text
			if !arguments.CaseSensitive {
				haystack = strings.ToLower(haystack)
			}
			if strings.Contains(haystack, query) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", entry.Path, line, text))
				if len(matches) >= arguments.MaxResults {
					return bounded(strings.Join(matches, "\n"), false, defaultOutputLimit), nil
				}
			}
		}
	}
	return bounded(strings.Join(matches, "\n"), false, defaultOutputLimit), nil
}

type applyPatchTool struct{ sandbox erkaruntime.Sandbox }

func (tool *applyPatchTool) Definition() erkaruntime.ToolDefinition {
	return definition("apply_patch", "Apply a unified text patch inside the workspace.",
		`{"type":"object","additionalProperties":false,"required":["patch"],"properties":{"patch":{"type":"string","minLength":1}}}`,
		true, false, defaultOutputLimit)
}

func (tool *applyPatchTool) Execute(ctx context.Context, invocation erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	var arguments struct {
		Patch string `json:"patch"`
	}
	if err := decode(invocation.Arguments, &arguments); err != nil {
		return toolError(err), nil
	}
	if err := validatePatch(arguments.Patch); err != nil {
		return toolError(err), nil
	}
	result, err := tool.sandbox.Run(ctx, erkaruntime.CommandRequest{
		Executable: "git", Arguments: []string{"apply", "--whitespace=nowarn", "--"}, Directory: ".",
		StandardInput: []byte(arguments.Patch), Timeout: defaultTimeout, OutputLimitBytes: defaultOutputLimit,
	})
	if err != nil {
		return resultFromError(err)
	}
	return commandOutput(result), nil
}

type runCommandTool struct{ sandbox erkaruntime.Sandbox }

func (tool *runCommandTool) Definition() erkaruntime.ToolDefinition {
	return definition("run_command", "Run one executable directly in the workspace without a shell.",
		`{"type":"object","additionalProperties":false,"required":["command"],"properties":{"command":{"type":"array","minItems":1,"maxItems":128,"items":{"type":"string"}},"directory":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600}}}`,
		true, false, defaultCommandLimit)
}

func (tool *runCommandTool) Execute(ctx context.Context, invocation erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	arguments := struct {
		Command        []string `json:"command"`
		Directory      string   `json:"directory"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}{Directory: ".", TimeoutSeconds: int(defaultTimeout / time.Second)}
	if err := decode(invocation.Arguments, &arguments); err != nil {
		return toolError(err), nil
	}
	if len(arguments.Command) == 0 || strings.TrimSpace(arguments.Command[0]) == "" {
		return toolError(errors.New("command requires an executable")), nil
	}
	if arguments.Directory == "" {
		arguments.Directory = "."
	}
	if arguments.TimeoutSeconds == 0 {
		arguments.TimeoutSeconds = int(defaultTimeout / time.Second)
	}
	timeout := time.Duration(arguments.TimeoutSeconds) * time.Second
	if timeout > maxTimeout {
		return toolError(errors.New("command timeout exceeds 600 seconds")), nil
	}
	result, err := tool.sandbox.Run(ctx, erkaruntime.CommandRequest{
		Executable: arguments.Command[0], Arguments: arguments.Command[1:], Directory: arguments.Directory,
		Timeout: timeout, OutputLimitBytes: defaultCommandLimit,
	})
	if err != nil {
		return resultFromError(err)
	}
	return commandOutput(result), nil
}

type gitStatusTool struct{ sandbox erkaruntime.Sandbox }

func (tool *gitStatusTool) Definition() erkaruntime.ToolDefinition {
	return definition("git_status", "Show concise Git workspace status.",
		`{"type":"object","additionalProperties":false}`, false, true, defaultOutputLimit)
}

func (tool *gitStatusTool) Execute(ctx context.Context, invocation erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	if err := decode(invocation.Arguments, &struct{}{}); err != nil {
		return toolError(err), nil
	}
	return executeCommand(ctx, tool.sandbox, []string{"git", "status", "--short", "--branch", "--untracked-files=all"}, ".", defaultOutputLimit)
}

type gitDiffTool struct{ sandbox erkaruntime.Sandbox }

func (tool *gitDiffTool) Definition() erkaruntime.ToolDefinition {
	return definition("git_diff", "Show an unstaged or staged Git diff, optionally for one path.",
		`{"type":"object","additionalProperties":false,"properties":{"staged":{"type":"boolean"},"path":{"type":"string"}}}`,
		false, true, defaultOutputLimit)
}

func (tool *gitDiffTool) Execute(ctx context.Context, invocation erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	var arguments struct {
		Staged bool   `json:"staged"`
		Path   string `json:"path"`
	}
	if err := decode(invocation.Arguments, &arguments); err != nil {
		return toolError(err), nil
	}
	command := []string{"git", "diff", "--no-ext-diff", "--no-color"}
	if arguments.Staged {
		command = append(command, "--cached")
	}
	command = append(command, "--")
	if arguments.Path != "" {
		if err := validateRelative(arguments.Path); err != nil {
			return toolError(err), nil
		}
		command = append(command, arguments.Path)
	}
	return executeCommand(ctx, tool.sandbox, command, ".", defaultOutputLimit)
}

func definition(name, description, schema string, mutating, parallel bool, limit int64) erkaruntime.ToolDefinition {
	return erkaruntime.ToolDefinition{Name: name, Description: description, InputSchema: json.RawMessage(schema), Mutating: mutating, ParallelSafe: parallel, OutputLimitBytes: limit}
}

func decode(data json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid arguments: expected one JSON object")
	}
	return nil
}

func walk(ctx context.Context, sandbox erkaruntime.Sandbox, root string, maxDepth int) ([]erkaruntime.FileEntry, error) {
	type pendingDirectory struct {
		path  string
		depth int
	}
	pending := []pendingDirectory{{root, 0}}
	entries := make([]erkaruntime.FileEntry, 0)
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current := pending[0]
		pending = pending[1:]
		children, err := sandbox.ListDir(ctx, current.path, maxEntries)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if path.Base(child.Path) == ".git" {
				continue
			}
			entries = append(entries, child)
			if len(entries) > maxEntries {
				return nil, errors.New("workspace entry limit exceeded")
			}
			if child.Directory && !child.Symlink && current.depth+1 < maxDepth {
				pending = append(pending, pendingDirectory{child.Path, current.depth + 1})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func validatePatch(patchText string) error {
	if int64(len(patchText)) > defaultCommandLimit {
		return errors.New("patch exceeds 1 MiB")
	}
	if strings.Contains(patchText, "GIT binary patch") || strings.Contains(patchText, "Binary files ") {
		return errors.New("binary patches are not supported")
	}
	found := false
	for _, line := range strings.Split(patchText, "\n") {
		var value string
		switch {
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
			value = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "rename from "):
			value = strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to "):
			value = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "copy from "):
			value = strings.TrimPrefix(line, "copy from ")
		case strings.HasPrefix(line, "copy to "):
			value = strings.TrimPrefix(line, "copy to ")
		default:
			continue
		}
		found = true
		if value == "/dev/null" {
			continue
		}
		if strings.HasPrefix(value, `"`) || strings.ContainsAny(value, "\t\r") {
			return errors.New("quoted or tab-separated patch paths are not supported")
		}
		if strings.HasPrefix(value, "a/") || strings.HasPrefix(value, "b/") {
			value = value[2:]
		}
		if err := validateRelative(value); err != nil {
			return fmt.Errorf("invalid patch path %q: %w", value, err)
		}
	}
	if !found {
		return errors.New("patch contains no file headers")
	}
	return nil
}

func validateRelative(value string) error {
	normalized := strings.ReplaceAll(value, `\`, "/")
	cleaned := path.Clean(normalized)
	if cleaned == "." || path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "//") || (len(cleaned) > 1 && cleaned[1] == ':') {
		return errors.New("path must stay inside the workspace")
	}
	return nil
}

func executeCommand(ctx context.Context, sandbox erkaruntime.Sandbox, command []string, directory string, limit int64) (erkaruntime.ToolExecutionResult, error) {
	result, err := sandbox.Run(ctx, erkaruntime.CommandRequest{Executable: command[0], Arguments: command[1:], Directory: directory, Timeout: defaultTimeout, OutputLimitBytes: limit})
	if err != nil {
		return resultFromError(err)
	}
	return commandOutput(result), nil
}

func commandOutput(result erkaruntime.CommandResult) erkaruntime.ToolExecutionResult {
	var output strings.Builder
	if result.StandardOutput != "" {
		output.WriteString(result.StandardOutput)
	}
	if result.StandardError != "" {
		if output.Len() > 0 && !strings.HasSuffix(output.String(), "\n") {
			output.WriteByte('\n')
		}
		output.WriteString(result.StandardError)
	}
	if output.Len() == 0 || result.ExitCode != 0 {
		if output.Len() > 0 && !strings.HasSuffix(output.String(), "\n") {
			output.WriteByte('\n')
		}
		output.WriteString("exit_code=")
		output.WriteString(strconv.Itoa(result.ExitCode))
	}
	return erkaruntime.ToolExecutionResult{Output: output.String(), IsError: result.ExitCode != 0, Truncated: result.Truncated}
}

func resultFromError(err error) (erkaruntime.ToolExecutionResult, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return erkaruntime.ToolExecutionResult{}, err
	}
	return toolError(err), nil
}

func toolError(err error) erkaruntime.ToolExecutionResult {
	return erkaruntime.ToolExecutionResult{Output: err.Error(), IsError: true}
}

func bounded(value string, isError bool, limit int64) erkaruntime.ToolExecutionResult {
	if int64(len(value)) <= limit {
		return erkaruntime.ToolExecutionResult{Output: value, IsError: isError}
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return erkaruntime.ToolExecutionResult{Output: value, IsError: isError, Truncated: true}
}

var _ erkaruntime.Tool = (*readFileTool)(nil)
var _ erkaruntime.Tool = (*listFilesTool)(nil)
var _ erkaruntime.Tool = (*searchTextTool)(nil)
var _ erkaruntime.Tool = (*applyPatchTool)(nil)
var _ erkaruntime.Tool = (*runCommandTool)(nil)
var _ erkaruntime.Tool = (*gitStatusTool)(nil)
var _ erkaruntime.Tool = (*gitDiffTool)(nil)
