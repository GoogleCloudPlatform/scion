package discord

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

func TestProtoToStructuredMessage_Nil(t *testing.T) {
	assert.Nil(t, protoToStructuredMessage(nil))
}

func TestStructuredMessageToProto_Nil(t *testing.T) {
	assert.Nil(t, structuredMessageToProto(nil))
}

func TestStructuredMessageRoundTrip(t *testing.T) {
	original := &messages.StructuredMessage{
		Version:      2,
		Timestamp:    "2026-06-10T12:00:00Z",
		Sender:       "agent:coder",
		SenderID:     "sender-123",
		Recipient:    "user:alice@example.com",
		RecipientID:  "recipient-456",
		Recipients:   "group:team",
		Msg:          "Hello, world!",
		Type:         messages.TypeAssistantReply,
		Plain:        true,
		Raw:          false,
		Urgent:       true,
		Broadcasted:  true,
		ObserverOnly: false,
		Status:       "RUNNING",
		Attachments:  []string{"file1.txt", "file2.png"},
		Metadata: map[string]string{
			"discord_channel_id": "ch-789",
			"project_id":         "proj-abc",
		},
		Channel:    "discord",
		ThreadID:   "thread-001",
		Visibility: "public",
	}

	pb := structuredMessageToProto(original)
	require.NotNil(t, pb)

	roundTripped := protoToStructuredMessage(pb)
	require.NotNil(t, roundTripped)

	assert.Equal(t, original.Version, roundTripped.Version)
	assert.Equal(t, original.Timestamp, roundTripped.Timestamp)
	assert.Equal(t, original.Sender, roundTripped.Sender)
	assert.Equal(t, original.SenderID, roundTripped.SenderID)
	assert.Equal(t, original.Recipient, roundTripped.Recipient)
	assert.Equal(t, original.RecipientID, roundTripped.RecipientID)
	assert.Equal(t, original.Recipients, roundTripped.Recipients)
	assert.Equal(t, original.Msg, roundTripped.Msg)
	assert.Equal(t, original.Type, roundTripped.Type)
	assert.Equal(t, original.Plain, roundTripped.Plain)
	assert.Equal(t, original.Raw, roundTripped.Raw)
	assert.Equal(t, original.Urgent, roundTripped.Urgent)
	assert.Equal(t, original.Broadcasted, roundTripped.Broadcasted)
	assert.Equal(t, original.ObserverOnly, roundTripped.ObserverOnly)
	assert.Equal(t, original.Status, roundTripped.Status)
	assert.Equal(t, original.Attachments, roundTripped.Attachments)
	assert.Equal(t, original.Metadata, roundTripped.Metadata)
	assert.Equal(t, original.Channel, roundTripped.Channel)
	assert.Equal(t, original.ThreadID, roundTripped.ThreadID)
	assert.Equal(t, original.Visibility, roundTripped.Visibility)
}

func TestStructuredMessageRoundTrip_ZeroValue(t *testing.T) {
	original := &messages.StructuredMessage{}
	pb := structuredMessageToProto(original)
	require.NotNil(t, pb)

	roundTripped := protoToStructuredMessage(pb)
	require.NotNil(t, roundTripped)

	assert.Equal(t, 0, roundTripped.Version)
	assert.Empty(t, roundTripped.Msg)
	assert.Nil(t, roundTripped.Attachments)
	assert.Nil(t, roundTripped.Metadata)
}

func TestStructuredMessageToProto_FieldMapping(t *testing.T) {
	msg := &messages.StructuredMessage{
		SenderID:    "sid",
		RecipientID: "rid",
		ThreadID:    "tid",
	}
	pb := structuredMessageToProto(msg)
	assert.Equal(t, "sid", pb.SenderId)
	assert.Equal(t, "rid", pb.RecipientId)
	assert.Equal(t, "tid", pb.ThreadId)
}

func TestHealthStatusToProto_Nil(t *testing.T) {
	assert.Nil(t, healthStatusToProto(nil))
}

func TestHealthStatusToProto(t *testing.T) {
	hs := &plugin.HealthStatus{
		Status:  "healthy",
		Message: "all good",
		Details: map[string]string{
			"subscriptions": "3",
			"bot_id":        "bot-123",
		},
	}
	pb := healthStatusToProto(hs)
	require.NotNil(t, pb)

	assert.Equal(t, "healthy", pb.Status)
	assert.Equal(t, "all good", pb.Message)
	assert.Equal(t, map[string]string{
		"subscriptions": "3",
		"bot_id":        "bot-123",
	}, pb.Details)
}

func TestHealthStatusToProto_EmptyDetails(t *testing.T) {
	hs := &plugin.HealthStatus{
		Status:  "degraded",
		Message: "no details",
	}
	pb := healthStatusToProto(hs)
	require.NotNil(t, pb)
	assert.Nil(t, pb.Details)
}

func TestPluginInfoToProto_Nil(t *testing.T) {
	assert.Nil(t, pluginInfoToProto(nil))
}

func TestPluginInfoToProto(t *testing.T) {
	info := &plugin.PluginInfo{
		Name:            "discord",
		Version:         "1.0.0",
		MinScionVersion: "0.5.0",
		ChannelID:       "discord",
		Capabilities:    []string{"echo-filter", "gateway-websocket"},
	}
	pb := pluginInfoToProto(info)
	require.NotNil(t, pb)

	assert.Equal(t, "discord", pb.Name)
	assert.Equal(t, "1.0.0", pb.Version)
	assert.Equal(t, "0.5.0", pb.MinScionVersion)
	assert.Equal(t, "discord", pb.ChannelId)
	assert.Equal(t, []string{"echo-filter", "gateway-websocket"}, pb.Capabilities)
}

func TestPluginInfoToProto_Empty(t *testing.T) {
	info := &plugin.PluginInfo{}
	pb := pluginInfoToProto(info)
	require.NotNil(t, pb)
	assert.Empty(t, pb.Name)
	assert.Nil(t, pb.Capabilities)
}

func TestNewBrokerGRPCServer(t *testing.T) {
	broker := NewBroker(nil)
	srv := NewBrokerGRPCServer(broker, nil, nil)
	assert.NotNil(t, srv)

	_, ok := srv.(brokerv1.BrokerServiceServer)
	assert.True(t, ok, "must implement BrokerServiceServer")
}
