package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dwrtz/mcp-go/pkg/logger"
	"github.com/dwrtz/mcp-go/pkg/mcp/client"
)

// TestMCPTermIntegration verifies that the mcpterm server app
// can be launched and interacted with via the MCP client.
func TestMCPTermIntegration(t *testing.T) {
	// Locate the mcpterm binary. Adjust if your build path differs.
	// For example, if you build into ./bin/mcpterm, you might do:
	//
	//   cmdPath := filepath.Join("bin", "mcpterm")
	//
	// Or set cmdPath to the absolute path if needed.
	cmdPath := filepath.Join("bin", "mcpterm")

	if _, err := os.Stat(cmdPath); os.IsNotExist(err) {
		t.Skipf("mcpterm binary not found at %s; build it before running this test", cmdPath)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create the MCP client, specifying the local mcpterm binary as the "server" to run.
	cl, err := client.NewDefaultClient(ctx, cmdPath)
	if err != nil {
		t.Fatalf("Failed to create MCP client with mcpterm: %v", err)
	}
	defer func() {
		_ = cl.Close()
	}()

	// Initialize the MCP connection.
	if err := cl.Initialize(ctx); err != nil {
		t.Fatalf("Client initialization failed: %v", err)
	}

	// Now call the "run" tool exposed by mcpterm to run a simple command.
	args := map[string]interface{}{
		"command":   "echo 'Hello from integration test'",
		"sessionId": "testSession123",
	}
	result, err := cl.CallTool(ctx, "run", args)
	if err != nil {
		t.Fatalf("CallTool(run) failed: %v", err)
	}

	// Verify output in the tool result.
	if len(result.Content) == 0 {
		t.Fatalf("Expected some tool output, got none.")
	}
	textContent, ok := result.Content[0].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected text content in map form, got: %T", result.Content[0])
	}

	gotText, _ := textContent["text"].(string)
	if gotText == "" {
		t.Errorf("Expected non-empty text output from run tool, got empty string")
	}
	t.Logf("Tool output: %q", gotText)
}

// Optionally, you can add more tests below that call "runScreen" or other tools
// to verify interactive behavior, multi-line commands, error handling, etc.
func TestMCPTerm_RunScreen(t *testing.T) {
	cmdPath := filepath.Join("bin", "mcpterm")
	if _, err := os.Stat(cmdPath); os.IsNotExist(err) {
		t.Skipf("mcpterm binary not found at %s; build it before running this test", cmdPath)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cl, err := client.NewDefaultClient(ctx, cmdPath)
	if err != nil {
		t.Fatalf("Failed to create MCP client: %v", err)
	}
	defer cl.Close()

	if err := cl.Initialize(ctx); err != nil {
		t.Fatalf("Client initialization failed: %v", err)
	}

	args := map[string]interface{}{
		"command":   "echo 'Screen test'\n",
		"sessionId": "testScreenSession",
	}
	result, err := cl.CallTool(ctx, "runScreen", args)
	if err != nil {
		t.Fatalf("CallTool(runScreen) failed: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatalf("Expected output from runScreen, got none.")
	}
	textContent, ok := result.Content[0].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected text content, got: %T", result.Content[0])
	}
	gotText, _ := textContent["text"].(string)
	t.Logf("runScreen output: %q", gotText)
}

// Example test for verifying multi-line outputs or other scenarios...
func TestMCPTerm_MultiLineOutput(t *testing.T) {
	cmdPath := filepath.Join("bin", "mcpterm")
	if _, err := os.Stat(cmdPath); os.IsNotExist(err) {
		t.Skipf("mcpterm binary not found at %s; build it before running this test", cmdPath)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cl, err := client.NewDefaultClient(ctx, cmdPath)
	if err != nil {
		t.Fatalf("Failed to create MCP client: %v", err)
	}
	defer cl.Close()

	if err := cl.Initialize(ctx); err != nil {
		t.Fatalf("Client initialization failed: %v", err)
	}
	lg := logger.NewStderrLogger("MCPTERM-TEST")
	lg.Logf("Initialized successfully")

	args := map[string]interface{}{
		"command":   "echo 'line1' && echo 'line2'",
		"sessionId": "multiLineSession",
	}

	result, err := cl.CallTool(ctx, "run", args)
	if err != nil {
		t.Fatalf("CallTool(run) failed: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatalf("Expected some tool output, got none.")
	}
	firstContent, ok := result.Content[0].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected text content, got: %T", result.Content[0])
	}

	gotText, _ := firstContent["text"].(string)
	if gotText == "" {
		t.Errorf("Expected multi-line text output, got empty.")
	}
	t.Logf("Multi-line output: %q", gotText)
}
