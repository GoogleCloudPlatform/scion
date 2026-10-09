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

package wsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"
)

// ptyScriptServer is a PTY endpoint whose behaviour is scripted per
// connection attempt. It records every text message each connection
// receives and the query of every attempt.
type ptyScriptServer struct {
	t   *testing.T
	srv *httptest.Server

	// refuse, if set, is called for every attempt before upgrading; true
	// rejects the attempt with HTTP 503. Set once, before the server starts.
	refuse func(idx int) bool
	// script drives accepted attempt idx. When it returns, the server
	// closes the TCP connection.
	script func(idx int, s *ptyScriptServer, conn *websocket.Conn)

	mu       sync.Mutex
	attempts int
	queries  []url.Values
	received map[int][]map[string]any
	notify   chan struct{}
}

func newPTYScriptServer(t *testing.T, refuse func(idx int) bool, script func(idx int, s *ptyScriptServer, conn *websocket.Conn)) *ptyScriptServer {
	t.Helper()
	s := &ptyScriptServer{t: t, refuse: refuse, script: script, received: map[int][]map[string]any{}, notify: make(chan struct{}, 64)}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		idx := s.attempts
		s.attempts++
		s.queries = append(s.queries, r.URL.Query())
		s.mu.Unlock()
		if s.refuse != nil && s.refuse(idx) {
			http.Error(w, "no session", http.StatusServiceUnavailable)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		go func() {
			for {
				_, data, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var m map[string]any
				if json.Unmarshal(data, &m) == nil {
					s.mu.Lock()
					s.received[idx] = append(s.received[idx], m)
					s.mu.Unlock()
					select {
					case s.notify <- struct{}{}:
					default:
					}
				}
			}
		}()
		s.script(idx, s, conn)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *ptyScriptServer) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// messagesOfType returns the messages of type typ connection idx received.
func (s *ptyScriptServer) messagesOfType(idx int, typ string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, m := range s.received[idx] {
		if m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

// sendData sends one PTY data frame.
func sendData(conn *websocket.Conn) {
	_ = conn.WriteJSON(wsprotocol.NewPTYDataMessage([]byte("screen")))
}

// sendClose sends a close frame with code and reason.
func sendClose(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}

// waitForResize blocks until connection idx has received a resize message,
// and returns it.
func (s *ptyScriptServer) waitForResize(idx int) map[string]any {
	deadline := time.After(10 * time.Second)
	for {
		if m := s.messagesOfType(idx, wsprotocol.TypeResize); len(m) > 0 {
			return m[0]
		}
		select {
		case <-s.notify:
		case <-deadline:
			s.t.Errorf("connection %d never received a resize", idx)
			return nil
		}
	}
}

// fakeTiming records the reconnect delays the client asks for. jitter
// returns half the window. after fires at once, unless block is set: then
// it signals waiting and fires only when the test sends on fire.
type fakeTiming struct {
	mu      sync.Mutex
	windows []time.Duration
	waits   []time.Duration
	block   bool
	waiting chan struct{}
	fire    chan time.Time
	clock   time.Time
	step    time.Duration
}

func (f *fakeTiming) jitter(maxDelay time.Duration) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.windows = append(f.windows, maxDelay)
	return maxDelay / 2
}

func (f *fakeTiming) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waits = append(f.waits, d)
	if f.block {
		if f.waiting != nil {
			close(f.waiting)
			f.waiting = nil
		}
		return f.fire
	}
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

func (f *fakeTiming) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(f.step)
	return f.clock
}

func (f *fakeTiming) jitterWindows() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.windows...)
}

// chanReader is a stdin whose reads are fed by the test. Each Read first
// signals on reads, so once the test has seen the Read after a chunk, the
// client's stdin goroutine has handed that chunk over (its channel is
// unbuffered), i.e. the client has consumed it.
type chanReader struct {
	chunks chan []byte
	reads  chan struct{}
	// armed: the signal of the Read now blocked waiting for a chunk has
	// already been consumed (by the previous typeAndWait).
	armed bool
}

func newChanReader() *chanReader {
	return &chanReader{chunks: make(chan []byte), reads: make(chan struct{}, 16)}
}

