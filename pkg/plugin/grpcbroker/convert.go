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

// Package grpcbroker provides gRPC transport for the broker plugin system.
// It includes proto ↔ Go type conversions, a generic gRPC server scaffolding
// that wraps any MessageBrokerPluginInterface, and a hub-side adapter that
// implements eventbus.EventBus over gRPC.
package grpcbroker

import (
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
)

// StructuredMessageToProto converts a Go StructuredMessage to its proto representation.
func StructuredMessageToProto(msg *messages.StructuredMessage) *brokerv1.StructuredMessage {
	if msg == nil {
		return nil
	}
	pb := &brokerv1.StructuredMessage{
		Version:      int32(msg.Version),
		Timestamp:    msg.Timestamp,
		Sender:       msg.Sender,
		SenderId:     msg.SenderID,
		Recipient:    msg.Recipient,
		RecipientId:  msg.RecipientID,
		Recipients:   msg.Recipients,
		Msg:          msg.Msg,
		Type:         msg.Type,
		Plain:        msg.Plain,
		Raw:          msg.Raw,
		Urgent:       msg.Urgent,
		Broadcasted:  msg.Broadcasted,
		ObserverOnly: msg.ObserverOnly,
		Status:       msg.Status,
		Channel:      msg.Channel,
		ThreadId:     msg.ThreadID,
		Visibility:   msg.Visibility,
	}
	if len(msg.Attachments) > 0 {
		pb.Attachments = make([]string, len(msg.Attachments))
		copy(pb.Attachments, msg.Attachments)
	}
	if len(msg.Metadata) > 0 {
		pb.Metadata = make(map[string]string, len(msg.Metadata))
		for k, v := range msg.Metadata {
			pb.Metadata[k] = v
		}
	}
	return pb
}

// ProtoToStructuredMessage converts a proto StructuredMessage to the Go type.
func ProtoToStructuredMessage(pb *brokerv1.StructuredMessage) *messages.StructuredMessage {
	if pb == nil {
		return nil
	}
	msg := &messages.StructuredMessage{
		Version:      int(pb.Version),
		Timestamp:    pb.Timestamp,
		Sender:       pb.Sender,
		SenderID:     pb.SenderId,
		Recipient:    pb.Recipient,
		RecipientID:  pb.RecipientId,
		Recipients:   pb.Recipients,
		Msg:          pb.Msg,
		Type:         pb.Type,
		Plain:        pb.Plain,
		Raw:          pb.Raw,
		Urgent:       pb.Urgent,
		Broadcasted:  pb.Broadcasted,
		ObserverOnly: pb.ObserverOnly,
		Status:       pb.Status,
		Channel:      pb.Channel,
		ThreadID:     pb.ThreadId,
		Visibility:   pb.Visibility,
	}
	if len(pb.Attachments) > 0 {
		msg.Attachments = make([]string, len(pb.Attachments))
		copy(msg.Attachments, pb.Attachments)
	}
	if len(pb.Metadata) > 0 {
		msg.Metadata = make(map[string]string, len(pb.Metadata))
		for k, v := range pb.Metadata {
			msg.Metadata[k] = v
		}
	}
	return msg
}

// HealthStatusToProto converts a Go HealthStatus to a proto HealthCheckResponse.
func HealthStatusToProto(hs *plugin.HealthStatus) *brokerv1.HealthCheckResponse {
	if hs == nil {
		return &brokerv1.HealthCheckResponse{}
	}
	resp := &brokerv1.HealthCheckResponse{
		Status:  hs.Status,
		Message: hs.Message,
	}
	if len(hs.Details) > 0 {
		resp.Details = make(map[string]string, len(hs.Details))
		for k, v := range hs.Details {
			resp.Details[k] = v
		}
	}
	return resp
}

// ProtoToHealthStatus converts a proto HealthCheckResponse to a Go HealthStatus.
func ProtoToHealthStatus(pb *brokerv1.HealthCheckResponse) *plugin.HealthStatus {
	if pb == nil {
		return nil
	}
	hs := &plugin.HealthStatus{
		Status:  pb.Status,
		Message: pb.Message,
	}
	if len(pb.Details) > 0 {
		hs.Details = make(map[string]string, len(pb.Details))
		for k, v := range pb.Details {
			hs.Details[k] = v
		}
	}
	return hs
}

// PluginInfoToProto converts a Go PluginInfo to a proto GetInfoResponse.
func PluginInfoToProto(info *plugin.PluginInfo) *brokerv1.GetInfoResponse {
	if info == nil {
		return &brokerv1.GetInfoResponse{}
	}
	resp := &brokerv1.GetInfoResponse{
		Name:             info.Name,
		Version:          info.Version,
		MinScionVersion:  info.MinScionVersion,
		ChannelId:        info.ChannelID,
	}
	if len(info.Capabilities) > 0 {
		resp.Capabilities = make([]string, len(info.Capabilities))
		copy(resp.Capabilities, info.Capabilities)
	}
	return resp
}

// ProtoToPluginInfo converts a proto GetInfoResponse to a Go PluginInfo.
func ProtoToPluginInfo(pb *brokerv1.GetInfoResponse) *plugin.PluginInfo {
	if pb == nil {
		return nil
	}
	info := &plugin.PluginInfo{
		Name:            pb.Name,
		Version:         pb.Version,
		MinScionVersion: pb.MinScionVersion,
		ChannelID:       pb.ChannelId,
	}
	if len(pb.Capabilities) > 0 {
		info.Capabilities = make([]string, len(pb.Capabilities))
		copy(info.Capabilities, pb.Capabilities)
	}
	return info
}
