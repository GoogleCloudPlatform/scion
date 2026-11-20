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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type scriptedHeartbeatService struct {
	mockRuntimeBrokerService
	onHeartbeat func(call int) (*hubclient.BrokerHeartbeatResponse, error)
	calls       atomic.Int32
}

func (s *scriptedHeartbeatService) Heartbeat(ctx context.Context, brokerID string, status *hubclient.BrokerHeartbeat) (*hubclient.BrokerHeartbeatResponse, error) {
	n := int(s.calls.Add(1))
	if s.onHeartbeat != nil {
		return s.onHeartbeat(n)
	}
	return s.mockRuntimeBrokerService.Heartbeat(ctx, brokerID, status)
}

func TestHeartbeatService_StopsOnBrokerDeregistered(t *testing.T) {
	deregErr := &apiclient.APIError{
		StatusCode: http.StatusUnauthorized,
		Code:       apiclient.ErrCodeBrokerDeregistered,
		Message:    "unknown broker: broker-1",
	}

	t.Run("stops on initial deregistered heartbeat", func(t *testing.T) {
		mockClient := &scriptedHeartbeatService{
			onHeartbeat: func(int) (*hubclient.BrokerHeartbeatResponse, error) {
				return nil, deregErr
			},
		}
		svc := NewHeartbeatService(mockClient, "broker-1", 20*time.Millisecond, nil, nil, slog.Default())
		svc.interval = 20 * time.Millisecond
		svc.Start(context.Background())

		require.Eventually(t, func() bool {
			return !svc.IsRunning()
		}, 2*time.Second, 10*time.Millisecond, "heartbeat service should stop on broker_deregistered")

		// Wait several intervals to ensure no further retries occur.
		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, int32(1), mockClient.calls.Load(), "must not retry after broker_deregistered")
		svc.Stop()
	})

	t.Run("stops on subsequent deregistered heartbeat", func(t *testing.T) {
		mockClient := &scriptedHeartbeatService{
			onHeartbeat: func(call int) (*hubclient.BrokerHeartbeatResponse, error) {
				if call == 1 {
					return &hubclient.BrokerHeartbeatResponse{}, nil
				}
				return nil, deregErr
			},
		}
		svc := NewHeartbeatService(mockClient, "broker-1", 20*time.Millisecond, nil, nil, slog.Default())
		svc.interval = 20 * time.Millisecond
		svc.Start(context.Background())

		require.Eventually(t, func() bool {
			return !svc.IsRunning()
		}, 2*time.Second, 10*time.Millisecond, "heartbeat service should stop after deregistered tick")

		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, int32(2), mockClient.calls.Load(), "must stop immediately after the deregistered response")
		svc.Stop()
	})

	t.Run("generic broker_auth_failed keeps retrying", func(t *testing.T) {
		authErr := &apiclient.APIError{
			StatusCode: http.StatusUnauthorized,
			Code:       "broker_auth_failed",
			Message:    "timestamp outside acceptable range",
		}
		mockClient := &scriptedHeartbeatService{
			onHeartbeat: func(int) (*hubclient.BrokerHeartbeatResponse, error) {
				return nil, authErr
			},
		}
		svc := NewHeartbeatService(mockClient, "broker-1", 20*time.Millisecond, nil, nil, slog.Default())
		svc.interval = 20 * time.Millisecond
		svc.Start(context.Background())
		t.Cleanup(svc.Stop)

		require.Eventually(t, func() bool {
			return mockClient.calls.Load() >= 3
		}, 2*time.Second, 10*time.Millisecond, "generic auth failures must still retry")
		assert.True(t, svc.IsRunning())
	})
}

func TestControlChannelClient_StopsOnBrokerDeregistered(t *testing.T) {
	t.Run("stops after single attempt on broker_deregistered", func(t *testing.T) {
		var attempts atomic.Int32
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"broker_deregistered","message":"unknown broker: b1"}}`))
		}))
		t.Cleanup(hub.Close)

		cfg := DefaultControlChannelConfig()
		cfg.HubEndpoint = hub.URL
		cfg.BrokerID = "b1"
		cfg.ReconnectInitial = 10 * time.Millisecond
		cfg.ReconnectMax = 20 * time.Millisecond
		client := NewControlChannelClient(cfg, http.NotFoundHandler(), nil, "test", slog.Default())

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := client.Connect(ctx)
		require.Error(t, err)
		assert.NoError(t, ctx.Err(), "Connect should return before context timeout")
		assert.Equal(t, int32(1), attempts.Load(), "must not retry after broker_deregistered")
	})

	t.Run("generic broker_auth_failed keeps retrying", func(t *testing.T) {
		var attempts atomic.Int32
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"broker_auth_failed","message":"invalid signature"}}`))
		}))
		t.Cleanup(hub.Close)

		cfg := DefaultControlChannelConfig()
		cfg.HubEndpoint = hub.URL
		cfg.BrokerID = "b1"
		cfg.ReconnectInitial = 10 * time.Millisecond
		cfg.ReconnectMax = 20 * time.Millisecond
		client := NewControlChannelClient(cfg, http.NotFoundHandler(), nil, "test", slog.Default())

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- client.Connect(ctx)
		}()

		require.Eventually(t, func() bool {
			return attempts.Load() >= 3
		}, 2*time.Second, 10*time.Millisecond, "generic broker_auth_failed must keep retrying")
		cancel()
		assert.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestConduitDialer_StopsOnBrokerDeregistered(t *testing.T) {
	body := `{"error":{"code":"broker_deregistered","message":"unknown broker: b1"}}`
	hub := newFakeConduitHub(t, hubStep{status: http.StatusUnauthorized, body: body})
	ends, logs, stop := startTestDialer(t, hub.srv.URL, clock.NewFake(time.Now()), maxRand, nil)

	e := nextEnd(t, ends)
	require.Error(t, e.end.DialErr)
	require.Error(t, e.err, "broker_deregistered must stop the conduit dialer")
	assert.Equal(t, time.Duration(0), e.delay)
	assert.Contains(t, logs.String(), "deregistered")

	err := stop()
	require.Error(t, err, "Run must exit with terminal deregistered error")
}
