package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

func TestNewBroker(t *testing.T) {
	b := NewBroker(nil)
	require.NotNil(t, b)
	assert.Equal(t, "slack", b.pluginName)
	assert.NotNil(t, b.subs)
	assert.NotNil(t, b.sentIDs)
}

func TestSlackBrokerImplementsInterface(t *testing.T) {
	var _ plugin.MessageBrokerPluginInterface = (*SlackBroker)(nil)
}

func TestSlackBrokerImplementsHostCallbacksAware(t *testing.T) {
	var _ plugin.HostCallbacksAware = (*SlackBroker)(nil)
}

func TestGetInfo(t *testing.T) {
	b := NewBroker(nil)
	info, err := b.GetInfo()
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "slack", info.Name)
	assert.Equal(t, "1.0.0", info.Version)
	assert.Equal(t, "slack", info.ChannelID)
	assert.Contains(t, info.Capabilities, "socket-mode")
	assert.Contains(t, info.Capabilities, "slash-commands")
}

func TestHealthCheckUnconfigured(t *testing.T) {
	b := NewBroker(nil)
	status, err := b.HealthCheck()
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, "degraded", status.Status)
}

func TestHealthCheckClosed(t *testing.T) {
	b := NewBroker(nil)
	b.closed = true
	status, err := b.HealthCheck()
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, "unhealthy", status.Status)
}

func TestParseTopicComponents(t *testing.T) {
	tests := []struct {
		topic     string
		projectID string
		agentSlug string
	}{
		{"scion.project.proj123.agent.myagent", "proj123", "myagent"},
		{"scion.grove.proj123.agent.myagent", "proj123", "myagent"},
		{"some-topic", "some-topic", ""},
	}

	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			pid, slug := parseTopicComponents(tt.topic)
			assert.Equal(t, tt.projectID, pid)
			assert.Equal(t, tt.agentSlug, slug)
		})
	}
}

func TestMsgDedupKey(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender:    "agent:foo",
		Recipient: "user:bar",
		Timestamp: "2024-01-01T00:00:00Z",
		Type:      messages.TypeInstruction,
		Msg:       "hello",
	}

	key1 := msgDedupKey(msg)
	assert.NotEmpty(t, key1)

	key2 := msgDedupKey(msg)
	assert.Equal(t, key1, key2)

	msg2 := &messages.StructuredMessage{
		Sender:    "agent:foo",
		Recipient: "user:bar",
		Timestamp: "2024-01-01T00:00:01Z",
		Type:      messages.TypeInstruction,
		Msg:       "hello",
	}
	key3 := msgDedupKey(msg2)
	assert.NotEqual(t, key1, key3)
}

func TestMsgDedupKeyEmpty(t *testing.T) {
	assert.Empty(t, msgDedupKey(nil))
	assert.Empty(t, msgDedupKey(&messages.StructuredMessage{}))
}

func TestFormatMessage(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender: "agent:myagent",
		Msg:    "Hello world",
		Type:   messages.TypeInstruction,
	}

	text := FormatMessage(msg, "myagent")
	assert.Contains(t, text, "myagent")
	assert.Contains(t, text, "Hello world")
}

func TestFormatWebhookMessage(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender: "agent:myagent",
		Msg:    "Hello world",
		Type:   messages.TypeInstruction,
	}

	text := FormatWebhookMessage(msg)
	assert.Contains(t, text, "Hello world")
	assert.NotContains(t, text, "myagent")
}

func TestFormatStateChange(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender: "agent:myagent",
		Status: "idle",
		Msg:    "Agent finished",
		Type:   messages.TypeStateChange,
	}

	text := FormatStateChange(msg, "myagent")
	assert.Contains(t, text, "IDLE")
	assert.Contains(t, text, "myagent")
	assert.Contains(t, text, "Agent finished")
}

func TestStripBotMention(t *testing.T) {
	assert.Equal(t, "hello", stripBotMention("<@U12345> hello"))
	assert.Equal(t, "hello", stripBotMention("hello"))
	assert.Equal(t, "", stripBotMention("<@U12345>"))
}

func TestSubscribeUnsubscribe(t *testing.T) {
	b := NewBroker(nil)

	err := b.Subscribe("test.pattern")
	assert.NoError(t, err)
	assert.True(t, b.subs["test.pattern"])

	err = b.Subscribe("test.pattern")
	assert.NoError(t, err)

	err = b.Unsubscribe("test.pattern")
	assert.NoError(t, err)
	assert.False(t, b.subs["test.pattern"])
}

func TestSubscribeClosedBroker(t *testing.T) {
	b := NewBroker(nil)
	b.closed = true

	err := b.Subscribe("test.pattern")
	assert.Error(t, err)
}

func TestCloseIdempotent(t *testing.T) {
	b := NewBroker(nil)

	err := b.Close()
	assert.NoError(t, err)

	err = b.Close()
	assert.NoError(t, err)
}

func TestGenerateRequestID(t *testing.T) {
	id1 := generateRequestID()
	id2 := generateRequestID()
	assert.NotEmpty(t, id1)
	assert.NotEmpty(t, id2)
	assert.NotEqual(t, id1, id2)
}

func TestHubErrorUserFacingMessage(t *testing.T) {
	tests := []struct {
		code     string
		expected string
	}{
		{"agent_not_found", "Target agent not found"},
		{"forbidden", "don't have permission"},
		{"broker_auth_failed", "Authentication error"},
		{"unauthorized", "Authentication error"},
		{"unknown", "Failed to deliver"},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			he := &hubError{Code: tt.code}
			assert.Contains(t, he.userFacingMessage(), tt.expected)
		})
	}
}
