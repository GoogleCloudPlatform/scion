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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3850: a synchronous create waits on the broker launch for up
// to syncDispatchTimeout, longer than the server-wide WriteTimeout. A launch
// that finishes between the two must get its real response, not a dropped
// connection (which a front proxy turns into an empty 502). The timeouts are
// scaled down: WriteTimeout 200ms, launch 600ms, dispatch wait 3s.

// slowCreateDispatcher succeeds the create after delay.
type slowCreateDispatcher struct {
	createAgentDispatcher
	delay time.Duration
}

func (d *slowCreateDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.createAgentDispatcher.DispatchAgentCreateWithGather(ctx, agent)
}

func TestCreateAgent_LaunchAfterWriteTimeoutWithinDispatchWait_Succeeds(t *testing.T) {
	const writeTimeout = 200 * time.Millisecond
	for _, gather := range []bool{false, true} {
		name := "plain"
		if gather {
			name = "gather-env"
		}
		t.Run(name, func(t *testing.T) {
			// The hub serves with a short WriteTimeout, configured and
			// applied by the HTTP server, as in production.
			runSlowCreateThroughListener(t, "slow-launch-"+name, gather, writeTimeout, writeTimeout)
		})
	}
}

// In combo mode the hub handler is served by the web listener, whose
// WriteTimeout (60s) is not the hub's configured one. The deadline must
// follow the listener actually serving the request: here the configured hub
// WriteTimeout is unbounded or longer than the budget, which alone would
// skip the extension, while the serving listener cuts at 200ms.
func TestCreateAgent_LaunchAfterServingListenerWriteTimeout_Succeeds(t *testing.T) {
	const listenerWriteTimeout = 200 * time.Millisecond
	for _, tc := range []struct {
		name       string
		configured time.Duration
	}{
		{"configured-unbounded", 0},
		{"configured-longer", time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runSlowCreateThroughListener(t, "combo-"+tc.name, false, tc.configured, listenerWriteTimeout)
		})
	}
}

// runSlowCreateThroughListener serves the create handler through a real
// http.Server with listenerWriteTimeout, configures the hub with
// configuredWriteTimeout, and checks that a create whose launch outlasts the
// listener's WriteTimeout (but not the dispatch wait) gets its response.
func runSlowCreateThroughListener(t *testing.T, agentName string, gather bool, configuredWriteTimeout, listenerWriteTimeout time.Duration) {
	t.Helper()
	const launch = 600 * time.Millisecond
	shortenSyncDispatchTimeout(t, 3*time.Second)
	require.Greater(t, launch, listenerWriteTimeout)
	require.Less(t, launch, syncDispatchTimeout)

	disp := &slowCreateDispatcher{delay: launch}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.config.WriteTimeout = configuredWriteTimeout
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.Config.WriteTimeout = listenerWriteTimeout
	hs.Start()
	t.Cleanup(hs.Close)

	body, err := json.Marshal(CreateAgentRequest{
		Name: agentName, ProjectID: project.ID, Task: "work", GatherEnv: gather,
	})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/agents", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	start := time.Now()
	resp, err := hs.Client().Do(req)
	require.NoError(t, err, "the response must not be dropped at the listener's WriteTimeout")
	defer func() { _ = resp.Body.Close() }()
	assert.GreaterOrEqual(t, time.Since(start), launch, "the launch must outlast the WriteTimeout")

	var got CreateAgentResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got), "the response body must arrive in full")
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotNil(t, got.Agent)
	assert.Equal(t, agentName, got.Agent.Name)

	_, err = s.GetAgent(context.Background(), got.Agent.ID)
	assert.NoError(t, err, "the agent row is kept")
}

// The create's write deadline covers the dispatch wait plus slack, and is
// never set when the server's WriteTimeout is unbounded or already longer.
func TestExtendWriteDeadlineForSyncDispatch(t *testing.T) {
	budget := syncDispatchWriteBudget()
	assert.Equal(t, syncDispatchTimeout+syncDispatchWriteSlack, budget)
	assert.Greater(t, budget, DefaultServerConfig().WriteTimeout, "must outlast the default WriteTimeout")

	withListener := func(wt time.Duration) context.Context {
		return context.WithValue(context.Background(), http.ServerContextKey, &http.Server{WriteTimeout: wt})
	}
	for _, tc := range []struct {
		name       string
		ctx        context.Context
		configured time.Duration
		want       bool
	}{
		{"default", context.Background(), DefaultServerConfig().WriteTimeout, true},
		{"unbounded", context.Background(), 0, false},
		{"already longer", context.Background(), budget + time.Minute, false},
		// The serving listener's WriteTimeout wins over the configured one.
		{"listener shorter than unbounded config", withListener(time.Minute), 0, true},
		{"listener shorter than longer config", withListener(time.Minute), budget + time.Minute, true},
		{"listener unbounded", withListener(0), DefaultServerConfig().WriteTimeout, false},
		{"listener already longer", withListener(budget + time.Minute), DefaultServerConfig().WriteTimeout, false},
		// context.WithoutCancel (detachLaunchFromClient) keeps the server.
		{"listener through detached ctx", detachLaunchFromClient(withListener(time.Minute)), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			start := time.Now()
			// Wrapped as the hub's request logger wraps it: the deadline
			// must reach the writer through Unwrap.
			extendWriteDeadlineForSyncDispatch(tc.ctx, &responseWriter{ResponseWriter: rec}, tc.configured)
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if !tc.want {
				assert.Empty(t, rec.deadlines)
				return
			}
			require.Len(t, rec.deadlines, 1)
			assert.False(t, rec.deadlines[0].Before(start.Add(budget)))
			assert.False(t, rec.deadlines[0].After(time.Now().Add(budget)))
		})
	}

	// A writer without deadline support is tolerated.
	assert.NotPanics(t, func() {
		extendWriteDeadlineForSyncDispatch(context.Background(), noDeadlineWriter{httptest.NewRecorder()}, time.Second)
	})
}

// noDeadlineWriter hides the recorder's optional interfaces.
type noDeadlineWriter struct{ w http.ResponseWriter }

func (n noDeadlineWriter) Header() http.Header         { return n.w.Header() }
func (n noDeadlineWriter) Write(b []byte) (int, error) { return n.w.Write(b) }
func (n noDeadlineWriter) WriteHeader(code int)        { n.w.WriteHeader(code) }
