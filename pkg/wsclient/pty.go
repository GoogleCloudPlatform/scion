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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
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

// attachUnsupportedMessage is the one fixed, actionable error text a caller
// sees when the target runtime has no exec/attach/TTY primitive at all,
// regardless of which of the two points where the broker can learn that
// rejects the attempt: the pre-upgrade HTTP 501/runtime_attach_unsupported
// response Connect checks for below, or the post-upgrade
// 4501/attach_unsupported close code readFromWebSocket checks for. Kept as
// one constant so the two call sites can never drift into two different
// wordings for the same outcome.
const attachUnsupportedMessage = "attach is not supported for this agent's runtime"

// PTYCloseError reports that the server ended a PTY session with a close
// code other than a clean detach (1000). Callers classify Code with
// wsprotocol.ClassifyPTYClose to decide what to tell the user. Run has
// already made its one automatic reconnect attempt, if the code allows one
// (wsprotocol.PTYReconnectTiming), before it returns this error.
type PTYCloseError struct {
	// Code is the WebSocket close code (see the wsprotocol ClosePTY* constants).
	// 1006 means the connection dropped without a close frame.
	Code int
	// Reason is the machine-readable close reason, possibly empty.
	Reason string
}

func (e *PTYCloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("attach session closed by server (code %d)", e.Code)
	}
	return fmt.Sprintf("attach session closed by server (code %d: %s)", e.Code, e.Reason)
}

// PTYReconnectError reports that the server closed a PTY session with a code
// that allows one automatic reconnect, and that reconnect failed: the dial
// failed, or the new session ended before it delivered any data. It unwraps
// to both the original close (Close) and the reconnect failure (Err), so
// errors.As finds the original *PTYCloseError first.
type PTYReconnectError struct {
	// Close is the close that triggered the reconnect.
	Close *PTYCloseError
	// Err is why the reconnect failed.
	Err error
}

func (e *PTYReconnectError) Error() string {
	return fmt.Sprintf("%v; automatic reconnect failed: %v", e.Close, e.Err)
}

// Unwrap returns the original close and the reconnect failure.
func (e *PTYReconnectError) Unwrap() []error { return []error{e.Close, e.Err} }

// Reconnect backoff for wsprotocol.ReconnectBackoff (4504, 1011): exponential from
// reconnectBackoffBase to reconnectBackoffMax with full jitter, reset once a
// session has lived reconnectBackoffResetAfter.
const (
	reconnectBackoffBase       = 1 * time.Second
	reconnectBackoffMax        = 60 * time.Second
	reconnectBackoffResetAfter = 60 * time.Second
)

// maxShortReconnects bounds a run of automatic reconnects: after this many
// consecutive retry closes of sessions that each lived less than
// reconnectBackoffResetAfter, Run stops instead of reconnecting again. It
// keeps a server that accepts, sends some output and closes again from
// holding the client in a reconnect loop.
const maxShortReconnects = 3

// ErrPTYReconnectLimit is the PTYReconnectError.Err Run reports when it
// stops reconnecting because of maxShortReconnects.
var ErrPTYReconnectLimit = fmt.Errorf("stopped after %d automatic reconnects whose sessions each ended within a minute", maxShortReconnects)

// Bytes that stop a pending reconnect when typed during the wait: Ctrl-C
// and Ctrl-D cancel it, and the tmux detach sequence (Ctrl-b d) detaches.
const (
	keyCtrlC  = 0x03
	keyCtrlD  = 0x04
	keyCtrlB  = 0x02
	keyDetach = 'd'
)

// writeFailGrace bounds how long a failed stdin write waits for the
// WebSocket reader to report the server's close code, so a write that races
// a server close still reports (and reconnects on) the close code.
const writeFailGrace = 2 * time.Second

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
	config PTYClientConfig
	// conn is the current connection. Run replaces it on reconnect, under
	// writeMu, so writers always see either the old or the new connection.
	conn      *websocket.Conn
	termState *term.State
	oldFd     int
	writeMu   sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	// receivedData records whether any data arrived from the server. It is
	// written by the WebSocket reader goroutine and read by Run when it
	// restores the terminal, which may happen while that goroutine is still
	// running, so it is atomic.
	receivedData atomic.Bool
	// connLive records whether the current connection has delivered any
	// data. A reconnected session that closes before it is live counts as a
	// failed reconnect and is not retried again.
	connLive atomic.Bool

	// stdin is read once here and never re-read from the os.Stdin package
	// variable elsewhere, so a test can inject its own reader without ever
	// mutating the (process-wide, shared) global: startStdinReader's
	// reader goroutine can outlive both Run() and the test that started it
	// (it only returns once its blocking Read call itself returns), so
	// swapping os.Stdin back out from under it would be a data race, not
	// just a functional risk.
	stdin io.Reader

	// Test seams. NewPTYClient sets real implementations.
	// notice receives the one-line reconnect notice.
	notice io.Writer
	// jitter returns a duration drawn uniformly from [0, maxDelay].
	jitter func(maxDelay time.Duration) time.Duration
	// after waits for d, like time.After.
	after func(d time.Duration) <-chan time.Time
	// now returns the current time, used to measure session lifetimes.
	now func() time.Time
	// termSize reports the local terminal size; ok is false when stdin is
	// not a terminal.
	termSize func() (cols, rows int, ok bool)
	// restoreTerm restores the terminal state, like term.Restore.
	restoreTerm func(fd int, state *term.State) error
	// redialFn opens a replacement connection; NewPTYClient sets it to dial.
	redialFn func(ctx context.Context) (*websocket.Conn, error)
}

