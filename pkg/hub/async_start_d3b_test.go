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

// Design wave 3 phase D3b (ptone/scion#4669), store-free: the all-live-hubs
// check, the agent ID on the start URL and the lifecycle start body.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubInstanceListStore serves ListHubInstances from rows (or err), counts
// the reads, and blocks each read until release is closed when set.
type hubInstanceListStore struct {
	store.Store
	mu      sync.Mutex
	rows    []store.HubInstance
	now     time.Time
	err     error
	reads   atomic.Int32
	release chan struct{}
}

func (s *hubInstanceListStore) ListHubInstances(_ context.Context, _ time.Duration) ([]store.HubInstance, time.Time, error) {
	s.reads.Add(1)
	s.mu.Lock()
	release := s.release
	s.mu.Unlock()
	if release != nil {
		<-release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.HubInstance(nil), s.rows...), s.now, s.err
}

func (s *hubInstanceListStore) set(rows []store.HubInstance, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows, s.err = rows, err
}

// hubRow is a registry row last written age before now, with features.
func hubRow(t *testing.T, label string, now time.Time, age time.Duration, stopped bool, features ...string) store.HubInstance {
	t.Helper()
	stats, err := json.Marshal(api.HubInstanceStats{Features: features})
	require.NoError(t, err)
	row := store.HubInstance{ID: "id-" + label, Label: label, Version: "v-" + label, LastSeen: now.Add(-age), Stats: stats}
	if stopped {
		at := now.Add(-age)
		row.StoppedAt = &at
	}
	return row
}

// countingHandler counts Warn records whose message contains substr.
type countingHandler struct {
	mu     sync.Mutex
	warns  []string
	infos  []string
	substr string
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !strings.Contains(r.Message, h.substr) {
		return nil
	}
	if r.Level >= slog.LevelWarn {
		h.warns = append(h.warns, r.Message)
	} else {
		h.infos = append(h.infos, r.Message)
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.warns), len(h.infos)
}

// TestAsyncStartGate_AllLiveHubsHave pins design D-T13: every live row with
// async_start_v1 → true; one live row without it → false, logged once
// while the answer stays the same; a stale or stopped row without it is
// ignored; a row whose stats cannot be read, a list error and an empty
// registry → false.
func TestAsyncStartGate_AllLiveHubsHave(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := api.HubInstanceFeatureAsyncStartV1
	cases := []struct {
		name string
		rows []store.HubInstance
		err  error
		want bool
		why  string
	}{
		{"all live have it", []store.HubInstance{hubRow(t, "a", now, time.Second, false, f), hubRow(t, "b", now, 10*time.Second, false, "other", f)}, nil, true, ""},
		{"one live lacks it", []store.HubInstance{hubRow(t, "a", now, time.Second, false, f), hubRow(t, "old", now, 2*time.Second, false)}, nil, false, "hub instance old (version v-old) lacks async_start_v1"},
		{"stale row ignored", []store.HubInstance{hubRow(t, "a", now, time.Second, false, f), hubRow(t, "gone", now, hubInstanceStaleAfter+time.Second, false)}, nil, true, ""},
		{"stopped row ignored", []store.HubInstance{hubRow(t, "a", now, time.Second, false, f), hubRow(t, "down", now, time.Second, true)}, nil, true, ""},
		{"unreadable stats", []store.HubInstance{{ID: "x", Label: "bad", Version: "v", LastSeen: now, Stats: json.RawMessage(`{"features":`)}}, nil, false, "hub instance bad (version v) has unreadable stats"},
		{"list error", nil, errors.New("db down"), false, "hub instance registry read failed: db down"},
		{"no live row", []store.HubInstance{hubRow(t, "gone", now, hubInstanceStaleAfter+time.Second, false, f)}, nil, false, "no live hub instance in the registry yet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &hubInstanceListStore{rows: c.rows, now: now, err: c.err}
			srv := &Server{store: st}
			got, why := srv.computeAsyncStartGate(context.Background())
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.why, why)
		})
	}

	// Logged once per change of the answer.
	h := &countingHandler{substr: "async start"}
	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "old", now, time.Second, false)}}
	srv := &Server{store: st, agentLifecycleLog: slog.New(h)}
	srv.refreshAsyncStartGate(context.Background())
	srv.refreshAsyncStartGate(context.Background())
	warns, infos := h.counts()
	assert.Equal(t, 1, warns, "the same false answer is logged once")
	assert.Equal(t, 0, infos)
	st.set([]store.HubInstance{hubRow(t, "old", now, time.Second, false, f)}, nil)
	srv.refreshAsyncStartGate(context.Background())
	srv.refreshAsyncStartGate(context.Background())
	warns, infos = h.counts()
	assert.Equal(t, 1, warns)
	assert.Equal(t, 1, infos, "turning on is logged once")
}

