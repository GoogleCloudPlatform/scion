// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package wsclient provides WebSocket client utilities for the CLI.
package wsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

const (
	// connectTimeout is the maximum time to wait for WebSocket connection
	connectTimeout = 30 * time.Second
	// initialDataTimeout is the maximum time to wait for first data from server
	// This helps detect when the server-side PTY stream fails silently
	initialDataTimeout = 30 * time.Second
)

// PTYClientConfig holds configuration for the PTY client.
type PTYClientConfig struct {
	// Endpoint is the Hub or Runtime Broker URL.
	Endpoint string
	// Token is the Bearer token for authentication.
	Token string
	// Slug is the agent's URL-safe identifier.
	Slug string
	// Cols is the initial terminal width.
	Cols int
	// Rows is the initial terminal height.
	Rows int
	// TransportSource provides OIDC transport tokens for IAP traversal.
	TransportSource transportauth.TokenSource
	// TransportMode controls header placement for the transport token.
	TransportMode transportauth.HeaderMode
}

// PTYClient manages a WebSocket PTY connection.
type PTYClient struct {
	config       PTYClientConfig
	conn         *websocket.Conn
	termState    *term.State
	oldFd        int
	writeMu      sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	receivedData bool // tracks whether we've received any data
}

// NewPTYClient creates a new PTY client.
func NewPTYClient(config PTYClientConfig) *PTYClient {
	return &PTYClient{
		config: config,
		oldFd:  int(os.Stdin.Fd()),
	}
}

// Connect establishes the WebSocket connection.
func (c *PTYClient) Connect(ctx context.Context) error {
	c.ctx, c.cancel = context.WithCancel(ctx)

	// Build WebSocket URL
	wsURL, err := c.buildWebSocketURL()
	if err != nil {
		return fmt.Errorf("failed to build URL: %w", err)
	}

	// Build headers
	headers := http.Header{}
	if c.config.Token != "" {
		headers.Set("Authorization", "Bearer "+c.config.Token)
	}

	// Apply transport auth headers for IAP/Cloud Run traversal.
	if c.config.TransportSource != nil {
		if err := transportauth.ApplyHeaders(headers, c.config.TransportSource, c.config.TransportMode); err != nil {
			slog.Debug("Transport auth header failed, proceeding without", "error", err)
		}
	}

	// Connect with timeout
	dialCtx, dialCancel := context.WithTimeout(ctx, connectTimeout)
	defer dialCancel()

	dialer := websocket.Dialer{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
	}

	conn, resp, err := dialer.DialContext(dialCtx, wsURL, headers)
	if err != nil {
		if dialCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("connection timed out after %v", connectTimeout)
		}
		if resp != nil && resp.StatusCode >= 400 {
			return fmt.Errorf("connection failed with status %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("connection failed: %w", err)
	}

	c.conn = conn
	return nil
}

// buildWebSocketURL constructs the WebSocket URL.
func (c *PTYClient) buildWebSocketURL() (string, error) {
	u, err := url.Parse(c.config.Endpoint)
	if err != nil {
		return "", err
	}

	// Convert http(s) to ws(s)
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
		// Already WebSocket
	default:
		u.Scheme = "ws"
	}

	// Build path
	u.Path = fmt.Sprintf("/api/v1/agents/%s/pty", c.config.Slug)

	// Add query params for terminal size
	q := u.Query()
	if c.config.Cols > 0 {
		q.Set("cols", fmt.Sprintf("%d", c.config.Cols))
	}
	if c.config.Rows > 0 {
		q.Set("rows", fmt.Sprintf("%d", c.config.Rows))
	}
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// Run starts the PTY session and blocks until it ends.
func (c *PTYClient) Run() error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	slog.Debug("PTY client Run() starting")

	// Put terminal in raw mode
	if err := c.setupTerminal(); err != nil {
		return fmt.Errorf("failed to setup terminal: %w", err)
	}
	var runErr error
	defer func() {
		slog.Debug("PTY client restoring terminal", "had_error", runErr != nil)
		c.restoreTerminal(runErr == nil)
	}()

	// Send initial resize so the remote PTY matches our terminal size,
	// even if the server didn't use the query-param hints.
	if term.IsTerminal(c.oldFd) {
		if cols, rows, err := term.GetSize(c.oldFd); err == nil {
			msg := wsprotocol.NewPTYResizeMessage(cols, rows)
			_ = c.writeToWebSocket(msg)
		}
	}

	// Set up signal handler for resize
	go c.handleResize()

	// Set up signal handler for interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			slog.Debug("PTY client received signal", "signal", sig)
			c.cancel()
		case <-c.ctx.Done():
			slog.Debug("PTY client signal handler: context done")
		}
	}()

	errCh := make(chan error, 2)

	// Read from stdin, send to WebSocket
	go func() {
		slog.Debug("PTY client stdin reader starting")
		err := c.readFromStdin()
		slog.Debug("PTY client stdin reader exited", "error", err)
		errCh <- err
	}()

	// Read from WebSocket, write to stdout
	go func() {
		slog.Debug("PTY client websocket reader starting")
		err := c.readFromWebSocket()
		slog.Debug("PTY client websocket reader exited", "error", err)
		errCh <- err
	}()

	// Wait for either direction to fail
	slog.Debug("PTY client waiting for goroutines")
	err := <-errCh
	slog.Debug("PTY client first goroutine returned", "error", err)
	c.cancel()

	// Close connection
	slog.Debug("PTY client sending close message")
	c.writeMu.Lock()
	_ = c.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
	)
	c.writeMu.Unlock()

	slog.Debug("PTY client Run() returning", "error", err)
	runErr = err
	return err
}

