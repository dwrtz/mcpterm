package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/dwrtz/mcp-go/pkg/logger"
	"github.com/dwrtz/mcp-go/pkg/mcp/server"
	"github.com/dwrtz/mcp-go/pkg/types"
)

const cmdMarker = "#__RUNTOOL_CMD_DONE__"

type RunInput struct {
	Command   string `json:"command" jsonschema:"description=Command to execute synchronously,required"`
	SessionID string `json:"sessionId" jsonschema:"description=Session identifier,required"`
}

type RunScreenInput struct {
	Command   string `json:"command" jsonschema:"description=Raw bytes to send to terminal,required"`
	SessionID string `json:"sessionId" jsonschema:"description=Session identifier,required"`
}

type Session struct {
	ptmx    *os.File
	cmd     *exec.Cmd
	mu      sync.Mutex
	lastUse time.Time
}

// filterOutCommandLines removes any lines from `output` that match the typed command.
func filterOutCommandLines(output, cmd string) string {
	lines := strings.Split(output, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == cmd {
			// skip
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// readUntilMarker continuously reads PTY output into a buffer
// until we see cmdMarker or the context is done.
func readUntilMarker(ctx context.Context, ptmx *os.File) (string, error) {
	marker := []byte(cmdMarker)
	var outputBuf bytes.Buffer
	readBuf := make([]byte, 1024)

	for {
		select {
		case <-ctx.Done():
			// Return whatever we have plus the context error
			return outputBuf.String(), ctx.Err()
		default:
			// Set a deadline so we don't block forever if there's no data
			ptmx.SetDeadline(time.Now().Add(200 * time.Millisecond))

			n, err := ptmx.Read(readBuf)
			if err != nil {
				// EOF means the shell ended. Just return what we got so far.
				if err == io.EOF {
					return outputBuf.String(), nil
				}
				// If it's a timeout or EAGAIN, keep trying until context is canceled
				if os.IsTimeout(err) || err == syscall.EAGAIN {
					continue
				}
				// Otherwise, return partial output plus the error
				return outputBuf.String(), err
			}

			// Clear the read deadline so subsequent reads don't fail immediately.
			ptmx.SetDeadline(time.Time{})

			// Append to our growing buffer
			outputBuf.Write(readBuf[:n])

			// See if the marker appears in our buffer
			allBytes := outputBuf.Bytes()
			idx := bytes.Index(allBytes, marker)
			if idx >= 0 {
				// Extract everything up to (but not including) the marker
				beforeMarker := allBytes[:idx]
				return string(beforeMarker), nil
			}
		}
	}
}

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errs []error
	if s.ptmx != nil {
		if err := s.ptmx.Close(); err != nil {
			errs = append(errs, fmt.Errorf("ptmx close error: %w", err))
		}
	}

	if s.cmd != nil && s.cmd.Process != nil {
		if err := s.cmd.Process.Kill(); err != nil {
			errs = append(errs, fmt.Errorf("process kill error: %w", err))
		}
		if err := s.cmd.Wait(); err != nil {
			// Ignore wait errors after kill
			_ = err
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("session close errors: %v", errs)
	}
	return nil
}

type SessionManager struct {
	sessions map[string]*Session
	mu       sync.RWMutex
	stopChan chan struct{}
}

func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*Session),
		stopChan: make(chan struct{}),
	}
	go sm.cleanupLoop()
	return sm
}

func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-sm.stopChan:
			return
		case <-ticker.C:
			sm.cleanup()
		}
	}
}

func (sm *SessionManager) cleanup() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for id, session := range sm.sessions {
		if time.Since(session.lastUse) > 30*time.Minute {
			session.Close()
			delete(sm.sessions, id)
		}
	}
}

