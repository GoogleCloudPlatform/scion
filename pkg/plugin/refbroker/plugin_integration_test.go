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

package refbroker

import (
	"context"
	"log/slog"
	"net"
	"net/rpc"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startRefBrokerRPCServer starts a real RefBroker behind an RPC server and returns a client.
// This exercises the full RPC transport path: client -> net/rpc -> BrokerRPCServer -> RefBroker.
func startRefBrokerRPCServer(t *testing.T) (*plugin.BrokerRPCClient, *RefBroker) {
	t.Helper()

	impl := New(slog.Default())
	t.Cleanup(func() { _ = impl.Close() })

	server := rpc.NewServer()
	rpcServer := &plugin.BrokerRPCServer{Impl: impl}
	require.NoError(t, server.RegisterName("Plugin", rpcServer))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	go server.Accept(listener)

	client, err := rpc.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return plugin.NewBrokerRPCClient(client), impl
}

func TestRPCIntegration_ConfigurePublishSubscribe(t *testing.T) {
	client, impl := startRefBrokerRPCServer(t)

	// Set up in-process handler to capture delivered messages
	var deliveredTopic string
	var deliveredMsg *messages.StructuredMessage
	done := make(chan struct{})
	impl.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		deliveredTopic = topic
		deliveredMsg = msg
		close(done)
	}

	// Configure
	err := client.Configure(map[string]string{"plugin_name": "rpc-test"})
	require.NoError(t, err)

	// Subscribe
	err = client.Subscribe("scion.grove.g1.agent.*.messages")
	require.NoError(t, err)

	// Publish (from external source, no echo marker)
	msg := messages.NewInstruction("user:alice", "agent:coder", "hello via rpc")
	err = client.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", msg)
	require.NoError(t, err)

	// Wait for delivery
	<-done
	assert.Equal(t, "scion.grove.g1.agent.coder.messages", deliveredTopic)
	assert.Equal(t, "hello via rpc", deliveredMsg.Msg)
}

func TestRPCIntegration_EchoFilteringOverRPC(t *testing.T) {
	client, impl := startRefBrokerRPCServer(t)

	delivered := make(chan struct{}, 1)
	impl.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		delivered <- struct{}{}
	}

	err := client.Subscribe("test.>")
	require.NoError(t, err)

	// Publish echo message
	echoMsg := messages.NewInstruction(
		OriginMarkerKey+":"+OriginMarkerValue+":hub",
		"agent:coder",
		"echo msg",
	)
	err = client.Publish(context.Background(), "test.topic", echoMsg)
	require.NoError(t, err)

	// Publish non-echo message
	normalMsg := messages.NewInstruction("user:alice", "agent:coder", "real msg")
	err = client.Publish(context.Background(), "test.topic", normalMsg)
	require.NoError(t, err)

	// Should only get the non-echo message
	<-delivered
	select {
	case <-delivered:
		t.Fatal("received unexpected second delivery (echo was not filtered)")
	default:
		// expected: only one message delivered
	}
}

func TestRPCIntegration_UnsubscribeOverRPC(t *testing.T) {
	client, impl := startRefBrokerRPCServer(t)

	delivered := make(chan struct{}, 1)
	impl.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		delivered <- struct{}{}
	}

	err := client.Subscribe("test.>")
	require.NoError(t, err)

	err = client.Unsubscribe("test.>")
	require.NoError(t, err)

	msg := messages.NewInstruction("user:alice", "agent:coder", "after unsub")
	err = client.Publish(context.Background(), "test.topic", msg)
	require.NoError(t, err)

	select {
	case <-delivered:
		t.Fatal("received message after unsubscribe")
	default:
		// expected
	}
}

func TestRPCIntegration_GetInfo(t *testing.T) {
	client, _ := startRefBrokerRPCServer(t)

	info, err := client.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "refbroker", info.Name)
	assert.Equal(t, "0.1.0", info.Version)
	assert.Contains(t, info.Capabilities, "echo-filter")
}

func TestRPCIntegration_CloseOverRPC(t *testing.T) {
	client, _ := startRefBrokerRPCServer(t)

	err := client.Close()
	require.NoError(t, err)
}

func TestRPCIntegration_FullLifecycle(t *testing.T) {
	client, impl := startRefBrokerRPCServer(t)

	// 1. GetInfo
	info, err := client.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "refbroker", info.Name)

	// 2. Configure
	err = client.Configure(map[string]string{"plugin_name": "lifecycle-test"})
	require.NoError(t, err)

	// 3. Subscribe
	deliveries := make(chan string, 10)
	impl.InboundHandler = func(topic string, _ *messages.StructuredMessage) {
		deliveries <- topic
	}
	err = client.Subscribe("scion.grove.*.>")
	require.NoError(t, err)

	// 4. Publish
	msg := messages.NewInstruction("user:bob", "agent:reviewer", "review this")
	err = client.Publish(context.Background(), "scion.grove.g1.agent.reviewer.messages", msg)
	require.NoError(t, err)

	topic := <-deliveries
	assert.Equal(t, "scion.grove.g1.agent.reviewer.messages", topic)

	// 5. Unsubscribe
	err = client.Unsubscribe("scion.grove.*.>")
	require.NoError(t, err)

	// 6. Close
	err = client.Close()
	require.NoError(t, err)
}