// NewPTYClient creates a new PTY client.
func NewPTYClient(config PTYClientConfig) *PTYClient {
	c := &PTYClient{
		config:      config,
		oldFd:       int(os.Stdin.Fd()),
		stdin:       os.Stdin,
		notice:      os.Stderr,
		jitter:      fullJitter,
		after:       time.After,
		now:         time.Now,
		restoreTerm: term.Restore,
	}
	c.termSize = c.localTermSize
	c.redialFn = c.dial
	return c
}

// fullJitter returns a duration drawn uniformly from [0, maxDelay].
func fullJitter(maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(maxDelay) + 1))
}

// localTermSize reports the size of the terminal on stdin.
func (c *PTYClient) localTermSize() (cols, rows int, ok bool) {
	if !term.IsTerminal(c.oldFd) {
		return 0, 0, false
	}
	cols, rows, err := term.GetSize(c.oldFd)
	if err != nil {
		return 0, 0, false
	}
	return cols, rows, true
}

// Connect establishes the WebSocket connection.
func (c *PTYClient) Connect(ctx context.Context) error {
	c.ctx, c.cancel = context.WithCancel(ctx)
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}

// dial opens one WebSocket connection to the PTY endpoint. Transport
// headers are applied on every dial, so a reconnect presents a fresh
// transport token.
func (c *PTYClient) dial(ctx context.Context) (*websocket.Conn, error) {
	// Build WebSocket URL
	wsURL, err := c.buildWebSocketURL()
	if err != nil {
		return nil, fmt.Errorf("failed to build URL: %w", err)
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
		// gorilla/websocket returns a non-nil resp (with an unread body) on a
		// failed handshake so callers can inspect the status/body. Close it
		// once parseAttachFailureBody has had a chance to read it, or it leaks.
		if resp != nil && resp.Body != nil {
			defer func() { _ = resp.Body.Close() }()
		}
		if dialCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("connection timed out after %v", connectTimeout)
		}
		if resp != nil && resp.StatusCode >= 400 {
			code, detail := parseAttachFailureBody(resp, err)
			if resp.StatusCode == http.StatusNotImplemented && code == wsprotocol.ErrCodeRuntimeAttachUnsupported {
				return nil, errors.New(attachUnsupportedMessage)
			}
			return nil, fmt.Errorf("connection failed with status %d: %s", resp.StatusCode, detail)
		}
		return nil, fmt.Errorf("connection failed: %w", err)
	}
	return conn, nil
}

