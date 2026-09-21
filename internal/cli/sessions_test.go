package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type fakeLister struct {
	root  string
	tasks []erkaruntime.TaskSnapshot
}

func (lister fakeLister) ListConversations(context.Context) ([]erkaruntime.TaskSnapshot, error) {
	return lister.tasks, nil
}
func (lister fakeLister) Root() string { return lister.root }

func snapshots(ids ...string) []erkaruntime.TaskSnapshot {
	result := make([]erkaruntime.TaskSnapshot, len(ids))
	for index, id := range ids {
		result[index] = erkaruntime.TaskSnapshot{Task: erkaruntime.Task{ID: erkaruntime.TaskID(id)}}
	}
	return result
}

func TestResolveSessionAcceptsFullIDsAndUniquePrefixes(t *testing.T) {
	t.Parallel()
	lister := fakeLister{root: "/repo", tasks: snapshots("task_3fa2c1d0aa11", "task_3fa2c1d0bb22", "task_9b0e77aa0000")}
	for value, want := range map[string]erkaruntime.TaskID{
		"task_9b0e77aa0000": "task_9b0e77aa0000",
		"9b0e":              "task_9b0e77aa0000",
		"task_9b0e77":       "task_9b0e77aa0000",
		"3fa2c1d0b":         "task_3fa2c1d0bb22",
		"":                  "",
	} {
		got, err := resolveSession(context.Background(), lister, value)
		if err != nil || got != want {
			t.Errorf("resolveSession(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"3fa2c1d0", "ffff"} {
		if _, err := resolveSession(context.Background(), lister, value); !errors.Is(err, ErrUsage) {
			t.Errorf("resolveSession(%q) error = %v, want usage error", value, err)
		}
	}
}

func TestShortSessionIDIsUniqueAndAtLeastEightCharacters(t *testing.T) {
	t.Parallel()
	tasks := snapshots("task_3fa2c1d0aa11", "task_3fa2c1d0bb22", "task_9b0e77aa0000")
	if got := shortSessionID("task_9b0e77aa0000", tasks); got != "9b0e77aa" {
		t.Fatalf("short ID = %q", got)
	}
	if got := shortSessionID("task_3fa2c1d0aa11", tasks); got != "3fa2c1d0a" {
		t.Fatalf("short ID with shared prefix = %q", got)
	}
}

func TestPrintResumeHintNamesTheSessionAndRepository(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	lister := fakeLister{root: "/elsewhere/my repo", tasks: snapshots("task_9b0e77aa0000")}
	printResumeHint(context.Background(), &output, lister, "task_9b0e77aa0000")
	if got := output.String(); !strings.Contains(got, "erka -s 9b0e77aa --cwd '/elsewhere/my repo'") {
		t.Fatalf("resume hint = %q", got)
	}
	output.Reset()
	printResumeHint(context.Background(), &output, lister, "")
	if output.Len() != 0 {
		t.Fatalf("hint printed without a session: %q", output.String())
	}
}
