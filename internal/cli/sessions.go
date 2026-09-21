package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const minSessionPrefix = 8

type conversationLister interface {
	ListConversations(context.Context) ([]erkaruntime.TaskSnapshot, error)
	Root() string
}

// resolveSession accepts a full task ID or a unique prefix of one, with or
// without the task_ prefix.
func resolveSession(ctx context.Context, lister conversationLister, value string) (erkaruntime.TaskID, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	conversations, err := lister.ListConversations(ctx)
	if err != nil {
		return "", err
	}
	needle := strings.TrimPrefix(value, "task_")
	matches := make([]erkaruntime.TaskID, 0, 1)
	for _, conversation := range conversations {
		if string(conversation.ID) == value {
			return conversation.ID, nil
		}
		if strings.HasPrefix(strings.TrimPrefix(string(conversation.ID), "task_"), needle) {
			matches = append(matches, conversation.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("%w: no session %q in %s; run erka from the session's repository or pass --cwd", ErrUsage, value, lister.Root())
	default:
		return "", fmt.Errorf("%w: session %q is ambiguous (%d matches); use more characters", ErrUsage, value, len(matches))
	}
}

// shortSessionID is the shortest prefix of at least eight characters that
// identifies the session among this repository's conversations.
func shortSessionID(id erkaruntime.TaskID, conversations []erkaruntime.TaskSnapshot) string {
	full := strings.TrimPrefix(string(id), "task_")
	for length := min(minSessionPrefix, len(full)); length < len(full); length++ {
		prefix, unique := full[:length], true
		for _, conversation := range conversations {
			other := strings.TrimPrefix(string(conversation.ID), "task_")
			if conversation.ID != id && strings.HasPrefix(other, prefix) {
				unique = false
				break
			}
		}
		if unique {
			return prefix
		}
	}
	return full
}

// printResumeHint tells the user how to reopen the conversation they left.
func printResumeHint(ctx context.Context, output io.Writer, lister conversationLister, id erkaruntime.TaskID) {
	if id == "" {
		return
	}
	conversations, err := lister.ListConversations(ctx)
	if err != nil {
		return
	}
	found := false
	for _, conversation := range conversations {
		found = found || conversation.ID == id
	}
	if !found {
		return
	}
	command := "erka -s " + shortSessionID(id, conversations)
	if working, err := os.Getwd(); err == nil && !within(lister.Root(), working) {
		command += " --cwd " + shellQuote(lister.Root())
	}
	_, _ = fmt.Fprintf(output, "\nResume this session with:\n  %s\n", command)
}

func within(root, path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	relative, err := filepath.Rel(root, resolved)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func shellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n'\"\\$`!*?&;|<>()[]{}#~") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