// attachErrorBody mirrors the shape of runtimebroker's JSON error envelope
// (APIError/ErrorResponse) closely enough to pull out the machine-readable
// code and human-readable message without importing the broker package.
type attachErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// parseAttachFailureBody reads resp's body once and returns the parsed error
// code (empty if the body isn't the runtimebroker error envelope shape) and
// a best-effort human-readable detail: the envelope's message when present,
// otherwise the raw body, otherwise fallback's own text. Centralizing the
// single body read here (rather than letting each caller drain it) avoids
// handing back an empty detail to a second reader of an already-consumed
// body.
func parseAttachFailureBody(resp *http.Response, fallback error) (code, detail string) {
	if resp == nil || resp.Body == nil {
		return "", fallback.Error()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || len(body) == 0 {
		return "", fallback.Error()
	}

	var parsed attachErrorBody
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Code, parsed.Error.Message
	}
	return "", strings.TrimSpace(string(body))
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

	// Build path. Keep any path prefix on the endpoint (e.g. a hub served
	// under https://example.com/scion), matching how the REST client joins
	// BaseURL and path.
	u.Path = joinEndpointPath(u.Path, fmt.Sprintf("/api/v1/agents/%s/pty", c.config.Slug))

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

// joinEndpointPath appends an API path to the endpoint's own path prefix.
// It strips any slashes at the end of the prefix and the start of apiPath,
// then joins them with exactly one "/", so the result never has "//"
// whether or not either side carries a slash.
func joinEndpointPath(prefix, apiPath string) string {
	return strings.TrimRight(prefix, "/") + "/" + strings.TrimLeft(apiPath, "/")
}

// Run starts the PTY session and blocks until it ends.
//
// When the server closes the session with a code that
// wsprotocol.PTYReconnectTiming allows (4503, 4504, 1011), Run makes one reconnect
// attempt for that close: after a full-jitter delay of up to
// wsprotocol.PTYPromptReconnectMaxDelay for 4503, or after the normal
// exponential backoff with full jitter for 4504 and 1011. A successful reconnect
// sends the current terminal size so the remote tmux redraws at the right
// size. If the reconnect fails (dial error, or the new session closes before
// it delivers any data), Run returns a *PTYReconnectError and does not try
// again. Every other close ends Run. The terminal stays in raw mode across a
// reconnect and is restored once, when Run returns, on every path.
func (c *PTYClient) Run() error {
	conn := c.currentConn()
	if conn == nil {
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
		c.restoreAfterRun(runErr)
	}()

	// Send initial resize so the remote PTY matches our terminal size,
	// even if the server didn't use the query-param hints.
	c.sendResize()

	// Set up signal handler for resize
	go c.handleResize()

	// Set up signal handler for interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case sig := <-sigCh:
			slog.Debug("PTY client received signal", "signal", sig)
			c.cancel()
		case <-c.ctx.Done():
			slog.Debug("PTY client signal handler: context done")
		}
	}()

	// Stop the resize and signal goroutines when Run returns.
	defer c.cancel()

	stdinCh := c.startStdinReader()

	// pendingClose is the close that triggered the reconnect now in
	// progress; the new connection must deliver data before it is live.
	var pendingClose *PTYCloseError
	backoffAttempt := 0
	shortCloses := 0
	for {
		started := c.now()
		err := c.runConnection(conn, stdinCh)
		if c.ctx.Err() != nil {
			// Interrupted: report that, not whatever the connection saw.
			runErr = c.ctx.Err()
			return runErr
		}
		if pendingClose != nil && !c.connLive.Load() && err != nil {
			// The reconnected session ended before it was live. One
			// reconnect per close: do not try again.
			runErr = &PTYReconnectError{Close: pendingClose, Err: err}
			return runErr
		}
		pendingClose = nil

		var closeErr *PTYCloseError
		timing := wsprotocol.ReconnectNever
		if errors.As(err, &closeErr) {
			timing = wsprotocol.PTYReconnectTiming(closeErr.Code)
		}
		if timing == wsprotocol.ReconnectNever {
			slog.Debug("PTY client Run() returning", "error", err)
			runErr = err
			return err
		}

		if c.now().Sub(started) >= reconnectBackoffResetAfter {
			backoffAttempt = 0
			shortCloses = 0
		} else {
			shortCloses++
		}
		if shortCloses > maxShortReconnects {
			slog.Debug("PTY client: too many short-lived sessions, not reconnecting", "closes", shortCloses)
			runErr = &PTYReconnectError{Close: closeErr, Err: ErrPTYReconnectLimit}
			return runErr
		}
		var delay time.Duration
		if timing == wsprotocol.ReconnectPrompt {
			delay = c.jitter(wsprotocol.PTYPromptReconnectMaxDelay)
		} else {
			delay = c.jitter(backoffCeiling(backoffAttempt))
			backoffAttempt++
		}
		_, _ = fmt.Fprintf(c.notice, "\r\n%v; reconnecting (press Ctrl-C to stop)...\r\n", closeErr)
		slog.Debug("PTY client reconnecting", "code", closeErr.Code, "reason", closeErr.Reason, "delay", delay)

		newConn, stop, reconnErr := c.reconnect(delay, stdinCh, closeErr)
		if stop {
			runErr = reconnErr
			return runErr
		}
		if reconnErr != nil {
			runErr = &PTYReconnectError{Close: closeErr, Err: reconnErr}
			return runErr
		}
		conn = newConn
		pendingClose = closeErr
		// Redraw: make the new remote PTY match the local terminal.
		c.sendResize()
	}
}

// backoffCeiling is the upper bound of the full-jitter window for the
// attempt-th consecutive backoff reconnect.
func backoffCeiling(attempt int) time.Duration {
	d := reconnectBackoffBase
	for i := 0; i < attempt && d < reconnectBackoffMax; i++ {
		d *= 2
	}
	return min(d, reconnectBackoffMax)
}