func (r *chanReader) Read(p []byte) (int, error) {
	r.reads <- struct{}{}
	b, ok := <-r.chunks
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

// typeAndWait sends b and waits until the client has consumed it. Each
// Read signals exactly once, so this consumes exactly one signal per Read:
// the signal of the Read that takes b (unless an earlier call already
// consumed it), then the signal of the next Read, which starts only after
// the client has taken b from the stdin goroutine.
func (r *chanReader) typeAndWait(t *testing.T, b []byte) {
	t.Helper()
	wait := func(what string) {
		select {
		case <-r.reads:
		case <-time.After(10 * time.Second):
			t.Fatal(what)
		}
	}
	if !r.armed {
		wait("client never read stdin")
	}
	select {
	case r.chunks <- b:
	case <-time.After(10 * time.Second):
		t.Fatal("client never took the stdin chunk")
	}
	wait("client never consumed stdin input")
	r.armed = true
}

// scriptedClient is a client wired to fake timing, a fixed 100x30 terminal
// size, a test-fed stdin and a terminal state marked as raw, with every
// terminal restore counted.
type scriptedClient struct {
	*PTYClient
	notice   *bytes.Buffer
	restores atomic.Int32
	in       *chanReader
}

func newScriptedClient(t *testing.T, s *ptyScriptServer, ft *fakeTiming) *scriptedClient {
	t.Helper()
	sc := &scriptedClient{notice: &bytes.Buffer{}, in: newChanReader()}
	c := NewPTYClient(PTYClientConfig{Endpoint: s.srv.URL, Slug: "a1", Cols: 80, Rows: 24})
	c.stdin = sc.in
	c.oldFd = -1 // not a terminal: setupTerminal leaves termState alone
	c.termState = &term.State{}
	c.restoreTerm = func(int, *term.State) error { sc.restores.Add(1); return nil }
	c.notice = sc.notice
	c.jitter = ft.jitter
	c.after = ft.after
	c.now = ft.now
	c.termSize = func() (int, int, bool) { return 100, 30, true }
	require.NoError(t, c.Connect(context.Background()))
	sc.PTYClient = c
	t.Cleanup(func() { _ = c.Close() })
	return sc
}

// assertRestoredOnce checks the terminal was restored exactly once.
func (sc *scriptedClient) assertRestoredOnce(t *testing.T) {
	t.Helper()
	assert.Nil(t, sc.termState, "terminal restored")
	assert.EqualValues(t, 1, sc.restores.Load(), "terminal restored exactly once")
}

// runAsync starts Run and returns a channel for its result.
func (sc *scriptedClient) runAsync() <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- sc.Run() }()
	return ch
}

func waitRun(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

var reconnectCodes = []int{wsprotocol.ClosePTYUpstreamUnavailable, wsprotocol.ClosePTYUpstreamTimeout, wsprotocol.ClosePTYInternalError}

// TestRun_ReconnectsOncePerRetryClose: one reconnect per 4503, 4504 and
// 1011 close. A reconnected session that goes live re-arms the next close.
func TestRun_ReconnectsOncePerRetryClose(t *testing.T) {
	for _, code := range reconnectCodes {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := newPTYScriptServer(t, nil, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
				sendData(conn)
				if idx < 2 {
					sendClose(conn, code, "relay_restart")
					return
				}
				sendClose(conn, wsprotocol.ClosePTYNormal, "")
			})
			ft := &fakeTiming{}
			sc := newScriptedClient(t, s, ft)

			require.NoError(t, sc.Run())
			assert.Equal(t, 3, s.attemptCount(), "each retry close gets exactly one reconnect")
			assert.Len(t, ft.jitterWindows(), 2)
			assert.Contains(t, sc.notice.String(), "reconnecting (press Ctrl-C to stop)")
			sc.assertRestoredOnce(t)
		})
	}
}

