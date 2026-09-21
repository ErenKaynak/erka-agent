package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

var (
	ErrPathEscape      = errors.New("path escapes workspace")
	ErrSpecialFile     = errors.New("special files are not supported")
	ErrFileTooLarge    = errors.New("file exceeds read limit")
	ErrDirectoryLimit  = errors.New("directory entry limit exceeded")
	ErrCommandTimedOut = errors.New("command timed out")
	// ErrProtectedPath rejects writes to repository metadata, where a planted
	// hook or config entry would later run outside the sandbox.
	ErrProtectedPath = errors.New("path is protected repository metadata")
)

type Options struct {
	// Environment is filtered through a small allowlist. Nil uses os.Environ.
	Environment []string
	// Isolation confines commands with the OS sandbox. The zero value is off,
	// which trusted internal callers such as Git maintenance rely on.
	Isolation Isolation
}

// Local scopes direct filesystem operations and process working directories to
// one canonical workspace. It is the initial personal-use sandbox boundary;
// stronger OS isolation can replace it behind runtime.Sandbox later.
type Local struct {
	root        string
	environment []string
	isolation   Isolation
	backend     string
	unavailable string
}

func NewLocal(root string, options Options) (*Local, error) {
	if root == "" {
		return nil, errors.New("workspace root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root symlinks: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("workspace root is not a directory")
	}
	environment := options.Environment
	if environment == nil {
		environment = os.Environ()
	}
	local := &Local{root: filepath.Clean(canonical), environment: filterEnvironment(environment), isolation: options.Isolation}
	if local.isolation.enabled() {
		local.backend, local.unavailable = detectBackend()
	}
	return local, nil
}

func (local *Local) ReadFile(ctx context.Context, name string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, errors.New("read limit must be positive")
	}
	filename, err := local.resolveExisting(name)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect %q: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read %q: %w", name, ErrSpecialFile)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("read %q (%d bytes): %w", name, info.Size(), ErrFileTooLarge)
	}
	reader := io.LimitReader(&contextReader{ctx: ctx, reader: file}, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("read %q: %w", name, ErrFileTooLarge)
	}
	return data, nil
}

func (local *Local) ListDir(ctx context.Context, name string, limit int) ([]erkaruntime.FileEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, errors.New("directory entry limit must be positive")
	}
	directory, err := local.resolveExisting(name)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", name, err)
	}
	if len(entries) > limit {
		return nil, fmt.Errorf("list %q: %w", name, ErrDirectoryLimit)
	}
	result := make([]erkaruntime.FileEntry, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect %q: %w", filepath.Join(name, entry.Name()), err)
		}
		relative, err := filepath.Rel(local.root, filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		result = append(result, erkaruntime.FileEntry{
			Path: filepath.ToSlash(relative), Size: info.Size(), Mode: info.Mode(),
			Directory: entry.IsDir(), Symlink: entry.Type()&fs.ModeSymlink != 0,
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Path < result[right].Path })
	return result, nil
}

func (local *Local) WriteFile(ctx context.Context, name string, data []byte, mode fs.FileMode) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mode.Perm() == 0 {
		return errors.New("write mode must grant owner permissions")
	}
	filename, err := local.resolveWritable(name)
	if err != nil {
		return err
	}
	if info, statErr := os.Lstat(filename); statErr == nil {
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("write %q: symlink target is not writable", name)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("write %q: %w", name, ErrSpecialFile)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("inspect write target %q: %w", name, statErr)
	}

	temporary, err := os.CreateTemp(filepath.Dir(filename), ".erka-write-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %q: %w", name, err)
	}
	temporaryName := temporary.Name()
	defer func() {
		closeErr := temporary.Close()
		removeErr := os.Remove(temporaryName)
		if err != nil {
			err = errors.Join(err, closeErr, ignoreNotExist(removeErr))
		}
	}()
	if err := temporary.Chmod(mode.Perm()); err != nil {
		return fmt.Errorf("set temporary mode for %q: %w", name, err)
	}
	if _, err := (&contextWriter{ctx: ctx, writer: temporary}).Write(data); err != nil {
		return fmt.Errorf("write temporary file for %q: %w", name, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary file for %q: %w", name, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file for %q: %w", name, err)
	}
	if err := replaceFile(temporaryName, filename); err != nil {
		return fmt.Errorf("replace %q atomically: %w", name, err)
	}
	return nil
}

