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

//go:build !no_sqlite

package attache2e_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

const (
	devToken = "scion_dev_attache2e_test_0123456789abcdef"

	agentSlug   = "attach-e2e"
	containerID = "attach-e2e-container"

	// requireTmuxEnv turns the missing-tmux skip into a failure, so a CI
	// job that installs tmux cannot pass by skipping.
	requireTmuxEnv = "SCION_TEST_REQUIRE_TMUX"

	// firstDataTimeout bounds the wait for the first terminal output after
	// the WebSocket upgrade. It is well under the CLI client's own 30s
	// initial-data deadline, so a chain that only just meets that deadline
	// still fails here.
	firstDataTimeout = 10 * time.Second
	stepTimeout      = 10 * time.Second
)

// dbSeq keeps each run's in-memory database unique under -count.
var dbSeq atomic.Int64

// fixture is an in-process hub on loopback, a standalone Runtime Broker
// registered with it and connected over its control channel (through a
// loopback proxy the test can cut), and one agent whose runtime is a real
// tmux server reached through a fake runtime command.
type fixture struct {
	hubURL   string
	agentID  string
	brokerID string
	ccMgr    *hub.ControlChannelManager
	proxy    *cutProxy

	tmuxBin string
	// socket is the current subtest's tmux socket (newSession).
	socket string
	// phase is the container phase the fake runtime reports.
	phase atomic.Value
}

