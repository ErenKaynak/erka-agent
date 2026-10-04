// Package mcp is a minimal Model Context Protocol client for tools. It
// supports the stdio and streamable HTTP transports and the lifecycle,
// tools/list, and tools/call methods; erka exposes each discovered tool to
// the model as mcp__<server>__<tool>.
package mcp

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ServerConfig configures one MCP server under `mcp_servers:`.
type ServerConfig struct {
	// Command and Args start a stdio server.
	Command string   `yaml:"command,omitempty"`
	Args    []string `yaml:"args,omitempty"`
	// EnvFrom names environment variables forwarded to a stdio server, on
	// top of a small base set (PATH, HOME, ...). Values never enter config,
	// prompts, logs, or events.
	EnvFrom []string `yaml:"env_from,omitempty"`
	// URL selects the streamable HTTP transport.
	URL string `yaml:"url,omitempty"`
	// BearerTokenEnv names an environment variable holding an HTTP bearer
	// token.
	BearerTokenEnv string `yaml:"bearer_token_env,omitempty"`
	// ReadOnly declares that every tool of this server is free of side
	// effects. Server-provided annotations are not trusted for this.
	ReadOnly bool `yaml:"read_only,omitempty"`
	// Timeout bounds connection setup and each tool call. Default 60s.
	Timeout  time.Duration `yaml:"timeout,omitempty"`
	Disabled bool          `yaml:"disabled,omitempty"`
}

const (
	DefaultTimeout = 60 * time.Second
	MaxTimeout     = 30 * time.Minute
)

var (
	serverNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Validate checks every configured server.
func Validate(servers map[string]ServerConfig) error {
	for name, server := range servers {
		if err := server.validate(name); err != nil {
			return err
		}
	}
	return nil
}

func (server ServerConfig) validate(name string) error {
	if !serverNamePattern.MatchString(name) || strings.Contains(name, "__") {
		return fmt.Errorf("mcp_servers: invalid server name %q (use lowercase letters, digits, - and _)", name)
	}
	hasCommand, hasURL := strings.TrimSpace(server.Command) != "", strings.TrimSpace(server.URL) != ""
	if hasCommand == hasURL {
		return fmt.Errorf("mcp_servers.%s: configure exactly one of command or url", name)
	}
	if server.Timeout < 0 || server.Timeout > MaxTimeout {
		return fmt.Errorf("mcp_servers.%s: timeout must be between 0 and %s", name, MaxTimeout)
	}
	for _, variable := range server.EnvFrom {
		if !envNamePattern.MatchString(variable) {
			return fmt.Errorf("mcp_servers.%s: invalid env_from name %q", name, variable)
		}
	}
	if hasCommand && server.BearerTokenEnv != "" {
		return fmt.Errorf("mcp_servers.%s: bearer_token_env applies only to url servers", name)
	}
	if hasURL {
		if len(server.Args) > 0 || len(server.EnvFrom) > 0 {
			return fmt.Errorf("mcp_servers.%s: args and env_from apply only to command servers", name)
		}
		if server.BearerTokenEnv != "" && !envNamePattern.MatchString(server.BearerTokenEnv) {
			return fmt.Errorf("mcp_servers.%s: invalid bearer_token_env %q", name, server.BearerTokenEnv)
		}
		parsed, err := url.Parse(server.URL)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("mcp_servers.%s: invalid url", name)
		}
		if parsed.User != nil {
			return fmt.Errorf("mcp_servers.%s: url must not embed credentials; use bearer_token_env", name)
		}
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
			return fmt.Errorf("mcp_servers.%s: url must use https unless it targets a loopback address", name)
		}
	}
	return nil
}

func (server ServerConfig) timeout() time.Duration {
	if server.Timeout > 0 {
		return server.Timeout
	}
	return DefaultTimeout
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var errClosed = errors.New("mcp connection is closed")
