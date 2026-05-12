package builtin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/builtin"
)

func TestSafeJoin_TraversalRejected(t *testing.T) {
	root := t.TempDir()
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reg.SafeJoin("../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestSafeJoin_AbsolutePathRejected(t *testing.T) {
	root := t.TempDir()
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reg.SafeJoin("/etc/passwd")
	if err == nil {
		t.Fatal("expected error for absolute path")
	}
}

func TestSafeJoin_SiblingDirRejected(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	sibling := filepath.Join(parent, "ws-evil")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reg.SafeJoin("../ws-evil/secret")
	if err == nil {
		t.Fatal("expected error for sibling dir escape")
	}
}

func TestReadFile_ReturnsContent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterInto(srv)

	args, _ := json.Marshal(map[string]string{"path": "hello.txt"})
	result := invokeBuiltin(t, srv, "workspace_read_file", args)
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.Content)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if text != "world" {
		t.Fatalf("got %q, want %q", text, "world")
	}
}

func TestListFiles_ReturnsDirEntries(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	_ = os.Mkdir(filepath.Join(root, "sub"), 0o755)

	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterInto(srv)

	args, _ := json.Marshal(map[string]string{"dir": "."})
	result := invokeBuiltin(t, srv, "workspace_list_files", args)
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.Content)
	}
}

func TestRunShell_Success(t *testing.T) {
	root := t.TempDir()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterInto(srv)

	args, _ := json.Marshal(map[string]string{"command": "echo hello"})
	result := invokeBuiltin(t, srv, "workspace_run_shell", args)
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.Content)
	}
	var out struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", out.ExitCode)
	}
	if !strings.Contains(out.Stdout, "hello") {
		t.Fatalf("expected stdout to contain 'hello', got %q", out.Stdout)
	}
}

func TestRunShell_NonZeroExit(t *testing.T) {
	root := t.TempDir()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterInto(srv)

	args, _ := json.Marshal(map[string]string{"command": "exit 42"})
	result := invokeBuiltin(t, srv, "workspace_run_shell", args)
	if result.IsError {
		t.Fatalf("unexpected tool-level error: %v", result.Content)
	}
	var out struct {
		ExitCode int `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 42 {
		t.Fatalf("expected exit_code 42, got %d", out.ExitCode)
	}
}

func TestRunShell_MissingCommand(t *testing.T) {
	root := t.TempDir()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterInto(srv)

	args, _ := json.Marshal(map[string]string{})
	result := invokeBuiltin(t, srv, "workspace_run_shell", args)
	if !result.IsError {
		t.Fatal("expected IsError=true for missing command")
	}
}

func TestRunShell_CwdEscapeRejected(t *testing.T) {
	root := t.TempDir()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterInto(srv)

	args, _ := json.Marshal(map[string]string{"command": "pwd", "cwd": "../../"})
	result := invokeBuiltin(t, srv, "workspace_run_shell", args)
	if !result.IsError {
		t.Fatal("expected IsError=true for cwd traversal")
	}
}

// invokeBuiltin connects a test client to srv (in-memory) and calls toolName.
// args is a JSON object encoded as raw bytes.
func invokeBuiltin(t *testing.T, srv *mcp.Server, toolName string, args json.RawMessage) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()

	// Servers must be connected before clients per SDK docs.
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("srv.Connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "testclient"}, nil)
	clientSess, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer clientSess.Close()

	result, err := clientSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", toolName, err)
	}
	return result
}

func TestRunShell_TruncatesLargeOutput(t *testing.T) {
	root := t.TempDir()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterInto(srv)

	// Generate output larger than maxOutputBytes (4 MiB); dd writes 5 MiB zeros.
	args, _ := json.Marshal(map[string]string{"command": "dd if=/dev/zero bs=1M count=5 2>/dev/null | cat"})
	result := invokeBuiltin(t, srv, "workspace_run_shell", args)
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	var out struct {
		Stdout    string `json:"stdout"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Truncated {
		t.Fatal("expected truncated=true for 5 MiB output")
	}
	if len(out.Stdout) > 4<<20+1 {
		t.Fatalf("stdout exceeds 4 MiB cap: %d bytes", len(out.Stdout))
	}
}


func TestRunShell_CustomTimeout(t *testing.T) {
	root := t.TempDir()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterInto(srv)

	// 1s timeout, command sleeps 10s — should time out.
	args, _ := json.Marshal(map[string]any{"command": "sleep 10", "timeout_secs": 1})
	result := invokeBuiltin(t, srv, "workspace_run_shell", args)
	if !result.IsError {
		t.Fatal("expected IsError=true for timed-out command")
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "timed out") {
		t.Fatalf("expected 'timed out' in error, got %q", text)
	}
}
