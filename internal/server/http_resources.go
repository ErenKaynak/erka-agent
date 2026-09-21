package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/memory"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

// MemoryAPIStore is the durable memory state behind /v1/memories.
type MemoryAPIStore interface {
	memory.Store
	ListMemories(context.Context, string, int) ([]store.MemoryNote, error)
	DeleteMemory(context.Context, string) error
}

func (api *API) resourceRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/previews/presets", api.authenticate("read", http.HandlerFunc(api.listPreviewPresets)))
	mux.Handle("GET /v1/tasks/{id}/previews", api.authenticate("read", http.HandlerFunc(api.listPreviews)))
	mux.Handle("POST /v1/tasks/{id}/previews", api.authenticate("write", http.HandlerFunc(api.startPreview)))
	mux.Handle("DELETE /v1/previews/{id}", api.authenticate("write", http.HandlerFunc(api.stopPreview)))
	mux.Handle("GET /v1/previews/{id}/log", api.authenticate("read", http.HandlerFunc(api.previewLog)))
	mux.Handle("GET /v1/memories", api.authenticate("read", http.HandlerFunc(api.listMemories)))
	mux.Handle("POST /v1/memories", api.authenticate("write", http.HandlerFunc(api.createMemory)))
	mux.Handle("DELETE /v1/memories/{id}", api.authenticate("write", http.HandlerFunc(api.deleteMemory)))
}

func (api *API) listPreviewPresets(response http.ResponseWriter, _ *http.Request) {
	presets := api.previews.Presets()
	if presets == nil {
		presets = []string{}
	}
	writeJSON(response, http.StatusOK, map[string]any{"presets": presets})
}

func (api *API) listPreviews(response http.ResponseWriter, request *http.Request) {
	taskID := erkaruntime.TaskID(request.PathValue("id"))
	if _, err := api.store.LoadServerTask(request.Context(), taskID); err != nil {
		writeMappedError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"previews": api.previews.List(taskID)})
}

func (api *API) startPreview(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Preset string `json:"preset"`
	}
	if err := decodeJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	task, err := api.store.LoadServerTask(request.Context(), erkaruntime.TaskID(request.PathValue("id")))
	if err != nil {
		writeMappedError(response, err)
		return
	}
	preview, err := api.previews.Start(task.TaskID, task.WorktreeRoot, strings.TrimSpace(input.Preset))
	if err != nil {
		writePreviewError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, preview)
}

func (api *API) stopPreview(response http.ResponseWriter, request *http.Request) {
	if err := api.previews.Stop(request.PathValue("id")); err != nil {
		writePreviewError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"status": "stopped"})
}

func (api *API) previewLog(response http.ResponseWriter, request *http.Request) {
	log, err := api.previews.Log(request.PathValue("id"))
	if err != nil {
		writePreviewError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"log": log})
}

func writePreviewError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrPreviewsDisabled):
		writeAPIError(response, http.StatusNotImplemented, "previews_disabled", "Configure server.previews on the server to enable previews")
	case errors.Is(err, erkaruntime.ErrNotFound):
		writeAPIError(response, http.StatusNotFound, "not_found", err.Error())
	default:
		writeAPIError(response, http.StatusUnprocessableEntity, "preview_failed", err.Error())
	}
}

type memoryDTO struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	Kind      string    `json:"kind"`
	Origin    string    `json:"origin"`
	Text      string    `json:"text"`
	TaskID    string    `json:"taskId,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func memoryResponse(note store.MemoryNote) memoryDTO {
	return memoryDTO{ID: note.ID, Scope: note.Scope, Kind: note.Kind, Origin: note.Origin, Text: note.Text, TaskID: string(note.SourceTask), UpdatedAt: note.UpdatedAt}
}

// projectMemoryKey resolves a server project to the memory key shared by its
// source repository and every task worktree.
func (api *API) projectMemoryKey(ctx context.Context, projectID string) (string, error) {
	if projectID == "" {
		return "", nil
	}
	project, err := api.store.LoadServerProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	return memory.ProjectKey(project.RepositoryRoot)
}

func (api *API) listMemories(response http.ResponseWriter, request *http.Request) {
	if api.memories == nil {
		writeAPIError(response, http.StatusNotImplemented, "memory_unavailable", "Memory is not available on this server")
		return
	}
	key, err := api.projectMemoryKey(request.Context(), strings.TrimSpace(request.URL.Query().Get("projectId")))
	if err != nil {
		writeMappedError(response, err)
		return
	}
	var notes []store.MemoryNote
	if query := strings.TrimSpace(request.URL.Query().Get("q")); query != "" {
		notes, err = api.memories.SearchMemories(request.Context(), key, query, 50)
	} else {
		notes, err = api.memories.ListMemories(request.Context(), key, 500)
	}
	if err != nil {
		writeMappedError(response, err)
		return
	}
	result := make([]memoryDTO, len(notes))
	for index, note := range notes {
		result[index] = memoryResponse(note)
	}
	writeJSON(response, http.StatusOK, map[string]any{"memories": result})
}

func (api *API) createMemory(response http.ResponseWriter, request *http.Request) {
	if api.memories == nil {
		writeAPIError(response, http.StatusNotImplemented, "memory_unavailable", "Memory is not available on this server")
		return
	}
	var input struct {
		Text      string `json:"text"`
		Kind      string `json:"kind"`
		ProjectID string `json:"projectId"`
	}
	if err := decodeJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	key, err := api.projectMemoryKey(request.Context(), strings.TrimSpace(input.ProjectID))
	if err != nil {
		writeMappedError(response, err)
		return
	}
	scope, kind := store.MemoryGlobal, strings.TrimSpace(input.Kind)
	if key != "" {
		scope = store.MemoryProject
	}
	if kind == "" {
		kind = "fact"
		if scope == store.MemoryGlobal {
			kind = "preference"
		}
	}
	id, err := (erkaruntime.RandomIDGenerator{}).NewID("memory")
	if err != nil {
		writeMappedError(response, err)
		return
	}
	now := api.clock().UTC()
	note, err := api.memories.SaveMemory(request.Context(), store.MemoryNote{ID: id, Scope: scope, ProjectKey: key, Kind: kind, Origin: "user", Text: input.Text, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_memory", err.Error())
		return
	}
	writeJSON(response, http.StatusCreated, memoryResponse(note))
}

func (api *API) deleteMemory(response http.ResponseWriter, request *http.Request) {
	if api.memories == nil {
		writeAPIError(response, http.StatusNotImplemented, "memory_unavailable", "Memory is not available on this server")
		return
	}
	if err := api.memories.DeleteMemory(request.Context(), request.PathValue("id")); err != nil {
		writeMappedError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"status": "deleted"})
}

var _ MemoryAPIStore = (*store.SQLite)(nil)
