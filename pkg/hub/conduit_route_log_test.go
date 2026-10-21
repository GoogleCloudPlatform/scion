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

package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
)

// Routing and ownership log fields (ptone/scion#5076): the stream open
// lines show whether a stream was routed to a session this node holds
// (route=local) or to another relay (route=remote), and which relay holds
// it (owner_instance); the owning relay logs each internal stream it opens.

// routeLogCapture collects the JSON log records written through
// slog.Default while it is installed. Writes and reads are serialized, so
// records logged from relay goroutines are safe to read.
type routeLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *routeLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// captureRouteLogs installs a debug-level JSON capture as slog.Default for
// the rest of the test. Install it before the fixture, so the relays'
// loggers write to it too.
func captureRouteLogs(t *testing.T) *routeLogCapture {
	t.Helper()
	c := &routeLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// raw returns everything captured so far.
func (c *routeLogCapture) raw() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// with returns the records whose msg is msg.
func (c *routeLogCapture) with(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(c.raw(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// waitWith waits until at least n records with msg are captured and
// returns them.
func (c *routeLogCapture) waitWith(t *testing.T, msg string, n int) []map[string]any {
	t.Helper()
	var recs []map[string]any
	require.Eventually(t, func() bool { recs = c.with(t, msg); return len(recs) >= n }, recheckWait, 10*time.Millisecond,
		"expected %d %q records", n, msg)
	return recs
}

// routeLogMessages are the lines that carry the routing fields.
var routeLogMessages = []string{"Conduit tunnel opened", "Conduit internal stream opened", "Conduit PTY stream opened", "PTY session ended", "Proxy request"}

// assertNoCredentials fails if any captured line carries the caller's
// bearer token, or a routing line has a credential-looking field.
func assertNoCredentials(t *testing.T, c *routeLogCapture, bearer string) {
	t.Helper()
	assert.NotContains(t, c.raw(), bearer, "no log line carries the caller's token")
	for _, msg := range routeLogMessages {
		for _, rec := range c.with(t, msg) {
			for k := range rec {
				lk := strings.ToLower(k)
				for _, bad := range []string{"token", "grant", "authorization", "secret", "credential"} {
					assert.NotContains(t, lk, bad, "%q carries field %q", msg, k)
				}
			}
		}
	}
}

// TestConduitRouteLog_TunnelLocal: a tunnel to a session this node's relay
// holds logs one "Conduit tunnel opened" line with route=local and this
// relay as owner_instance, however much data it carries; no internal
// stream is opened.
func TestConduitRouteLog_TunnelLocal(t *testing.T) {
	logs := captureRouteLogs(t)
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	recs := tf.agentSessions(t)
	require.Len(t, recs, 1)
	c := tf.ownerClient(t)

	st := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	<-tf.dials
	roundTrip(t, st, "hello")
	roundTrip(t, st, strings.Repeat("x", 300<<10)) // many frames
	require.NoError(t, st.Close())

	opened := logs.waitWith(t, "Conduit tunnel opened", 1)
	require.Len(t, opened, 1, "one line per open, not per frame")
	assert.Equal(t, conduitRouteLocal, opened[0]["route"])
	assert.Equal(t, recs[0].RelayInstanceID, opened[0]["owner_instance"])
	assert.Equal(t, recs[0].SessionID, opened[0]["target_session_id"])
	assert.Empty(t, logs.with(t, "Conduit internal stream opened"), "a local open uses no internal stream")
	assertNoCredentials(t, logs, tf.userToken)
}

// TestConduitRouteLog_TunnelCrossNode: a tunnel to a session another
// relay holds logs route=remote with that relay as owner_instance on the
// node that took the request, and the owning relay logs one "Conduit
// internal stream opened" line per stream, with its session, the stream
// kind and the calling peer.
func TestConduitRouteLog_TunnelCrossNode(t *testing.T) {
	logs := captureRouteLogs(t)
	tf := newTunnelFixture(t)
	hubB := tf.startPeerRelay(t, "hub-b")
	tf.startAgent(t, hubB, tf.launched)
	recs := tf.agentSessions(t)
	require.Len(t, recs, 1)
	require.Equal(t, "hub-b", recs[0].RelayInstanceID)
	c := tf.ownerClient(t)

	st := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	<-tf.dials
	roundTrip(t, st, strings.Repeat("y", 300<<10)) // many frames
	require.NoError(t, st.Close())

	opened := logs.waitWith(t, "Conduit tunnel opened", 1)
	require.Len(t, opened, 1)
	assert.Equal(t, conduitRouteRemote, opened[0]["route"])
	assert.Equal(t, "hub-b", opened[0]["owner_instance"])

	internal := logs.waitWith(t, "Conduit internal stream opened", 1)
	require.Len(t, internal, 1, "one line per internal stream, not per frame")
	assert.Equal(t, "hub-b", internal[0]["relay_instance_id"], "logged by the owning relay")
	assert.Equal(t, recs[0].SessionID, internal[0]["session_id"])
	assert.Equal(t, "STREAM_KIND_TCP", internal[0]["kind"])
	assert.Equal(t, "hub-a", internal[0]["peer"])

	pty := c.open(t, tf.launched.ID, grant.StreamKindPTY, nil)
	p := waitSpawn(t, tf.spawned)
	roundTrip(t, pty, "pty across nodes")
	require.NoError(t, pty.Close())
	waitClosedPTY(t, p)
	internal = logs.waitWith(t, "Conduit internal stream opened", 2)
	require.Len(t, internal, 2, "one more line for the second stream")
	assert.Equal(t, "STREAM_KIND_PTY", internal[1]["kind"])
	assertNoCredentials(t, logs, tf.userToken)
}

// TestConduitRouteLog_AgentPTY: a web PTY attach on the agent path logs
// one "Conduit PTY stream opened" line with the conduit session and the
// routing fields, and "PTY session ended" carries them too.
func TestConduitRouteLog_AgentPTY(t *testing.T) {
	logs := captureRouteLogs(t)
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)

	c, _, err := f.dialPTY(t, "")
	require.NoError(t, err)
	waitSpawn(t, f.spawned)
	for i := range 20 {
		echoRoundTrip(t, c, strings.Repeat("z", 64)+string(rune('a'+i)))
	}
	require.NoError(t, c.Close())

	opened := logs.waitWith(t, "Conduit PTY stream opened", 1)
	require.Len(t, opened, 1, "one line per attach, not per frame")
	assert.Equal(t, conduitRouteLocal, opened[0]["route"])
	assert.Equal(t, "hub-a", opened[0]["owner_instance"])
	assert.NotEmpty(t, opened[0]["conduit_session_id"])

	ended := logs.waitWith(t, "PTY session ended", 1)
	assert.Equal(t, conduitRouteLocal, ended[0]["route"])
	assert.Equal(t, "hub-a", ended[0]["owner_instance"])
	assertNoCredentials(t, logs, f.userToken)
}

// TestConduitRouteLog_ProxyRequestDebug: the port proxy's per-request line
// stays at debug level and carries the routing fields.
func TestConduitRouteLog_ProxyRequestDebug(t *testing.T) {
	logs := captureRouteLogs(t)
	f := newConduitProxyFixture(t, nil)
	f.startAgent(t)
	for range 2 {
		resp := f.get(t, "/route", nil)
		require.Equal(t, 200, resp.StatusCode)
	}

	recs := logs.waitWith(t, "Proxy request", 2)
	for _, rec := range recs {
		assert.Equal(t, "DEBUG", rec["level"])
		assert.Equal(t, "conduit", rec["transport"])
		assert.Equal(t, conduitRouteLocal, rec["route"])
		assert.Equal(t, "hub-a", rec["owner_instance"])
	}
	assert.Empty(t, logs.with(t, "Conduit tunnel opened"))
	assertNoCredentials(t, logs, f.userToken)
}
