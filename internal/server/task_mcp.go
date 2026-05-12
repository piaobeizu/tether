package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	mcphost "github.com/piaobeizu/tether/internal/mcp/host"
	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/permission"
)

// registerTaskMCPAPI mounts the per-task MCP lifecycle REST endpoints.
//
//	POST   /api/v1/tasks/{id}/mcp   → start a per-task MCPInstance
//	DELETE /api/v1/tasks/{id}/mcp   → stop the instance for task {id}
//	GET    /api/v1/tasks/{id}/mcp   → inspect the running instance (port, token)
func registerTaskMCPAPI(mux *http.ServeMux, lm *mcplifecycle.LifecycleManager, pm *permission.Manager) {
	mux.HandleFunc("/api/v1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		// ServeMux routes all /api/v1/tasks/{...} paths here.
		// We only handle paths of the form /api/v1/tasks/{id}/mcp.
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 || parts[1] != "mcp" {
			http.NotFound(w, r)
			return
		}
		taskID := parts[0]
		if taskID == "" {
			http.Error(w, "task id required", http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodPost:
			handleTaskMCPStart(w, r, taskID, lm, pm)
		case http.MethodDelete:
			handleTaskMCPStop(w, r, taskID, lm)
		case http.MethodGet:
			handleTaskMCPGet(w, r, taskID, lm)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// startTaskMCPRequest is the POST body for starting a per-task MCP instance.
type startTaskMCPRequest struct {
	// TaskSlug is the human-readable slug used as the settings key suffix.
	TaskSlug string `json:"slug"`
	// WsRoot is the absolute filesystem path for the task's workspace.
	WsRoot string `json:"ws_root"`
	// ExtraServers are optional additional stdio MCP servers for this task.
	ExtraServers map[string]mcphost.ServerConfig `json:"extra_servers,omitempty"`
	// SkipInject suppresses settings.json mutation (useful in CI).
	SkipInject bool `json:"skip_inject,omitempty"`
}

// startTaskMCPResponse is the POST response body.
type startTaskMCPResponse struct {
	TaskID   string `json:"task_id"`
	TaskSlug string `json:"task_slug"`
	Port     int    `json:"port"`
	Token    string `json:"token"`
}

func handleTaskMCPStart(w http.ResponseWriter, r *http.Request, taskID string, lm *mcplifecycle.LifecycleManager, pm *permission.Manager) {
	var req startTaskMCPRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.TaskSlug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	if req.WsRoot == "" {
		http.Error(w, "ws_root is required", http.StatusBadRequest)
		return
	}

	inst, err := lm.StartTask(r.Context(), mcplifecycle.TaskConfig{
		TaskID:       taskID,
		TaskSlug:     req.TaskSlug,
		WsRoot:       req.WsRoot,
		ExtraServers: req.ExtraServers,
		PermManager:  pm,
		SkipInject:   req.SkipInject,
	})
	if err != nil {
		slog.Error("task mcp: start failed", "task_id", taskID, "err", err)
		http.Error(w, "failed to start MCP instance: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(startTaskMCPResponse{
		TaskID:   inst.TaskID,
		TaskSlug: inst.TaskSlug,
		Port:     inst.Port,
		Token:    inst.Token,
	})
}

func handleTaskMCPStop(w http.ResponseWriter, r *http.Request, taskID string, lm *mcplifecycle.LifecycleManager) {
	stopCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := lm.StopTask(stopCtx, taskID); err != nil {
		slog.Warn("task mcp: stop failed", "task_id", taskID, "err", err)
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleTaskMCPGet(w http.ResponseWriter, r *http.Request, taskID string, lm *mcplifecycle.LifecycleManager) {
	inst, ok := lm.Get(taskID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task_id":   inst.TaskID,
		"task_slug": inst.TaskSlug,
		"ws_root":   inst.WsRoot,
		"port":      inst.Port,
		"token":     inst.Token,
	})
}
