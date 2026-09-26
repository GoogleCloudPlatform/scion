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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// newDirectAttachFixture starts a real HTTP server serving handleAgentAttach
// against a real tmux session reached through a private local socket (same
// fixture style as the control-channel classifier tests): only the runtime
// exec is adapted, so this exercises the production WebSocket, keepalive and
// close-frame code, not a mock of it.
func newDirectAttachFixture(t *testing.T) (dial func() (*websocket.Conn, *http.Response, error), tmuxCmd func(args ...string) (string, error)) {
	t.Helper()
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("real-tmux direct-attach test requires tmux in PATH")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TMUX", "")

	dir := t.TempDir()
	socket := filepath.Join(dir, "tmux.sock")
	tmuxCommand := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tmux, append([]string{"-S", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	_, err = tmuxCommand("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "exec cat")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })

	// Flexible adapter: skips any recognized flag/value pair regardless of
	// order, then drops the container ID and the literal "tmux" before
	// forwarding to the real binary. Covers both
	// `exec --user <user> <container> tmux has-session -t scion`
	// (waitForTmuxSession) and
	// `exec -it -e TERM=... --user <user> <container> tmux attach-session -t scion`
	// (startDockerExec).
	adapter := filepath.Join(dir, "runtime-exec")
	require.NoError(t, os.WriteFile(adapter, []byte(`#!/bin/sh
set -eu
shift
while [ $# -gt 0 ]; do
	case "$1" in
		-it|-i|-t) shift ;;
		-e) shift 2 ;;
		--user) shift 2 ;;
		*) break ;;
	esac
done
shift
shift
exec "$TW_DIRECT_ATTACH_TMUX" -S "$TW_DIRECT_ATTACH_SOCKET" "$@"
`), 0700))
	t.Setenv("TW_DIRECT_ATTACH_TMUX", tmux)
	t.Setenv("TW_DIRECT_ATTACH_SOCKET", socket)

	const slug = "direct-attach-fixture"
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{{
		ContainerID: "direct-attach-container",
		Name:        slug,
		Labels:      map[string]string{"scion.name": slug},
	}}
	rt := &runtime.MockRuntime{NameFunc: func() string { return adapter }}
	srv := New(DefaultServerConfig(), mgr, rt)

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handleAgentAttach))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/api/v1/agents/" + slug + "/attach"
	dialFn := func() (*websocket.Conn, *http.Response, error) {
		return websocket.DefaultDialer.Dial(wsURL, nil)
	}
	return dialFn, tmuxCommand
}

// TestHandleAgentAttach_NormalEndSendsClassifiedCloseFrame covers the half of
// direct-attach's close behavior where a normal attach end sends a close
// frame carrying classifyAttachEnd's code, not a bare conn.Close() (which a
// client can only ever observe as an abnormal 1006 closure).
func TestHandleAgentAttach_NormalEndSendsClassifiedCloseFrame(t *testing.T) {
	dial, tmuxCmd := newDirectAttachFixture(t)

	conn, _, err := dial()
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	require.Eventually(t, func() bool {
		out, err := tmuxCmd("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
		return err == nil && out == "80x24"
	}, 5*time.Second, 20*time.Millisecond, "one real tmux client should attach")

	// C-b d: the tmux detach keystroke, delivered exactly as a real client
	// keypress would arrive.
	require.NoError(t, conn.WriteJSON(wsprotocol.NewPTYDataMessage([]byte{2, 'd'})))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	var closeErr *websocket.CloseError
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			require.True(t, errors.As(err, &closeErr), "connection ended without a close frame: %v", err)
			break
		}
	}
	require.Equal(t, wsprotocol.ClosePTYNormal, closeErr.Code)
}

// TestHandleAgentAttach_IdlePeerClosedWithinDeadline covers the other half of
// direct-attach's close behavior: an idle client that stops answering pings
// is detected and disconnected within the deadline, via a real close frame
// rather than hanging or a bare TCP drop.
//
// directAttachKeepaliveConfig is temporarily overridden with short intervals
// so this test runs in a few seconds rather than needing to wait out the
// real 60s pong-wait deadline; production callers always use the
// package-level default (30s ping / 60s pong wait). PongWait must still
// comfortably exceed waitForTmuxSession's fixed ~500ms first-poll delay, or
// the keepalive deadline would expire before the attach exec even starts.
func TestHandleAgentAttach_IdlePeerClosedWithinDeadline(t *testing.T) {
	orig := directAttachKeepaliveConfig
	directAttachKeepaliveConfig = wsprotocol.ConnectionConfig{
		PingInterval: 200 * time.Millisecond,
		PongWait:     2 * time.Second,
		WriteWait:    500 * time.Millisecond,
	}
	t.Cleanup(func() { directAttachKeepaliveConfig = orig })

	dial, tmuxCmd := newDirectAttachFixture(t)

	conn, _, err := dial()
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// gorilla answers a ping with a pong automatically during any read call,
	// using its default ping handler — that would make this client keep the
	// server's keepalive alive the moment it starts reading again below to
	// look for the close frame, defeating the whole point of this test.
	// Overriding the ping handler with a no-op (never sending a pong) is
	// what actually makes this client "idle" from the server's perspective.
	conn.SetPingHandler(func(appData string) error { return nil })

	require.Eventually(t, func() bool {
		out, err := tmuxCmd("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
		return err == nil && out == "80x24"
	}, 5*time.Second, 20*time.Millisecond, "one real tmux client should attach")

	// Bounded to PongWait plus a few seconds of teardown headroom (the
	// broker's own exec-reap grace periods and the classifier's post-exit
	// probe), tracking the deadline this test actually exercises rather
	// than an arbitrary large timeout: a regression that doubled the
	// effective deadline would still be caught. Loop past any data frames
	// already queued (e.g. the initial active-window message) to reach the
	// close; incoming pings are silently dropped by the handler above,
	// never answered.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(directAttachKeepaliveConfig.PongWait+5*time.Second)))
	var closeErr *websocket.CloseError
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			require.True(t, errors.As(err, &closeErr), "expected a real close frame, not a bare TCP drop or hang: %v", err)
			break
		}
	}
	// The session is still alive when the transport is torn down (a killed
	// exec/read, not a session end), which classifyAttachEnd reports as
	// ClosePTYUpstreamUnavailable — never the reserved, local-only
	// CloseAbnormalClosure (1006) a bare TCP drop would produce.
	require.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, closeErr.Code)
}
