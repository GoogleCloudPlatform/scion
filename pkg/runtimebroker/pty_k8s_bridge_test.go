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

package runtimebroker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/remotecommand"
)

// fakeK8sExecutor implements remotecommand.Executor with an injectable
// StreamWithContext, so tests can drive StreamPTYHandler.bridgeK8sExec and
// LocalPTYSession.bridgeK8sExec directly — this is the actual call-site
// wiring between the executor and the two I/O pumps, which
// TestAwaitK8sExecEnd's hand-built channels cannot reach (see that test's
// doc comment and pr/p2-pr-body.md).
type fakeK8sExecutor struct {
	stream func(ctx context.Context, opts remotecommand.StreamOptions) error
}

func (f *fakeK8sExecutor) Stream(opts remotecommand.StreamOptions) error {
	return f.stream(context.Background(), opts)
}

func (f *fakeK8sExecutor) StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error {
	return f.stream(ctx, opts)
}

// eofFirstDelay bounds how long closeStdoutThenReturn waits after closing
// stdout before returning the executor's result. It exists to pin the
// EOF-first ordering that a real transport drop can produce: without it, the
// bridge's own executor goroutine (which closes stdout and then sends the
// result in the same goroutine, with no intervening yield point) almost
// always wins awaitK8sExecEnd's first select over the stdout-reading
// goroutine's EOF, so the transport-drop rows below would only ever exercise
// the easy ordering (execErrCh arriving first) and rarely the harder one
// where the stdout EOF arrives first and could otherwise be mistaken for a
// clean exit. 20ms is long enough in practice for the stdout reader to
// observe the EOF and send it to errCh first, and short enough not to
// meaningfully slow the suite.
const eofFirstDelay = 20 * time.Millisecond

// closeStdoutThenReturn closes the stdout pipe before returning err, then
// waits eofFirstDelay — reproducing both the ordering and the timing the
// call site's own executor goroutine produces (it closes the stdout pipe
// itself right before sending the executor's result; the real SPDY executor
// in client-go v0.35 never closes Stdout itself, it only copies into it). A
// correct call site must still resolve cleanExit from the returned err, not
// from the stdout pump's EOF, regardless of which one awaitK8sExecEnd's
// first select happens to observe first.
func closeStdoutThenReturn(err error) func(ctx context.Context, opts remotecommand.StreamOptions) error {
	return func(ctx context.Context, opts remotecommand.StreamOptions) error {
		_ = opts.Stdout.(io.Closer).Close()
		time.Sleep(eofFirstDelay)
		return err
	}
}

// TestBridgeK8sExec drives StreamPTYHandler.bridgeK8sExec — the control
// channel (Hub-facing) k8s bridge — with a fake executor. TestAwaitK8sExecEnd
// tests awaitK8sExecEnd's ordering logic with hand-built channels, but cannot
// see whether runK8sExec's call site actually wires execErrCh/errCh
// correctly; this test exercises that wiring directly, with no k8s cluster,
// kubectl, or tmux involved.
func TestBridgeK8sExec(t *testing.T) {
	newHandler := func(t *testing.T, streamID string) *StreamPTYHandler {
		t.Helper()
		handler := &StreamHandler{
			streamID: streamID,
			slug:     "k8s-bridge-fixture",
			dataCh:   make(chan []byte, 1),
			resizeCh: make(chan [2]int, 1),
			closeCh:  make(chan struct{}),
		}
		client := &ControlChannelClient{} // SendStreamData is never reached: no test row produces stdout data.
		return NewStreamPTYHandler(client, handler, "container-1", "kubernetes", "scion", "", 80, 24, nil, nil)
	}

	t.Run("transport drop leaves cleanExit false", func(t *testing.T) {
		h := newHandler(t, "k8s-bridge-drop")
		fake := &fakeK8sExecutor{stream: closeStdoutThenReturn(errors.New("transport dropped"))}

		err := h.bridgeK8sExec(fake)

		require.Error(t, err)
		require.False(t, h.cleanExit, "a transport drop must not be reported as a clean exit")
	})

	t.Run("clean exit sets cleanExit true", func(t *testing.T) {
		h := newHandler(t, "k8s-bridge-clean")
		fake := &fakeK8sExecutor{stream: closeStdoutThenReturn(nil)}

		err := h.bridgeK8sExec(fake)

		requireCleanExitErr(t, err)
		require.True(t, h.cleanExit, "a clean tmux exit must be reported as clean")
	})
}

// requireCleanExitErr asserts the error awaitK8sExecEnd returns for a clean
// exit is either nil or io.EOF. Which one it is depends on whether the
// executor's own result or the stdout pump's EOF wins awaitK8sExecEnd's
// first select — a benign, expected race (not a data race: -race does not
// flag it) between two independently-correct goroutines, not something the
// bridge code controls. cleanExit is unaffected either way: it is always
// resolved from the executor's own result (see awaitK8sExecEnd), never from
// this returned error. Production already treats the two identically —
// handlePTYStreamWithAgent only logs Run()'s error when it is neither nil
// nor io.EOF.
func requireCleanExitErr(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("want nil or io.EOF for a clean exit, got: %v", err)
	}
}

// newRawWSPair sets up a real client/server websocket pair over a local
// httptest server, for tests that need an actual *websocket.Conn (as
// LocalPTYSession does) rather than the wsprotocol.Connection wrapper
// newWSPair returns.
func newRawWSPair(t *testing.T) (client, server *websocket.Conn, cleanup func()) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverReady := make(chan *websocket.Conn, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverReady <- ws
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	s := <-serverReady

	return c, s, func() {
		_ = c.Close()
		_ = s.Close()
		srv.Close()
	}
}

// TestLocalPTYSessionBridgeK8sExec is TestBridgeK8sExec's twin for the
// direct-attach path: LocalPTYSession.bridgeK8sExec pumps to a real
// *websocket.Conn instead of the control channel, so this uses a real
// (loopback) websocket pair rather than fixedAgentLookup-style fakes. The
// server end is never read from in these rows (no stdout data is produced),
// so its only job is to give the control-reader goroutine's blocking
// ReadMessage a peer to unblock against once the pair closes.
func TestLocalPTYSessionBridgeK8sExec(t *testing.T) {
	newSession := func(t *testing.T) (*LocalPTYSession, func()) {
		t.Helper()
		client, _, cleanup := newRawWSPair(t)
		s := newLocalPTYSession(context.Background(), "agent-1", "container-1", "kubernetes", "scion", "", client, 80, 24, nil, nil)
		return s, cleanup
	}

	t.Run("transport drop leaves cleanExit false", func(t *testing.T) {
		s, cleanup := newSession(t)
		defer cleanup()
		fake := &fakeK8sExecutor{stream: closeStdoutThenReturn(errors.New("transport dropped"))}

		err := s.bridgeK8sExec(fake)

		require.Error(t, err)
		require.False(t, s.cleanExit, "a transport drop must not be reported as a clean exit")
	})

	t.Run("clean exit sets cleanExit true", func(t *testing.T) {
		s, cleanup := newSession(t)
		defer cleanup()
		fake := &fakeK8sExecutor{stream: closeStdoutThenReturn(nil)}

		err := s.bridgeK8sExec(fake)

		requireCleanExitErr(t, err)
		require.True(t, s.cleanExit, "a clean tmux exit must be reported as clean")
	})
}
