// Package builtin provides built-in MCP tools for the tether workspace daemon.
// All tools are scoped to a workspace root; path traversal attacks are
// rejected via filepath.EvalSymlinks before any filesystem access.
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Registry holds builtin MCP tool registrations scoped to a workspace root.
type Registry struct {
	root string // resolved via filepath.EvalSymlinks at construction
}

// New creates a Registry anchored at workspaceRoot.
// Returns an error if workspaceRoot cannot be resolved.
func New(workspaceRoot string) (*Registry, error) {
	resolved, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("builtin: resolve workspace root: %w", err)
	}
	return &Registry{root: resolved}, nil
}

// SafeJoin resolves input relative to the workspace root, rejecting any path
// that escapes the root via traversal or symlinks. Exported for testing.
func (r *Registry) SafeJoin(input string) (string, error) {
	if filepath.IsAbs(input) {
		return "", fmt.Errorf("absolute path not allowed")
	}
	joined := filepath.Join(r.root, input)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("path not accessible: %w", err)
	}
	// Separator-suffixed prefix prevents /tmp/ws matching /tmp/ws-evil.
	rootWithSep := r.root + string(os.PathSeparator)
	if resolved != r.root && !strings.HasPrefix(resolved, rootWithSep) {
		return "", fmt.Errorf("path escapes workspace root")
	}
	return resolved, nil
}

// RegisterInto adds workspace_read_file and workspace_list_files to srv.
func (r *Registry) RegisterInto(srv *mcp.Server) {
	srv.AddTool(&mcp.Tool{
		Name:        "workspace_read_file",
		Description: "Read a file from the active workspace.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Relative path within workspace"}},"required":["path"]}`),
	}, r.handleReadFile)

	srv.AddTool(&mcp.Tool{
		Name:        "workspace_list_files",
		Description: "List files and directories one level deep within the workspace.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"dir":{"type":"string","description":"Relative directory path (default: workspace root)"}}}`),
	}, r.handleListFiles)
}

func (r *Registry) handleReadFile(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &params); err != nil || params.Path == "" {
		return errResult("workspace_read_file: missing required field 'path'"), nil
	}
	resolved, err := r.SafeJoin(params.Path)
	if err != nil {
		return errResult(fmt.Sprintf("workspace_read_file: %v", err)), nil
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return errResult(fmt.Sprintf("workspace_read_file: %v", err)), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil
}

type fileEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
}

func (r *Registry) handleListFiles(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Dir string `json:"dir"`
	}
	_ = json.Unmarshal(req.Params.Arguments, &params)
	if params.Dir == "" {
		params.Dir = "."
	}
	resolved, err := r.SafeJoin(params.Dir)
	if err != nil {
		return errResult(fmt.Sprintf("workspace_list_files: %v", err)), nil
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return errResult(fmt.Sprintf("workspace_list_files: %v", err)), nil
	}
	files := make([]fileEntry, len(entries))
	for i, e := range entries {
		files[i] = fileEntry{Name: e.Name(), IsDir: e.IsDir()}
	}
	b, _ := json.Marshal(files)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
