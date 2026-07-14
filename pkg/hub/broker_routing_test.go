//go:build !no_sqlite

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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// fakeHTTPClient records calls to MessageAgent so we can verify the HTTP
// fallback path. Other methods are stubs.
type fakeHTTPClient struct {
	messageAgentCalled bool
	startAgentCalled   bool
	stopAgentCalled    bool
	deleteAgentCalled  bool
}

func (f *fakeHTTPClient) MessageAgent(context.Context, string, string, string, string, string, bool, *messages.StructuredMessage) error {
	f.messageAgentCalled = true
	return nil
}

// Stub implementations for the RuntimeBrokerClient interface — only MessageAgent matters.
func (f *fakeHTTPClient) CreateAgent(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	return nil, nil
}
func (f *fakeHTTPClient) StartAgent(context.Context, string, string, string, string, string, string, string, string, map[string]string, []ResolvedSecret, *api.ScionConfig, []api.SharedDir, bool, bool) (*RemoteAgentResponse, error) {
	f.startAgentCalled = true
	return nil, nil
}
func (f *fakeHTTPClient) StopAgent(context.Context, string, string, string, string) error {
	f.stopAgentCalled = true
	return nil
}
func (f *fakeHTTPClient) RestartAgent(context.Context, string, string, string, string, map[string]string) error {
	return nil
}
func (f *fakeHTTPClient) ResetAuthAgent(context.Context, string, string, string, string, string) error {
	return nil
}
func (f *fakeHTTPClient) DeleteAgent(context.Context, string, string, string, string, bool, bool, bool, time.Time) error {
	f.deleteAgentCalled = true
	return nil
}
func (f *fakeHTTPClient) CheckAgentPrompt(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}
func (f *fakeHTTPClient) CreateAgentWithGather(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	return nil, nil, nil
}
func (f *fakeHTTPClient) GetAgentLogs(context.Context, string, string, string, string, int) (string, error) {
	return "", nil
}
func (f *fakeHTTPClient) ExecAgent(context.Context, string, string, string, string, []string, int) (string, int, error) {
	return "", 0, nil
}
func (f *fakeHTTPClient) CleanupProject(context.Context, string, string, string, string) error {
	return nil
}

func TestHybridBrokerClient_Route(t *testing.T) {
	ctx := context.Background()
	const localBroker = "broker-local"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	// Seed a live local socket for localBroker only.
	mgr.mu.Lock()
	mgr.connections[localBroker] = &BrokerConnection{brokerID: localBroker, sessionID: "s1"}
	mgr.mu.Unlock()

	c := NewHybridBrokerClient(mgr, nil, nil, false)

	cases := []struct {
		name     string
		brokerID string
		endpoint string
		affOwner string
		affAlive bool
		want     routeDecision
	}{
		{"local socket wins", localBroker, "", "", false, routeLocal},
		{"local wins even over alive affinity", localBroker, "http://x", "hubA", true, routeLocal},
		{"alive owner -> forward", "b1", "", "hubA", true, routeForward},
		{"alive owner -> forward (endpoint ignored)", "b1", "http://x", "hubA", true, routeForward},
		{"no owner, endpoint set -> http", "b2", "http://x", "", false, routeHTTP},
		{"stale owner, endpoint set -> http", "b3", "http://x", "hubA", false, routeHTTP},
		{"stale owner, no endpoint -> undeliverable", "b4", "", "hubA", false, routeUndeliverable},
		{"no owner, no endpoint -> undeliverable", "b5", "", "", false, routeUndeliverable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.SetAffinityLookup(func(context.Context, string) (string, bool) { return tc.affOwner, tc.affAlive })
			got := c.route(ctx, tc.brokerID, tc.endpoint)
			assert.Equal(t, tc.want, got, "route(%s, endpoint=%q, owner=%q alive=%v)", tc.brokerID, tc.endpoint, tc.affOwner, tc.affAlive)
		})
	}
}

func TestHybridBrokerClient_Route_NilAffinityIsSafe(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	c := NewHybridBrokerClient(mgr, nil, nil, false)
	// No affinity lookup set: a non-local broker with no endpoint is undeliverable.
	assert.Equal(t, routeUndeliverable, c.route(context.Background(), "b-none", ""))
	assert.Equal(t, routeHTTP, c.route(context.Background(), "b-ep", "http://x"))
}

func TestHybridBrokerClient_Route_StatelessBrokerIgnoresAffinity(t *testing.T) {
	ctx := context.Background()
	const brokerID = "cloudrun-logical-broker"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	mgr.mu.Lock()
	mgr.connections[brokerID] = &BrokerConnection{brokerID: brokerID, sessionID: "s1"}
	mgr.mu.Unlock()

	c := NewHybridBrokerClient(mgr, nil, nil, false)
	c.SetStatelessLocalBrokers([]string{brokerID})
	c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "other-replica", true })

	assert.Equal(t, routeHTTP, c.route(ctx, brokerID, "http://localhost:9800"))
	assert.Equal(t, routeUndeliverable, c.route(ctx, brokerID, ""))
}

