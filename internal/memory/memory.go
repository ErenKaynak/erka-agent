// Package memory provides erka's provider-neutral long-term memory: durable
// notes recalled into every model's context and editable by the user.
package memory

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

const (
	recallLimit       = 8
	preferenceLimit   = 10
	maxRecallBytes    = 4 << 10
	searchResultLimit = 20
)

type Store interface {
	SaveMemory(context.Context, store.MemoryNote) (store.MemoryNote, error)
	SearchMemories(context.Context, string, string, int) ([]store.MemoryNote, error)
	Preferences(context.Context, int) ([]store.MemoryNote, error)
}

// ProjectKey identifies a repository independently of its checkout, so a
// server task worktree shares memory with its source repository. It reads the
// Git common directory from files and never runs Git.
func ProjectKey(root string) (string, error) {
	marker := filepath.Join(root, ".git")
	info, err := os.Lstat(marker)
	if err != nil {
		return "", fmt.Errorf("inspect repository marker: %w", err)
	}
	common := marker
	if !info.IsDir() {
		gitDir, err := readGitDirFile(marker)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
		common = gitDir
		if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
			relative := strings.TrimSpace(string(data))
			if filepath.IsAbs(relative) {
				common = relative
			} else {
				common = filepath.Join(gitDir, relative)
			}
		}
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(common))
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory: %w", err)
	}
	sum := sha256.Sum256([]byte(resolved))
	return "repo_" + hex.EncodeToString(sum[:16]), nil
}

func readGitDirFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		if value, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "gitdir:"); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", errors.New(".git file does not name a gitdir")
}

// Recall renders the memories relevant to an objective: every user
// preference plus the best full-text matches, bounded in size.
func Recall(ctx context.Context, memories Store, projectKey, objective string) (string, error) {
	preferences, err := memories.Preferences(ctx, preferenceLimit)
	if err != nil {
		return "", err
	}
	matches, err := memories.SearchMemories(ctx, projectKey, objective, recallLimit)
	if err != nil {
		return "", err
	}
	seen := make(map[string]bool)
	var builder strings.Builder
	for _, note := range append(preferences, matches...) {
		if seen[note.ID] {
			continue
		}
		seen[note.ID] = true
		line := "- " + Describe(note) + "\n"
		if builder.Len()+len(line) > maxRecallBytes {
			break
		}
		builder.WriteString(line)
	}
	return strings.TrimSpace(builder.String()), nil
}

// Describe renders one memory as a single labeled line.
func Describe(note store.MemoryNote) string {
	label := note.Scope + " " + note.Kind
	if note.Origin == "model" {
		label += ", unverified"
	}
	return "[" + label + "] " + strings.Join(strings.Fields(note.Text), " ")
}

// Tools exposes memory search to every stage and project-scoped saves. Models
// cannot write global memory: global notes follow the user everywhere, so only
// the user adds them.
func Tools(memories Store, projectKey string, taskID erkaruntime.TaskID, ids erkaruntime.IDGenerator, clock func() time.Time) []erkaruntime.Tool {
	if ids == nil {
		ids = erkaruntime.RandomIDGenerator{}
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return []erkaruntime.Tool{
		&searchTool{memories: memories, projectKey: projectKey},
		&saveTool{memories: memories, projectKey: projectKey, taskID: taskID, ids: ids, clock: clock},
	}
}

type searchTool struct {
	memories   Store
	projectKey string
}

func (*searchTool) Definition() erkaruntime.ToolDefinition {
	return erkaruntime.ToolDefinition{
		Name: "memory_search", Description: "Search long-term memory saved across earlier sessions and models for facts, preferences, and decisions about this project or the user.",
		InputSchema:      json.RawMessage(`{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string","minLength":1,"maxLength":500}}}`),
		ParallelSafe:     true,
		OutputLimitBytes: 16 << 10,
	}
}

func (tool *searchTool) Execute(ctx context.Context, invocation erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	var arguments struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(invocation.Arguments, &arguments); err != nil {
		return erkaruntime.ToolExecutionResult{Output: "invalid arguments: " + err.Error(), IsError: true}, nil
	}
	notes, err := tool.memories.SearchMemories(ctx, tool.projectKey, arguments.Query, searchResultLimit)
	if err != nil {
		return erkaruntime.ToolExecutionResult{}, err
	}
	if len(notes) == 0 {
		return erkaruntime.ToolExecutionResult{Output: "No matching memories."}, nil
	}
	lines := make([]string, len(notes))
	for index, note := range notes {
		lines[index] = "- " + Describe(note)
	}
	return erkaruntime.ToolExecutionResult{Output: strings.Join(lines, "\n")}, nil
}

type saveTool struct {
	memories   Store
	projectKey string
	taskID     erkaruntime.TaskID
	ids        erkaruntime.IDGenerator
	clock      func() time.Time
}

func (*saveTool) Definition() erkaruntime.ToolDefinition {
	return erkaruntime.ToolDefinition{
		Name:             "memory_save",
		Description:      "Save one durable project note for future sessions and other models: a fact, convention, or decision that is not obvious from the code. Do not save secrets, transient status, or things already in the repository.",
		InputSchema:      json.RawMessage(`{"type":"object","additionalProperties":false,"required":["text","kind"],"properties":{"text":{"type":"string","minLength":1,"maxLength":2000},"kind":{"type":"string","enum":["fact","preference","decision"]}}}`),
		OutputLimitBytes: 4 << 10,
	}
}

func (tool *saveTool) Execute(ctx context.Context, invocation erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	var arguments struct {
		Text string `json:"text"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(invocation.Arguments, &arguments); err != nil {
		return erkaruntime.ToolExecutionResult{Output: "invalid arguments: " + err.Error(), IsError: true}, nil
	}
	id, err := tool.ids.NewID("memory")
	if err != nil {
		return erkaruntime.ToolExecutionResult{}, err
	}
	now := tool.clock().UTC()
	note, err := tool.memories.SaveMemory(ctx, store.MemoryNote{ID: id, Scope: store.MemoryProject, ProjectKey: tool.projectKey, Kind: arguments.Kind, Origin: "model", Text: arguments.Text, SourceTask: tool.taskID, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return erkaruntime.ToolExecutionResult{Output: err.Error(), IsError: true}, nil
	}
	return erkaruntime.ToolExecutionResult{Output: "Saved project memory " + note.ID}, nil
}

var _ Store = (*store.SQLite)(nil)