// TestAsyncStartGate_CachedAndRefreshedInBackground pins the cache: the
// gate reads false until its first refresh, a start never waits for a
// refresh (the read here blocks until released), the answer is reused for
// asyncStartGateTTL and refreshed after it, and a list error turns it
// false.
func TestAsyncStartGate_CachedAndRefreshedInBackground(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	var clockMu sync.Mutex
	prev := asyncStartGateNow
	asyncStartGateNow = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	t.Cleanup(func() { asyncStartGateNow = prev })
	advance := func(d time.Duration) { clockMu.Lock(); clock = clock.Add(d); clockMu.Unlock() }

	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}, release: make(chan struct{})}
	srv := &Server{store: st}

	done := make(chan bool)
	go func() { done <- srv.allLiveHubsHaveAsyncStart() }()
	select {
	case got := <-done:
		assert.False(t, got, "false before the first refresh")
	case <-time.After(5 * time.Second):
		t.Fatal("a start waited for the registry read")
	}
	assert.False(t, srv.allLiveHubsHaveAsyncStart(), "still false while the first refresh runs")
	close(st.release)
	require.Eventually(t, srv.allLiveHubsHaveAsyncStart, 5*time.Second, 5*time.Millisecond, "true once the refresh read the registry")
	assert.Equal(t, int32(1), st.reads.Load(), "one refresh at a time")

	// Within the TTL: cached, no read.
	st.set(nil, errors.New("db down"))
	advance(asyncStartGateTTL - time.Second)
	assert.True(t, srv.allLiveHubsHaveAsyncStart())
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(1), st.reads.Load(), "no read within the TTL")

	// After the TTL: the cached value is returned once more while a
	// refresh runs, then the list error turns it false.
	advance(2 * time.Second)
	assert.True(t, srv.allLiveHubsHaveAsyncStart(), "the cached value while the refresh runs")
	require.Eventually(t, func() bool { return !srv.allLiveHubsHaveAsyncStart() }, 5*time.Second, 5*time.Millisecond, "a list error turns it false")
	assert.Equal(t, int32(2), st.reads.Load())
}

// TestAsyncStartGate_ExpiredAnswerNotUsed pins that an answer
// older than asyncStartGateMaxAge (an idle gate) is not used. The start
// reads false while the refresh runs, and the refreshed answer is used once
// it lands.
func TestAsyncStartGate_ExpiredAnswerNotUsed(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	var clockMu sync.Mutex
	prev := asyncStartGateNow
	asyncStartGateNow = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	t.Cleanup(func() { asyncStartGateNow = prev })
	advance := func(d time.Duration) { clockMu.Lock(); clock = clock.Add(d); clockMu.Unlock() }

	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}}
	srv := &Server{store: st}
	assert.False(t, srv.allLiveHubsHaveAsyncStart())
	require.Eventually(t, srv.allLiveHubsHaveAsyncStart, 5*time.Second, 5*time.Millisecond)

	// Idle past the bound, with the next read held.
	release := make(chan struct{})
	st.mu.Lock()
	st.release = release
	st.mu.Unlock()
	advance(asyncStartGateMaxAge + time.Second)
	assert.False(t, srv.allLiveHubsHaveAsyncStart(), "an expired true is not used")
	assert.False(t, srv.allLiveHubsHaveAsyncStart(), "still false while the refresh runs")
	close(release)
	require.Eventually(t, srv.allLiveHubsHaveAsyncStart, 5*time.Second, 5*time.Millisecond, "the refreshed answer is used")
	assert.Equal(t, int32(2), st.reads.Load())
}

// TestAsyncStartAllowed_ExperimentFirst pins that the registry is not read
// while hub.async_agent_start is off (its registry default).
func TestAsyncStartAllowed_ExperimentFirst(t *testing.T) {
	st := &hubInstanceListStore{now: time.Now()}
	srv := &Server{store: st}
	assert.False(t, srv.asyncStartAllowed())
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(0), st.reads.Load(), "no registry read while the experiment is off")
}

