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

package grpcbroker

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/refbroker"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// startTestServer starts a gRPC server with the given broker implementation
// on a random port and returns the address and a stop function.
func startTestServer(t *testing.T, impl *refbroker.RefBroker) (string, func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := grpc.NewServer()
	brokerv1.RegisterBrokerServiceServer(s, NewServer(impl))

	go s.Serve(lis)

	return lis.Addr().String(), func() {
		s.GracefulStop()
	}
}

func TestAdapterPublishSubscribe(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer broker.Close()

	var received []string
	var mu sync.Mutex
	broker.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		mu.Lock()
		received = append(received, msg.Msg)
		mu.Unlock()
	}

	addr, stop := startTestServer(t, broker)
	defer stop()

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: addr,
		Logger:  slog.Default(),
	})
	defer adapter.Close()

	// Subscribe
	handler := func(_ context.Context, _ string, _ *messages.StructuredMessage) {}
	sub, err := adapter.Subscribe("scion.grove.g1.agent.*.messages", handler)
	require.NoError(t, err)
	require.NotNil(t, sub)

	// Publish
	msg := messages.NewInstruction("user:alice", "agent:coder", "hello via grpc")
	err = adapter.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", msg)
	require.NoError(t, err)

	// Wait for async delivery in refbroker
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1
	}, 2*time.Second, 10*time.Millisecond)

	mu.Lock()
	assert.Equal(t, "hello via grpc", received[0])
	mu.Unlock()

	// Unsubscribe
	require.NoError(t, sub.Unsubscribe())
}

func TestAdapterGetInfo(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer broker.Close()

	addr, stop := startTestServer(t, broker)
	defer stop()

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: addr,
		Logger:  slog.Default(),
	})
	defer adapter.Close()

	info, err := adapter.GetInfo()
	require.NoError(t, err)
	require.NotNil(t, info)

	assert.Equal(t, "refbroker", info.Name)
	assert.Equal(t, "0.1.0", info.Version)
	assert.Contains(t, info.Capabilities, "echo-filter")
}

func TestAdapterHealthCheck(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer broker.Close()

	addr, stop := startTestServer(t, broker)
	defer stop()

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: addr,
		Logger:  slog.Default(),
	})
	defer adapter.Close()

	health, err := adapter.HealthCheck()
	require.NoError(t, err)
	require.NotNil(t, health)

	assert.Equal(t, "healthy", health.Status)
	assert.Contains(t, health.Message, "operational")
}

func TestAdapterConfigure(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer broker.Close()

	addr, stop := startTestServer(t, broker)
	defer stop()

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: addr,
		Logger:  slog.Default(),
	})
	defer adapter.Close()

	err := adapter.Configure(map[string]string{
		"hub_url":     "http://localhost:8080",
		"plugin_name": "test",
	})
	require.NoError(t, err)
}

func TestAdapterReconnectAfterServerRestart(t *testing.T) {
	broker := refbroker.New(slog.Default())

	var received []string
	var mu sync.Mutex
	broker.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		mu.Lock()
		received = append(received, msg.Msg)
		mu.Unlock()
	}

	// Start server on a fixed port.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	fixedAddr := lis.Addr().String()

	s1 := grpc.NewServer()
	brokerv1.RegisterBrokerServiceServer(s1, NewServer(broker))
	go s1.Serve(lis)

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: fixedAddr,
		Logger:  slog.Default(),
	})
	defer adapter.Close()

	// Subscribe with handler.
	handler := func(_ context.Context, _ string, _ *messages.StructuredMessage) {}
	sub, err := adapter.Subscribe("scion.grove.g1.>", handler)
	require.NoError(t, err)
	require.NotNil(t, sub)

	// Publish successfully.
	msg1 := messages.NewInstruction("user:alice", "agent:coder", "before restart")
	require.NoError(t, adapter.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", msg1))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Kill the server and force-close the adapter's connection so the next
	// RPC call sees a transport error and triggers reconnect logic.
	s1.Stop()
	broker.Close()

	adapter.mu.Lock()
	if adapter.conn != nil {
		adapter.conn.Close()
		adapter.conn = nil
		adapter.client = nil
	}
	adapter.mu.Unlock()

	// Start a new server on the same port.
	broker2 := refbroker.New(slog.Default())
	defer broker2.Close()

	var received2 []string
	broker2.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		mu.Lock()
		received2 = append(received2, msg.Msg)
		mu.Unlock()
	}

	lis2, err := net.Listen("tcp", fixedAddr)
	require.NoError(t, err)

	s2 := grpc.NewServer()
	brokerv1.RegisterBrokerServiceServer(s2, NewServer(broker2))
	go s2.Serve(lis2)
	defer s2.GracefulStop()

	// ensureConnected will re-dial; tryReconnect will re-subscribe.
	// Publish to trigger connect → subscribe → publish.
	msg2 := messages.NewInstruction("user:bob", "agent:coder", "after restart")
	err = adapter.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", msg2)
	require.NoError(t, err)

	// The adapter should have re-subscribed after reconnect, so the message
	// published to the refbroker reaches the InboundHandler.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received2) >= 1
	}, 2*time.Second, 10*time.Millisecond)

	mu.Lock()
	assert.Equal(t, "after restart", received2[0])
	mu.Unlock()
}

