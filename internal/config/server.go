package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

const (
	NotificationNtfy    = "ntfy"
	NotificationWebhook = "webhook"
)

// Server configures the optional personal server. Secrets are referenced by
// environment variable name and never stored in YAML.
type Server struct {
	// PublicURL is the address other devices use to reach the server, such
	// as its VPN address. It is used in notification links and actions.
	PublicURL string `yaml:"public_url,omitempty"`
	// TrustedCIDRs allows plain HTTP on a non-loopback address when every
	// client connects from these networks, such as a WireGuard or Tailscale
	// range. Bearer tokens remain required.
	TrustedCIDRs  []string             `yaml:"trusted_cidrs,omitempty"`
	Notifications []NotificationTarget `yaml:"notifications,omitempty"`
	Previews      Previews             `yaml:"previews,omitempty"`
}

type NotificationTarget struct {
	Name string `yaml:"name,omitempty"`
	Type string `yaml:"type"`
	URL  string `yaml:"url"`
	// TokenEnv names a bearer token for the target, such as an ntfy access token.
	TokenEnv string `yaml:"token_env,omitempty"`
	// SecretEnv names an HMAC-SHA256 signing secret for generic webhooks.
	SecretEnv string `yaml:"secret_env,omitempty"`
	// ActionTokenEnv names an erka API token with the approve scope. When set,
	// ntfy notifications carry approve and reject buttons. The token is sent
	// to the ntfy server, so use it only with a self-hosted private instance.
	ActionTokenEnv string `yaml:"action_token_env,omitempty"`
	// Events limits delivery to these notification kinds. Empty means all.
	Events []string `yaml:"events,omitempty"`
}

// Previews configures managed dev-server previews for server task worktrees.
type Previews struct {
	// BindAddress is the interface address previews listen on, normally the
	// server's VPN address.
	BindAddress string `yaml:"bind_address,omitempty"`
	PortMin     int    `yaml:"port_min,omitempty"`
	PortMax     int    `yaml:"port_max,omitempty"`
	// Commands are the only processes a preview may start, keyed by preset
	// name, so an API token can never run an arbitrary command. Arguments may
	// use {port} and {host}; PORT and HOST are also set in the environment.
	Commands map[string][]string `yaml:"commands,omitempty"`
}

var notificationKinds = map[string]bool{
	"question": true, "permission": true, "accept_changes": true, "task.failed": true, "task.waiting": true,
}

func validateServer(server *Server) error {
	if server.PublicURL != "" {
		parsed, err := url.Parse(server.PublicURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("server.public_url must be an absolute http or https URL")
		}
		server.PublicURL = strings.TrimRight(server.PublicURL, "/")
	}
	for index, cidr := range server.TrustedCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil {
			return fmt.Errorf("server.trusted_cidrs[%d]: %w", index, err)
		}
		server.TrustedCIDRs[index] = network.String()
	}
	names := make(map[string]bool)
	for index := range server.Notifications {
		target := &server.Notifications[index]
		target.Type = strings.ToLower(strings.TrimSpace(target.Type))
		if target.Type != NotificationNtfy && target.Type != NotificationWebhook {
			return fmt.Errorf("server.notifications[%d] has unsupported type %q", index, target.Type)
		}
		parsed, err := url.Parse(target.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("server.notifications[%d] requires an absolute http or https URL", index)
		}
		if target.Name == "" {
			target.Name = fmt.Sprintf("%s-%d", target.Type, index+1)
		}
		if names[target.Name] {
			return fmt.Errorf("duplicate notification target name %q", target.Name)
		}
		names[target.Name] = true
		for _, env := range []string{target.TokenEnv, target.SecretEnv, target.ActionTokenEnv} {
			if env != "" && !validEnvironmentName(env) {
				return fmt.Errorf("notification target %q has invalid environment name %q", target.Name, env)
			}
		}
		if target.ActionTokenEnv != "" && target.Type != NotificationNtfy {
			return fmt.Errorf("notification target %q: action_token_env is only supported for ntfy", target.Name)
		}
		if target.ActionTokenEnv != "" && server.PublicURL == "" {
			return fmt.Errorf("notification target %q: action buttons require server.public_url", target.Name)
		}
		for _, kind := range target.Events {
			if !notificationKinds[kind] {
				return fmt.Errorf("notification target %q has unsupported event %q", target.Name, kind)
			}
		}
	}
	previews := &server.Previews
	if previews.BindAddress != "" && net.ParseIP(previews.BindAddress) == nil {
		return fmt.Errorf("server.previews.bind_address must be an IP address")
	}
	if previews.PortMin == 0 && previews.PortMax == 0 {
		previews.PortMin, previews.PortMax = 4100, 4199
	}
	if previews.PortMin < 1024 || previews.PortMax > 65535 || previews.PortMin > previews.PortMax {
		return fmt.Errorf("server.previews port range must be within 1024-65535 and ordered")
	}
	if len(previews.Commands) > 0 {
		ip := net.ParseIP(previews.BindAddress)
		if ip == nil || ip.IsUnspecified() {
			return fmt.Errorf("server.previews.commands require a specific bind_address, such as the server's VPN address")
		}
	}
	for name, command := range previews.Commands {
		if strings.TrimSpace(name) == "" || len(command) == 0 || strings.TrimSpace(command[0]) == "" {
			return fmt.Errorf("server.previews.commands.%s must name an executable", name)
		}
	}
	return nil
}

