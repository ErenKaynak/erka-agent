package config

import (
	"context"
	"strings"
	"testing"
)

func loadYAML(t *testing.T, content string) (Resolved, error) {
	t.Helper()
	return Load(context.Background(), Options{GOOS: "linux", HomeDir: "/home/erka", LookupEnv: lookup(nil), ReadFile: func(string) ([]byte, error) { return []byte(content), nil }})
}

const tieredBase = `version: 1
pipeline: smart
connections:
  coder:
    provider: deepseek
pipelines:
  smart:
    routing: tiered
    triage:
      candidate: coder/deepseek-v4-flash
    stages:
      planning: {candidates: [coder/deepseek-v4-pro]}
      coding: {candidates: [coder/deepseek-v4-pro]}
      validation: {candidates: [coder/deepseek-v4-flash]}
`

func TestTieredPipelineFillsDefaultTiers(t *testing.T) {
	t.Parallel()
	resolved, err := loadYAML(t, tieredBase)
	if err != nil {
		t.Fatal(err)
	}
	tiers := resolved.Pipelines["smart"].Tiers
	if got := strings.Join(tiers["trivial"].Stages, ","); got != "coding" {
		t.Fatalf("trivial stages = %s", got)
	}
	if got := strings.Join(tiers["standard"].Stages, ","); got != "coding,validation" {
		t.Fatalf("standard stages = %s", got)
	}
	if got := strings.Join(tiers["complex"].Stages, ","); got != "planning,coding,validation" {
		t.Fatalf("complex stages = %s", got)
	}
}

func TestTieredPipelineRejectsInvalidTiers(t *testing.T) {
	t.Parallel()
	for name, suffix := range map[string]string{
		"unknown tier":         "    tiers:\n      huge: {stages: [coding]}\n",
		"missing coding":       "    tiers:\n      trivial: {stages: [validation]}\n",
		"unconfigured stage":   "    tiers:\n      complex: {stages: [coding, review]}\n",
		"override unrun stage": "    tiers:\n      trivial: {stages: [coding], candidates: {validation: [coder/x]}}\n",
	} {
		if _, err := loadYAML(t, tieredBase+suffix); err == nil {
			t.Errorf("%s: configuration was accepted", name)
		}
	}
	fixed := strings.Replace(tieredBase, "routing: tiered", "routing: fixed", 1)
	if _, err := loadYAML(t, fixed); err == nil {
		t.Error("triage on a fixed pipeline was accepted")
	}
}

func TestServerConfigValidatesNotificationsAndNetworks(t *testing.T) {
	t.Parallel()
	valid := "version: 1\nserver:\n  public_url: http://100.64.0.1:7432/\n  trusted_cidrs: [100.64.0.0/10]\n  notifications:\n    - type: ntfy\n      url: https://ntfy.internal/erka\n      action_token_env: ERKA_APPROVE_TOKEN\n"
	resolved, err := loadYAML(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Server.PublicURL != "http://100.64.0.1:7432" || resolved.Server.Notifications[0].Name != "ntfy-1" {
		t.Fatalf("server config = %#v", resolved.Server)
	}
	for name, content := range map[string]string{
		"bad cidr":            "version: 1\nserver:\n  trusted_cidrs: [100.64.0.0/99]\n",
		"unknown target":      "version: 1\nserver:\n  notifications:\n    - {type: email, url: https://x}\n",
		"actions without url": "version: 1\nserver:\n  notifications:\n    - {type: ntfy, url: https://ntfy/x, action_token_env: T}\n",
		"webhook actions":     "version: 1\nserver:\n  public_url: https://x\n  notifications:\n    - {type: webhook, url: https://x, action_token_env: T}\n",
	} {
		if _, err := loadYAML(t, content); err == nil {
			t.Errorf("%s: configuration was accepted", name)
		}
	}
}