func (local *Local) Run(ctx context.Context, request erkaruntime.CommandRequest) (result erkaruntime.CommandResult, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if strings.TrimSpace(request.Executable) == "" {
		return result, errors.New("command executable is empty")
	}
	if request.Timeout <= 0 || request.OutputLimitBytes <= 0 {
		return result, errors.New("command timeout and output limit must be positive")
	}
	directory, err := local.resolveExisting(request.Directory)
	if err != nil {
		return result, fmt.Errorf("resolve command directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return result, fmt.Errorf("command directory %q is not a directory", request.Directory)
	}

	command, cleanup, err := local.newCommand(request.Executable, request.Arguments, directory, request.Unconfined, request.Environment)
	if err != nil {
		return result, err
	}
	defer cleanup()
	command.Stdin = bytes.NewReader(request.StandardInput)
	budget := &outputBudget{remaining: request.OutputLimitBytes}
	stdout := &boundedBuffer{budget: budget}
	stderr := &boundedBuffer{budget: budget}
	command.Stdout = stdout
	command.Stderr = stderr
	configureProcess(command)

	started := time.Now()
	if err := command.Start(); err != nil {
		return result, fmt.Errorf("start command %q: %w", request.Executable, err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	timer := time.NewTimer(request.Timeout)
	defer timer.Stop()

	select {
	case waitErr := <-waited:
		err = waitErr
	case <-ctx.Done():
		killErr := terminateProcess(command)
		waitErr := <-waited
		return commandResult(command, stdout, stderr, budget, started), errors.Join(ctx.Err(), killErr, ignoreExitError(waitErr))
	case <-timer.C:
		killErr := terminateProcess(command)
		waitErr := <-waited
		return commandResult(command, stdout, stderr, budget, started), errors.Join(ErrCommandTimedOut, killErr, ignoreExitError(waitErr))
	}

	result = commandResult(command, stdout, stderr, budget, started)
	var exitError *exec.ExitError
	if err == nil {
		return result, nil
	}
	if errors.As(err, &exitError) {
		return result, nil
	}
	return result, fmt.Errorf("wait for command %q: %w", request.Executable, err)
}

func commandResult(command *exec.Cmd, stdout, stderr *boundedBuffer, budget *outputBudget, started time.Time) erkaruntime.CommandResult {
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	return erkaruntime.CommandResult{
		ExitCode: exitCode, StandardOutput: stdout.String(), StandardError: stderr.String(),
		Truncated: budget.Truncated(), Duration: time.Since(started),
	}
}

func (local *Local) resolveExisting(name string) (string, error) {
	candidate, err := local.join(name)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", name, err)
	}
	if !within(local.root, canonical) {
		return "", fmt.Errorf("resolve %q: %w", name, ErrPathEscape)
	}
	return canonical, nil
}

func (local *Local) resolveWritable(name string) (string, error) {
	candidate, err := local.join(name)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(candidate))
	if err != nil {
		return "", fmt.Errorf("resolve parent of %q: %w", name, err)
	}
	if !within(local.root, parent) {
		return "", fmt.Errorf("resolve %q: %w", name, ErrPathEscape)
	}
	resolved := filepath.Join(parent, filepath.Base(candidate))
	if relative, err := filepath.Rel(local.root, resolved); err == nil {
		for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
			if strings.EqualFold(part, ".git") {
				return "", fmt.Errorf("write %q: %w", name, ErrProtectedPath)
			}
		}
	}
	return resolved, nil
}

func (local *Local) join(name string) (string, error) {
	normalized := strings.ReplaceAll(name, `\`, "/")
	if normalized == "" {
		normalized = "."
	}
	cleaned := path.Clean(normalized)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") || windowsAbsolute(cleaned) {
		return "", fmt.Errorf("resolve %q: %w", name, ErrPathEscape)
	}
	candidate := filepath.Join(local.root, filepath.FromSlash(cleaned))
	if !within(local.root, candidate) {
		return "", fmt.Errorf("resolve %q: %w", name, ErrPathEscape)
	}
	return candidate, nil
}

func within(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func windowsAbsolute(value string) bool {
	return strings.HasPrefix(value, "//") ||
		(len(value) >= 2 && value[1] == ':' && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')))
}

var allowedEnvironment = map[string]struct{}{
	"COMSPEC": {}, "HOME": {}, "LANG": {}, "LC_ALL": {}, "PATH": {},
	"PATHEXT": {}, "SYSTEMROOT": {}, "TEMP": {}, "TMP": {}, "TMPDIR": {},
	"USERPROFILE": {}, "WINDIR": {},
}

func filterEnvironment(environment []string) []string {
	values := make(map[string]string)
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		upper := strings.ToUpper(key)
		if _, allowed := allowedEnvironment[upper]; allowed {
			values[upper] = value
		}
	}
	return environmentMap(values)
}

func mergeEnvironment(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		key, value, found := strings.Cut(entry, "=")
		if found {
			values[strings.ToUpper(key)] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	return environmentMap(values)
}

func environmentMap(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(data)
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (writer *contextWriter) Write(data []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	return writer.writer.Write(data)
}

type outputBudget struct {
	mu        sync.Mutex
	remaining int64
	truncated bool
}

func (budget *outputBudget) Truncated() bool {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.truncated
}

type boundedBuffer struct {
	budget *outputBudget
	buffer bytes.Buffer
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	buffer.budget.mu.Lock()
	defer buffer.budget.mu.Unlock()
	originalLength := len(data)
	allowed := int64(len(data))
	if allowed > buffer.budget.remaining {
		allowed = buffer.budget.remaining
		buffer.budget.truncated = true
	}
	data = data[:allowed]
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	_, _ = buffer.buffer.Write(data)
	buffer.budget.remaining -= int64(len(data))
	return originalLength, nil
}

func (buffer *boundedBuffer) String() string {
	buffer.budget.mu.Lock()
	defer buffer.budget.mu.Unlock()
	return buffer.buffer.String()
}

func ignoreExitError(err error) error {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return nil
	}
	return err
}

func ignoreNotExist(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

var _ erkaruntime.Sandbox = (*Local)(nil)
