package app

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/providers/deepseek"
)

// This test is intentionally opt-in because it makes billable network calls.
// Run with ERKA_DEEPSEEK_INTEGRATION=1 and ERKA_DEEPSEEK_API_KEY set.
func TestDeepSeekFixtureIntegration(t *testing.T) {
	if os.Getenv("ERKA_DEEPSEEK_INTEGRATION") != "1" {
		t.Skip("set ERKA_DEEPSEEK_INTEGRATION=1 to run the live provider fixture")
	}
	key := os.Getenv("ERKA_DEEPSEEK_API_KEY")
	if key == "" {
		t.Fatal("ERKA_DEEPSEEK_API_KEY is required for the live provider fixture")
	}
	root := t.TempDir()
	writeFixture(t, root)
	initRepository(t, root)
	provider, err := deepseek.New(deepseek.Options{APIKey: key, Model: os.Getenv("ERKA_MODEL")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	result, err := RunLocal(ctx, LocalOptions{
		WorkingDirectory: root, StateDir: t.TempDir(), Provider: provider,
		AllowFullExecution: true,
		Objective:          "Fix the failing Add test with the smallest change. Run go test ./... and inspect the Git diff before finishing.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("result = %#v", result)
	}
	command := exec.Command("go", "test", "./...")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fixture tests failed: %v\n%s", err, output)
	}
	command = exec.Command("git", "diff", "--", "add.go")
	command.Dir = root
	diff, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diff), "return a + b") {
		t.Fatalf("unexpected diff:\n%s", diff)
	}
}
