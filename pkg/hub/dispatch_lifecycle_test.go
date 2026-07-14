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
	"log/slog"
	"testing"
	"time"

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
		_, err := c.StartAgent(context.Background(), remoteBroker, "", "a1", "p1", "", "", "", "", nil, nil, nil, nil, false, false)
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})

	t.Run("routeUndeliverable returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
		_, err := c.StartAgent(context.Background(), remoteBroker, "", "a1", "p1", "", "", "", "", nil, nil, nil, nil, false, false)
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
		Task: "build the widget",
	}

	raw, err := MarshalDispatchArgs(original)
	require.NoError(t, err)
	require.NotEmpty(t, raw)

	got, err := UnmarshalStartArgs(raw)
	require.NoError(t, err)
	assert.Equal(t, original.Task, got.Task)
}

func TestRestartDispatchArgs_RoundTrip(t *testing.T) {
	raw, err := MarshalDispatchArgs(&RestartDispatchArgs{})
	require.NoError(t, err)
	assert.Equal(t, "{}", raw)
}

func TestStopDispatchArgs_RoundTrip(t *testing.T) {
	raw, err := MarshalDispatchArgs(&StopDispatchArgs{})
	require.NoError(t, err)
	assert.Equal(t, "{}", raw)
}

// =========================================================================
// B4-3: Route-gating tests for DeleteAgent
// =========================================================================

func TestHybridBrokerClient_DeleteAgent_RouteGate(t *testing.T) {
	const remoteBroker = "broker-remote"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	c := NewHybridBrokerClient(mgr, &fakeHTTPClient{}, nil, false)

	t.Run("routeForward returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "hubA", true })
		err := c.DeleteAgent(context.Background(), remoteBroker, "", "a1", "p1", false, false, false, time.Time{})
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})

	t.Run("routeUndeliverable returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
		err := c.DeleteAgent(context.Background(), remoteBroker, "", "a1", "p1", false, false, false, time.Time{})
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})
}

// =========================================================================
// B4-4: Route-gating tests for CheckAgentPrompt / CreateAgentWithGather / FinalizeEnv
// =========================================================================

func TestHybridBrokerClient_CheckAgentPrompt_RouteGate(t *testing.T) {
	const remoteBroker = "broker-remote"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	c := NewHybridBrokerClient(mgr, &fakeHTTPClient{}, nil, false)

	t.Run("routeForward returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "hubA", true })
		_, err := c.CheckAgentPrompt(context.Background(), remoteBroker, "", "a1", "p1")
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})

	t.Run("routeUndeliverable returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
		_, err := c.CheckAgentPrompt(context.Background(), remoteBroker, "", "a1", "p1")
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})
}

func TestHybridBrokerClient_CreateAgentWithGather_RouteGate(t *testing.T) {
	const remoteBroker = "broker-remote"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	c := NewHybridBrokerClient(mgr, &fakeHTTPClient{}, nil, false)

	t.Run("routeForward returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "hubA", true })
		_, _, err := c.CreateAgentWithGather(context.Background(), remoteBroker, "", &RemoteCreateAgentRequest{})
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})

	t.Run("routeUndeliverable returns ErrLifecycleDeferred", func(t *testing.T) {
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
		_, _, err := c.CreateAgentWithGather(context.Background(), remoteBroker, "", &RemoteCreateAgentRequest{})
		assert.ErrorIs(t, err, ErrLifecycleDeferred)
	})
}

// =========================================================================
// B4-3/B4-4: Dispatch args round-trip
// =========================================================================

func TestDeleteDispatchArgs_RoundTrip(t *testing.T) {
	original := &DeleteDispatchArgs{
		DeleteFiles:  true,
		RemoveBranch: true,
		SoftDelete:   false,
	}

	raw, err := MarshalDispatchArgs(original)
	require.NoError(t, err)
	require.NotEmpty(t, raw)

	got, err := UnmarshalDeleteArgs(raw)
	require.NoError(t, err)
	assert.Equal(t, original.DeleteFiles, got.DeleteFiles)
	assert.Equal(t, original.RemoveBranch, got.RemoveBranch)
	assert.Equal(t, original.SoftDelete, got.SoftDelete)
}

func TestFinalizeEnvDispatchArgs_RoundTrip(t *testing.T) {
	original := &FinalizeEnvDispatchArgs{
		Env: map[string]string{"KEY": "val", "SECRET": "abc"},
	}

	raw, err := MarshalDispatchArgs(original)
	require.NoError(t, err)

	got, err := UnmarshalFinalizeEnvArgs(raw)
	require.NoError(t, err)
	assert.Equal(t, original.Env, got.Env)
}

func TestCheckPromptDispatchArgs_RoundTrip(t *testing.T) {
	raw, err := MarshalDispatchArgs(&CheckPromptDispatchArgs{})
	require.NoError(t, err)
	assert.Equal(t, "{}", raw)
}
