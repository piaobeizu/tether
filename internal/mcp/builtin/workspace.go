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

// The refusals SafeJoin and SafeJoinDir produce themselves, as sentinels rather
// than bare strings, so a caller can turn one into a status AND a body without
// reading err.Error() (tether#159 — internal/workspace/api.go is the caller that
// needs it, and its readRefusal is where the mapping lives).
//
// Each text is byte-identical to the fmt.Errorf string it replaced, so every MCP
// tool result below says exactly what it said before: this changes the errors'
// IDENTITY, not their wording. The remaining error this file builds inline —
// `path not accessible: %w` — keeps wrapping, because the thing a caller needs
// from it is the wrapped fs.ErrNotExist, not the sentence.
var (
	// ErrAbsolutePath — the input named an absolute path. Every SafeJoin input is
	// relative to the workspace root by construction, so an absolute one is not a
	// path this Registry can resolve at all.
	ErrAbsolutePath = errors.New("absolute path not allowed")

	// ErrPathEscapesRoot — the input resolved to something outside the root,
	// through `..`, through a symlink, or through a sibling whose name starts with
	// the root's.
	ErrPathEscapesRoot = errors.New("path escapes workspace root")

	// ErrNotDirectory — the input does not name a directory, and the caller asked
	// for one. Only SafeJoinDir produces it; see its doc for why SafeJoin does not.
	ErrNotDirectory = errors.New("path is not a directory")
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
//
// It answers exactly one question — is this inside the root — and deliberately
// says nothing about the SHAPE of what it found. SafeJoinDir is the variant for a
// caller that needs a directory, and its doc explains why that check cannot be
// folded in here.
func (r *Registry) SafeJoin(input string) (string, error) {
	if filepath.IsAbs(input) {
		return "", ErrAbsolutePath
	}
	joined := filepath.Join(r.root, input)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("path not accessible: %w", err)
	}
	// Separator-suffixed prefix prevents /tmp/ws matching /tmp/ws-evil.
	rootWithSep := r.root + string(os.PathSeparator)
	if resolved != r.root && !strings.HasPrefix(resolved, rootWithSep) {
		return "", ErrPathEscapesRoot
	}
	return resolved, nil
}

// SafeJoinDir is SafeJoin for a caller that will then read the target AS a
// directory: it adds the one check SafeJoin does not make.
//
// # Why this is a sibling and not a check inside SafeJoin
//
// There were five call sites when this was written, and they do not agree on
// what shape the target must have. Two want a FILE: handleReadFile below
// (os.ReadFile) and workspace.ReadFileContent, which stats the result and refuses
// a directory itself. Three want a directory: handleListFiles (os.ReadDir),
// handleRunShell (Cmd.Dir), and workspace.handleFiles (os.ReadDir via listFiles),
// which is the one that now comes through here instead. An IsDir gate inside
// SafeJoin would therefore refuse every workspace_read_file call and every GET
// /api/v1/workspaces/{id}/file, so the check cannot live there however much the
// /files caller wants it to (tether#159). Not an argument: putting the check in
// SafeJoin fails TestSafeJoin_StillAcceptsARegularFile,
// TestReadFile_ReturnsContent, both TestReadFileContent_* and
// TestFileHandler_ReadsFileContent, which is what that first test is for.
//
// The two MCP tools stay on the plain SafeJoin. Their error text is returned to
// the agent that supplied the path and never reaches an HTTP client, so rewording
// it buys nothing and would only put a second set of strings under test.
//
// # What leaving the shape unchecked was costing
//
// GET /api/v1/workspaces/{id}/files?dir=<a regular file> passed containment,
// reached os.ReadDir, and the *fs.PathError that came back — `open
// /home/u/ws/a.txt: not a directory` — was the 500 body verbatim, so any
// authenticated caller could read the daemon's absolute path for any file in a
// workspace in one request. Refusing here is a stronger fix than rewording that
// body: after this there is no raw filesystem error on the path to reword.
//
// # Both ways of not being a directory answer the same
//
// A path whose LAST component is a regular file is caught by the stat below. A
// path that names something UNDER a regular file (`a.txt/b`) never gets that far:
// EvalSymlinks fails first, with a bare syscall.ENOTDIR. They are one caller
// mistake with two spellings, so they share one sentinel. The ENOTDIR test is
// inert on Windows, which reports a path error instead — that leaves `a.txt/b`
// there in the caller's unclassified default, which is a worse STATUS for one odd
// request and not a leak, because the body is chosen by the refusal's identity
// either way.
func (r *Registry) SafeJoinDir(input string) (string, error) {
	resolved, err := r.SafeJoin(input)
	if err != nil {
		if errors.Is(err, syscall.ENOTDIR) {
			return "", fmt.Errorf("%w: %w", ErrNotDirectory, err)
		}
		return "", err
	}
	// resolved came back from EvalSymlinks, so it exists and holds no links:
	// os.Stat and os.Lstat cannot disagree about it.
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		// input, not resolved: the caller's own string is safe to carry into a log
		// line, and the daemon-side path is what this whole function exists to keep
		// out of one.
		return "", fmt.Errorf("%w: %q", ErrNotDirectory, input)
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
	// Set up whatever the timeout path below will kill. Both this call and the
	// killOnTimeout call are per-platform (proc_unix.go, proc_windows.go), and
	// they do not reach equally far: read the platform file for what actually
	// dies on a timeout there.
	setKillScope(cmd)

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
		// Kill what setKillScope put in reach, so the timeout does not leave
		// the command running behind the error we are about to return.
		_ = killOnTimeout(cmd)
		<-done
		return errResult("workspace_run_shell: timed out"), nil
	}

	exitCode := 0
	cutShort := false
	if runErr != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.As(runErr, &exitErr):
			exitCode = exitErr.ExitCode()
		case errors.Is(runErr, exec.ErrWaitDelay):
			// Both platforms' setKillScope set Cmd.WaitDelay, so this is
			// reachable everywhere. It means Wait stopped reading the pipes
			// because something the command left behind was still holding
			// them — and os/exec returns it "instead of nil", i.e. the command
			// itself exited successfully. Reporting it as a tool error would
			// turn a command that worked into a failure with no output at all.
			// What differs per platform is which survivors can get here: on
			// !windows only one that left the process group, since the rest are
			// killed with it; on windows any child at all.
			cutShort = true
		default:
			return errResult(fmt.Sprintf("workspace_run_shell: %v", runErr)), nil
		}
	}

	b, _ := json.Marshal(shellResult{
		Stdout:    stdoutBuf.String(),
		Stderr:    stderrBuf.String(),
		ExitCode:  exitCode,
		Truncated: stdoutLW.truncated || stderrLW.truncated || cutShort,
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
