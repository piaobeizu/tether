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
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	shellDefaultTimeout = 30 * time.Second
	shellMaxTimeout     = 300 * time.Second
	maxOutputBytes      = 4 << 20 // 4 MiB per stream
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
		Description: "Run a shell command inside the workspace (30s default timeout, max 300s). Returns stdout, stderr, exit_code, and truncated as JSON. Each output stream is capped at 4 MiB.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Shell command to execute via sh -c"},"cwd":{"type":"string","description":"Working directory relative to workspace root (default: workspace root)"},"timeout_secs":{"type":"integer","description":"Timeout in seconds (1–300, default 30)"}},"required":["command"]}`),
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
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (r *Registry) handleRunShell(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Command     string `json:"command"`
		Cwd         string `json:"cwd"`
		TimeoutSecs int    `json:"timeout_secs"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
		return errResult(fmt.Sprintf("workspace_run_shell: invalid arguments: %v", err)), nil
	}
	if params.Command == "" {
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

	timeout := shellDefaultTimeout
	if params.TimeoutSecs > 0 {
		t := time.Duration(params.TimeoutSecs) * time.Second
		if t > shellMaxTimeout {
			t = shellMaxTimeout
		}
		timeout = t
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command("sh", "-c", params.Command)
	cmd.Dir = cwd
	// Setpgid puts the child in its own process group so we can kill the
	// entire group (child + any spawned subprocesses) on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutLW := &limitWriter{w: &stdoutBuf, remaining: maxOutputBytes}
	stderrLW := &limitWriter{w: &stderrBuf, remaining: maxOutputBytes}
	cmd.Stdout = stdoutLW
	cmd.Stderr = stderrLW

	if err := cmd.Start(); err != nil {
		return errResult(fmt.Sprintf("workspace_run_shell: %v", err)), nil
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var runErr error
	select {
	case runErr = <-done:
		// normal completion
	case <-ctx.Done():
		// Kill the entire process group to avoid orphaned children.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
		return errResult("workspace_run_shell: timed out"), nil
	}

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return errResult(fmt.Sprintf("workspace_run_shell: %v", runErr)), nil
		}
	}

	b, _ := json.Marshal(shellResult{
		Stdout:    stdoutBuf.String(),
		Stderr:    stderrBuf.String(),
		ExitCode:  exitCode,
		Truncated: stdoutLW.truncated || stderrLW.truncated,
	})
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

// limitWriter caps writes at remaining bytes; excess bytes are silently
// discarded (truncated=true). Always reports the full len(p) to callers so
// exec's pipe machinery doesn't stall.
type limitWriter struct {
	w         *bytes.Buffer
	remaining int64
	truncated bool
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	orig := len(p)
	if lw.remaining <= 0 {
		lw.truncated = true
		return orig, nil
	}
	if int64(len(p)) > lw.remaining {
		p = p[:lw.remaining]
		lw.truncated = true
	}
	n, err := lw.w.Write(p)
	lw.remaining -= int64(n)
	return orig, err
}

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
