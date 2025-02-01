package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
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
	Command   string `json:"command" jsonschema:"type=string,description=The command to run,required"`
	SessionID string `json:"sessionId" jsonschema:"type=string,description=The session ID to run the command in,required"`
}

type RunScreenInput struct {
	Command   string `json:"command" jsonschema:"type=string,description=The command to run,required"`
	SessionID string `json:"sessionId" jsonschema:"type=string,description=The session ID to run the command in,required"`
	RawOutput bool   `json:"rawOutput,omitempty" jsonschema:"type=boolean,description=If true, include ANSI escape sequences"`
}

// ---------------------------------------------------------------------
// Utility: filterOutCommandLines
// ---------------------------------------------------------------------
// filterOutCommandLines removes the command from output
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

// processControlSequences handles common control character notations
func processControlSequences(input string) string {
	replacements := map[string]string{
		"^X": "\x18", // Ctrl+X
		"^O": "\x0F", // Ctrl+O
		"^J": "\x0A", // Enter
		"^C": "\x03", // Ctrl+C
		"^D": "\x04", // Ctrl+D
		"^Z": "\x1A", // Ctrl+Z
		"^[": "\x1B", // Escape
		"^H": "\x08", // Backspace
		"^M": "\x0D", // Carriage return
		"^L": "\x0C", // Form feed
		"^G": "\x07", // Bell
		"^U": "\x15", // Clear line
		"^W": "\x17", // Delete word
		"^Y": "\x19", // Paste from kill buffer
		"^V": "\x16", // Literal input
		"^K": "\x0B", // Kill line
		"^E": "\x05", // End of line
		"^A": "\x01", // Beginning of line
	}

	result := input
	for notation, ctrl := range replacements {
		result = strings.ReplaceAll(result, notation, ctrl)
	}
	return result
}

// filterAnsiSequences removes ANSI escape sequences from the output
func filterAnsiSequences(input string) string {
	ansiRegex := regexp.MustCompile(`\x1B\[[0-9;]*[a-zA-Z]`)
	return ansiRegex.ReplaceAllString(input, "")
}

// normalizeNewlines replaces \r\n => \n, then remove extra \r, then trim
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
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
	debug   bool
	logger  *logger.FileLogger
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

func (s *Session) logDebug(format string, args ...interface{}) {
	if s.debug {
		(*s.logger).Logf(format, args...)
	}
}

// Continuously read from the PTY, appending to s.buf
func (s *Session) readerLoop() {
	bufTmp := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(bufTmp)
		s.mu.Lock()
		if err != nil {
			s.logDebug("Read error: %v", err)
			s.mu.Unlock()
			return
		}
		if n > 0 {
			s.logDebug("Read %d bytes: %q", n, bufTmp[:n])
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
	logger   *logger.FileLogger
}

func NewSessionManager(logger *logger.FileLogger) *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*Session),
		stopChan: make(chan struct{}),
		logger:   logger,
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
		"TERM=xterm-256color",
		"LANG=en_US.UTF-8",
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty.Start error: %w", err)
	}

	// Comprehensive terminal setup
	setupCommands := []string{
		"unset PROMPT_COMMAND",
		"PS1=''",
		"stty -echo",
		"stty rows 24 cols 80",
		"stty -isig",   // Disable signal handling
		"stty -icanon", // Disable canonical mode
		"export TERM=xterm-256color",
	}
	setupScript := strings.Join(setupCommands, "; ") + "\n"
	_, _ = ptmx.Write([]byte(setupScript))

	sess := &Session{
		ptmx:    ptmx,
		cmd:     cmd,
		lastUse: time.Now(),
		debug:   false,     // Set to true for debugging
		logger:  sm.logger, // Add logger from SessionManager
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

	// Process input for control characters
	data := processControlSequences(in.Command)

	// Handle newlines
	data = strings.ReplaceAll(data, "\n", "\r")
	if !strings.HasSuffix(data, "\r") {
		data += "\r"
	}

	if _, werr := sess.ptmx.Write([]byte(data)); werr != nil {
		return nil, werr
	}

	// Allow more time for TUI apps to render
	select {
	case <-time.After(250 * time.Millisecond):
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

	// Process output
	outChunk = normalizeNewlines(outChunk)
	if !in.RawOutput {
		outChunk = filterAnsiSequences(outChunk)
	}

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
	lg, err := logger.NewFileLogger("/tmp/mcpterm.log", "MCPTERM-SERVER")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create logger: %v\n", err)
		os.Exit(1)
	}

	sm := NewSessionManager(lg) // Pass logger to SessionManager

	runTool := types.NewTool(
		"run",
		"Execute a command in the same bash session.",
		sm.Run,
	)
	runScreenTool := types.NewTool(
		"runScreen",
		"Send input to the bash session, read partial output. Useful for TUI apps.",
		sm.RunScreen,
	)

	srv := server.NewDefaultServer(
		server.WithLogger(lg),
		server.WithTools(runTool, runScreenTool),
	)

	ctx := context.Background()
	lg.Logf("Starting server...")
	if err := srv.Start(ctx); err != nil {
		lg.Logf("Server start error: %v\n", err)
		os.Exit(1)
	}
	lg.Logf("Server started")

	<-ctx.Done()
	lg.Logf("Exiting...")
}