// TestAttachEndToEnd drives a real attach through every hop: WebSocket
// client -> hub PTY handler -> hub control-channel manager -> Runtime Broker
// control-channel client -> runtime exec (tmux attach-session).
// Close codes come from pkg/wsprotocol/pty_close.go.
func TestAttachEndToEnd(t *testing.T) {
	f := newFixture(t)

	t.Run("RoundTrip", func(t *testing.T) {
		f.newSession(t)
		conn := f.attach(t, 80, 24)
		defer closeQuietly(conn)

		marker := "e2e-" + uuid.NewString()[:8]
		require.NoError(t, conn.WriteJSON(wsprotocol.NewPTYDataMessage([]byte(marker+"\r"))))
		readUntil(t, conn, stepTimeout, "the typed text echoed back to the client", func(out string) bool {
			return strings.Contains(out, marker)
		})
		// The input reached the program: the pane shows the tty echo and
		// cat's copy of the line.
		require.Eventually(t, func() bool {
			out, err := f.tmux("capture-pane", "-p", "-t", "scion")
			return err == nil && strings.Count(out, marker) >= 2
		}, stepTimeout, 20*time.Millisecond, "cat should print the line it read")
	})

	t.Run("Resize", func(t *testing.T) {
		f.newSession(t)
		conn := f.attach(t, 80, 24)
		defer closeQuietly(conn)

		require.NoError(t, conn.WriteJSON(wsprotocol.NewPTYResizeMessage(100, 30)))
		f.requireClientSize(t, "100x30")
		require.NoError(t, conn.WriteJSON(wsprotocol.NewPTYResizeMessage(120, 40)))
		f.requireClientSize(t, "120x40")
	})

	t.Run("DetachByClientClose", func(t *testing.T) {
		f.newSession(t)
		conn := f.attach(t, 80, 24)
		defer closeQuietly(conn)

		require.NoError(t, conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
		ce := readClose(t, conn, stepTimeout)
		require.Equal(t, wsprotocol.ClosePTYNormal, ce.Code)

		// The hub closed the broker stream, which ended the tmux client;
		// the agent's session itself is untouched.
		require.Eventually(t, func() bool {
			out, err := f.tmux("list-clients", "-t", "scion")
			return err == nil && out == ""
		}, stepTimeout, 20*time.Millisecond, "the tmux client should be gone after the detach")
		_, err := f.tmux("has-session", "-t", "scion")
		require.NoError(t, err, "a detach must not end the agent's session")
	})

	t.Run("DetachByTmuxKey", func(t *testing.T) {
		f.newSession(t)
		conn := f.attach(t, 80, 24)
		defer closeQuietly(conn)

		// C-b d, the tmux detach keystroke, sent as terminal input.
		require.NoError(t, conn.WriteJSON(wsprotocol.NewPTYDataMessage([]byte{2, 'd'})))
		ce := readClose(t, conn, stepTimeout)
		require.Equal(t, wsprotocol.ClosePTYNormal, ce.Code)
		require.Empty(t, ce.Text)
		require.Equal(t, wsprotocol.DispositionDetached, wsprotocol.ClassifyPTYClose(ce.Code))
		_, err := f.tmux("has-session", "-t", "scion")
		require.NoError(t, err, "a detach must not end the agent's session")
	})

	t.Run("AgentStoppedDuringSession", func(t *testing.T) {
		f.newSession(t)
		conn := f.attach(t, 80, 24)
		defer closeQuietly(conn)

		// The agent stops: its container reports stopped and its tmux
		// session ends. The container still resolves, so the broker's
		// end-of-attach classifier reports the session as ended.
		f.setPhase(state.PhaseStopped)
		_, err := f.tmux("kill-session", "-t", "scion")
		require.NoError(t, err)
		ce := readClose(t, conn, stepTimeout)
		require.Equal(t, wsprotocol.ClosePTYSessionGone, ce.Code)
		require.Equal(t, wsprotocol.CloseReasonSessionEnded, ce.Text)
		require.Equal(t, wsprotocol.DispositionTerminal, wsprotocol.ClassifyPTYClose(ce.Code))
	})

	t.Run("AgentAlreadyStopped", func(t *testing.T) {
		f.newSession(t)
		f.setPhase(state.PhaseStopped)
		conn := f.dialOK(t, 80, 24)
		defer closeQuietly(conn)

		// The broker refuses before waiting for tmux.
		ce := readClose(t, conn, stepTimeout)
		require.Equal(t, wsprotocol.ClosePTYSessionGone, ce.Code)
		require.Equal(t, wsprotocol.CloseReasonAgentStopped, ce.Text)
		require.Equal(t, wsprotocol.DispositionTerminal, wsprotocol.ClassifyPTYClose(ce.Code))
		out, err := f.tmux("list-clients", "-t", "scion")
		require.NoError(t, err)
		require.Empty(t, out, "no tmux client may attach to a stopped agent")
	})

	// Last: it drops the broker's control channel.
	t.Run("BrokerDrop", func(t *testing.T) {
		f.newSession(t)
		conn := f.attach(t, 80, 24)
		defer closeQuietly(conn)
		before := f.ccMgr.GetConnection(f.brokerID)
		require.NotNil(t, before)

		require.Positive(t, f.proxy.cut(), "the broker's control channel should run through the proxy")
		ce := readClose(t, conn, stepTimeout)
		require.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, ce.Code)
		require.Equal(t, wsprotocol.CloseReasonBrokerDisconnected, ce.Text)
		require.Equal(t, wsprotocol.DispositionRetry, wsprotocol.ClassifyPTYClose(ce.Code))
		require.Equal(t, wsprotocol.ReconnectPrompt, wsprotocol.PTYReconnectTiming(ce.Code))

		// The broker reconnects on its own. The hub closes the old
		// session's streams before it unregisters that session, so wait for
		// the new session to be registered before attaching again.
		require.Eventually(t, func() bool {
			c := f.ccMgr.GetConnection(f.brokerID)
			return c != nil && c != before
		}, 20*time.Second, 50*time.Millisecond, "the broker should reconnect its control channel")
		again := f.attach(t, 80, 24)
		defer closeQuietly(again)
	})
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		if os.Getenv(requireTmuxEnv) == "1" {
			t.Fatalf("tmux not found in PATH and %s=1: %v", requireTmuxEnv, err)
		}
		t.Skip("requires tmux in PATH")
	}
	testutil.ScrubHubEnv(t)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TMUX", "")
	home := shortTempDir(t)
	t.Setenv("HOME", home)

	ctx := context.Background()
	f := &fixture{tmuxBin: tmuxBin}
	f.setPhase(state.PhaseRunning)

	// Hub: in-memory SQLite, dev auth, served on loopback.
	dbName := fmt.Sprintf("attache2e_%d", dbSeq.Add(1))
	client, err := entc.OpenSQLite("file:"+dbName+"?mode=memory&cache=shared", entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(ctx, client))
	s := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = s.Close() })

	cfg := hub.DefaultServerConfig()
	cfg.DevAuthToken = devToken
	cfg.DisableCloudLogQuery = true
	cfg.DevUserConfig = hub.DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	srv, err := hub.New(cfg, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ops := hub.NewOperationalSettings(s, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	f.ccMgr = srv.GetControlChannelManager()

	hubTS := httptest.NewServer(srv.Handler())
	t.Cleanup(hubTS.Close)
	f.hubURL = hubTS.URL
	f.proxy = newCutProxy(t, hubTS.Listener.Addr().String())

	// Registration: the project, the broker and the broker's credentials,
	// and the agent placed on that broker.
	now := time.Now()
	projectID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "attach-e2e", Slug: "attach-e2e",
		OwnerID: hub.DevUserID, CreatedBy: hub.DevUserID, Created: now, Updated: now,
	}))
	f.brokerID = uuid.NewString()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: f.brokerID, Name: "attach-e2e-broker", Slug: "attach-e2e-broker",
		Status: store.BrokerStatusOnline, Created: now, Updated: now,
	}))
	brokerKey := make([]byte, 32)
	_, err = rand.Read(brokerKey)
	require.NoError(t, err)
	require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID: f.brokerID, SecretKey: brokerKey, Status: store.BrokerSecretStatusActive,
	}))
	f.agentID = uuid.NewString()
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: f.agentID, Slug: agentSlug, Name: agentSlug, ProjectID: projectID,
		Phase: string(state.PhaseRunning), Runtime: "docker", RuntimeBrokerID: f.brokerID,
		OwnerID: hub.DevUserID, CreatedBy: hub.DevUserID, Created: now, Updated: now,
	}))

	// Runtime: everything is real except the runtime command, a script
	// that runs the broker's "exec ... tmux ..." against the subtest's own
	// tmux server, and the container listing.
	adapter := filepath.Join(home, "runtime-exec")
	require.NoError(t, os.WriteFile(adapter, []byte(runtimeExecScript), 0o700))
	t.Setenv("ATTACH_E2E_TMUX", tmuxBin)
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return adapter },
		ListFunc: func(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			info := api.AgentInfo{
				ContainerID: containerID,
				Name:        agentSlug,
				ProjectID:   projectID,
				Phase:       f.phase.Load().(string),
				Labels: map[string]string{
					"scion.name":               agentSlug,
					projectkeys.LabelProjectID: projectID,
				},
			}
			for k, v := range filter {
				if info.Labels[k] != v {
					return []api.AgentInfo{}, nil
				}
			}
			return []api.AgentInfo{info}, nil
		},
	}

	// Broker: standalone, registered with the hub above, reaching it
	// through the proxy.
	bcfg := runtimebroker.DefaultServerConfig()
	bcfg.Host = "127.0.0.1"
	bcfg.BrokerID = f.brokerID
	bcfg.BrokerName = "attach-e2e-broker"
	bcfg.StateDir = filepath.Join(home, "broker-state")
	bcfg.HubEnabled = true
	bcfg.HubEndpoint = f.proxy.URL()
	bcfg.ControlChannelEnabled = true
	bcfg.InMemoryCredentials = &brokercredentials.BrokerCredentials{
		BrokerID:    f.brokerID,
		SecretKey:   base64.StdEncoding.EncodeToString(brokerKey),
		HubEndpoint: f.proxy.URL(),
	}
	broker := runtimebroker.New(bcfg, agent.NewManager(rt), rt, config.RuntimeBuildRecord{})
	bctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, broker.StartServices(bctx))
	t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = broker.Shutdown(sctx)
	})
	require.Eventually(t, func() bool { return f.ccMgr.IsConnected(f.brokerID) },
		15*time.Second, 50*time.Millisecond, "the broker should connect its control channel to the hub")
	return f
}

