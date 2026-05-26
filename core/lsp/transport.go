// Package lsp provides a language-agnostic JSON-RPC 2.0 transport and shared
// protocol types for Language Server Protocol clients.
//
// The Transport type manages a subprocess's stdin/stdout using Content-Length
// framing as specified by the LSP base protocol. Multiple language providers
// (Go, C#, etc.) share this single implementation.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Response is the result dispatched to a caller waiting for a JSON-RPC 2.0
// response from the LSP subprocess.
type Response struct {
	Result json.RawMessage
	Err    error
}

// Transport implements Content-Length framed JSON-RPC 2.0 over a subprocess's
// stdin/stdout pipes. It is safe for concurrent use after Start() returns.
//
// Lifecycle:
//
//	NewTransport(binary, args...) → Start() → SendRequest / SendNotify (concurrent) → Stop()
type Transport struct {
	cmd    *exec.Cmd
	stdin  *bufio.Writer // wraps cmd's StdinPipe
	stdout *bufio.Reader // wraps cmd's StdoutPipe

	// mu protects stdin writes and nextID. Held during the entire
	// id-allocation + channel-registration + write sequence so that
	// concurrent senders do not interleave their frames.
	mu     sync.Mutex
	nextID int

	// pendingMu guards the pending map. Never acquired while mu is held.
	pendingMu sync.Mutex
	pending   map[int]chan Response

	// crashed is set by readerLoop when stdout returns an error or EOF.
	crashed atomic.Bool
	// closed is set by Stop() to prevent new requests after shutdown.
	closed atomic.Bool
	// started is set by Start() to track whether the reader goroutine was launched.
	started atomic.Bool

	// done is closed by readerLoop when it exits, signalling that all
	// in-flight dispatches are complete.
	done chan struct{}
}

// NewTransport creates a Transport for the given binary path and optional extra
// arguments. The subprocess is NOT started until Start() is called.
func NewTransport(binaryPath string, args ...string) *Transport {
	cmd := exec.Command(binaryPath, args...)
	cmd.Stderr = nil // suppress diagnostic/log output from the language server
	return &Transport{
		cmd:     cmd,
		pending: make(map[int]chan Response),
		done:    make(chan struct{}),
	}
}

// Start launches the subprocess and begins the background reader goroutine.
// Must be called exactly once before any SendRequest or SendNotify calls.
func (t *Transport) Start() error {
	stdinPipe, err := t.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("lsp transport: StdinPipe: %w", err)
	}

	stdoutPipe, err := t.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("lsp transport: StdoutPipe: %w", err)
	}

	if err := t.cmd.Start(); err != nil {
		return fmt.Errorf("lsp transport: starting %s: %w", t.cmd.Path, err)
	}

	t.stdin = bufio.NewWriter(stdinPipe)
	t.stdout = bufio.NewReader(stdoutPipe)
	t.started.Store(true)

	go t.readerLoop()
	return nil
}