// TestStartAgentURL_AgentID pins design R1: both transports send the hub's
// agent ID (api.AgentIDQueryParam) on the start URL when the dispatcher
// attached one, next to projectId and runId, and nothing otherwise.
func TestStartAgentURL_AgentID(t *testing.T) {
	ctx := withDispatchAgentID(context.Background(), "agent a&b")

	var mu sync.Mutex
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"created":false}`))
	}))
	defer server.Close()
	client := NewHTTPRuntimeBrokerClient()
	_, err := client.StartAgent(ctx, tid("broker-1"), server.URL, "a1", "p1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{RunID: "run-1"})
	require.NoError(t, err)
	_, err = client.StartAgent(context.Background(), tid("broker-1"), server.URL, "a1", "", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{})
	require.NoError(t, err)
	mu.Lock()
	require.Len(t, queries, 2)
	assert.Equal(t, "agent a&b", queries[0].Get(api.AgentIDQueryParam), "HTTP: agentId sent")
	assert.Equal(t, "p1", queries[0].Get("projectId"))
	assert.Equal(t, "run-1", queries[0].Get("runId"))
	assert.False(t, queries[1].Has(api.AgentIDQueryParam), "HTTP: no agentId without an agent ID")
	mu.Unlock()

	runOverHubTunnels(t, func(t *testing.T, tr hubTunnelTransport) {
		tunnel := &mockControlChannelTunnel{connected: true}
		cc := &ControlChannelBrokerClient{manager: tr.wrap(t, tunnel), signer: &mockBrokerSigner{}}
		_, err := cc.StartAgent(ctx, "broker-1", "unused", "a1", "", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{})
		require.NoError(t, err)
		require.NotNil(t, tunnel.lastRequest)
		assert.Equal(t, "/api/v1/agents/a1/start?"+api.AgentIDQueryParam+"=agent+a%26b", tunnel.lastRequest.Path, "control channel: agentId sent")

		_, err = cc.StartAgent(context.Background(), "broker-1", "unused", "a1", "p1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{})
		require.NoError(t, err)
		assert.Equal(t, "/api/v1/agents/a1/start?projectId=p1", tunnel.lastRequest.Path, "control channel: no agentId without an agent ID")
	})
}

// TestLifecycleStartHasInputs_IgnoresAcceptAsync pins that the asynchronous
// opt-in alone is not an input a launching agent ignores (no warning),
// while forceResume still is.
func TestLifecycleStartHasInputs_IgnoresAcceptAsync(t *testing.T) {
	req := func(body string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/start", bytes.NewBufferString(body))
	}
	assert.False(t, lifecycleStartHasInputs(req(`{"acceptAsyncLaunch":true}`)), "the opt-in alone is not an ignored input")
	assert.True(t, lifecycleStartHasInputs(req(`{"forceResume":true}`)))
	assert.True(t, lifecycleStartHasInputs(req(`{"forceResume":true,"acceptAsyncLaunch":true}`)))
	assert.False(t, lifecycleStartHasInputs(req(`{}`)))
}

// TestStartDispatchOptions_LaunchKind pins the kind default.
func TestStartDispatchOptions_LaunchKind(t *testing.T) {
	assert.Equal(t, store.LaunchKindStart, StartDispatchOptions{}.launchKind())
	assert.Equal(t, store.LaunchKindStart, StartDispatchOptions{Kind: store.LaunchKindStart}.launchKind())
	assert.Equal(t, store.LaunchKindRestart, StartDispatchOptions{Kind: store.LaunchKindRestart}.launchKind())
}

// TestStartDidNotHappen_DeleteWrapped is the startDidNotHappen unit row the
// P3.3-c owner asked for: an error that wraps store.ErrDeleteInProgress is
// never a start that did not happen, whatever else it wraps; the same
// refusal without the delete sentinel still is.
func TestStartDidNotHappen_DeleteWrapped(t *testing.T) {
	assert.False(t, startDidNotHappen(fmt.Errorf("%w: %w: %w", ErrLaunchInvalidPhase, store.ErrInvalidPhase, store.ErrDeleteInProgress)),
		"a delete-held launch refusal is the delete engine's")
	assert.True(t, startDidNotHappen(fmt.Errorf("%w: %w", ErrLaunchInvalidPhase, store.ErrInvalidPhase)),
		"a plain launch refusal did not happen, as on main")
}