// runtimeExecScript stands in for the container runtime CLI. It accepts
// the broker's "exec [-it] [-e K=V] [--detach-keys=..] --user U <container>
// tmux <args>" and runs "tmux <args>" on the current subtest's server.
const runtimeExecScript = `#!/bin/sh
set -eu
[ "${1:-}" = exec ] || exit 64
shift
while [ $# -gt 0 ]; do
	case "$1" in
		-it|-i|-t) shift ;;
		-e) shift 2 ;;
		--detach-keys=*) shift ;;
		--user) shift 2 ;;
		*) break ;;
	esac
done
[ $# -ge 2 ] || exit 64
shift
[ "$1" = tmux ] || exit 64
shift
exec "$ATTACH_E2E_TMUX" -S "$ATTACH_E2E_SOCKET" "$@"
`

// newSession gives the subtest its own tmux server running the agent
// session "scion" (a cat process) plus a spare session that keeps the
// server alive when "scion" ends, and marks the container running.
func (f *fixture) newSession(t *testing.T) {
	t.Helper()
	f.socket = filepath.Join(shortTempDir(t), "tmux.sock")
	// Unix socket paths are limited to about 107 bytes; fail clearly
	// rather than with a tmux connect error.
	require.Less(t, len(f.socket), 100, "tmux socket path too long; use a shorter TMPDIR: %s", f.socket)
	// Registered before the first new-session, so a failed setup still
	// stops the tmux server.
	socket := f.socket
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, f.tmuxBin, "-S", socket, "kill-server").Run()
	})
	t.Setenv("ATTACH_E2E_SOCKET", f.socket)
	f.setPhase(state.PhaseRunning)
	out, err := f.tmux("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "exec cat")
	require.NoError(t, err, "new-session: %s", out)
	out, err = f.tmux("new-session", "-d", "-s", "spare", "exec cat")
	require.NoError(t, err, "new-session spare: %s", out)
}

