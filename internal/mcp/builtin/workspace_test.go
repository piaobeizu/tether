package builtin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
