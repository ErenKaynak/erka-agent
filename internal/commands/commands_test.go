package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMergesUserAndProjectCommands(t *testing.T) {
	t.Parallel()
	root, user := t.TempDir(), t.TempDir()
	project := filepath.Join(root, ".erka", "commands")
	write(t, user, "review.md", "User review of $ARGUMENTS")
	write(t, user, "standup.md", "---\ndescription: \"Daily summary\"\n---\nSummarize yesterday's commits.")
	write(t, project, "review.md", "---\ndescription: Review the diff\n---\r\nReview the working-tree diff, focusing on $ARGUMENTS.\r\n")
	write(t, project, "help.md", "shadowing a built-in")
	write(t, project, "Bad Name.md", "x")
	write(t, project, "empty.md", "---\ndescription: nothing\n---\n   ")
	write(t, project, "notes.txt", "ignored")
	commands, warnings := Load(root, user, map[string]bool{"help": true})
	var names []string
	for _, command := range commands {
		names = append(names, command.Name+":"+command.Source)
	}
	if strings.Join(names, ",") != "review:project,standup:user" {
		t.Fatalf("commands = %v", names)
	}
	if commands[0].Description != "Review the diff" || commands[1].Description != "Daily summary" {
		t.Fatalf("descriptions = %q %q", commands[0].Description, commands[1].Description)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"shadow a built-in", "invalid name", "template is empty"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings %q lack %q", joined, want)
		}
	}
	if got := commands[0].Expand("  error handling "); got != "Review the working-tree diff, focusing on error handling." {
		t.Fatalf("Expand = %q", got)
	}
	if got := commands[1].Expand("include PR #4"); got != "Summarize yesterday's commits.\n\ninclude PR #4" {
		t.Fatalf("Expand without placeholder = %q", got)
	}
}

func TestLoadToleratesMissingDirectories(t *testing.T) {
	t.Parallel()
	commands, warnings := Load(filepath.Join(t.TempDir(), "missing"), "", nil)
	if len(commands) != 0 || len(warnings) != 0 {
		t.Fatalf("commands = %v warnings = %v", commands, warnings)
	}
}