// TestRun_ReconnectDelay: 4503 waits a full-jitter delay from [0, 5s]; 4504
// and 1011 use exponential backoff with full jitter, reset after a long
// session.
func TestRun_ReconnectDelay(t *testing.T) {
	run := func(t *testing.T, code int, step time.Duration) *fakeTiming {
		s := newPTYScriptServer(t, nil, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
			sendData(conn)
			if idx < 3 {
				sendClose(conn, code, "")
				return
			}
			sendClose(conn, wsprotocol.ClosePTYNormal, "")
		})
		ft := &fakeTiming{step: step}
		sc := newScriptedClient(t, s, ft)
		require.NoError(t, sc.Run())
		sc.assertRestoredOnce(t)
		return ft
	}

	t.Run("4503 prompt full jitter", func(t *testing.T) {
		ft := run(t, wsprotocol.ClosePTYUpstreamUnavailable, 0)
		want := wsprotocol.PTYPromptReconnectMaxDelay
		assert.Equal(t, []time.Duration{want, want, want}, ft.jitterWindows())
		assert.Contains(t, ft.waits, want/2, "the client waits the jittered delay")
	})
	t.Run("4504 exponential backoff", func(t *testing.T) {
		ft := run(t, wsprotocol.ClosePTYUpstreamTimeout, 0)
		assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, ft.jitterWindows())
	})
	t.Run("1011 exponential backoff", func(t *testing.T) {
		ft := run(t, wsprotocol.ClosePTYInternalError, 0)
		assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, ft.jitterWindows())
	})
	t.Run("4504 backoff resets after a long session", func(t *testing.T) {
		ft := run(t, wsprotocol.ClosePTYUpstreamTimeout, reconnectBackoffResetAfter)
		assert.Equal(t, []time.Duration{time.Second, time.Second, time.Second}, ft.jitterWindows())
	})
}

func TestBackoffCeiling(t *testing.T) {
	assert.Equal(t, time.Second, backoffCeiling(0))
	assert.Equal(t, 2*time.Second, backoffCeiling(1))
	assert.Equal(t, 32*time.Second, backoffCeiling(5))
	assert.Equal(t, 60*time.Second, backoffCeiling(6))
	assert.Equal(t, 60*time.Second, backoffCeiling(100))
}

// TestRun_FailedReconnectDoesNotLoop: a second consecutive close after a
// failed reconnect ends Run; there is no further attempt.
func TestRun_FailedReconnectDoesNotLoop(t *testing.T) {
	for _, code := range reconnectCodes {
		t.Run(fmt.Sprintf("%d closed before live", code), func(t *testing.T) {
			s := newPTYScriptServer(t, nil, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
				if idx == 0 {
					sendData(conn)
				}
				// Every later attempt closes again before sending data.
				sendClose(conn, code, "draining")
			})
			ft := &fakeTiming{}
			sc := newScriptedClient(t, s, ft)

			err := sc.Run()
			var re *PTYReconnectError
			require.True(t, errors.As(err, &re), "got %T: %v", err, err)
			assert.Equal(t, code, re.Close.Code)
			var second *PTYCloseError
			require.True(t, errors.As(re.Err, &second))
			assert.Equal(t, code, second.Code)
			assert.Equal(t, 2, s.attemptCount(), "exactly one reconnect")
			assert.Len(t, ft.jitterWindows(), 1)
			sc.assertRestoredOnce(t)
		})
		t.Run(fmt.Sprintf("%d dial refused", code), func(t *testing.T) {
			s := newPTYScriptServer(t, func(idx int) bool { return idx > 0 }, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
				sendData(conn)
				sendClose(conn, code, "")
			})
			ft := &fakeTiming{}
			sc := newScriptedClient(t, s, ft)

			err := sc.Run()
			var re *PTYReconnectError
			require.True(t, errors.As(err, &re), "got %T: %v", err, err)
			assert.Contains(t, re.Err.Error(), "status 503")
			var ce *PTYCloseError
			require.True(t, errors.As(err, &ce), "the original close stays reachable")
			assert.Equal(t, code, ce.Code)
			assert.Equal(t, 2, s.attemptCount())
			sc.assertRestoredOnce(t)
		})
	}
}

// TestRun_ReconnectLiveOnlyAfterDataFrame: a reconnected session that sends
// a frame other than data and then closes counts as a failed reconnect.
func TestRun_ReconnectLiveOnlyAfterDataFrame(t *testing.T) {
	s := newPTYScriptServer(t, nil, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
		if idx == 0 {
			sendData(conn)
		} else {
			_ = conn.WriteJSON(map[string]string{"type": "pong"})
		}
		sendClose(conn, wsprotocol.ClosePTYUpstreamUnavailable, "relay_restart")
	})
	ft := &fakeTiming{}
	sc := newScriptedClient(t, s, ft)

	err := sc.Run()
	var re *PTYReconnectError
	require.True(t, errors.As(err, &re), "got %T: %v", err, err)
	assert.Equal(t, 2, s.attemptCount())
	sc.assertRestoredOnce(t)
}

