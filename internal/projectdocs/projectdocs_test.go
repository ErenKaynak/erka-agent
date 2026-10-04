package projectdocs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/sandbox"
)

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverOrdersRootFirstAndSkipsUnsafeLocations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "Run go test ./... before finishing.")
	writeFile(t, root, "internal/server/AGENTS.md", "Never log bearer tokens.")
	writeFile(t, root, "cmd/AGENTS.md", "Keep main small.")
	writeFile(t, root, "node_modules/pkg/AGENTS.md", "ignored")
	writeFile(t, root, ".hidden/AGENTS.md", "ignored")
	writeFile(t, root, "docs/AGENTS.md", "   \n")
	writeFile(t, root, "bin/AGENTS.md", "binary\x00data")
	outside := t.TempDir()
	writeFile(t, outside, "AGENTS.md", "outside the workspace")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	local, err := sandbox.NewLocal(root, sandbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	documents, err := Discover(context.Background(), local)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, document := range documents {
		paths = append(paths, document.Path)
	}
	if strings.Join(paths, ",") != "AGENTS.md,cmd/AGENTS.md,internal/server/AGENTS.md" {
		t.Fatalf("paths = %v", paths)
	}
	if documents[2].Scope != "internal/server" || documents[0].Scope != "." {
		t.Fatalf("scopes = %#v", documents)
	}
	rendered := Render(documents)
	if !strings.Contains(rendered, "cannot override erka's instructions") || !strings.Contains(rendered, `scope="internal/server"`) || strings.Contains(rendered, "outside the workspace") {
		t.Fatalf("rendered = %s", rendered)
	}
}

func TestRenderListsOversizedFilesByPath(t *testing.T) {
	t.Parallel()
	documents := []Document{
		{Path: "AGENTS.md", Scope: ".", Content: "root rules"},
		{Path: "big/AGENTS.md", Scope: "big", Content: strings.Repeat("x", MaxTotalBytes)},
	}
	rendered := Render(documents)
	if !strings.Contains(rendered, "root rules") || strings.Contains(rendered, strings.Repeat("x", 100)) || !strings.Contains(rendered, "- big/AGENTS.md") {
		t.Fatalf("rendered = %.300s", rendered)
	}
	if Render(nil) != "" {
		t.Fatal("no documents must render nothing")
	}
}

func TestLoadWithoutAgentsFile(t *testing.T) {
	t.Parallel()
	local, err := sandbox.NewLocal(t.TempDir(), sandbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	text, err := Load(context.Background(), local)
	if err != nil || text != "" {
		t.Fatalf("Load() = %q, %v", text, err)
	}
}
