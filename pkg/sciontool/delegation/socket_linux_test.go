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

//go:build linux

package delegation

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// socketOwner returns the socket's mode bits and owner.
func socketOwner(t *testing.T, path string) (os.FileMode, uint32, uint32) {
	t.Helper()
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	require.NotZero(t, fi.Mode()&os.ModeSocket, "%s is not a socket", path)
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return fi.Mode().Perm(), st.Uid, st.Gid
}

// Flow 31 (.design/agent-delegation.md §17.2): the helper socket is mode
// 0600 and owned by the agent user.
func TestFlow31_SocketModeAndOwner(t *testing.T) {
	hs := newHarness(t)
	perm, uid, _ := socketOwner(t, hs.sock)
	assert.Equal(t, os.FileMode(0o600), perm)
	assert.Equal(t, uint32(os.Geteuid()), uid, "UID <= 0 leaves the socket to sciontool's user")

	if os.Geteuid() == 0 {
		// Running as root, as sciontool init does: the socket belongs to
		// the agent user, and root may still use it.
		agent := newHarness(t, func(_ *fakeHub, cfg *Config) { cfg.UID, cfg.GID = 65534, 65534 })
		perm, uid, gid := socketOwner(t, agent.sock)
		assert.Equal(t, os.FileMode(0o600), perm)
		assert.Equal(t, uint32(65534), uid)
		assert.Equal(t, uint32(65534), gid)
		assert.Equal(t, http.StatusOK, agent.get(t, "grant-1").status)
	}
}

func TestSocket_RefusesPeerOfAnotherUser(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "s", SocketName)
	requireDialablePath(t, path)
	ln, err := listenSocket(socketConfig{path: path})
	require.NoError(t, err)
	pl, ok := ln.(*peerListener)
	require.True(t, ok)
	// No peer user is allowed, so this process counts as another user.
	pl.allowed = map[uint32]bool{}
	var served atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served.Store(true) })}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	hc := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}
	defer hc.CloseIdleConnections()
	// The connection itself succeeds (the path is dialable) and is then
	// closed by the listener without a byte, so the refusal is the peer
	// check and not a failed dial.
	raw, err := net.Dial("unix", path)
	require.NoError(t, err)
	require.NoError(t, raw.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = raw.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "the listener closes a connection from a user that is not allowed")
	_ = raw.Close()
	resp, err := hc.Get("http://delegation/api/v1/agents")
	if err == nil {
		_ = resp.Body.Close()
	}
	assert.Error(t, err, "a connection from a user that is not allowed is closed")
	assert.False(t, served.Load())

	// This process's own user is allowed by default.
	assert.True(t, allowedPeers(0)[uint32(os.Geteuid())])
	assert.True(t, allowedPeers(4242)[4242])
	assert.Len(t, allowedPeers(-1), 1)
}

func TestSocket_ReplacesStaleAndRemovesOnClose(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "s", SocketName)
	requireDialablePath(t, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	stale, err := net.Listen("unix", path)
	require.NoError(t, err)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())
	_, err = os.Lstat(path)
	require.NoError(t, err, "the stale socket is left behind")

	h := New(Config{Client: newFakeClientForSocketTests(t), SocketPath: path})
	require.NoError(t, h.Start())
	perm, _, _ := socketOwner(t, path)
	assert.Equal(t, os.FileMode(0o600), perm)

	require.NoError(t, h.Close())
	_, err = os.Lstat(path)
	assert.True(t, os.IsNotExist(err), "Close removes the socket")
}

func TestSocket_RefusesNonSocketAndSymlinkedDirectory(t *testing.T) {
	t.Run("regular file at the socket path", func(t *testing.T) {
		path := filepath.Join(shortTempDir(t), SocketName)
		require.NoError(t, os.WriteFile(path, []byte("keep"), 0o600))
		h := New(Config{Client: newFakeClientForSocketTests(t), SocketPath: path})
		require.Error(t, h.Start())
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "keep", string(data), "the file is left alone")
	})

	t.Run("symlinked socket directory", func(t *testing.T) {
		base := shortTempDir(t)
		realDir := filepath.Join(base, "real")
		require.NoError(t, os.Mkdir(realDir, 0o700))
		link := filepath.Join(base, "link")
		require.NoError(t, os.Symlink(realDir, link))
		h := New(Config{Client: newFakeClientForSocketTests(t), SocketPath: filepath.Join(link, SocketName)})
		require.Error(t, h.Start())
		_, err := os.Lstat(filepath.Join(realDir, SocketName))
		assert.True(t, os.IsNotExist(err), "nothing is created through the symlink")
	})
}

// newFakeClientForSocketTests is a hub client for tests that exercise only
// the socket; it is never asked to reach a hub.
func newFakeClientForSocketTests(t *testing.T) HubClient {
	t.Helper()
	return hub.NewClientWithConfig("http://127.0.0.1:1", "agent-jwt", testAgentID)
}