// currentConn returns the current connection.
func (c *PTYClient) currentConn() *websocket.Conn {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn
}

// sendResize sends the local terminal size, if stdin is a terminal.
func (c *PTYClient) sendResize() {
	if c.termSize == nil {
		return
	}
	if cols, rows, ok := c.termSize(); ok {
		_ = c.writeToWebSocket(wsprotocol.NewPTYResizeMessage(cols, rows))
	}
}

// stdinResult is one read from stdin.
type stdinResult struct {
	data []byte
	err  error
}

// startStdinReader starts the one goroutine that reads stdin for the whole
// of Run, across reconnects. stdin's Read is a blocking call that does not
// support deadlines, so it runs in its own goroutine and hands results over
// a channel. The goroutine exits when stdin returns an error (or stays
// blocked until the process exits).
func (c *PTYClient) startStdinReader() <-chan stdinResult {
	ch := make(chan stdinResult)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := c.stdin.Read(buf)
			if err != nil {
				slog.Debug("PTY stdin inner reader got error", "error", err)
				ch <- stdinResult{nil, err}
				return
			}
			if n > 0 {
				// Copy the data to avoid race conditions
				data := make([]byte, n)
				copy(data, buf[:n])
				ch <- stdinResult{data, nil}
			}
		}
	}()
	return ch
}

// reconnect waits for delay, then opens a new connection at the current
// terminal size and makes it the current connection, closing the old one.
//
// Stdin is watched from the close until the new connection is up, through
// both the wait and the dial, and nothing typed in that time reaches the
// new session: Ctrl-C or Ctrl-D stops the reconnect and returns closeErr,
// the tmux detach sequence (Ctrl-b d) stops it as a clean detach (nil
// error), and anything else is discarded. stdin ending returns closeErr;
// a cancelled context returns its error. In all of those cases stop is
// true, and a dial still in flight is abandoned (closed if it completes
// later). The WebSocket handshake does not itself stop when the context
// is cancelled, which is another reason the dial runs in its own
// goroutine. When the dial fails, stop is false and err is the dial error.
func (c *PTYClient) reconnect(delay time.Duration, stdinCh <-chan stdinResult, closeErr *PTYCloseError) (conn *websocket.Conn, stop bool, err error) {
	type dialResult struct {
		conn *websocket.Conn
		err  error
	}
	var results chan dialResult // nil (never ready) until the dial starts
	abandon := func() {
		if results == nil {
			return
		}
		go func(results <-chan dialResult) {
			if r := <-results; r.conn != nil {
				_ = r.conn.Close()
			}
		}(results)
	}
	timer := c.after(delay)
	prevCtrlB := false
	for {
		select {
		case <-timer:
			timer = nil
			if c.termSize != nil {
				if cols, rows, ok := c.termSize(); ok {
					c.config.Cols, c.config.Rows = cols, rows
				}
			}
			results = make(chan dialResult, 1)
			go func(results chan<- dialResult) {
				conn, err := c.redialFn(c.ctx)
				results <- dialResult{conn, err}
			}(results)
		case r := <-results:
			if r.err != nil {
				// A dial cut short by the cancel can report its error
				// before the select sees ctx.Done: report the cancel.
				if c.ctx.Err() != nil {
					return nil, true, c.ctx.Err()
				}
				return nil, false, r.err
			}
			c.writeMu.Lock()
			old := c.conn
			c.conn = r.conn
			c.writeMu.Unlock()
			if old != nil {
				_ = old.Close()
			}
			return r.conn, false, nil
		case <-c.ctx.Done():
			abandon()
			return nil, true, c.ctx.Err()
		case in := <-stdinCh:
			if in.err != nil {
				abandon()
				if in.err == io.EOF {
					return nil, true, closeErr
				}
				return nil, true, in.err
			}
			for _, b := range in.data {
				switch {
				case b == keyCtrlC, b == keyCtrlD:
					abandon()
					return nil, true, closeErr
				case prevCtrlB && b == keyDetach:
					abandon()
					return nil, true, nil
				}
				prevCtrlB = b == keyCtrlB
			}
		}
	}
}

