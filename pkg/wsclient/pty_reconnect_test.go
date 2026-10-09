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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
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

	// refuse, if set, rejects attempt idx with HTTP 503 before upgrading.
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

func newPTYScriptServer(t *testing.T, script func(idx int, s *ptyScriptServer, conn *websocket.Conn)) *ptyScriptServer {
	t.Helper()
	s := &ptyScriptServer{t: t, script: script, received: map[int][]map[string]any{}, notify: make(chan struct{}, 64)}
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
		s.mu.Lock()
		for _, m := range s.received[idx] {
			if m["type"] == wsprotocol.TypeResize {
				s.mu.Unlock()
				return m
			}
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-deadline:
			s.t.Errorf("connection %d never received a resize", idx)
			return nil
		}
	}
}

// fakeTiming records the reconnect delays the client asks for. jitter
// returns half the window; after fires at once unless block is set.
type fakeTiming struct {
	mu      sync.Mutex
	windows []time.Duration
	waits   []time.Duration
	block   bool
	waiting chan struct{}
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
		return make(chan time.Time)
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

// newScriptedClient connects a client to s with fake timing, a fixed
// 100x30 terminal size, a pipe for stdin that never ends, and a terminal
// state marked as raw so tests can check it is restored.
func newScriptedClient(t *testing.T, s *ptyScriptServer, ft *fakeTiming) (*PTYClient, *bytes.Buffer) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	notice := &bytes.Buffer{}
	c := NewPTYClient(PTYClientConfig{Endpoint: s.srv.URL, Slug: "a1", Cols: 80, Rows: 24})
	c.stdin = r
	c.oldFd = int(r.Fd()) // not a terminal: setupTerminal leaves termState alone
	c.termState = &term.State{}
	c.notice = notice
	c.jitter = ft.jitter
	c.after = ft.after
	c.now = ft.now
	c.termSize = func() (int, int, bool) { return 100, 30, true }
	require.NoError(t, c.Connect(context.Background()))
	t.Cleanup(func() { _ = c.Close() })
	return c, notice
}

// TestRun_ReconnectsOncePerRetryClose: one reconnect per 4503, 4504 and
// 1011 close. A reconnected session that goes live re-arms the next close.
func TestRun_ReconnectsOncePerRetryClose(t *testing.T) {
	for _, code := range []int{wsprotocol.ClosePTYUpstreamUnavailable, wsprotocol.ClosePTYUpstreamTimeout, wsprotocol.ClosePTYInternalError} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := newPTYScriptServer(t, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
				sendData(conn)
				if idx < 2 {
					sendClose(conn, code, "relay_restart")
					return
				}
				sendClose(conn, wsprotocol.ClosePTYNormal, "")
			})
			ft := &fakeTiming{}
			c, notice := newScriptedClient(t, s, ft)

			require.NoError(t, c.Run())
			assert.Equal(t, 3, s.attemptCount(), "each retry close gets exactly one reconnect")
			assert.Len(t, ft.jitterWindows(), 2)
			assert.Contains(t, notice.String(), "reconnecting")
			assert.Nil(t, c.termState, "terminal restored")
		})
	}
}