func TestAdapterClosedReturnsError(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer broker.Close()

	addr, stop := startTestServer(t, broker)
	defer stop()

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: addr,
		Logger:  slog.Default(),
	})

	require.NoError(t, adapter.Close())

	err := adapter.Publish(context.Background(), "test", messages.NewInstruction("a", "b", "c"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "closed")

	_, err = adapter.Subscribe("test", func(_ context.Context, _ string, _ *messages.StructuredMessage) {})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

func TestIsLocalAddress(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"localhost:9090", true},
		{"127.0.0.1:9090", true},
		{"[::1]:9090", true},
		{":9090", true},
		{"example.com:9090", false},
		{"10.0.0.1:9090", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			assert.Equal(t, tt.want, isLocalAddress(tt.addr))
		})
	}
}

func TestServerWrapsPluginInterface(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer broker.Close()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := grpc.NewServer()
	brokerv1.RegisterBrokerServiceServer(s, NewServer(broker))
	go s.Serve(lis)
	defer s.GracefulStop()

	// Connect directly with a raw gRPC client
	conn, err := grpc.Dial(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	client := brokerv1.NewBrokerServiceClient(conn)

	// Configure
	_, err = client.Configure(context.Background(), &brokerv1.ConfigureRequest{
		Config: map[string]string{"hub_url": "http://test:8080"},
	})
	require.NoError(t, err)

	// GetInfo
	infoResp, err := client.GetInfo(context.Background(), &brokerv1.GetInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, "refbroker", infoResp.Name)

	// HealthCheck
	healthResp, err := client.HealthCheck(context.Background(), &brokerv1.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, "healthy", healthResp.Status)

	// Subscribe
	_, err = client.Subscribe(context.Background(), &brokerv1.SubscribeRequest{Pattern: "test.>"})
	require.NoError(t, err)

	// Publish
	_, err = client.Publish(context.Background(), &brokerv1.PublishRequest{
		Topic: "test.topic",
		Message: &brokerv1.StructuredMessage{
			Version:   1,
			Sender:    "user:test",
			Recipient: "agent:test",
			Msg:       "direct grpc",
			Type:      "instruction",
		},
	})
	require.NoError(t, err)

	// Unsubscribe
	_, err = client.Unsubscribe(context.Background(), &brokerv1.UnsubscribeRequest{Pattern: "test.>"})
	require.NoError(t, err)
}

func TestAdapterImplementsEventBus(t *testing.T) {
	// Compile-time check that GRPCBrokerAdapter implements eventbus.EventBus.
	var _ eventbus.EventBus = (*GRPCBrokerAdapter)(nil)
}

func TestAdapterReconnectCallback(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer broker.Close()

	addr, stop := startTestServer(t, broker)

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: addr,
		Logger:  slog.Default(),
	})
	defer adapter.Close()

	var callbackCount int
	var cbMu sync.Mutex
	adapter.OnReconnect(func() {
		cbMu.Lock()
		callbackCount++
		cbMu.Unlock()
	})

	// Initial subscribe to establish connection.
	_, err := adapter.Subscribe("test.>", func(_ context.Context, _ string, _ *messages.StructuredMessage) {})
	require.NoError(t, err)

	// Stop server and restart to trigger reconnect.
	stop()

	broker2 := refbroker.New(slog.Default())
	defer broker2.Close()

	addr2, stop2 := startTestServer(t, broker2)
	defer stop2()

	// Close the adapter connection to simulate disconnect,
	// then point to the new server address.
	_ = addr2
	adapter.mu.Lock()
	if adapter.conn != nil {
		adapter.conn.Close()
		adapter.conn = nil
		adapter.client = nil
	}
	adapter.address = addr2
	adapter.mu.Unlock()

	// Next publish triggers reconnect.
	ctx := context.Background()
	_ = adapter.Publish(ctx, "test.topic", &messages.StructuredMessage{
		Version: 1,
		Msg:     "after-reconnect",
	})

	time.Sleep(50 * time.Millisecond)

	cbMu.Lock()
	count := callbackCount
	cbMu.Unlock()

	assert.GreaterOrEqual(t, count, 1, "reconnect callback should have been called")
}
