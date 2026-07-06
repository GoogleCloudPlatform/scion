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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestBroker(t *testing.T) *RefBroker {
	t.Helper()
	b := New(slog.Default())
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestConfigure(t *testing.T) {
	b := New(slog.Default())
	defer func() { _ = b.Close() }()

	err := b.Configure(map[string]string{
		"hub_url":     "http://localhost:8080",
		"hmac_key":    "secret",
		"plugin_name": "test-broker",
	})
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8080", b.hubURL)
	assert.Equal(t, "secret", b.hmacKey)
	assert.Equal(t, "test-broker", b.pluginName)
}

func TestPublishToSubscriber(t *testing.T) {
	b := newTestBroker(t)

	var received []publishedMessage
	var mu sync.Mutex
	b.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		mu.Lock()
		received = append(received, publishedMessage{topic: topic, msg: msg})
		mu.Unlock()
	}

	require.NoError(t, b.Subscribe("scion.grove.g1.agent.*.messages"))

	msg := messages.NewInstruction("user:alice", "agent:coder", "hello")
	require.NoError(t, b.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", msg))

	// Wait for async delivery
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1
	}, time.Second, 10*time.Millisecond)

	mu.Lock()
	assert.Equal(t, "scion.grove.g1.agent.coder.messages", received[0].topic)
	assert.Equal(t, "hello", received[0].msg.Msg)
	mu.Unlock()
}

func TestPublishNoMatch(t *testing.T) {
	b := newTestBroker(t)

	var received int
	var mu sync.Mutex
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		mu.Lock()
		received++
		mu.Unlock()
	}

	require.NoError(t, b.Subscribe("scion.grove.g1.agent.coder.messages"))

	msg := messages.NewInstruction("user:alice", "agent:other", "nope")
	require.NoError(t, b.Publish(context.Background(), "scion.grove.g2.agent.other.messages", msg))

	// Give some time to ensure no delivery
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	assert.Equal(t, 0, received)
	mu.Unlock()
}

func TestEchoFiltering(t *testing.T) {
	b := newTestBroker(t)

	var received int
	var mu sync.Mutex
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		mu.Lock()
		received++
		mu.Unlock()
	}

	require.NoError(t, b.Subscribe("scion.grove.g1.agent.*.messages"))

	// Publish a message with the origin marker — should be filtered
	echoMsg := messages.NewInstruction(
		OriginMarkerKey+":"+OriginMarkerValue+":hub-process",
		"agent:coder",
		"this is an echo",
	)
	require.NoError(t, b.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", echoMsg))

	// Give time for potential delivery
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	assert.Equal(t, 0, received, "echo message should have been filtered")
	mu.Unlock()

	// Now publish a non-echo message — should be delivered
	normalMsg := messages.NewInstruction("user:alice", "agent:coder", "real message")
	require.NoError(t, b.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", normalMsg))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return received == 1
	}, time.Second, 10*time.Millisecond)
}

func TestUnsubscribe(t *testing.T) {
	b := newTestBroker(t)

	var received int
	var mu sync.Mutex
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		mu.Lock()
		received++
		mu.Unlock()
	}

	require.NoError(t, b.Subscribe("scion.grove.g1.agent.*.messages"))
	require.NoError(t, b.Unsubscribe("scion.grove.g1.agent.*.messages"))

	msg := messages.NewInstruction("user:alice", "agent:coder", "after unsub")
	require.NoError(t, b.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", msg))

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	assert.Equal(t, 0, received, "should not receive after unsubscribe")
	mu.Unlock()
}

func TestUnsubscribeNonexistent(t *testing.T) {
	b := newTestBroker(t)
	// Should not error
	require.NoError(t, b.Unsubscribe("nonexistent.pattern"))
}

func TestDoubleSubscribe(t *testing.T) {
	b := newTestBroker(t)
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {}

	require.NoError(t, b.Subscribe("scion.grove.g1.>"))
	require.NoError(t, b.Subscribe("scion.grove.g1.>")) // idempotent

	b.mu.RLock()
	assert.Len(t, b.subs, 1)
	b.mu.RUnlock()
}

func TestClose(t *testing.T) {
	b := New(slog.Default())
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {}

	require.NoError(t, b.Subscribe("test.>"))
	require.NoError(t, b.Close())

	// Operations after close should fail
	err := b.Publish(context.Background(), "test.topic", messages.NewInstruction("a", "b", "c"))
	assert.Error(t, err)

	err = b.Subscribe("test.new")
	assert.Error(t, err)

	// Double close is safe
	require.NoError(t, b.Close())
}