func (f *fixture) setPhase(p state.Phase) { f.phase.Store(string(p)) }

func (f *fixture) tmux(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, f.tmuxBin, append([]string{"-S", f.socket}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// requireClientSize waits until the one tmux client attached to "scion" has
// the given size.
func (f *fixture) requireClientSize(t *testing.T, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		out, err := f.tmux("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
		return err == nil && out == want
	}, stepTimeout, 20*time.Millisecond, "tmux client size: want %s", want)
}

func (f *fixture) dial(cols, rows int) (*websocket.Conn, *http.Response, error) {
	u := fmt.Sprintf("ws%s/api/v1/agents/%s/pty?cols=%d&rows=%d", strings.TrimPrefix(f.hubURL, "http"), f.agentID, cols, rows)
	return websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + devToken}})
}

// dialOK opens the attach WebSocket and requires the upgrade to succeed.
func (f *fixture) dialOK(t *testing.T, cols, rows int) *websocket.Conn {
	t.Helper()
	conn, resp, err := f.dial(cols, rows)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			if resp.Body != nil {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				t.Fatalf("attach upgrade failed: %v (HTTP %d: %s)", err, status, body)
			}
		}
		t.Fatalf("attach upgrade failed: %v (HTTP %d)", err, status)
	}
	return conn
}

// attach opens the attach WebSocket, reads the first terminal output, and
// waits until the tmux client is attached at the requested size, so no input
// is sent before the tmux client is attached.
func (f *fixture) attach(t *testing.T, cols, rows int) *websocket.Conn {
	t.Helper()
	conn := f.dialOK(t, cols, rows)
	readUntil(t, conn, firstDataTimeout, "the first terminal output", func(out string) bool { return out != "" })
	f.requireClientSize(t, fmt.Sprintf("%dx%d", cols, rows))
	return conn
}

// readUntil reads data frames, accumulating their bytes, until done reports
// true. It fails, showing the output so far, on a timeout or a close.
func readUntil(t *testing.T, conn *websocket.Conn, timeout time.Duration, what string, done func(out string) bool) string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(timeout)))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	var out strings.Builder
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %s: %v; output so far: %q", what, err, out.String())
		}
		var m wsprotocol.PTYDataMessage
		if json.Unmarshal(msg, &m) != nil || m.Type != wsprotocol.TypeData {
			continue
		}
		out.Write(m.Data)
		if done(out.String()) {
			return out.String()
		}
	}
}

// readClose reads until the connection closes and returns the close frame
// the hub sent.
func readClose(t *testing.T, conn *websocket.Conn, timeout time.Duration) *websocket.CloseError {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(timeout)))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("expected a close frame, got %v", err)
		}
		return ce
	}
}

func closeQuietly(conn *websocket.Conn) {
	if conn != nil {
		_ = conn.Close()
	}
}

// shortTempDir is a fresh directory directly under the system temp dir.
// t.TempDir paths can be too long for a tmux socket.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ae")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// cutProxy is a loopback TCP proxy to the hub. cut closes every proxied
// connection at once, which the hub sees as the broker's control channel
// dropping; new connections are still accepted afterwards.
type cutProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
	wg     sync.WaitGroup
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &cutProxy{ln: ln, target: target}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(p.close)
	return p
}

func (p *cutProxy) URL() string { return "http://" + p.ln.Addr().String() }

func (p *cutProxy) serve() {
	defer p.wg.Done()
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = down.Close()
			continue
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = down.Close()
			_ = up.Close()
			continue
		}
		p.conns = append(p.conns, down, up)
		p.mu.Unlock()
		p.wg.Add(2)
		go p.pipe(up, down)
		go p.pipe(down, up)
	}
}

func (p *cutProxy) pipe(dst, src net.Conn) {
	defer p.wg.Done()
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}

// cut closes every connection proxied so far and returns how many
// connection pairs it closed.
func (p *cutProxy) cut() int {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return len(conns) / 2
}

func (p *cutProxy) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	_ = p.ln.Close()
	p.cut()
	p.wg.Wait()
}
