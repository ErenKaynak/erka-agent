package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/sandbox"
)

// TestPreviewHelperProcess is a tiny dev server started as a preview preset.
func TestPreviewHelperProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-3] != "--preview-helper" {
		return
	}
	address := net.JoinHostPort(os.Args[len(os.Args)-2], os.Args[len(os.Args)-1])
	_ = http.ListenAndServe(address, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(response, "hello from %s", os.Getenv("PORT"))
	}))
	os.Exit(0)
}

func TestPreviewManagerRunsPresetOnBindAddressAndStops(t *testing.T) {
	t.Parallel()
	manager := NewPreviewManager(config.Previews{
		BindAddress: "127.0.0.1", PortMin: 42100, PortMax: 42199,
		Commands: map[string][]string{"dev": {os.Args[0], "-test.run=TestPreviewHelperProcess", "--", "--preview-helper", "{host}", "{port}"}},
	}, t.TempDir(), sandbox.Isolation{Mode: sandbox.IsolationAuto, Network: true})
	t.Cleanup(manager.StopAll)
	if _, err := manager.Start("task-1", t.TempDir(), "unknown"); err == nil {
		t.Fatal("unknown preset was started")
	}
	preview, err := manager.Start("task-1", t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(preview.URL, "http://127.0.0.1:421") {
		t.Fatalf("preview URL = %q", preview.URL)
	}
	deadline := time.Now().Add(10 * time.Second)
	for manager.List("task-1")[0].Status != "ready" {
		if time.Now().After(deadline) {
			log, _ := manager.Log(preview.ID)
			t.Fatalf("preview never became ready: %#v\n%s", manager.List("task-1"), log)
		}
		time.Sleep(50 * time.Millisecond)
	}
	response, err := http.Get(preview.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != fmt.Sprintf("hello from %d", preview.Port) {
		t.Fatalf("preview body = %q", body)
	}
	again, err := manager.Start("task-1", t.TempDir(), "dev")
	if err != nil || again.ID != preview.ID {
		t.Fatalf("second Start() = %#v, %v; want the running preview", again, err)
	}
	if err := manager.Stop(preview.ID); err != nil {
		t.Fatal(err)
	}
	if len(manager.List("task-1")) != 0 {
		t.Fatal("stopped preview is still listed")
	}
	if _, err := http.Get(preview.URL); err == nil {
		t.Fatal("preview still serves after Stop")
	}
}

func TestPreviewsDisabledWithoutPresets(t *testing.T) {
	t.Parallel()
	if _, err := NewPreviewManager(config.Previews{}, t.TempDir(), sandbox.Isolation{}).Start("task-1", t.TempDir(), "dev"); err != ErrPreviewsDisabled {
		t.Fatalf("Start() error = %v, want disabled", err)
	}
}