func TestGetInfo(t *testing.T) {
	b := New(slog.Default())
	defer func() { _ = b.Close() }()

	info, err := b.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "refbroker", info.Name)
	assert.Equal(t, "0.1.0", info.Version)
	assert.Contains(t, info.Capabilities, "echo-filter")
	assert.Contains(t, info.Capabilities, "in-memory")
}

func TestHubAPIDelivery(t *testing.T) {
	var receivedPayloads []inboundPayload
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/broker/inbound", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "test-ref", r.Header.Get("X-Scion-Plugin-Name"))

		body, _ := io.ReadAll(r.Body)
		var p inboundPayload
		require.NoError(t, json.Unmarshal(body, &p))

		mu.Lock()
		receivedPayloads = append(receivedPayloads, p)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := New(slog.Default())
	defer func() { _ = b.Close() }()

	require.NoError(t, b.Configure(map[string]string{
		"hub_url":     srv.URL,
		"plugin_name": "test-ref",
	}))

	require.NoError(t, b.Subscribe("scion.grove.g1.agent.*.messages"))

	msg := messages.NewInstruction("user:alice", "agent:coder", "via hub api")
	require.NoError(t, b.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", msg))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(receivedPayloads) == 1
	}, 2*time.Second, 10*time.Millisecond)

	mu.Lock()
	assert.Equal(t, "scion.grove.g1.agent.coder.messages", receivedPayloads[0].Topic)
	assert.Equal(t, "via hub api", receivedPayloads[0].Message.Msg)
	mu.Unlock()
}

func TestMultipleSubscriptions(t *testing.T) {
	b := newTestBroker(t)

	var received []string
	var mu sync.Mutex
	b.InboundHandler = func(topic string, _ *messages.StructuredMessage) {
		mu.Lock()
		received = append(received, topic)
		mu.Unlock()
	}

	require.NoError(t, b.Subscribe("scion.grove.g1.agent.coder.messages"))
	require.NoError(t, b.Subscribe("scion.grove.g1.broadcast"))

	msg := messages.NewInstruction("user:alice", "agent:coder", "direct")
	require.NoError(t, b.Publish(context.Background(), "scion.grove.g1.agent.coder.messages", msg))

	broadcast := messages.NewInstruction("user:alice", "grove:g1", "broadcast")
	broadcast.Broadcasted = true
	require.NoError(t, b.Publish(context.Background(), "scion.grove.g1.broadcast", broadcast))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 2
	}, time.Second, 10*time.Millisecond)

	mu.Lock()
	assert.Contains(t, received, "scion.grove.g1.agent.coder.messages")
	assert.Contains(t, received, "scion.grove.g1.broadcast")
	mu.Unlock()
}

func TestSubjectMatchesPattern(t *testing.T) {
	tests := []struct {
		pattern string
		subject string
		want    bool
	}{
		{"foo.bar", "foo.bar", true},
		{"foo.bar", "foo.baz", false},
		{"foo.*", "foo.bar", true},
		{"foo.*", "foo.bar.baz", false},
		{"foo.>", "foo.bar", true},
		{"foo.>", "foo.bar.baz", true},
		{"foo.>", "foo", false},
		{"scion.grove.*.agent.*.messages", "scion.grove.g1.agent.coder.messages", true},
		{"scion.grove.*.agent.*.messages", "scion.grove.g1.broadcast", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.subject, func(t *testing.T) {
			assert.Equal(t, tt.want, subjectMatchesPattern(tt.pattern, tt.subject))
		})
	}
}

func TestIsEcho(t *testing.T) {
	assert.True(t, isEcho(messages.NewInstruction(
		OriginMarkerKey+":"+OriginMarkerValue+":hub",
		"agent:coder",
		"echo",
	)))
	assert.False(t, isEcho(messages.NewInstruction(
		"user:alice",
		"agent:coder",
		"not echo",
	)))
	assert.False(t, isEcho(nil))
}

func TestPublishExternal(t *testing.T) {
	b := newTestBroker(t)

	var received int
	var mu sync.Mutex
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		mu.Lock()
		received++
		mu.Unlock()
	}

	require.NoError(t, b.Subscribe("test.>"))

	msg := messages.NewInstruction("external:system", "agent:coder", "from outside")
	require.NoError(t, b.PublishExternal("test.topic", msg))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return received == 1
	}, time.Second, 10*time.Millisecond)
}