// runConnection pumps one connection: stdin to the WebSocket in this
// goroutine, the WebSocket to stdout in another. It returns when either
// direction ends, after sending a normal close frame.
func (c *PTYClient) runConnection(conn *websocket.Conn, stdinCh <-chan stdinResult) error {
	c.connLive.Store(false)
	wsErrCh := make(chan error, 1)
	go func() {
		slog.Debug("PTY client websocket reader starting")
		err := c.readFromWebSocket(conn)
		slog.Debug("PTY client websocket reader exited", "error", err)
		wsErrCh <- err
	}()

	err := c.pumpStdin(stdinCh, wsErrCh)
	slog.Debug("PTY client connection ended", "error", err)

	// Close connection
	slog.Debug("PTY client sending close message")
	c.writeMu.Lock()
	_ = conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
	)
	c.writeMu.Unlock()
	return err
}

// pumpStdin forwards stdin to the current connection until the connection's
// reader ends, stdin ends, or the context is cancelled.
func (c *PTYClient) pumpStdin(stdinCh <-chan stdinResult, wsErrCh <-chan error) error {
	send := func(data []byte) error {
		if err := c.writeToWebSocket(wsprotocol.NewPTYDataMessage(data)); err != nil {
			slog.Debug("PTY stdin reader: write error", "error", err)
			// The write may have raced a server close; prefer the close
			// code if the reader reports one promptly.
			select {
			case wsErr := <-wsErrCh:
				var closeErr *PTYCloseError
				if errors.As(wsErr, &closeErr) {
					return wsErr
				}
			case <-c.after(writeFailGrace):
			}
			return err
		}
		return nil
	}
	for {
		select {
		case <-c.ctx.Done():
			slog.Debug("PTY stdin reader: context cancelled")
			return c.ctx.Err()
		case err := <-wsErrCh:
			return err
		case r := <-stdinCh:
			if r.err != nil {
				if r.err == io.EOF {
					slog.Debug("PTY stdin reader: EOF")
					return nil
				}
				slog.Debug("PTY stdin reader: error", "error", r.err)
				return r.err
			}
			if err := send(r.data); err != nil {
				return err
			}
		}
	}
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
		restore := c.restoreTerm
		if restore == nil {
			restore = term.Restore
		}
		_ = restore(c.oldFd, c.termState)
		c.termState = nil
	}
}

// restoreAfterRun restores the terminal when Run ends. On a clean end it
// always writes the reset sequences. On an error it writes them only when the
// server has sent data: by then the remote tmux has switched to the
// alternate screen and drawn its status line, so the CLI must leave it before
// printing the error, or the error lands on top of the remote screen. If no
// data ever arrived, the local screen was never touched, and skipping the
// sequences keeps earlier output (and the cursor) where they were. After a
// reset on error it also starts a fresh line for the error message.
func (c *PTYClient) restoreAfterRun(runErr error) {
	hadTerminal := c.termState != nil
	writeReset := runErr == nil || c.receivedData.Load()
	c.restoreTerminal(writeReset)
	if hadTerminal && writeReset && runErr != nil {
		_, _ = os.Stdout.Write([]byte("\r\n"))
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

// readFromWebSocket reads from conn and writes to stdout.
func (c *PTYClient) readFromWebSocket(conn *websocket.Conn) error {
	gotMessage := false
	// Set initial read deadline to detect if server-side PTY fails to start
	if err := conn.SetReadDeadline(time.Now().Add(initialDataTimeout)); err != nil {
		return fmt.Errorf("failed to set read deadline: %w", err)
	}

	for {
		select {
		case <-c.ctx.Done():
			slog.Debug("PTY websocket reader: context cancelled")
			return c.ctx.Err()
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			slog.Debug("PTY websocket reader: read error", "error", err)
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				if wsprotocol.ClassifyPTYClose(closeErr.Code) == wsprotocol.DispositionDetached {
					slog.Debug("PTY websocket reader: clean close")
					return nil
				}
				if closeErr.Code == wsprotocol.ClosePTYAttachUnsupported {
					return errors.New(attachUnsupportedMessage)
				}
				return &PTYCloseError{Code: closeErr.Code, Reason: closeErr.Text}
			}
			// Check if this is a timeout on initial data
			if !gotMessage {
				if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
					return fmt.Errorf("timed out waiting for PTY data (server may have failed to start the session)")
				}
			}
			return err
		}

		// Clear the read deadline after the first message from the server.
		if !gotMessage {
			gotMessage = true
			slog.Debug("PTY websocket reader: received first message, clearing deadline")
			if err := conn.SetReadDeadline(time.Time{}); err != nil {
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
			// Only a data frame makes the connection live: a session that
			// sends anything else and closes did not serve the terminal.
			c.connLive.Store(true)
			c.receivedData.Store(true)
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
	if conn := c.currentConn(); conn != nil {
		slog.Debug("PTY client closing websocket connection")
		return conn.Close()
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

	u.Path = joinEndpointPath(u.Path, fmt.Sprintf("/api/v1/agents/%s/attach", slug))

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
