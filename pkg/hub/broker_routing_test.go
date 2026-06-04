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
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
)

// fakeHTTPClient records calls to MessageAgent so we can verify the HTTP
// fallback path. Other methods are stubs.
type fakeHTTPClient struct {
	messageAgentCalled bool
}

func (f *fakeHTTPClient) MessageAgent(context.Context, string, string, string, string, string, bool, *messages.StructuredMessage) error {
	f.messageAgentCalled = true
	return nil
}

// Stub implementations for the RuntimeBrokerClient interface — only MessageAgent matters.
func (f *fakeHTTPClient) CreateAgent(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	return nil, nil
}
func (f *fakeHTTPClient) StartAgent(context.Context, string, string, string, string, string, string, string, string, map[string]string, []ResolvedSecret, *api.ScionConfig, []api.SharedDir, bool) (*RemoteAgentResponse, error) {
	return nil, nil
}
func (f *fakeHTTPClient) StopAgent(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeHTTPClient) RestartAgent(context.Context, string, string, string, string, map[string]string) error {
	return nil
}
func (f *fakeHTTPClient) DeleteAgent(context.Context, string, string, string, string, bool, bool, bool, time.Time) error {
	return nil
}
func (f *fakeHTTPClient) CheckAgentPrompt(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}
func (f *fakeHTTPClient) CreateAgentWithGather(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	return nil, nil, nil
}
func (f *fakeHTTPClient) FinalizeEnv(context.Context, string, string, string, map[string]string) (*RemoteAgentResponse, error) {
	return nil, nil
}
func (f *fakeHTTPClient) GetAgentLogs(context.Context, string, string, string, string, int) (string, error) {
	return "", nil
}
func (f *fakeHTTPClient) ExecAgent(context.Context, string, string, string, string, []string, int) (string, int, error) {
	return "", 0, nil
}
func (f *fakeHTTPClient) CleanupProject(context.Context, string, string, string) error {
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
