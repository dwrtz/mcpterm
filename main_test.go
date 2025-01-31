package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dwrtz/mcp-go/pkg/types"
)

func TestSessionManager(t *testing.T) {
	// Create session manager and ensure cleanup
	sm := NewSessionManager()
	t.Cleanup(func() {
		if err := sm.Close(); err != nil {
			t.Errorf("Failed to close session manager: %v", err)
		}
	})

	t.Run("CreateSession", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		done := make(chan struct{})
		var session *Session
		var err error

		go func() {
			session, err = sm.GetOrCreate("test1")
			close(done)
		}()

		select {
		case <-done:
			if err != nil {
				t.Fatalf("Failed to create session: %v", err)
			}
			if session == nil {
				t.Fatal("Session is nil")
			}
			if session.ptmx == nil {
				t.Fatal("PTY is nil")
			}
		case <-ctx.Done():
			t.Fatal("Test timed out")
		}
	})

	t.Run("ReuseSession", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		done := make(chan struct{})
		var session1, session2 *Session
		var err1, err2 error

		go func() {
			session1, err1 = sm.GetOrCreate("test2")
			session2, err2 = sm.GetOrCreate("test2")
			close(done)
		}()

		select {
		case <-done:
			if err1 != nil {
				t.Fatalf("Failed to create first session: %v", err1)
			}
			if err2 != nil {
				t.Fatalf("Failed to get existing session: %v", err2)
			}
			if session1 != session2 {
				t.Error("Got different session instances for same ID")
			}
		case <-ctx.Done():
			t.Fatal("Test timed out")
		}
	})

	t.Run("CleanupSession", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		done := make(chan struct{})
		var session *Session
		var err error

		go func() {
			session, err = sm.GetOrCreate("test3")
			if err != nil {
				close(done)
				return
			}

			session.lastUse = time.Now().Add(-31 * time.Minute)
			sm.cleanup()

			sm.mu.RLock()
			_, exists := sm.sessions["test3"]
			sm.mu.RUnlock()

			if exists {
				err = fmt.Errorf("session was not cleaned up")
			}
			close(done)
		}()

		select {
		case <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("Test timed out")
		}
	})
}

func TestRunTool(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Close()
	ctx := context.Background()

	runTool := types.NewTool[RunInput](
		"run",
		"Execute a command synchronously and return its output",
		sm.Run,
	)

	t.Run("SimpleCommand", func(t *testing.T) {
		result, err := runTool.GetHandler()(ctx, map[string]interface{}{
			"command":   "echo 'hello world'",
			"sessionId": "test4",
		})

		if err != nil {
			t.Fatalf("Command failed: %v", err)
		}

		if len(result.Content) != 1 {
			t.Fatalf("Expected 1 content item, got %d", len(result.Content))
		}

		content, ok := result.Content[0].(types.TextContent)
		if !ok {
			t.Fatal("Content is not TextContent")
		}

		expected := "hello world"
		if strings.TrimSpace(content.Text) != expected {
			t.Errorf("Expected output %q, got %q", expected, content.Text)
		}
	})

	t.Run("MultiLineOutput", func(t *testing.T) {
		result, err := runTool.GetHandler()(ctx, map[string]interface{}{
			"command":   "echo 'line1' && echo 'line2'",
			"sessionId": "test5",
		})

		if err != nil {
			t.Fatalf("Command failed: %v", err)
		}

		content := result.Content[0].(types.TextContent)
		lines := strings.Split(strings.TrimSpace(content.Text), "\n")
		if len(lines) != 2 {
			t.Fatalf("Expected 2 lines, got %d", len(lines))
		}

		if lines[0] != "line1" || lines[1] != "line2" {
			t.Errorf("Unexpected output: %v", lines)
		}
	})

	t.Run("CommandWithError", func(t *testing.T) {
		result, err := runTool.GetHandler()(ctx, map[string]interface{}{
			"command":   "ls /nonexistent",
			"sessionId": "test6",
		})

		if err != nil {
			t.Fatalf("Handler returned error: %v", err)
		}

		content := result.Content[0].(types.TextContent)
		if !strings.Contains(content.Text, "No such file or directory") {
			t.Errorf("Expected error message in output, got: %s", content.Text)
		}
	})

	t.Run("StatePreservation", func(t *testing.T) {
		sessionId := "test7"

		// First command: create a file
		_, err := runTool.GetHandler()(ctx, map[string]interface{}{
			"command":   "echo 'test content' > /tmp/test.txt",
			"sessionId": sessionId,
		})
		if err != nil {
			t.Fatalf("First command failed: %v", err)
		}

		// Second command: read the file
		result, err := runTool.GetHandler()(ctx, map[string]interface{}{
			"command":   "cat /tmp/test.txt",
			"sessionId": sessionId,
		})
		if err != nil {
			t.Fatalf("Second command failed: %v", err)
		}

		content := result.Content[0].(types.TextContent)
		expected := "test content"
		if strings.TrimSpace(content.Text) != expected {
			t.Errorf("Expected %q, got %q", expected, content.Text)
		}

		// Cleanup
		_, err = runTool.GetHandler()(ctx, map[string]interface{}{
			"command":   "rm /tmp/test.txt",
			"sessionId": sessionId,
		})
		if err != nil {
			t.Errorf("Cleanup failed: %v", err)
		}
	})
}

