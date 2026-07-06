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

package plugin

import (
	"context"
	"net"
	"net/rpc"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockBrokerPlugin implements MessageBrokerPluginInterface for testing.
type mockBrokerPlugin struct {
	configured   map[string]string
	published    []PublishArgs
	subscribed   []string
	unsubscribed []string
	closed       bool
}

func (m *mockBrokerPlugin) Configure(config map[string]string) error {
	m.configured = config
	return nil
}

func (m *mockBrokerPlugin) Publish(_ context.Context, topic string, msg *messages.StructuredMessage) error {
	m.published = append(m.published, PublishArgs{Topic: topic, Msg: msg})
	return nil
}

func (m *mockBrokerPlugin) Subscribe(pattern string) error {
	m.subscribed = append(m.subscribed, pattern)
	return nil
}

func (m *mockBrokerPlugin) Unsubscribe(pattern string) error {
	m.unsubscribed = append(m.unsubscribed, pattern)
	return nil
}

func (m *mockBrokerPlugin) Close() error {
	m.closed = true
	return nil
}

func (m *mockBrokerPlugin) GetInfo() (*PluginInfo, error) {
	return &PluginInfo{
		Name:    "test-broker",
		Version: "1.0.0",
	}, nil
}

func (m *mockBrokerPlugin) HealthCheck() (*HealthStatus, error) {
	return &HealthStatus{
		Status:  "healthy",
		Message: "mock broker is healthy",
		Details: map[string]string{"test": "true"},
	}, nil
}

// startTestRPCServer starts an RPC server with the mock broker plugin and returns a client.
func startTestBrokerRPCServer(t *testing.T, impl MessageBrokerPluginInterface) *BrokerRPCClient {
	t.Helper()

	server := rpc.NewServer()
	rpcServer := &BrokerRPCServer{Impl: impl}
	require.NoError(t, server.RegisterName("Plugin", rpcServer))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	go server.Accept(listener)

	client, err := rpc.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return &BrokerRPCClient{client: client}
}

func TestBrokerRPC_Configure(t *testing.T) {
	mock := &mockBrokerPlugin{}
	client := startTestBrokerRPCServer(t, mock)

	cfg := map[string]string{"url": "nats://localhost:4222"}
	err := client.Configure(cfg)
	require.NoError(t, err)
	assert.Equal(t, cfg, mock.configured)
}

func TestBrokerRPC_Publish(t *testing.T) {
	mock := &mockBrokerPlugin{}
	client := startTestBrokerRPCServer(t, mock)

	msg := messages.NewInstruction("user:alice", "agent:coder", "hello")
	err := client.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", msg)
	require.NoError(t, err)

	require.Len(t, mock.published, 1)
	assert.Equal(t, "scion.grove.g1.agent.coder.messages", mock.published[0].Topic)
	assert.Equal(t, "hello", mock.published[0].Msg.Msg)
}

func TestBrokerRPC_Subscribe(t *testing.T) {
	mock := &mockBrokerPlugin{}
	client := startTestBrokerRPCServer(t, mock)

	err := client.Subscribe("scion.grove.g1.agent.*.messages")
	require.NoError(t, err)

	require.Len(t, mock.subscribed, 1)
	assert.Equal(t, "scion.grove.g1.agent.*.messages", mock.subscribed[0])
}

func TestBrokerRPC_Unsubscribe(t *testing.T) {
	mock := &mockBrokerPlugin{}
	client := startTestBrokerRPCServer(t, mock)

	err := client.Unsubscribe("scion.grove.g1.agent.*.messages")
	require.NoError(t, err)

	require.Len(t, mock.unsubscribed, 1)
}

func TestBrokerRPC_Close(t *testing.T) {
	mock := &mockBrokerPlugin{}
	client := startTestBrokerRPCServer(t, mock)

	err := client.Close()
	require.NoError(t, err)
	assert.True(t, mock.closed)
}

func TestBrokerRPC_GetInfo(t *testing.T) {
	mock := &mockBrokerPlugin{}
	client := startTestBrokerRPCServer(t, mock)

	info, err := client.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "test-broker", info.Name)
	assert.Equal(t, "1.0.0", info.Version)
}

func TestBrokerRPC_HealthCheck(t *testing.T) {
	mock := &mockBrokerPlugin{}
	client := startTestBrokerRPCServer(t, mock)

	status, err := client.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "healthy", status.Status)
	assert.Equal(t, "mock broker is healthy", status.Message)
	assert.Equal(t, "true", status.Details["test"])
}

func TestBrokerPluginAdapter_Publish(t *testing.T) {
	mock := &mockBrokerPlugin{}
	client := startTestBrokerRPCServer(t, mock)
	adapter := NewBrokerPluginAdapter(client)

	msg := messages.NewInstruction("user:alice", "agent:coder", "hello")
	err := adapter.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", msg)
	require.NoError(t, err)

	require.Len(t, mock.published, 1)
}

func TestBrokerPluginAdapter_Subscribe(t *testing.T) {
	mock := &mockBrokerPlugin{}
	client := startTestBrokerRPCServer(t, mock)
	adapter := NewBrokerPluginAdapter(client)

	sub, err := adapter.Subscribe("scion.grove.g1.agent.*.messages", func(ctx context.Context, topic string, msg *messages.StructuredMessage) {})
	require.NoError(t, err)
	require.NotNil(t, sub)

	// The handler callback should NOT be forwarded to the plugin
	require.Len(t, mock.subscribed, 1)

	// Unsubscribe
	err = sub.Unsubscribe()
	require.NoError(t, err)
	require.Len(t, mock.unsubscribed, 1)
}

func TestBrokerPluginAdapter_Close(t *testing.T) {
	mock := &mockBrokerPlugin{}
	client := startTestBrokerRPCServer(t, mock)
	adapter := NewBrokerPluginAdapter(client)

	err := adapter.Close()
	require.NoError(t, err)
	assert.True(t, mock.closed)
}
