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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =========================================================================
// Route-gating tests for StartAgent / StopAgent / RestartAgent
// =========================================================================

func TestHybridBrokerClient_StartAgent_RouteGate(t *testing.T) {
	const localBroker = "broker-local"
	const remoteBroker = "broker-remote"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	mgr.mu.Lock()
	mgr.connections[localBroker] = &BrokerConnection{brokerID: localBroker, sessionID: "s1"}
	mgr.mu.Unlock()

	httpClient := &fakeHTTPClient{}
	c := NewHybridBrokerClient(mgr, httpClient, nil, false)

	t.Run("routeLocal uses control channel (not deferred)", func(t *testing.T) {
		got := c.route(context.Background(), localBroker, "")
		assert.Equal(t, routeLocal, got)
	})

	t.Run("routeForward returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "hubA", true })
		_, err := c.StartAgent(context.Background(), remoteBroker, "", "a1", "p1", "", "", "", "", nil, nil, nil, nil, false)
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})

	t.Run("routeUndeliverable returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
		_, err := c.StartAgent(context.Background(), remoteBroker, "", "a1", "p1", "", "", "", "", nil, nil, nil, nil, false)
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})
}

func TestHybridBrokerClient_StopAgent_RouteGate(t *testing.T) {
	const remoteBroker = "broker-remote"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	c := NewHybridBrokerClient(mgr, &fakeHTTPClient{}, nil, false)

	t.Run("routeForward returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "hubA", true })
		err := c.StopAgent(context.Background(), remoteBroker, "", "a1", "p1")
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})

	t.Run("routeUndeliverable returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
		err := c.StopAgent(context.Background(), remoteBroker, "", "a1", "p1")
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})
}

func TestHybridBrokerClient_RestartAgent_RouteGate(t *testing.T) {
	const remoteBroker = "broker-remote"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	c := NewHybridBrokerClient(mgr, &fakeHTTPClient{}, nil, false)

	t.Run("routeForward returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "hubA", true })
		err := c.RestartAgent(context.Background(), remoteBroker, "", "a1", "p1", nil)
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})

	t.Run("routeUndeliverable returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
		err := c.RestartAgent(context.Background(), remoteBroker, "", "a1", "p1", nil)
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})
}

// =========================================================================
// Dispatch args round-trip (serialize -> deserialize lossless)
// =========================================================================

func TestStartDispatchArgs_RoundTrip(t *testing.T) {
	original := &StartDispatchArgs{
		Task:        "build the widget",
		ResolvedEnv: map[string]string{"API_KEY": "secret123", "SCION_AGENT_ID": "a1"},
		ResolvedSecrets: []ResolvedSecret{
			{Name: "gh-token", Type: "environment", Target: "GITHUB_TOKEN", Value: "ghp_xxx", Source: "project"},
		},
		SharedWorkspace: true,
		ProjectPath:     "/workspace",
		ProjectSlug:     "my-project",
		HarnessConfig:   "claude-code",
	}

	raw, err := MarshalDispatchArgs(original)
	require.NoError(t, err)
	require.NotEmpty(t, raw)

	got, err := UnmarshalStartArgs(raw)
	require.NoError(t, err)

	assert.Equal(t, original.Task, got.Task)
	assert.Equal(t, original.ResolvedEnv, got.ResolvedEnv)
	assert.Equal(t, len(original.ResolvedSecrets), len(got.ResolvedSecrets))
	assert.Equal(t, original.ResolvedSecrets[0].Name, got.ResolvedSecrets[0].Name)
	assert.Equal(t, original.ResolvedSecrets[0].Value, got.ResolvedSecrets[0].Value)
	assert.Equal(t, original.SharedWorkspace, got.SharedWorkspace)
	assert.Equal(t, original.ProjectPath, got.ProjectPath)
	assert.Equal(t, original.ProjectSlug, got.ProjectSlug)
	assert.Equal(t, original.HarnessConfig, got.HarnessConfig)
}

func TestRestartDispatchArgs_RoundTrip(t *testing.T) {
	original := &RestartDispatchArgs{
		ResolvedEnv: map[string]string{"SCION_AUTH_TOKEN": "token123", "SCION_HUB_ENDPOINT": "https://hub.example.com"},
	}

	raw, err := MarshalDispatchArgs(original)
	require.NoError(t, err)

	got, err := UnmarshalRestartArgs(raw)
	require.NoError(t, err)
	assert.Equal(t, original.ResolvedEnv, got.ResolvedEnv)
}

func TestStopDispatchArgs_RoundTrip(t *testing.T) {
	raw, err := MarshalDispatchArgs(&StopDispatchArgs{})
	require.NoError(t, err)
	assert.Equal(t, "{}", raw)
}