// TestRun_ShortLivedReconnectsAreBounded: a server that accepts, sends
// output and closes 4503 again and again is reconnected to at most
// maxShortReconnects times in a row; long-lived sessions reset the count.
func TestRun_ShortLivedReconnectsAreBounded(t *testing.T) {
	t.Run("short sessions stop", func(t *testing.T) {
		s := newPTYScriptServer(t, nil, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
			sendData(conn)
			sendClose(conn, wsprotocol.ClosePTYUpstreamUnavailable, "relay_restart")
		})
		ft := &fakeTiming{} // the clock never advances: every session is short
		sc := newScriptedClient(t, s, ft)

		err := sc.Run()
		var re *PTYReconnectError
		require.True(t, errors.As(err, &re), "got %T: %v", err, err)
		assert.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, re.Close.Code)
		assert.ErrorIs(t, err, ErrPTYReconnectLimit)
		assert.Equal(t, maxShortReconnects+1, s.attemptCount())
		assert.Len(t, ft.jitterWindows(), maxShortReconnects)
		sc.assertRestoredOnce(t)
	})
	t.Run("long sessions keep reconnecting", func(t *testing.T) {
		const closes = maxShortReconnects + 2
		s := newPTYScriptServer(t, nil, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
			sendData(conn)
			if idx < closes {
				sendClose(conn, wsprotocol.ClosePTYUpstreamUnavailable, "relay_restart")
				return
			}
			sendClose(conn, wsprotocol.ClosePTYNormal, "")
		})
		ft := &fakeTiming{step: reconnectBackoffResetAfter}
		sc := newScriptedClient(t, s, ft)

		require.NoError(t, sc.Run())
		assert.Equal(t, closes+1, s.attemptCount())
		sc.assertRestoredOnce(t)
	})
}

// TestRun_TerminalCloseNeverReconnects: 4401, 4403, 4404 (and the other
// terminal and detached codes) end Run with no reconnect.
func TestRun_TerminalCloseNeverReconnects(t *testing.T) {
	codes := []int{
		wsprotocol.ClosePTYAuthRequired, wsprotocol.ClosePTYForbidden, wsprotocol.ClosePTYAgentNotFound,
		wsprotocol.ClosePTYProtocolError, wsprotocol.ClosePTYSuperseded, wsprotocol.ClosePTYCancelled,
		wsprotocol.ClosePTYSessionGone, 4999,
		// Retry codes the CLI leaves to the user.
		wsprotocol.ClosePTYGoingAway, wsprotocol.ClosePTYTryAgainLater,
	}
	for _, code := range codes {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := newPTYScriptServer(t, nil, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
				sendData(conn)
				sendClose(conn, code, "x")
			})
			ft := &fakeTiming{}
			sc := newScriptedClient(t, s, ft)

			err := sc.Run()
			var ce *PTYCloseError
			require.True(t, errors.As(err, &ce), "got %T: %v", err, err)
			assert.Equal(t, code, ce.Code)
			var re *PTYReconnectError
			assert.False(t, errors.As(err, &re))
			assert.Equal(t, 1, s.attemptCount(), "no reconnect")
			assert.Empty(t, ft.jitterWindows())
			assert.Empty(t, sc.notice.String())
			sc.assertRestoredOnce(t)
		})
	}
}