// setupTerminal puts the terminal in raw mode.
func (c *PTYClient) setupTerminal() error {
	if !term.IsTerminal(c.oldFd) {
		return nil // Not a terminal, no setup needed
	}

	state, err := term.MakeRaw(c.oldFd)
	if err != nil {
		return err
	}
	c.termState = state

	return nil
}

// terminalResetSequences are escape sequences to undo terminal mode changes
// that tmux (or other programs) may have applied. In the WebSocket PTY path,
// these cleanup sequences can be lost if the connection closes before they're
// fully flushed, or if the session ends abruptly (e.g., tmux detach).
var terminalResetSequences = strings.Join([]string{
	"\x1b[?1049l", // Exit alternate screen buffer (rmcup)
	"\x1b[?25h",   // Show cursor (cnorm)
	"\x1b[r",      // Reset scroll region to full window
	"\x1b[?1000l", // Disable mouse click tracking
	"\x1b[?1002l", // Disable mouse drag tracking
	"\x1b[?1003l", // Disable mouse all-motion tracking
	"\x1b[?1006l", // Disable SGR mouse mode
	"\x1b[?2004l", // Disable bracketed paste mode
}, "")

// restoreTerminal restores the terminal to its original state.
// When writeResetSeqs is true, it writes escape sequences to undo terminal mode
// changes that may have been applied by programs running in the PTY session
// (e.g., tmux), then restores the original termios state. When false (i.e., on
// error), it skips the reset sequences so that error output remains visible.
// Subsequent calls after the first restore are no-ops.
func (c *PTYClient) restoreTerminal(writeResetSeqs bool) {
	if c.termState != nil {
		if writeResetSeqs {
			// Write reset sequences before restoring termios, while stdout is
			// still connected. These are idempotent — sending them when the
			// modes are already off is harmless.
			_, _ = os.Stdout.Write([]byte(terminalResetSequences))
		}
		_ = term.Restore(c.oldFd, c.termState)
		c.termState = nil
	}
}

// handleResize handles terminal resize events.
func (c *PTYClient) handleResize() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-sigCh:
			cols, rows, err := term.GetSize(c.oldFd)
			if err != nil {
				continue
			}
			msg := wsprotocol.NewPTYResizeMessage(cols, rows)
			_ = c.writeToWebSocket(msg)
		}
	}
}

// readFromStdin reads from stdin and sends to WebSocket.
func (c *PTYClient) readFromStdin() error {
	// Use a channel to receive stdin data from a dedicated reader goroutine.
	// This allows us to respect context cancellation even though os.Stdin.Read
	// is a blocking syscall that doesn't support deadlines.
	type readResult struct {
		data []byte
		err  error
	}
	readCh := make(chan readResult)

	// Start a dedicated reader goroutine
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				slog.Debug("PTY stdin inner reader got error", "error", err)
				readCh <- readResult{nil, err}
				return
			}
			if n > 0 {
				// Copy the data to avoid race conditions
				data := make([]byte, n)
				copy(data, buf[:n])
				readCh <- readResult{data, nil}
			}
		}
	}()

	for {
		select {
		case <-c.ctx.Done():
			// Context cancelled - return immediately.
			// The reader goroutine will eventually exit when stdin is closed
			// or when the process exits.
			slog.Debug("PTY stdin reader: context cancelled")
			return c.ctx.Err()
		case result := <-readCh:
			if result.err != nil {
				if result.err == io.EOF {
					slog.Debug("PTY stdin reader: EOF")
					return nil
				}
				slog.Debug("PTY stdin reader: error", "error", result.err)
				return result.err
			}

			msg := wsprotocol.NewPTYDataMessage(result.data)
			if err := c.writeToWebSocket(msg); err != nil {
				slog.Debug("PTY stdin reader: write error", "error", err)
				return err
			}
		}
	}
}

