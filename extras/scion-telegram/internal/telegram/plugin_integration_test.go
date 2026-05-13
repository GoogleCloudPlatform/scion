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

package telegram

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/rpc"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTelegramRPCServer starts a TelegramBroker behind an RPC server and
// returns a client. This exercises the full RPC transport path:
// client -> net/rpc -> BrokerRPCServer -> TelegramBroker.
func startTelegramRPCServer(t *testing.T, tgSrv *fakeTelegramServer) (*plugin.BrokerRPCClient, *TelegramBroker) {
	t.Helper()

	impl := New(slog.Default())
	t.Cleanup(func() { impl.Close() })

	server := rpc.NewServer()
	rpcServer := &plugin.BrokerRPCServer{Impl: impl}
	require.NoError(t, server.RegisterName("Plugin", rpcServer))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })

	go server.Accept(listener)

	client, err := rpc.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })

	return plugin.NewBrokerRPCClient(client), impl
}

func TestRPCIntegration_ConfigurePublishSubscribe(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	client, impl := startTelegramRPCServer(t, tgSrv)

	// Set up in-process handler to capture delivered messages
	var deliveredTopic string
	var deliveredMsg *messages.StructuredMessage
	done := make(chan struct{})
	impl.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		deliveredTopic = topic
		deliveredMsg = msg
		close(done)
	}

	// Configure with bot token
	err := client.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"plugin_name":  "rpc-test",
		"chat_routes":  `{"789": "scion.project.p1.agent.coder.messages"}`,
	})
	require.NoError(t, err)

	// Queue an inbound message
	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 1,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      1700000000,
				Text:      "hello via rpc",
			},
		},
	})

	// Subscribe triggers polling
	err = client.Subscribe("scion.project.p1.>")
	require.NoError(t, err)

	// Wait for delivery
	<-done
	assert.Equal(t, "scion.project.p1.agent.coder.messages", deliveredTopic)
	assert.Equal(t, "hello via rpc", deliveredMsg.Msg)
}

func TestRPCIntegration_PublishToTelegram(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	client, _ := startTelegramRPCServer(t, tgSrv)

	// Configure
	err := client.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"chat_routes":  `{"789": "scion.project.p1.agent.coder.messages"}`,
	})
	require.NoError(t, err)

	// Publish a message to a topic mapped to a Telegram chat
	msg := messages.NewInstruction("user:alice", "agent:coder", "hello from hub via rpc")
	err = client.Publish(context.Background(), "scion.project.p1.agent.coder.messages", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(789), sent[0].ChatID)
	assert.Contains(t, sent[0].Text, "hello from hub via rpc")
}

func TestRPCIntegration_GetInfo(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	client, _ := startTelegramRPCServer(t, tgSrv)

	info, err := client.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "telegram", info.Name)
	assert.Equal(t, "0.1.0", info.Version)
	assert.Contains(t, info.Capabilities, "echo-filter")
	assert.Contains(t, info.Capabilities, "long-polling")
}

func TestRPCIntegration_HealthCheck(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	client, _ := startTelegramRPCServer(t, tgSrv)

	// Before configure — degraded
	status, err := client.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "degraded", status.Status)

	// After configure — healthy
	err = client.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
	})
	require.NoError(t, err)

	status, err = client.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "healthy", status.Status)
	assert.Equal(t, "@test_bot", status.Details["bot_username"])
}

func TestRPCIntegration_CloseOverRPC(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	client, _ := startTelegramRPCServer(t, tgSrv)

	// Configure first
	err := client.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
	})
	require.NoError(t, err)

	err = client.Close()
	require.NoError(t, err)
}

func TestRPCIntegration_FullLifecycle(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	client, impl := startTelegramRPCServer(t, tgSrv)

	// 1. GetInfo
	info, err := client.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "telegram", info.Name)

	// 2. Configure
	err = client.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"plugin_name":  "lifecycle-test",
		"chat_routes":  `{"789": "scion.project.p1.agent.coder.messages"}`,
	})
	require.NoError(t, err)

	// 3. Subscribe and receive inbound
	deliveries := make(chan string, 10)
	impl.InboundHandler = func(topic string, _ *messages.StructuredMessage) {
		deliveries <- topic
	}

	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 1,
				From:      &TGUser{ID: 456, Username: "bob"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      1700000000,
				Text:      "lifecycle test msg",
			},
		},
	})

	err = client.Subscribe("scion.project.*.>")
	require.NoError(t, err)

	topic := <-deliveries
	assert.Equal(t, "scion.project.p1.agent.coder.messages", topic)

	// 4. Publish outbound
	msg := messages.NewInstruction("user:bob", "agent:coder", "reply from hub")
	err = client.Publish(context.Background(), "scion.project.p1.agent.coder.messages", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "reply from hub")

	// 5. Unsubscribe
	err = client.Unsubscribe("scion.project.*.>")
	require.NoError(t, err)

	// 6. Close
	err = client.Close()
	require.NoError(t, err)
}

// TestRPCIntegration_HubDelivery tests the full path through RPC including
// hub API delivery (not just InboundHandler).
func TestRPCIntegration_HubDelivery(t *testing.T) {
	var hubReceived int32
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/broker/inbound" {
			atomic.AddInt32(&hubReceived, 1)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer hubSrv.Close()

	tgSrv := newFakeTelegramServer(t)
	client, _ := startTelegramRPCServer(t, tgSrv)

	// Configure with hub URL (no InboundHandler — will POST to hub)
	err := client.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"hub_url":      hubSrv.URL,
		"plugin_name":  "rpc-hub-test",
	})
	require.NoError(t, err)

	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 1,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      1700000000,
				Text:      "hub delivery test",
			},
		},
	})

	err = client.Subscribe("scion.>")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&hubReceived) > 0
	}, 5*time.Second, 50*time.Millisecond)
}
