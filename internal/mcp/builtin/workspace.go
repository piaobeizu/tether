// Package builtin provides built-in MCP tools for the tether workspace daemon.
// All tools are scoped to a workspace root; path traversal attacks are
// rejected via filepath.EvalSymlinks before any filesystem access.
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const shellTimeout = 30 * time.Second

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

// RegisterInto adds workspace_read_file, workspace_list_files, and
// workspace_run_shell to srv.
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

	srv.AddTool(&mcp.Tool{
		Name:        "workspace_run_shell",
		Description: "Run a shell command inside the workspace. Returns stdout, stderr, and exit code as JSON.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Shell command to execute via sh -c"},"cwd":{"type":"string","description":"Working directory relative to workspace root (default: workspace root)"}},"required":["command"]}`),
	}, r.handleRunShell)
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

type shellResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func (r *Registry) handleRunShell(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &params); err != nil || params.Command == "" {
		return errResult("workspace_run_shell: missing required field 'command'"), nil
	}

	cwd := r.root
	if params.Cwd != "" {
		resolved, err := r.SafeJoin(params.Cwd)
		if err != nil {
			return errResult(fmt.Sprintf("workspace_run_shell: invalid cwd: %v", err)), nil
		}
		cwd = resolved
	}

	ctx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", params.Command)
	cmd.Dir = cwd

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return errResult(fmt.Sprintf("workspace_run_shell: %v", err)), nil
		}
	}

	b, _ := json.Marshal(shellResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	})
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
