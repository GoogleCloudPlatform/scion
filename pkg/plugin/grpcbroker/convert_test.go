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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStructuredMessageRoundTrip(t *testing.T) {
	original := &messages.StructuredMessage{
		Version:      1,
		Timestamp:    "2026-07-04T12:00:00Z",
		Sender:       "user:alice",
		SenderID:     "uid-001",
		Recipient:    "agent:coder",
		RecipientID:  "aid-002",
		Recipients:   "agent:coder,agent:reviewer",
		Msg:          "hello world",
		Type:         "instruction",
		Plain:        true,
		Raw:          false,
		Urgent:       true,
		Broadcasted:  false,
		ObserverOnly: true,
		Status:       "active",
		Attachments:  []string{"file1.txt", "file2.png"},
		Metadata:     map[string]string{"key1": "val1", "key2": "val2"},
		Channel:      "discord",
		ThreadID:     "thread-123",
		Visibility:   "normal",
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