// SendRequest writes a JSON-RPC 2.0 request and blocks until the matching
// response arrives or ctx is cancelled. Returns an error if the transport
// is in crashed or closed state.
func (t *Transport) SendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if t.closed.Load() {
		return nil, fmt.Errorf("lsp transport: closed")
	}
	if t.crashed.Load() {
		return nil, fmt.Errorf("lsp transport: crashed")
	}

	// Buffered channel so the reader goroutine never blocks on dispatch.
	ch := make(chan Response, 1)

	// Hold mu for the entire id-alloc + channel-register + write sequence.
	// This ensures the response channel is registered BEFORE the request
	// is on the wire, ruling out the fast-response race.
	t.mu.Lock()
	id := t.nextID
	t.nextID++

	t.pendingMu.Lock()
	t.pending[id] = ch
	t.pendingMu.Unlock()

	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	err := t.writeMessageLocked(msg)
	t.mu.Unlock()

	if err != nil {
		// Remove the pending channel — no response will arrive.
		t.pendingMu.Lock()
		delete(t.pending, id)
		t.pendingMu.Unlock()
		return nil, fmt.Errorf("lsp transport: send %s: %w", method, err)
	}

	// Wait for the response or cancellation.
	select {
	case resp := <-ch:
		return resp.Result, resp.Err
	case <-ctx.Done():
		t.pendingMu.Lock()
		delete(t.pending, id)
		t.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

// SendNotify writes a JSON-RPC 2.0 notification (no ID, no response).
func (t *Transport) SendNotify(method string, params any) error {
	if t.closed.Load() {
		return fmt.Errorf("lsp transport: closed")
	}

	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writeMessageLocked(msg)
}

// Stop drains all pending response channels with an error, kills the
// subprocess, and waits for the reader goroutine to exit.
// Safe to call multiple times — subsequent calls block until the first
// completes, then return immediately.
func (t *Transport) Stop() {
	if !t.closed.CompareAndSwap(false, true) {
		// Already stopped (or being stopped) — wait for reader to finish.
		if t.started.Load() {
			<-t.done
		}
		return
	}

	// Drain all in-flight pending channels.
	t.pendingMu.Lock()
	for id, ch := range t.pending {
		ch <- Response{Err: fmt.Errorf("lsp transport: stopped")}
		delete(t.pending, id)
	}
	t.pendingMu.Unlock()

	// Kill the subprocess (force kill — graceful shutdown via LSP shutdown/exit
	// should have been sent before Stop() is called).
	if t.cmd.Process != nil {
		t.cmd.Process.Kill() // error ignored — process may already be exiting
	}

	// Wait for the reader goroutine to detect the EOF and exit.
	if t.started.Load() {
		// Give the reader up to 5s to notice the process died.
		select {
		case <-t.done:
		case <-time.After(5 * time.Second):
		}
	}

	// Reap the process to avoid zombies. Error is expected (killed).
	if t.cmd.Process != nil {
		t.cmd.Wait()
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers (called with mu held unless noted)
// ──────────────────────────────────────────────────────────────────────────────

// writeMessageLocked serialises payload as JSON and writes a Content-Length
// framed message to stdin. Caller must hold t.mu.
func (t *Transport) writeMessageLocked(payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := t.stdin.WriteString(header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := t.stdin.Write(body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := t.stdin.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}

// readerLoop runs in a goroutine, reading Content-Length framed JSON-RPC 2.0
// messages from stdout and dispatching responses to pending request channels.
//
// Exits when stdout returns an error (including EOF after process death).
// Sets crashed=true and drains all pending channels before returning.
func (t *Transport) readerLoop() {
	defer close(t.done)

	for {
		msg, err := t.readMessage()
		if err != nil {
			// Propagate crash/EOF to all waiting callers.
			if !t.closed.Load() {
				t.crashed.Store(true)
			}
			t.pendingMu.Lock()
			for id, ch := range t.pending {
				ch <- Response{Err: fmt.Errorf("lsp transport: reader: %w", err)}
				delete(t.pending, id)
			}
			t.pendingMu.Unlock()
			return
		}

		// Only route messages that have an integer "id" field.
		// Notifications (no "id") and requests from the server (window/showMessage,
		// workspace/applyEdit, etc.) are silently discarded.
		rawID, hasID := msg["id"]
		if !hasID {
			continue
		}
		if string(rawID) == "null" {
			continue
		}

		var id int
		if err := json.Unmarshal(rawID, &id); err != nil {
			continue // non-integer id — skip
		}

		// Build response.
		var resp Response
		if errRaw, hasErr := msg["error"]; hasErr && string(errRaw) != "null" {
			var lspErr struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if jerr := json.Unmarshal(errRaw, &lspErr); jerr == nil {
				resp.Err = fmt.Errorf("lsp error %d: %s", lspErr.Code, lspErr.Message)
			} else {
				resp.Err = fmt.Errorf("lsp error: %s", string(errRaw))
			}
		} else if resultRaw, hasResult := msg["result"]; hasResult {
			resp.Result = resultRaw
		}

		// Dispatch to the waiting caller.
		t.pendingMu.Lock()
		if ch, ok := t.pending[id]; ok {
			delete(t.pending, id)
			ch <- resp
		}
		t.pendingMu.Unlock()
	}
}

// readMessage reads one Content-Length framed JSON message from stdout.
// Returns (nil, io.EOF) when the stream ends cleanly.
func (t *Transport) readMessage() (map[string]json.RawMessage, error) {
	var contentLength int

	// Read HTTP-style headers until a blank line.
	for {
		line, err := t.stdout.ReadString('\n')
		if err != nil {
			if err == io.EOF && line == "" {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("reading header: %w", err)
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // blank line — end of headers
		}

		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "content-length:") {
			val := strings.TrimSpace(line[len("content-length:"):])
			n, perr := strconv.Atoi(val)
			if perr != nil {
				return nil, fmt.Errorf("invalid Content-Length %q: %w", val, perr)
			}
			contentLength = n
		}
		// Content-Type and other headers are ignored.
	}

	if contentLength == 0 {
		return nil, fmt.Errorf("lsp: missing or zero Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(t.stdout, body); err != nil {
		return nil, fmt.Errorf("reading body (%d bytes): %w", contentLength, err)
	}

	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("unmarshalling message: %w", err)
	}
	return msg, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// File-URI helpers
// ──────────────────────────────────────────────────────────────────────────────

// PathToURI converts an absolute filesystem path to an LSP file:// URI.
//
//   - POSIX: /foo/bar.go  →  file:///foo/bar.go
//   - Windows: C:\foo\bar.go  →  file:///C:/foo/bar.go
//
// The implementation uses net/url so special characters (spaces, etc.) are
// percent-encoded correctly.
func PathToURI(absPath string) string {
	p := filepath.ToSlash(absPath)

	// Windows drive letter: "C:/foo" needs a leading "/" to give "/C:/foo"
	// so that url.URL produces three slashes (file:// + /C:/...).
	if runtime.GOOS == "windows" && len(p) >= 2 && p[1] == ':' {
		p = "/" + p
	}

	u := &url.URL{Scheme: "file", Path: p}
	return u.String()
}