// TestRun_ReconnectRedraws: the reconnect dials at the current terminal
// size and sends a resize, so the remote tmux redraws at that size. The old
// connection is closed.
func TestRun_ReconnectRedraws(t *testing.T) {
	s := newPTYScriptServer(t, nil, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
		sendData(conn)
		if idx == 0 {
			sendClose(conn, wsprotocol.ClosePTYUpstreamUnavailable, "relay_restart")
			return
		}
		s.waitForResize(idx)
		sendClose(conn, wsprotocol.ClosePTYNormal, "")
	})
	ft := &fakeTiming{}
	sc := newScriptedClient(t, s, ft)
	first := sc.currentConn()

	require.NoError(t, sc.Run())
	require.Equal(t, 2, s.attemptCount())
	resize := s.waitForResize(1)
	require.NotNil(t, resize)
	assert.EqualValues(t, 100, resize["cols"])
	assert.EqualValues(t, 30, resize["rows"])
	s.mu.Lock()
	assert.Equal(t, "100", s.queries[1].Get("cols"))
	assert.Equal(t, "30", s.queries[1].Get("rows"))
	s.mu.Unlock()
	assert.NotSame(t, first, sc.currentConn())
	assert.Error(t, first.NetConn().SetDeadline(time.Now()), "the old connection is closed")
	sc.assertRestoredOnce(t)
}

// startWaiting runs a client against a server that closes connection 0
// with code after one data frame, and returns once the client is waiting
// to reconnect.
func startWaiting(t *testing.T, code int) (*ptyScriptServer, *fakeTiming, *scriptedClient, <-chan error) {
	t.Helper()
	s := newPTYScriptServer(t, nil, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
		sendData(conn)
		if idx == 0 {
			sendClose(conn, code, "")
			return
		}
		// A later session waits for its resize, then detaches cleanly.
		s.waitForResize(idx)
		sendClose(conn, wsprotocol.ClosePTYNormal, "")
	})
	waiting := make(chan struct{})
	ft := &fakeTiming{block: true, waiting: waiting, fire: make(chan time.Time, 1)}
	sc := newScriptedClient(t, s, ft)
	ch := sc.runAsync()
	waitSignal(t, waiting, "the reconnect wait")
	return s, ft, sc, ch
}

// TestRun_InterruptDuringReconnectWait: cancelling while waiting to
// reconnect ends Run without dialing again and restores the terminal.
func TestRun_InterruptDuringReconnectWait(t *testing.T) {
	s, _, sc, ch := startWaiting(t, wsprotocol.ClosePTYUpstreamTimeout)
	sc.cancel()
	assert.ErrorIs(t, waitRun(t, ch), context.Canceled)
	assert.Equal(t, 1, s.attemptCount(), "no reconnect after cancel")
	sc.assertRestoredOnce(t)
}

// TestRun_KeysDuringReconnectWait: in raw mode Ctrl-C, Ctrl-D and the tmux
// detach sequence arrive as bytes. During the wait they stop the reconnect;
// other input is discarded and never reaches the new session.
func TestRun_KeysDuringReconnectWait(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   []byte
		wantNil bool
	}{
		{name: "ctrl-c", input: []byte{keyCtrlC}},
		{name: "ctrl-d", input: []byte{keyCtrlD}},
		{name: "ctrl-c after text", input: []byte("ls\x03")},
		{name: "detach", input: []byte{keyCtrlB, keyDetach}, wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, sc, ch := startWaiting(t, wsprotocol.ClosePTYUpstreamTimeout)
			sc.in.typeAndWait(t, tc.input)
			err := waitRun(t, ch)
			if tc.wantNil {
				assert.NoError(t, err)
			} else {
				var ce *PTYCloseError
				require.True(t, errors.As(err, &ce), "got %T: %v", err, err)
				assert.Equal(t, wsprotocol.ClosePTYUpstreamTimeout, ce.Code, "the original close is reported")
			}
			assert.Equal(t, 1, s.attemptCount(), "no reconnect")
			sc.assertRestoredOnce(t)
		})
	}
	t.Run("other input is discarded", func(t *testing.T) {
		s, ft, sc, ch := startWaiting(t, wsprotocol.ClosePTYUpstreamTimeout)
		sc.in.typeAndWait(t, []byte("rm -rf build\r"))
		ft.fire <- time.Time{}
		require.NoError(t, waitRun(t, ch))
		assert.Equal(t, 2, s.attemptCount())
		assert.Empty(t, s.messagesOfType(1, wsprotocol.TypeData), "input typed during the wait is not replayed")
		sc.assertRestoredOnce(t)
	})
}

