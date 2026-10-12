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
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestConduitBrokerTunnel_AdmittedBrokerSession runs the tunnel over a
// broker session the hub actually admitted: the broker's conduit dialer
// (with an RPC handler set through ConduitDialConfig.Session) signs in to
// the hub's /api/v1/conduit, the relay registers the session, and the
// tunnel reaches the broker through the relay's local session. A request
// and its response cross unchanged, a caller cancel reaches the broker's
// handler, and once the broker stops the tunnel reports it not connected.
func TestConduitBrokerTunnel_AdmittedBrokerSession(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)

	echo := &echoBroker{}
	blockStarted := make(chan struct{}, 1)
	blockCancelled := make(chan error, 1)
	rpc := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		if req.GetPath() == "/block" {
			blockStarted <- struct{}{}
			<-ctx.Done()
			blockCancelled <- ctx.Err()
			return nil
		}
		return echo.HandleRPC(ctx, req)
	})
	d, err := runtimebroker.NewConduitDialer(runtimebroker.ConduitDialConfig{
		HubEndpoint: f.public.URL,
		BrokerID:    f.brokerID,
		SecretKey:   f.secret,
		Incarnation: "proc-3-2",
		Rand:        func(int64) int64 { return 0 },
		Session:     conduit.Config{RPCHandler: rpc},
	})
	require.NoError(t, err)
	ctx, stopBroker := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()
	t.Cleanup(func() {
		stopBroker()
		<-done
	})

	require.Eventually(t, func() bool { return f.conduitSessionID(t) != "" }, 10*time.Second, 10*time.Millisecond)
	rt := f.srv.conduit.Load()
	require.NotNil(t, rt)
	tun := newConduitBrokerTunnel(func(id string) conduit.Session {
		if id != f.brokerID {
			return nil
		}
		ls, _, ok := rt.relay.Local(context.Background(), f.conduitSessionID(t))
		if !ok {
			return nil
		}
		return ls
	}, 0)
	require.True(t, tun.IsConnected(f.brokerID))
	assert.False(t, tun.IsConnected("other-broker"))

	// A request and its response cross unchanged.
	req := wsprotocol.NewRequestEnvelope("adm-1", http.MethodPost, "/api/v1/agents", "projectId=p1",
		map[string]string{"Content-Type": "application/json"}, []byte(`{"name":"a"}`))
	resp, err := tun.TunnelRequest(context.Background(), f.brokerID, req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "adm-1", resp.RequestID)
	assert.Equal(t, `{"name":"a"}`, string(resp.Body))
	assert.Equal(t, "application/json", resp.Headers["Content-Type"])
	seen := echo.requests()
	require.Len(t, seen, 1)
	assert.Equal(t, http.MethodPost, seen[0].Method)
	assert.Equal(t, "/api/v1/agents", seen[0].Path)
	assert.Equal(t, "projectId=p1", seen[0].Query)

	// A caller cancel reaches the broker's handler.
	cctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := tun.TunnelRequest(cctx, f.brokerID, wsprotocol.NewRequestEnvelope("adm-2", http.MethodPost, "/block", "", nil, nil))
		errCh <- err
	}()
	select {
	case <-blockStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the broker handler never started")
	}
	cancel()
	select {
	case err := <-errCh:
		assert.True(t, errors.Is(err, context.Canceled), "TunnelRequest err = %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("TunnelRequest did not return after cancel")
	}
	select {
	case err := <-blockCancelled:
		assert.True(t, errors.Is(err, context.Canceled), "broker handler ctx err = %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the broker handler never saw its context cancelled")
	}

	// Once the broker stops, the tunnel reports it not connected.
	stopBroker()
	<-done
	require.Eventually(t, func() bool { return !tun.IsConnected(f.brokerID) }, 10*time.Second, 10*time.Millisecond)
	_, err = tun.TunnelRequest(context.Background(), f.brokerID, wsprotocol.NewRequestEnvelope("adm-3", http.MethodGet, "/x", "", nil, nil))
	assert.True(t, errors.Is(err, errStartBrokerNotConnected), "err = %v", err)
}
