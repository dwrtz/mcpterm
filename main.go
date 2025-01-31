package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"

	"github.com/dwrtz/mcp-go/pkg/logger"
	"github.com/dwrtz/mcp-go/pkg/mcp/server"
	"github.com/dwrtz/mcp-go/pkg/types"
)

// Marker for end of synchronous commands
const cmdMarker = "RUNTOOL_DONE_MARKER"

// ---------------------------------------------------------------------
// Test tool request structs
// ---------------------------------------------------------------------
type RunInput struct {
	Command   string `json:"command"`
	SessionID string `json:"sessionId"`
}

type RunScreenInput struct {
	Command   string `json:"command"`
	SessionID string `json:"sessionId"`
}

// ---------------------------------------------------------------------
// Utility: filterOutCommandLines
// ---------------------------------------------------------------------
func filterOutCommandLines(output, cmd string) string {
	cmdTrim := strings.TrimSpace(cmd)
	lines := strings.Split(output, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == cmdTrim {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// ---------------------------------------------------------------------
// Session => background reading from PTY
// ---------------------------------------------------------------------
type Session struct {
	ptmx    *os.File
	cmd     *exec.Cmd
	mu      sync.Mutex
	cond    *sync.Cond
	buf     bytes.Buffer
	closed  bool
	lastUse time.Time
}

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

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
		_ = s.cmd.Wait()
	}

	// Wake any waiters
	s.cond.Broadcast()

	if len(errs) > 0 {
		return fmt.Errorf("session close errors: %v", errs)
	}
	return nil
}

// Continuously read from the PTY, appending to s.buf
func (s *Session) readerLoop() {

	bufTmp := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(bufTmp)
		s.mu.Lock()
		if err != nil {
			s.mu.Unlock()
			return
		}
		if n > 0 {
			s.buf.Write(bufTmp[:n])
			s.cond.Broadcast()
		}
		s.mu.Unlock()
	}
}

// readUntilMarkerInBuffer waits for cmdMarker in s.buf, or closed, or ctx done
func (s *Session) readUntilMarkerInBuffer(ctx context.Context, startOffset int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	markerBytes := []byte(cmdMarker)

	for {
		if s.closed {
			// session closed => partial
			if len(s.buf.Bytes()) > startOffset {
				return string(s.buf.Bytes()[startOffset:]), io.EOF
			}
			return "", io.EOF
		}

		data := s.buf.Bytes()
		if len(data) > startOffset {
			idx := bytes.Index(data[startOffset:], markerBytes)
			if idx >= 0 {
				pos := startOffset + idx
				return string(data[startOffset:pos]), nil
			}
		}

		select {
		case <-ctx.Done():
			partial := ""
			if len(s.buf.Bytes()) > startOffset {
				partial = string(s.buf.Bytes()[startOffset:])
			}
			return partial, ctx.Err()
		default:
			// not canceled => wait
		}

		s.cond.Wait()
	}
}

// ---------------------------------------------------------------------
// SessionManager
// ---------------------------------------------------------------------
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
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

	now := time.Now()
	for id, s := range sm.sessions {
		if now.Sub(s.lastUse) > 30*time.Minute {
			_ = s.Close()
			delete(sm.sessions, id)
		}
	}
}

func (sm *SessionManager) Close() error {
	close(sm.stopChan)

	sm.mu.Lock()
	defer sm.mu.Unlock()

	var errs []error
	for id, s := range sm.sessions {
		if err := s.Close(); err != nil {
			errs = append(errs, fmt.Errorf("session %s close error: %w", id, err))
		}
		delete(sm.sessions, id)
	}
	if len(errs) > 0 {
		return fmt.Errorf("session manager close errors: %v", errs)
	}
	return nil
}