func (sm *SessionManager) GetOrCreate(id string) (*Session, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if session, ok := sm.sessions[id]; ok {
		session.lastUse = time.Now()
		return session, nil
	}

	// 1) Use a minimal environment so we skip banners & default shell checks.
	cmd := exec.Command("env", "-i", "bash", "--noprofile", "--norc", "-i")
	// If you need PATH or others:
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty.Start failed: %w", err)
	}

	// 2) Optional: set blank prompt
	//    We'll do a short wait for the shell to start, then write the prompt line:
	time.Sleep(100 * time.Millisecond)
	_, _ = ptmx.Write([]byte("export PS1=''\n"))

	// 3) Discard any leftover data for ~200ms so we skip any partial prompts
	_ = ptmx.SetDeadline(time.Now().Add(200 * time.Millisecond))
	discBuf := make([]byte, 4096)
	for {
		if _, e := ptmx.Read(discBuf); e != nil {
			break
		}
	}
	_ = ptmx.SetDeadline(time.Time{})

	// (If you want to attempt stty -echo, do it here.
	//  But if you still see echoes, just post-process them out in Run.)

	session := &Session{
		ptmx:    ptmx,
		cmd:     cmd,
		lastUse: time.Now(),
	}
	sm.sessions[id] = session
	return session, nil
}

func (sm *SessionManager) Close() error {
	close(sm.stopChan)

	sm.mu.Lock()
	defer sm.mu.Unlock()

	var errs []error
	for id, session := range sm.sessions {
		if err := session.Close(); err != nil {
			errs = append(errs, fmt.Errorf("session %s close error: %w", id, err))
		}
		delete(sm.sessions, id)
	}

	if len(errs) > 0 {
		return fmt.Errorf("session manager close errors: %v", errs)
	}
	return nil
}

// Run executes a command synchronously in a session and returns its output
func (sm *SessionManager) Run(ctx context.Context, input RunInput) (*types.CallToolResult, error) {
	session, err := sm.GetOrCreate(input.SessionID)
	if err != nil {
		return nil, err
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	// Discard any leftover
	if err := session.ptmx.SetDeadline(time.Now().Add(100 * time.Millisecond)); err == nil {
		discard := make([]byte, 4096)
		for {
			if _, e := session.ptmx.Read(discard); e != nil {
				break
			}
		}
	}
	session.ptmx.SetDeadline(time.Time{})

	// Send the user's command plus your marker
	cmdLine := fmt.Sprintf("%s\necho %s\n", input.Command, cmdMarker)
	if _, err := session.ptmx.Write([]byte(cmdLine)); err != nil {
		return nil, err
	}

	// read until the marker
	rawOutput, err := readUntilMarker(ctx, session.ptmx) // your existing function
	if err != nil {
		// partial or error
		return nil, err
	}

	// post-process out any lines that match the typed command
	filtered := filterOutCommandLines(rawOutput, input.Command)
	output := strings.TrimSpace(filtered)

	return &types.CallToolResult{
		Content: []interface{}{
			types.TextContent{
				Type: "text",
				Text: output,
			},
		},
		IsError: false,
	}, nil
}

// RunScreen sends input to an interactive terminal session
func (sm *SessionManager) RunScreen(ctx context.Context, input RunScreenInput) (*types.CallToolResult, error) {
	session, err := sm.GetOrCreate(input.SessionID)
	if err != nil {
		return nil, err
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	// Send raw input
	cmdLine := strings.ReplaceAll(input.Command, "\n", "\r\n")
	if _, err := session.ptmx.Write([]byte(cmdLine)); err != nil {
		return nil, err
	}

	// Small delay to allow output to appear
	time.Sleep(50 * time.Millisecond)

	// Read current terminal content
	buf := make([]byte, 32*1024)
	n, err := session.ptmx.Read(buf)
	if err != nil {
		return nil, err
	}

	return &types.CallToolResult{
		Content: []interface{}{
			types.TextContent{
				Type: "text",
				Text: string(buf[:n]),
			},
		},
		IsError: false,
	}, nil
}

func main() {
	sm := NewSessionManager()

	runTool := types.NewTool[RunInput](
		"run",
		"Execute a command synchronously and return its output",
		sm.Run,
	)

	runScreenTool := types.NewTool[RunScreenInput](
		"runScreen",
		"Send input to an interactive terminal session",
		sm.RunScreen,
	)

	s := server.NewDefaultServer(
		server.WithLogger(logger.NewStderrLogger("TERMINAL-SERVER")),
		server.WithTools(runTool, runScreenTool),
	)

	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Server start error: %v\n", err)
		os.Exit(1)
	}

	select {}
}