// validateTiers checks tiered routing and fills default tiers: trivial runs
// coding only, standard adds validation and repair, and complex runs every
// configured stage.
func validateTiers(resolved *Resolved, name string, pipeline *Pipeline) error {
	if pipeline.Routing != RoutingTiered {
		if len(pipeline.Tiers) > 0 || pipeline.Triage.Candidate != "" {
			return fmt.Errorf("pipeline %q configures tiers or triage without routing: tiered", name)
		}
		return nil
	}
	if reference := pipeline.Triage.Candidate; reference != "" {
		connectionName, model, ok := strings.Cut(reference, "/")
		connection, exists := resolved.Connections[connectionName]
		if !ok || model == "" || !exists {
			return fmt.Errorf("pipeline %q triage candidate %q must reference a configured connection/model", name, reference)
		}
		if connection.Class == ConnectionSubscriptionBridge {
			return fmt.Errorf("pipeline %q triage candidate %q must be a native connection", name, reference)
		}
	}
	defaults := map[string][]string{
		"trivial":  {"coding"},
		"standard": {"coding", "validation", "repair"},
		"complex":  {"planning", "coding", "validation", "repair", "review"},
	}
	if pipeline.Tiers == nil {
		pipeline.Tiers = make(map[string]Tier)
	}
	for tierName := range pipeline.Tiers {
		if _, ok := defaults[tierName]; !ok {
			return fmt.Errorf("pipeline %q has unsupported tier %q", name, tierName)
		}
	}
	for _, tierName := range PipelineTiers {
		tier := pipeline.Tiers[tierName]
		if len(tier.Stages) == 0 {
			for _, stage := range defaults[tierName] {
				if _, ok := pipeline.Stages[stage]; ok {
					tier.Stages = append(tier.Stages, stage)
				}
			}
		}
		hasCoding := false
		for _, stage := range tier.Stages {
			if _, ok := pipeline.Stages[stage]; !ok {
				return fmt.Errorf("pipeline %q tier %q uses unconfigured stage %q", name, tierName, stage)
			}
			hasCoding = hasCoding || stage == "coding"
		}
		if !hasCoding {
			return fmt.Errorf("pipeline %q tier %q must include the coding stage", name, tierName)
		}
		for stage, candidates := range tier.Candidates {
			if !containsString(tier.Stages, stage) {
				return fmt.Errorf("pipeline %q tier %q overrides candidates for stage %q it does not run", name, tierName, stage)
			}
			if len(candidates) == 0 {
				return fmt.Errorf("pipeline %q tier %q stage %q has no candidates", name, tierName, stage)
			}
			for _, reference := range candidates {
				connectionName, model, ok := strings.Cut(reference, "/")
				connection, exists := resolved.Connections[connectionName]
				if !ok || model == "" || !exists {
					return fmt.Errorf("pipeline %q tier %q has invalid candidate %q", name, tierName, reference)
				}
				if connection.Class == ConnectionSubscriptionBridge && stage != "planning" && stage != "review" {
					return fmt.Errorf("pipeline %q tier %q stage %q cannot use subscription bridge %q", name, tierName, stage, connectionName)
				}
			}
		}
		pipeline.Tiers[tierName] = tier
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