func (sm *SessionManager) GetOrCreate(id string) (*Session, error) {
	sm.mu.RLock()
	existing, ok := sm.sessions[id]
	sm.mu.RUnlock()
	if ok {
		existing.lastUse = time.Now()
		return existing, nil
	}

	cmd := exec.Command("bash", "--noprofile", "--norc", "-i")
	cmd.Env = []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"TERM=xterm",
	}
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty.Start error: %w", err)
	}

	// Turn off echo + blank prompt
	_, _ = ptmx.Write([]byte("unset PROMPT_COMMAND; PS1=''; stty -echo\n"))

	sess := &Session{
		ptmx:    ptmx,
		cmd:     cmd,
		lastUse: time.Now(),
	}
	sess.cond = sync.NewCond(&sess.mu)
	go sess.readerLoop()

	// Let the shell do any initial printing
	time.Sleep(100 * time.Millisecond)

	sm.mu.Lock()
	sm.sessions[id] = sess
	sm.mu.Unlock()

	return sess, nil
}

// ---------------------------------------------------------------------
// Tools => Run / RunScreen
//   We add CRLF->LF normalization so multi-line output test passes
// ---------------------------------------------------------------------

// normalizeNewlines => replace \r\n => \n, then remove extra \r, then trim
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func (sm *SessionManager) Run(ctx context.Context, in RunInput) (*types.CallToolResult, error) {

	sess, err := sm.GetOrCreate(in.SessionID)
	if err != nil {
		return nil, err
	}
	sess.lastUse = time.Now()

	// no leftover discard
	sess.mu.Lock()
	startOffset := len(sess.buf.Bytes())
	sess.mu.Unlock()

	// write user command + marker
	cmdLine := fmt.Sprintf("%s\necho %s\n", in.Command, cmdMarker)
	if _, werr := sess.ptmx.Write([]byte(cmdLine)); werr != nil {
		return nil, fmt.Errorf("failed to write command: %w", werr)
	}

	ctxWait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rawOut, err2 := sess.readUntilMarkerInBuffer(ctxWait, startOffset)
	if err2 != nil && err2 != context.DeadlineExceeded && err2 != context.Canceled && err2 != io.EOF {
		return nil, err2
	}

	// **Convert CRLF => LF** to pass "MultiLineOutput" test
	norm := normalizeNewlines(rawOut)
	filtered := filterOutCommandLines(norm, in.Command)
	final := strings.TrimSpace(filtered)

	return &types.CallToolResult{
		Content: []interface{}{
			types.TextContent{Type: "text", Text: final},
		},
		IsError: false,
	}, nil
}

func (sm *SessionManager) RunScreen(ctx context.Context, in RunScreenInput) (*types.CallToolResult, error) {

	sess, err := sm.GetOrCreate(in.SessionID)
	if err != nil {
		return nil, err
	}
	sess.lastUse = time.Now()

	sess.mu.Lock()
	startOffset := len(sess.buf.Bytes())
	sess.mu.Unlock()

	// convert \n => \r
	data := strings.ReplaceAll(in.Command, "\n", "\r")
	if !strings.HasSuffix(data, "\r") {
		data += "\r"
	}
	if _, werr := sess.ptmx.Write([]byte(data)); werr != nil {
		return nil, werr
	}

	select {
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	sess.mu.Lock()
	outChunk := ""
	if len(sess.buf.Bytes()) >= startOffset {
		chunk := sess.buf.Bytes()[startOffset:]
		outChunk = string(chunk)
	}
	sess.mu.Unlock()

	// also CRLF->LF for runScreen
	outChunk = normalizeNewlines(outChunk)

	return &types.CallToolResult{
		Content: []interface{}{
			types.TextContent{Type: "text", Text: outChunk},
		},
		IsError: false,
	}, nil
}

// ---------------------------------------------------------------------
// main()
// ---------------------------------------------------------------------
func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	sm := NewSessionManager()

	runTool := types.NewTool[RunInput](
		"run",
		"Execute a command in the same bash session, wait for marker",
		sm.Run,
	)
	runScreenTool := types.NewTool[RunScreenInput](
		"runScreen",
		"Send input to the bash session, read partial output",
		sm.RunScreen,
	)

	srv := server.NewDefaultServer(
		server.WithLogger(logger.NewStderrLogger("MCPTERM-SERVER")),
		server.WithTools(runTool, runScreenTool),
	)

	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Server start error: %v\n", err)
		os.Exit(1)
	}

	select {}
}