// readFromWebSocket reads from WebSocket and writes to stdout.
func (c *PTYClient) readFromWebSocket() error {
	// Set initial read deadline to detect if server-side PTY fails to start
	if err := c.conn.SetReadDeadline(time.Now().Add(initialDataTimeout)); err != nil {
		return fmt.Errorf("failed to set read deadline: %w", err)
	}

	for {
		select {
		case <-c.ctx.Done():
			slog.Debug("PTY websocket reader: context cancelled")
			return c.ctx.Err()
		default:
		}

		_, data, err := c.conn.ReadMessage()
		if err != nil {
			slog.Debug("PTY websocket reader: read error", "error", err)
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				slog.Debug("PTY websocket reader: clean close")
				return nil
			}
			// Check if this is a timeout on initial data
			if !c.receivedData {
				if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
					return fmt.Errorf("timed out waiting for PTY data (server may have failed to start the session)")
				}
			}
			return err
		}

		// Clear read deadline after receiving first data
		if !c.receivedData {
			c.receivedData = true
			slog.Debug("PTY websocket reader: received first data, clearing deadline")
			if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
				return fmt.Errorf("failed to clear read deadline: %w", err)
			}
		}

		env, err := wsprotocol.ParseEnvelope(data)
		if err != nil {
			continue
		}

		switch env.Type {
		case wsprotocol.TypeData:
			var msg wsprotocol.PTYDataMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			_, _ = os.Stdout.Write(msg.Data)

		case wsprotocol.TypeError:
			var errMsg wsprotocol.ErrorMessage
			if err := json.Unmarshal(data, &errMsg); err != nil {
				continue
			}
			slog.Debug("PTY websocket reader: server error", "code", errMsg.Code, "message", errMsg.Message)
			return fmt.Errorf("server error: %s - %s", errMsg.Code, errMsg.Message)
		}
	}
}

// writeToWebSocket writes a message to the WebSocket connection.
func (c *PTYClient) writeToWebSocket(v interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	return c.conn.WriteJSON(v)
}

// Close closes the PTY client.
func (c *PTYClient) Close() error {
	slog.Debug("PTY client Close() called")
	if c.cancel != nil {
		c.cancel()
	}
	c.restoreTerminal(true)
	if c.conn != nil {
		slog.Debug("PTY client closing websocket connection")
		return c.conn.Close()
	}
	return nil
}

// AttachToAgent is a convenience function that connects and runs a PTY session.
func AttachToAgent(ctx context.Context, endpoint, token, slug string, opts ...AttachOption) error {
	// Get terminal size
	cols, rows := 80, 24
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		c, r, err := term.GetSize(fd)
		if err == nil {
			cols, rows = c, r
		}
	}

	cfg := PTYClientConfig{
		Endpoint: endpoint,
		Token:    token,
		Slug:     slug,
		Cols:     cols,
		Rows:     rows,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	client := NewPTYClient(cfg)

	if err := client.Connect(ctx); err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	return client.Run()
}

// AttachOption configures optional parameters for AttachToAgent.
type AttachOption func(*PTYClientConfig)

// WithTransport sets the transport auth source and header mode for IAP traversal.
func WithTransport(src transportauth.TokenSource, mode transportauth.HeaderMode) AttachOption {
	return func(c *PTYClientConfig) {
		c.TransportSource = src
		c.TransportMode = mode
	}
}

// BuildDirectAttachURL builds a URL for direct attachment to a runtime broker.
func BuildDirectAttachURL(hostEndpoint, slug string, cols, rows int) (string, error) {
	u, err := url.Parse(hostEndpoint)
	if err != nil {
		return "", err
	}

	// Convert to WebSocket scheme
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}

	u.Path = fmt.Sprintf("/api/v1/agents/%s/attach", slug)

	q := u.Query()
	q.Set("cols", fmt.Sprintf("%d", cols))
	q.Set("rows", fmt.Sprintf("%d", rows))
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// IsWebSocketURL checks if a URL is a WebSocket URL.
func IsWebSocketURL(urlStr string) bool {
	return strings.HasPrefix(urlStr, "ws://") || strings.HasPrefix(urlStr, "wss://")
}
