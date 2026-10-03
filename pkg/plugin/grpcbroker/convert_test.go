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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestStructuredMessageRoundTrip(t *testing.T) {
	original := &messages.StructuredMessage{
		Version:        1,
		Timestamp:      "2026-07-04T12:00:00Z",
		Sender:         "user:alice",
		SenderID:       "uid-001",
		Recipient:      "agent:coder",
		RecipientID:    "aid-002",
		Recipients:     "agent:coder,agent:reviewer",
		Msg:            "hello world",
		Type:           "instruction",
		Plain:          true,
		Urgent:         true,
		Broadcasted:    false,
		ObserverOnly:   true,
		Status:         "active",
		Attachments:    []string{"file1.txt", "file2.png"},
		Metadata:       map[string]string{"key1": "val1", "key2": "val2"},
		Channel:        "discord",
		ThreadID:       "thread-123",
		ConversationID: "conversation-456",
		DeliveryText:   "rendered delivery text",
	}

	pb := StructuredMessageToProto(original)
	require.NotNil(t, pb)

	roundTripped := ProtoToStructuredMessage(pb)
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
	assert.Equal(t, original.Urgent, roundTripped.Urgent)
	assert.Equal(t, original.Broadcasted, roundTripped.Broadcasted)
	assert.Equal(t, original.ObserverOnly, roundTripped.ObserverOnly)
	assert.Equal(t, original.Status, roundTripped.Status)
	assert.Equal(t, original.Attachments, roundTripped.Attachments)
	assert.Equal(t, original.Metadata, roundTripped.Metadata)
	assert.Equal(t, original.Channel, roundTripped.Channel)
	assert.Equal(t, original.ThreadID, roundTripped.ThreadID)
	assert.Equal(t, original.ConversationID, roundTripped.ConversationID)
	assert.Equal(t, original.DeliveryText, roundTripped.DeliveryText)
}

func TestStructuredMessageNilHandling(t *testing.T) {
	assert.Nil(t, StructuredMessageToProto(nil))
	assert.Nil(t, ProtoToStructuredMessage(nil))
}

func TestStructuredMessageEmptyFields(t *testing.T) {
	original := &messages.StructuredMessage{
		Version: 1,
		Msg:     "minimal",
		Type:    "instruction",
		Sender:  "user:bob",
	}

	pb := StructuredMessageToProto(original)
	roundTripped := ProtoToStructuredMessage(pb)

	assert.Equal(t, original.Version, roundTripped.Version)
	assert.Equal(t, original.Msg, roundTripped.Msg)
	assert.Nil(t, roundTripped.Attachments)
	assert.Nil(t, roundTripped.Metadata)
}

func TestStructuredMessageProtoSchema(t *testing.T) {
	descriptor := (&brokerv1.StructuredMessage{}).ProtoReflect().Descriptor()

	assert.Equal(t, protoreflect.FieldNumber(21), descriptor.Fields().ByName("conversation_id").Number())
	assert.Equal(t, protoreflect.FieldNumber(22), descriptor.Fields().ByName("delivery_text").Number())
	assert.Nil(t, descriptor.Fields().ByName("visibility"))
	assert.True(t, descriptor.ReservedRanges().Has(20))
	assert.True(t, descriptor.ReservedNames().Has("visibility"))
}

func TestHealthStatusRoundTrip(t *testing.T) {
	original := &plugin.HealthStatus{
		Status:  "healthy",
		Message: "all systems operational",
		Details: map[string]string{
			"connections":   "5",
			"last_activity": "2026-07-04T12:00:00Z",
		},
	}

	pb := HealthStatusToProto(original)
	roundTripped := ProtoToHealthStatus(pb)

	assert.Equal(t, original.Status, roundTripped.Status)
	assert.Equal(t, original.Message, roundTripped.Message)
	assert.Equal(t, original.Details, roundTripped.Details)
}

func TestHealthStatusNilHandling(t *testing.T) {
	pb := HealthStatusToProto(nil)
	assert.NotNil(t, pb)
	assert.Equal(t, "", pb.Status)

	assert.Nil(t, ProtoToHealthStatus(nil))
}

func TestPluginInfoRoundTrip(t *testing.T) {
	original := &plugin.PluginInfo{
		Name:            "discord",
		Version:         "1.2.3",
		MinScionVersion: "0.5.0",
		ChannelID:       "discord",
		Capabilities:    []string{"send", "receive", "webhooks"},
	}

	pb := PluginInfoToProto(original)
	roundTripped := ProtoToPluginInfo(pb)

	assert.Equal(t, original.Name, roundTripped.Name)
	assert.Equal(t, original.Version, roundTripped.Version)
	assert.Equal(t, original.MinScionVersion, roundTripped.MinScionVersion)
	assert.Equal(t, original.ChannelID, roundTripped.ChannelID)
	assert.Equal(t, original.Capabilities, roundTripped.Capabilities)
}

func TestPluginInfoNilHandling(t *testing.T) {
	pb := PluginInfoToProto(nil)
	assert.NotNil(t, pb)
	assert.Equal(t, "", pb.Name)

	assert.Nil(t, ProtoToPluginInfo(nil))
}

// TestStructuredMessageRawFieldReserved pins that the retired raw field
// (number 11) stays reserved by number and name so it cannot be reused with
// a different meaning, and that a message from an older peer that still
// sets it decodes as an ordinary message.
func TestStructuredMessageRawFieldReserved(t *testing.T) {
	md := (&brokerv1.StructuredMessage{}).ProtoReflect().Descriptor()
	assert.Nil(t, md.Fields().ByNumber(11), "field 11 must not be redefined")
	assert.Nil(t, md.Fields().ByName("raw"), "field raw must not be redefined")
	assert.True(t, md.ReservedRanges().Has(11), "field number 11 must stay reserved")
	assert.True(t, md.ReservedNames().Has("raw"), "field name raw must stay reserved")

	// Wire bytes for {msg: "hi" (field 8), raw: true (field 11)} as an
	// older peer would have sent them.
	wire := []byte{0x42, 0x02, 'h', 'i', 0x58, 0x01}
	var pb brokerv1.StructuredMessage
	require.NoError(t, proto.Unmarshal(wire, &pb))
	msg := ProtoToStructuredMessage(&pb)
	assert.Equal(t, "hi", msg.Msg)
	assert.False(t, msg.Plain)
}