func TestHybridBrokerClient_StatelessBrokerLifecycleUsesHTTP(t *testing.T) {
	ctx := context.Background()
	const brokerID = "cloudrun-logical-broker"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	httpClient := &fakeHTTPClient{}
	c := NewHybridBrokerClient(mgr, httpClient, nil, false)
	c.SetStatelessLocalBrokers([]string{brokerID})
	c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "other-replica", true })

	_, err := c.StartAgent(ctx, brokerID, "http://localhost:9800", "agent-1", "project-1", "", "", "", "", nil, nil, nil, nil, false, false)
	assert.NoError(t, err)
	assert.True(t, httpClient.startAgentCalled)

	err = c.StopAgent(ctx, brokerID, "http://localhost:9800", "agent-1", "project-1")
	assert.NoError(t, err)
	assert.True(t, httpClient.stopAgentCalled)

	err = c.DeleteAgent(ctx, brokerID, "http://localhost:9800", "agent-1", "project-1", false, false, false, time.Time{})
	assert.NoError(t, err)
	assert.True(t, httpClient.deleteAgentCalled)
}

func TestHybridBrokerClient_MessageAgent_RouteGate(t *testing.T) {
	const localBroker = "broker-local"
	const remoteBroker = "broker-remote"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	mgr.mu.Lock()
	mgr.connections[localBroker] = &BrokerConnection{brokerID: localBroker, sessionID: "s1"}
	mgr.mu.Unlock()

	httpClient := &fakeHTTPClient{}
	c := NewHybridBrokerClient(mgr, httpClient, nil, false)

	t.Run("routeLocal uses control channel (not deferred)", func(t *testing.T) {
		// Verify route() returns routeLocal for the locally connected broker.
		// We don't call MessageAgent directly because the stub BrokerConnection
		// doesn't have a real tunnel; the route decision is what matters.
		got := c.route(context.Background(), localBroker, "")
		assert.Equal(t, routeLocal, got, "should pick local tunnel for connected broker")
	})

	t.Run("routeHTTP delivers via HTTP client", func(t *testing.T) {
		httpClient.messageAgentCalled = false
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
		err := c.MessageAgent(context.Background(), remoteBroker, "http://endpoint", "a1", "p1", "hi", false, nil)
		assert.NoError(t, err)
		assert.True(t, httpClient.messageAgentCalled, "HTTP fallback should be used")
	})

	t.Run("routeForward returns ErrMessageDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "hubA", true })
		err := c.MessageAgent(context.Background(), remoteBroker, "", "a1", "p1", "hi", false, nil)
		assert.ErrorIs(t, err, ErrMessageDeferred)
	})

	t.Run("routeUndeliverable returns ErrMessageDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
		err := c.MessageAgent(context.Background(), remoteBroker, "", "a1", "p1", "hi", false, nil)
		assert.ErrorIs(t, err, ErrMessageDeferred)
	})
}

// retryMockDispatcher is a mock that returns ErrMessageDeferred a configurable
// number of times before succeeding (or returning a custom error).
type retryMockDispatcher struct {
	brokerMockDispatcher
	deferCount int32
	failErr    error
	calls      atomic.Int32
}

func (d *retryMockDispatcher) DispatchAgentMessage(_ context.Context, agent *store.Agent, msg string, urgent bool, structuredMsg *messages.StructuredMessage) error {
	n := d.calls.Add(1)
	if int32(n) <= atomic.LoadInt32(&d.deferCount) {
		return ErrMessageDeferred
	}
	if d.failErr != nil {
		return d.failErr
	}
	return nil
}

func TestDispatchWithBrokerRetry_ImmediateSuccess(t *testing.T) {
	d := &retryMockDispatcher{}
	agent := &store.Agent{ID: "a1", Slug: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := dispatchWithBrokerRetry(ctx, d, agent, "hello", false, nil)
	assert.NoError(t, err)
	assert.Equal(t, int32(1), d.calls.Load())
}

func TestDispatchWithBrokerRetry_RetryThenSuccess(t *testing.T) {
	d := &retryMockDispatcher{deferCount: 3}
	agent := &store.Agent{ID: "a1", Slug: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := dispatchWithBrokerRetry(ctx, d, agent, "hello", false, nil)
	assert.NoError(t, err)
	assert.Equal(t, int32(4), d.calls.Load())
}

func TestDispatchWithBrokerRetry_Timeout(t *testing.T) {
	d := &retryMockDispatcher{deferCount: 1000}
	agent := &store.Agent{ID: "a1", Slug: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := dispatchWithBrokerRetry(ctx, d, agent, "hello", false, nil)
	assert.ErrorIs(t, err, ErrBrokerTimeout)
}

func TestDispatchWithBrokerRetry_NonTransientError(t *testing.T) {
	nonTransient := errors.New("connection refused")
	d := &retryMockDispatcher{failErr: nonTransient}
	agent := &store.Agent{ID: "a1", Slug: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := dispatchWithBrokerRetry(ctx, d, agent, "hello", false, nil)
	assert.ErrorIs(t, err, nonTransient)
	assert.Equal(t, int32(1), d.calls.Load())
}
