// Package commands loads user-defined slash commands: Markdown prompt
// templates in .erka/commands (project) and <config dir>/commands (user).
package commands

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// ArgumentsPlaceholder is replaced by the text typed after the command.
	ArgumentsPlaceholder = "$ARGUMENTS"
	// ProjectDirectory is relative to the repository root.
	ProjectDirectory = ".erka/commands"
	maxTemplateBytes = 64 << 10
	maxCommands      = 200
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

// Command is one custom slash command.
type Command struct {
	Name        string
	Description string
	Template    string
	// Source is "project" or "user".
	Source string
	Path   string
}

// Expand fills the template. Without a placeholder, non-empty arguments are
// appended on a new line so they are never silently dropped.
func (command Command) Expand(arguments string) string {
	arguments = strings.TrimSpace(arguments)
	if strings.Contains(command.Template, ArgumentsPlaceholder) {
		return strings.TrimSpace(strings.ReplaceAll(command.Template, ArgumentsPlaceholder, arguments))
	}
	if arguments == "" {
		return strings.TrimSpace(command.Template)
	}
	return strings.TrimSpace(command.Template) + "\n\n" + arguments
}

// Load reads user commands first and project commands second, so a project
// command overrides a user command of the same name. Names in reserved
// (built-in commands) are skipped with a warning. Missing directories are
// not an error.
func Load(projectRoot, userDirectory string, reserved map[string]bool) ([]Command, []string) {
	byName := make(map[string]Command)
	var warnings []string
	for _, source := range []struct{ name, directory string }{
		{"user", userDirectory},
		{"project", joinIfSet(projectRoot, filepath.FromSlash(ProjectDirectory))},
	} {
		if source.directory == "" {
			continue
		}
		entries, err := os.ReadDir(source.directory)
		if err != nil {
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("read %s commands: %v", source.name, err))
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			name := strings.ToLower(strings.TrimSuffix(entry.Name(), ".md"))
			path := filepath.Join(source.directory, entry.Name())
			if !namePattern.MatchString(name) {
				warnings = append(warnings, fmt.Sprintf("skipped command %s: invalid name", path))
				continue
			}
			if reserved[name] {
				warnings = append(warnings, fmt.Sprintf("skipped command /%s from %s: it would shadow a built-in command", name, path))
				continue
			}
			command, err := parse(path)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("skipped command %s: %v", path, err))
				continue
			}
			command.Name, command.Source, command.Path = name, source.name, path
			byName[name] = command
		}
	}
	result := make([]Command, 0, len(byName))
	for _, command := range byName {
		result = append(result, command)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	if len(result) > maxCommands {
		warnings = append(warnings, fmt.Sprintf("only the first %d custom commands are loaded", maxCommands))
		result = result[:maxCommands]
	}
	return result, warnings
}

func joinIfSet(root, relative string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, relative)
}

// parse reads a template with optional front matter:
//
//	---
//	description: Review the current diff
//	---
//	Review the working-tree diff for $ARGUMENTS ...
func parse(path string) (Command, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Command{}, err
	}
	if info.Size() > maxTemplateBytes {
		return Command{}, fmt.Errorf("template exceeds %d bytes", maxTemplateBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Command{}, err
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return Command{}, fmt.Errorf("template is not UTF-8 text")
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	command := Command{}
	if strings.HasPrefix(text, "---\n") {
		end := strings.Index(text[4:], "\n---")
		if end < 0 {
			return Command{}, fmt.Errorf("front matter is not closed")
		}
		for _, line := range strings.Split(text[4:4+end], "\n") {
			key, value, found := strings.Cut(line, ":")
			if found && strings.TrimSpace(strings.ToLower(key)) == "description" {
				command.Description = strings.Trim(strings.TrimSpace(value), `"'`)
			}
		}
		text = text[4+end+4:]
	}
	command.Template = strings.TrimSpace(text)
	if command.Template == "" {
		return Command{}, fmt.Errorf("template is empty")
	}
	return command, nil
}
