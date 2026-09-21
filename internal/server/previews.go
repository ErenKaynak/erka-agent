package server

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/sandbox"
)

const (
	maxPreviewLogBytes  = 5 << 20
	previewLogTailBytes = 64 << 10
)

var ErrPreviewsDisabled = errors.New("previews are not configured on this server")

// Preview is a dev server running from a task worktree, reachable on the
// server's VPN address so a phone can open it.
type Preview struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"taskId"`
	Preset    string    `json:"preset"`
	Port      int       `json:"port"`
	URL       string    `json:"url"`
	Command   []string  `json:"command"`
	StartedAt time.Time `json:"startedAt"`
	// Status is starting, ready, or exited.
	Status   string `json:"status"`
	ExitNote string `json:"exitNote,omitempty"`
}

type previewProcess struct {
	preview Preview
	process *sandbox.BackgroundProcess
	logPath string
}

// PreviewManager runs configured preview presets. Processes live only as long
// as the server process; they are stopped on shutdown.
type PreviewManager struct {
	mu       sync.Mutex
	config   config.Previews
	logDir   string
	ids      erkaruntime.IDGenerator
	clock    func() time.Time
	previews map[string]*previewProcess
}

func NewPreviewManager(previews config.Previews, stateDir string) *PreviewManager {
	return &PreviewManager{config: previews, logDir: filepath.Join(stateDir, "previews"), ids: erkaruntime.RandomIDGenerator{}, clock: func() time.Time { return time.Now().UTC() }, previews: make(map[string]*previewProcess)}
}

func (manager *PreviewManager) Presets() []string {
	if manager == nil {
		return nil
	}
	names := make([]string, 0, len(manager.config.Commands))
	for name := range manager.config.Commands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Start runs a preset in a worktree, or returns the preset's running preview
// for that task.
func (manager *PreviewManager) Start(taskID erkaruntime.TaskID, worktree, preset string) (Preview, error) {
	if manager == nil || len(manager.config.Commands) == 0 {
		return Preview{}, ErrPreviewsDisabled
	}
	template, ok := manager.config.Commands[preset]
	if !ok {
		return Preview{}, fmt.Errorf("unknown preview preset %q: %w", preset, erkaruntime.ErrNotFound)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, existing := range manager.previews {
		if existing.preview.TaskID == string(taskID) && existing.preview.Preset == preset && !exited(existing) {
			return manager.snapshot(existing), nil
		}
	}
	port, err := manager.freePort()
	if err != nil {
		return Preview{}, err
	}
	host := manager.config.BindAddress
	arguments := make([]string, len(template))
	for index, value := range template {
		value = strings.ReplaceAll(value, "{port}", strconv.Itoa(port))
		arguments[index] = strings.ReplaceAll(value, "{host}", host)
	}
	workspace, err := sandbox.NewLocal(worktree, sandbox.Options{})
	if err != nil {
		return Preview{}, err
	}
	id, err := manager.ids.NewID("preview")
	if err != nil {
		return Preview{}, err
	}
	if err := os.MkdirAll(manager.logDir, 0o700); err != nil {
		return Preview{}, err
	}
	logPath := filepath.Join(manager.logDir, id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Preview{}, err
	}
	process, err := workspace.StartBackground(sandbox.BackgroundRequest{
		Executable: arguments[0], Arguments: arguments[1:], Directory: ".",
		Environment: map[string]string{"PORT": strconv.Itoa(port), "HOST": host, "HOSTNAME": host, "BROWSER": "none"},
		Output:      &cappedWriter{writer: logFile, remaining: maxPreviewLogBytes},
	})
	if err != nil {
		_ = logFile.Close()
		return Preview{}, err
	}
	entry := &previewProcess{
		preview: Preview{ID: id, TaskID: string(taskID), Preset: preset, Port: port, URL: "http://" + net.JoinHostPort(host, strconv.Itoa(port)), Command: arguments, StartedAt: manager.clock()},
		process: process, logPath: logPath,
	}
	manager.previews[id] = entry
	go func() {
		<-process.Done()
		_ = logFile.Close()
	}()
	return manager.snapshot(entry), nil
}

func (manager *PreviewManager) List(taskID erkaruntime.TaskID) []Preview {
	if manager == nil {
		return []Preview{}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	result := make([]Preview, 0)
	for _, entry := range manager.previews {
		if taskID == "" || entry.preview.TaskID == string(taskID) {
			result = append(result, manager.snapshot(entry))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StartedAt.Before(result[j].StartedAt) })
	return result
}

func (manager *PreviewManager) Stop(id string) error {
	if manager == nil {
		return ErrPreviewsDisabled
	}
	manager.mu.Lock()
	entry, ok := manager.previews[id]
	if ok {
		delete(manager.previews, id)
	}
	manager.mu.Unlock()
	if !ok {
		return fmt.Errorf("preview %q: %w", id, erkaruntime.ErrNotFound)
	}
	return entry.process.Stop()
}

// StopTask stops every preview for a task, for example after cancellation.
func (manager *PreviewManager) StopTask(taskID erkaruntime.TaskID) {
	for _, preview := range manager.List(taskID) {
		_ = manager.Stop(preview.ID)
	}
}

func (manager *PreviewManager) StopAll() {
	for _, preview := range manager.List("") {
		_ = manager.Stop(preview.ID)
	}
}

// Log returns the tail of a preview's combined output.
func (manager *PreviewManager) Log(id string) (string, error) {
	if manager == nil {
		return "", ErrPreviewsDisabled
	}
	manager.mu.Lock()
	entry, ok := manager.previews[id]
	manager.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("preview %q: %w", id, erkaruntime.ErrNotFound)
	}
	file, err := os.Open(entry.logPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	offset := info.Size() - previewLogTailBytes
	if offset < 0 {
		offset = 0
	}
	data, err := io.ReadAll(io.NewSectionReader(file, offset, info.Size()-offset))
	return strings.ToValidUTF8(string(data), ""), err
}

func (manager *PreviewManager) snapshot(entry *previewProcess) Preview {
	preview := entry.preview
	preview.Command = append([]string(nil), preview.Command...)
	switch {
	case exited(entry):
		preview.Status = "exited"
		if err := entry.process.Err(); err != nil {
			preview.ExitNote = err.Error()
		}
	case reachable(manager.config.BindAddress, preview.Port):
		preview.Status = "ready"
	default:
		preview.Status = "starting"
	}
	return preview
}

func (manager *PreviewManager) freePort() (int, error) {
	used := make(map[int]bool, len(manager.previews))
	for _, entry := range manager.previews {
		if !exited(entry) {
			used[entry.preview.Port] = true
		}
	}
	for port := manager.config.PortMin; port <= manager.config.PortMax; port++ {
		if used[port] {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(manager.config.BindAddress, strconv.Itoa(port)))
		if err != nil {
			continue
		}
		_ = listener.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free preview port in %d-%d", manager.config.PortMin, manager.config.PortMax)
}

func exited(entry *previewProcess) bool {
	select {
	case <-entry.process.Done():
		return true
	default:
		return false
	}
}

func reachable(host string, port int) bool {
	connection, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 150*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

// cappedWriter stops recording output after a byte budget but keeps
// accepting writes so a chatty dev server never blocks on its pipe.
type cappedWriter struct {
	mu        sync.Mutex
	writer    io.Writer
	remaining int64
}

func (writer *cappedWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.remaining > 0 {
		chunk := data
		if int64(len(chunk)) > writer.remaining {
			chunk = chunk[:writer.remaining]
		}
		written, _ := writer.writer.Write(chunk)
		writer.remaining -= int64(written)
	}
	return len(data), nil
}
