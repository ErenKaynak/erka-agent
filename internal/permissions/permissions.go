// Package permissions evaluates configured allow/ask/deny rules for tool
// calls.
//
// A rule is either a tool name ("apply_patch") or a tool name with an
// argument pattern ("run_command(go test *)"). Both parts accept `*`
// (any run of characters, including spaces and slashes) and `?` (one
// character). Precedence is deny, then ask, then allow. Without a matching
// rule, a call is allowed, except that run_command without --full consent
// requires approval.
package permissions

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

// CommandTool is the tool whose availability --full controls.
const CommandTool = "run_command"

// Rule is one parsed permission pattern.
type Rule struct {
	Raw     string
	tool    *regexp.Regexp
	pattern *regexp.Regexp // nil when the rule names only a tool
}

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_*?.-]+$`)

// ParseRule parses "tool" or "tool(pattern)".
func ParseRule(raw string) (Rule, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Rule{}, errors.New("permission rule is empty")
	}
	name, argument, hasArgument := text, "", false
	if open := strings.IndexByte(text, '('); open >= 0 {
		if !strings.HasSuffix(text, ")") {
			return Rule{}, fmt.Errorf("permission rule %q has an unclosed argument pattern", raw)
		}
		name, argument, hasArgument = strings.TrimSpace(text[:open]), text[open+1:len(text)-1], true
		if strings.TrimSpace(argument) == "" {
			return Rule{}, fmt.Errorf("permission rule %q has an empty argument pattern", raw)
		}
	}
	if !toolNamePattern.MatchString(name) {
		return Rule{}, fmt.Errorf("permission rule %q has an invalid tool name", raw)
	}
	rule := Rule{Raw: text, tool: wildcard(name)}
	if hasArgument {
		rule.pattern = wildcard(strings.TrimSpace(argument))
	}
	return rule, nil
}

func wildcard(pattern string) *regexp.Regexp {
	quoted := regexp.QuoteMeta(pattern)
	quoted = strings.ReplaceAll(quoted, `\*`, `.*`)
	quoted = strings.ReplaceAll(quoted, `\?`, `.`)
	return regexp.MustCompile(`^` + quoted + `$`)
}

// namesTool reports whether the rule's tool pattern matches name.
func (rule Rule) namesTool(name string) bool { return rule.tool.MatchString(name) }

// matchAny is used for deny and ask: one matching subject is enough.
func (rule Rule) matchAny(tool string, subjects []string) bool {
	if !rule.namesTool(tool) {
		return false
	}
	if rule.pattern == nil {
		return true
	}
	for _, subject := range subjects {
		if rule.pattern.MatchString(subject) {
			return true
		}
	}
	return false
}

// matchAll is used for allow: every subject must match, so a patch touching
// one disallowed file is not allowed by a rule for other files.
func (rule Rule) matchAll(tool string, subjects []string) bool {
	if !rule.namesTool(tool) {
		return false
	}
	if rule.pattern == nil {
		return true
	}
	if len(subjects) == 0 {
		return false
	}
	for _, subject := range subjects {
		if !rule.pattern.MatchString(subject) {
			return false
		}
	}
	return true
}

// Rules is the configured rule text, validated by New.
type Rules struct {
	Allow []string
	Ask   []string
	Deny  []string
}

// Empty reports whether no rules are configured.
func (rules Rules) Empty() bool { return len(rules.Allow)+len(rules.Ask)+len(rules.Deny) == 0 }

// Validate checks rule syntax without building a policy.
func (rules Rules) Validate() error {
	_, err := New(rules, false)
	return err
}

// Policy is an immutable, deterministic permission evaluator.
type Policy struct {
	allow, ask, deny []Rule
	full             bool
}

// New builds a policy. full is the user's --full consent for unrestricted
// command execution.
func New(rules Rules, full bool) (*Policy, error) {
	policy := &Policy{full: full}
	for _, group := range []struct {
		name   string
		source []string
		target *[]Rule
	}{{"allow", rules.Allow, &policy.allow}, {"ask", rules.Ask, &policy.ask}, {"deny", rules.Deny, &policy.deny}} {
		for _, raw := range group.source {
			rule, err := ParseRule(raw)
			if err != nil {
				return nil, fmt.Errorf("permissions.rules.%s: %w", group.name, err)
			}
			*group.target = append(*group.target, rule)
		}
	}
	return policy, nil
}

// ExposesCommand reports whether run_command should be offered to the model
// without --full: some allow or ask rule targets it explicitly.
func (policy *Policy) ExposesCommand() bool {
	if policy == nil {
		return false
	}
	for _, group := range [][]Rule{policy.allow, policy.ask} {
		for _, rule := range group {
			if rule.namesTool(CommandTool) && rule.Raw != "*" && !strings.HasPrefix(rule.Raw, "*(") {
				return true
			}
		}
	}
	return false
}

// Decide implements runtime.PermissionPolicy.
func (policy *Policy) Decide(tool string, arguments json.RawMessage) erkaruntime.PermissionDecision {
	if policy == nil {
		return erkaruntime.PermissionDecision{Action: erkaruntime.PermissionAllow}
	}
	subjects := Subjects(tool, arguments)
	for _, rule := range policy.deny {
		if rule.matchAny(tool, subjects) {
			return erkaruntime.PermissionDecision{Action: erkaruntime.PermissionDeny, Rule: rule.Raw, Reason: "matched a deny rule"}
		}
	}
	for _, rule := range policy.ask {
		if rule.matchAny(tool, subjects) {
			return erkaruntime.PermissionDecision{Action: erkaruntime.PermissionAsk, Rule: rule.Raw, Reason: "matched an ask rule"}
		}
	}
	for _, rule := range policy.allow {
		if rule.matchAll(tool, subjects) {
			return erkaruntime.PermissionDecision{Action: erkaruntime.PermissionAllow, Rule: rule.Raw}
		}
	}
	if tool == CommandTool && !policy.full {
		return erkaruntime.PermissionDecision{Action: erkaruntime.PermissionAsk, Reason: "command is not allow-listed and --full is off"}
	}
	return erkaruntime.PermissionDecision{Action: erkaruntime.PermissionAllow}
}

// Subjects extracts the strings that argument patterns match for a tool:
// the space-joined argv for run_command, workspace paths for file tools,
// and every touched path for apply_patch. Other tools have no subjects and
// match only tool-name rules.
func Subjects(tool string, arguments json.RawMessage) []string {
	switch tool {
	case CommandTool:
		var value struct {
			Command []string `json:"command"`
		}
		if json.Unmarshal(arguments, &value) == nil && len(value.Command) > 0 {
			return []string{strings.Join(value.Command, " ")}
		}
	case "read_file", "edit_file":
		var value struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(arguments, &value) == nil && value.Path != "" {
			return []string{cleanPath(value.Path)}
		}
	case "list_files", "search_text":
		var value struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(arguments, &value) == nil {
			if value.Path == "" {
				value.Path = "."
			}
			return []string{cleanPath(value.Path)}
		}
	case "apply_patch":
		var value struct {
			Patch string `json:"patch"`
		}
		if json.Unmarshal(arguments, &value) == nil {
			return patchPaths(value.Patch)
		}
	}
	return nil
}

func cleanPath(value string) string {
	return path.Clean(strings.ReplaceAll(strings.TrimSpace(value), `\`, "/"))
}

// patchPaths returns every file named in unified-diff headers, so path
// rules apply to both the source and destination of a rename.
func patchPaths(patch string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, line := range strings.Split(patch, "\n") {
		var value string
		switch {
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
			value = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "rename to "):
			value = strings.TrimSpace(line[strings.Index(line, " ")+1:])
			value = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(value, "from "), "to "))
		default:
			continue
		}
		if tab := strings.IndexByte(value, '\t'); tab >= 0 {
			value = value[:tab]
		}
		if value == "" || value == "/dev/null" {
			continue
		}
		if strings.HasPrefix(value, "a/") || strings.HasPrefix(value, "b/") {
			value = value[2:]
		}
		value = cleanPath(value)
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

var _ erkaruntime.PermissionPolicy = (*Policy)(nil)