// TestRun_StdinEOFDuringReconnectWait: stdin ending during the wait ends
// Run with the original close.
func TestRun_StdinEOFDuringReconnectWait(t *testing.T) {
	s, _, sc, ch := startWaiting(t, wsprotocol.ClosePTYUpstreamUnavailable)
	close(sc.in.chunks)
	err := waitRun(t, ch)
	var ce *PTYCloseError
	require.True(t, errors.As(err, &ce), "got %T: %v", err, err)
	assert.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, ce.Code)
	assert.Equal(t, 1, s.attemptCount())
	sc.assertRestoredOnce(t)
}

// TestRun_CancelBeforeReconnectedSessionIsLive: cancelling after the
// redial, before the new session sent data, reports the cancel, not a
// failed reconnect.
func TestRun_CancelBeforeReconnectedSessionIsLive(t *testing.T) {
	release := make(chan struct{})
	s := newPTYScriptServer(t, nil, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
		if idx == 0 {
			sendData(conn)
			sendClose(conn, wsprotocol.ClosePTYUpstreamUnavailable, "")
			return
		}
		<-release
	})
	// Registered after the server, so it runs before the server closes.
	t.Cleanup(func() { close(release) })
	ft := &fakeTiming{}
	sc := newScriptedClient(t, s, ft)
	ch := sc.runAsync()
	s.waitForResize(1)
	sc.cancel()
	err := waitRun(t, ch)
	assert.ErrorIs(t, err, context.Canceled)
	var re *PTYReconnectError
	assert.False(t, errors.As(err, &re))
	sc.assertRestoredOnce(t)
}

// TestRun_CancelDuringRedial: cancelling while the redial is in flight
// reports the cancel, not a failed reconnect.
func TestRun_CancelDuringRedial(t *testing.T) {
	dialing := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s := newPTYScriptServer(t, func(idx int) bool {
		if idx == 0 {
			return false
		}
		once.Do(func() { close(dialing) })
		<-release
		return true
	}, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
		sendData(conn)
		sendClose(conn, wsprotocol.ClosePTYUpstreamUnavailable, "")
	})
	// Registered after the server, so it runs before the server closes.
	t.Cleanup(func() { close(release) })
	ft := &fakeTiming{}
	sc := newScriptedClient(t, s, ft)
	ch := sc.runAsync()
	waitSignal(t, dialing, "the redial")
	sc.cancel()
	err := waitRun(t, ch)
	assert.ErrorIs(t, err, context.Canceled)
	var re *PTYReconnectError
	assert.False(t, errors.As(err, &re))
	sc.assertRestoredOnce(t)
}

// TestRun_KeysDuringRedial: stdin is watched while the redial is in flight
// too. Ctrl-C stops the reconnect with the original close and Ctrl-b d
// detaches; nothing typed reaches the new connection.
func TestRun_KeysDuringRedial(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   []byte
		wantNil bool
	}{
		{name: "ctrl-c", input: []byte{keyCtrlC}},
		{name: "detach", input: []byte{keyCtrlB, keyDetach}, wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialing := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			s := newPTYScriptServer(t, func(idx int) bool {
				if idx == 0 {
					return false
				}
				once.Do(func() { close(dialing) })
				<-release
				return false // let the abandoned dial complete; the client closes it
			}, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
				sendData(conn)
				if idx == 0 {
					sendClose(conn, wsprotocol.ClosePTYUpstreamTimeout, "")
					return
				}
				// Hold the late connection open until the test ends. (The
				// server's recorder goroutine is this connection's only reader.)
				<-release
			})
			// Registered after the server, so it runs before the server closes.
			t.Cleanup(func() { close(release) })
			ft := &fakeTiming{}
			sc := newScriptedClient(t, s, ft)
			ch := sc.runAsync()
			waitSignal(t, dialing, "the redial")
			sc.in.typeAndWait(t, tc.input)
			err := waitRun(t, ch)
			if tc.wantNil {
				assert.NoError(t, err)
			} else {
				var ce *PTYCloseError
				require.True(t, errors.As(err, &ce), "got %T: %v", err, err)
				assert.Equal(t, wsprotocol.ClosePTYUpstreamTimeout, ce.Code, "the original close is reported")
				var re *PTYReconnectError
				assert.False(t, errors.As(err, &re))
			}
			assert.Empty(t, s.messagesOfType(1, wsprotocol.TypeData), "nothing typed reaches the new connection")
			sc.assertRestoredOnce(t)
		})
	}
}