// TestRun_ReconnectDelay: 4503 waits a full-jitter delay from [0, 5s]; 4504
// uses exponential backoff with full jitter, reset after a long session.
func TestRun_ReconnectDelay(t *testing.T) {
	run := func(t *testing.T, code int, step time.Duration) *fakeTiming {
		s := newPTYScriptServer(t, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
			sendData(conn)
			if idx < 3 {
				sendClose(conn, code, "")
				return
			}
			sendClose(conn, wsprotocol.ClosePTYNormal, "")
		})
		ft := &fakeTiming{step: step}
		c, _ := newScriptedClient(t, s, ft)
		require.NoError(t, c.Run())
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
	for _, code := range []int{wsprotocol.ClosePTYUpstreamUnavailable, wsprotocol.ClosePTYUpstreamTimeout, wsprotocol.ClosePTYInternalError} {
		t.Run(fmt.Sprintf("%d closed before live", code), func(t *testing.T) {
			s := newPTYScriptServer(t, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
				if idx == 0 {
					sendData(conn)
				}
				// Every later attempt closes again before sending data.
				sendClose(conn, code, "draining")
			})
			ft := &fakeTiming{}
			c, _ := newScriptedClient(t, s, ft)

			err := c.Run()
			var re *PTYReconnectError
			require.True(t, errors.As(err, &re), "got %T: %v", err, err)
			assert.Equal(t, code, re.Close.Code)
			var second *PTYCloseError
			require.True(t, errors.As(re.Err, &second))
			assert.Equal(t, code, second.Code)
			assert.Equal(t, 2, s.attemptCount(), "exactly one reconnect")
			assert.Len(t, ft.jitterWindows(), 1)
			assert.Nil(t, c.termState, "terminal restored")
		})
		t.Run(fmt.Sprintf("%d dial refused", code), func(t *testing.T) {
			s := newPTYScriptServer(t, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
				sendData(conn)
				sendClose(conn, code, "")
			})
			s.refuse = func(idx int) bool { return idx > 0 }
			ft := &fakeTiming{}
			c, _ := newScriptedClient(t, s, ft)

			err := c.Run()
			var re *PTYReconnectError
			require.True(t, errors.As(err, &re), "got %T: %v", err, err)
			assert.Contains(t, re.Err.Error(), "status 503")
			var ce *PTYCloseError
			require.True(t, errors.As(err, &ce), "the original close stays reachable")
			assert.Equal(t, code, ce.Code)
			assert.Equal(t, 2, s.attemptCount())
			assert.Nil(t, c.termState, "terminal restored")
		})
	}
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
			s := newPTYScriptServer(t, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
				sendData(conn)
				sendClose(conn, code, "x")
			})
			ft := &fakeTiming{}
			c, notice := newScriptedClient(t, s, ft)

			err := c.Run()
			var ce *PTYCloseError
			require.True(t, errors.As(err, &ce), "got %T: %v", err, err)
			assert.Equal(t, code, ce.Code)
			var re *PTYReconnectError
			assert.False(t, errors.As(err, &re))
			assert.Equal(t, 1, s.attemptCount(), "no reconnect")
			assert.Empty(t, ft.jitterWindows())
			assert.Empty(t, notice.String())
			assert.Nil(t, c.termState, "terminal restored")
		})
	}
}

// TestRun_ReconnectRedraws: the reconnect dials at the current terminal
// size and sends a resize, so the remote tmux redraws at that size.
func TestRun_ReconnectRedraws(t *testing.T) {
	s := newPTYScriptServer(t, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
		sendData(conn)
		if idx == 0 {
			sendClose(conn, wsprotocol.ClosePTYUpstreamUnavailable, "relay_restart")
			return
		}
		s.waitForResize(idx)
		sendClose(conn, wsprotocol.ClosePTYNormal, "")
	})
	ft := &fakeTiming{}
	c, _ := newScriptedClient(t, s, ft)

	require.NoError(t, c.Run())
	require.Equal(t, 2, s.attemptCount())
	resize := s.waitForResize(1)
	require.NotNil(t, resize)
	assert.EqualValues(t, 100, resize["cols"])
	assert.EqualValues(t, 30, resize["rows"])
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, "100", s.queries[1].Get("cols"))
	assert.Equal(t, "30", s.queries[1].Get("rows"))
}

// TestRun_InterruptDuringReconnectWait: cancelling while waiting to
// reconnect ends Run without dialing again and restores the terminal.
func TestRun_InterruptDuringReconnectWait(t *testing.T) {
	s := newPTYScriptServer(t, func(idx int, s *ptyScriptServer, conn *websocket.Conn) {
		sendData(conn)
		sendClose(conn, wsprotocol.ClosePTYUpstreamTimeout, "")
	})
	waiting := make(chan struct{})
	ft := &fakeTiming{block: true, waiting: waiting}
	c, _ := newScriptedClient(t, s, ft)

	errCh := make(chan error, 1)
	go func() { errCh <- c.Run() }()
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("client never started waiting to reconnect")
	}
	c.cancel()
	select {
	case err := <-errCh:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	assert.Equal(t, 1, s.attemptCount(), "no reconnect after cancel")
	assert.Nil(t, c.termState, "terminal restored")
}