func TestRunScreenTool(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Close()
	ctx := context.Background()

	runScreenTool := types.NewTool[RunScreenInput](
		"runScreen",
		"Send input to an interactive terminal session",
		sm.RunScreen,
	)

	t.Run("InteractiveCommand", func(t *testing.T) {
		result, err := runScreenTool.GetHandler()(ctx, map[string]interface{}{
			"command":   "echo 'interactive test'\n",
			"sessionId": "test8",
		})

		if err != nil {
			t.Fatalf("Command failed: %v", err)
		}

		content := result.Content[0].(types.TextContent)
		if !strings.Contains(content.Text, "interactive test") {
			t.Errorf("Expected output to contain 'interactive test', got: %s", content.Text)
		}
	})

	t.Run("PromptPreservation", func(t *testing.T) {
		sessionId := "test9"

		// First command to set up environment
		_, err := runScreenTool.GetHandler()(ctx, map[string]interface{}{
			"command":   "PS1='custom> '\n",
			"sessionId": sessionId,
		})
		if err != nil {
			t.Fatalf("Setup failed: %v", err)
		}

		// Second command should show custom prompt
		result, err := runScreenTool.GetHandler()(ctx, map[string]interface{}{
			"command":   "echo 'test'\n",
			"sessionId": sessionId,
		})
		if err != nil {
			t.Fatalf("Command failed: %v", err)
		}

		content := result.Content[0].(types.TextContent)
		if !strings.Contains(content.Text, "custom>") {
			t.Errorf("Expected output to contain custom prompt, got: %s", content.Text)
		}
	})
}

func TestConcurrentSessions(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Close()

	// Create several concurrent sessions
	const numSessions = 5
	var wg sync.WaitGroup
	errChan := make(chan error, numSessions)

	for i := 0; i < numSessions; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sessionId := fmt.Sprintf("concurrent%d", id)

			session, err := sm.GetOrCreate(sessionId)
			if err != nil {
				errChan <- fmt.Errorf("session %s creation failed: %v", sessionId, err)
				return
			}

			// Write some data
			session.mu.Lock()
			_, err = session.ptmx.Write([]byte(fmt.Sprintf("echo 'test%d'\n", id)))
			session.mu.Unlock()

			if err != nil {
				errChan <- fmt.Errorf("session %s write failed: %v", sessionId, err)
			}
		}(i)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Errorf("Concurrent session error: %v", err)
	}

	// Verify session count
	sm.mu.RLock()
	sessionCount := len(sm.sessions)
	sm.mu.RUnlock()

	if sessionCount != numSessions {
		t.Errorf("Expected %d sessions, got %d", numSessions, sessionCount)
	}
}
