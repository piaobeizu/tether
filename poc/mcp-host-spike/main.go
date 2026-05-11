// Spike: verify go-sdk Client+Server in-process wiring.
// Tests: NewInMemoryTransports, AddTool (generic), ListTools, CallTool.
// Run: go run ./poc/mcp-host-spike/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- tool input/output types ---

type GreetInput struct {
	Name string `json:"name" jsonschema:"Name to greet"`
}

type GreetOutput struct {
	Message string `json:"message"`
}

type EchoInput struct {
	Text string `json:"text" jsonschema:"Text to echo back"`
}

type EchoOutput struct {
	Echo string `json:"echo"`
}

// --- tool handlers ---

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func greetHandler(_ context.Context, _ *mcp.CallToolRequest, in GreetInput) (*mcp.CallToolResult, GreetOutput, error) {
	msg := "Hello, " + in.Name + "!"
	return textResult(msg), GreetOutput{Message: msg}, nil
}

func echoHandler(_ context.Context, _ *mcp.CallToolRequest, in EchoInput) (*mcp.CallToolResult, EchoOutput, error) {
	return textResult(in.Text), EchoOutput{Echo: in.Text}, nil
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.Background()

	// --- build server ---
	server := mcp.NewServer(&mcp.Implementation{Name: "spike-server", Version: "v0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "greet", Description: "Say hello to someone"}, greetHandler)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo text back"}, echoHandler)

	// --- in-process transports (no subprocess needed) ---
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	// connect server in background
	serverErrCh := make(chan error, 1)
	go func() {
		_, err := server.Connect(ctx, serverTransport, nil)
		serverErrCh <- err
	}()

	// --- build client and connect ---
	client := mcp.NewClient(&mcp.Implementation{Name: "spike-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		log.Error("client connect failed", "err", err)
		os.Exit(1)
	}
	defer session.Close()

	// --- A: list tools ---
	fmt.Println("=== ListTools ===")
	listResult, err := session.ListTools(ctx, nil)
	if err != nil {
		log.Error("ListTools failed", "err", err)
		os.Exit(1)
	}
	for _, t := range listResult.Tools {
		schema, _ := json.Marshal(t.InputSchema)
		fmt.Printf("  tool: %-10s  desc: %-30s  schema: %s\n", t.Name, t.Description, schema)
	}

	// --- B: call greet ---
	fmt.Println("\n=== CallTool: greet ===")
	greetRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "greet",
		Arguments: map[string]any{"name": "tether"},
	})
	if err != nil {
		log.Error("CallTool greet failed", "err", err)
		os.Exit(1)
	}
	printResult(greetRes)

	// --- C: call echo ---
	fmt.Println("\n=== CallTool: echo ===")
	echoRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "MCP host spike OK"},
	})
	if err != nil {
		log.Error("CallTool echo failed", "err", err)
		os.Exit(1)
	}
	printResult(echoRes)

	fmt.Println("\n[spike PASS]")
}

func printResult(r *mcp.CallToolResult) {
	for _, c := range r.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			fmt.Printf("  -> %s\n", v.Text)
		default:
			fmt.Printf("  -> (content type %T)\n", v)
		}
	}
	if r.IsError {
		fmt.Println("  [IsError=true]")
	}
}
