package discord

import (
	"context"
	"log/slog"
	"sync/atomic"

	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

type brokerGRPCServer struct {
	brokerv1.UnimplementedBrokerServiceServer
	broker   *DiscordBroker
	log      *slog.Logger
	isLeader *atomic.Bool
}

// NewBrokerGRPCServer creates a gRPC server that delegates to the given DiscordBroker.
// The isLeader flag gates mutating RPCs so that standby instances reject requests.
func NewBrokerGRPCServer(broker *DiscordBroker, log *slog.Logger, isLeader *atomic.Bool) brokerv1.BrokerServiceServer {
	if log == nil {
		log = slog.Default()
	}
	return &brokerGRPCServer{
		broker:   broker,
		log:      log,
		isLeader: isLeader,
	}
}

func (s *brokerGRPCServer) Configure(_ context.Context, req *brokerv1.ConfigureRequest) (*brokerv1.ConfigureResponse, error) {
	if s.isLeader != nil && !s.isLeader.Load() {
		return nil, status.Error(codes.Unavailable, "this instance is not the leader")
	}
	if err := s.broker.Configure(req.GetConfig()); err != nil {
		return nil, err
	}
	return &brokerv1.ConfigureResponse{}, nil
}

func (s *brokerGRPCServer) Publish(ctx context.Context, req *brokerv1.PublishRequest) (*brokerv1.PublishResponse, error) {
	if s.isLeader != nil && !s.isLeader.Load() {
		return nil, status.Error(codes.Unavailable, "this instance is not the leader")
	}
	msg := protoToStructuredMessage(req.GetMessage())
	if err := s.broker.Publish(ctx, req.GetTopic(), msg); err != nil {
		return nil, err
	}
	return &brokerv1.PublishResponse{}, nil
}

func (s *brokerGRPCServer) Subscribe(_ context.Context, req *brokerv1.SubscribeRequest) (*brokerv1.SubscribeResponse, error) {
	if s.isLeader != nil && !s.isLeader.Load() {
		return nil, status.Error(codes.Unavailable, "this instance is not the leader")
	}
	if err := s.broker.Subscribe(req.GetPattern()); err != nil {
		return nil, err
	}
	return &brokerv1.SubscribeResponse{}, nil
}

func (s *brokerGRPCServer) Unsubscribe(_ context.Context, req *brokerv1.UnsubscribeRequest) (*brokerv1.UnsubscribeResponse, error) {
	if err := s.broker.Unsubscribe(req.GetPattern()); err != nil {
		return nil, err
	}
	return &brokerv1.UnsubscribeResponse{}, nil
}

func (s *brokerGRPCServer) HealthCheck(_ context.Context, _ *brokerv1.HealthCheckRequest) (*brokerv1.HealthCheckResponse, error) {
	hs, err := s.broker.HealthCheck()
	if err != nil {
		return nil, err
	}
	return &brokerv1.HealthCheckResponse{
		Status: healthStatusToProto(hs),
	}, nil
}

func (s *brokerGRPCServer) GetInfo(_ context.Context, _ *brokerv1.GetInfoRequest) (*brokerv1.GetInfoResponse, error) {
	info, err := s.broker.GetInfo()
	if err != nil {
		return nil, err
	}
	return &brokerv1.GetInfoResponse{
		Info: pluginInfoToProto(info),
	}, nil
}

// --- Conversion functions ---

func protoToStructuredMessage(pb *brokerv1.StructuredMessage) *messages.StructuredMessage {
	if pb == nil {
		return nil
	}
	return &messages.StructuredMessage{
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
		Attachments:  pb.Attachments,
		Metadata:     pb.Metadata,
		Channel:      pb.Channel,
		ThreadID:     pb.ThreadId,
		Visibility:   pb.Visibility,
	}
}

func structuredMessageToProto(msg *messages.StructuredMessage) *brokerv1.StructuredMessage {
	if msg == nil {
		return nil
	}
	return &brokerv1.StructuredMessage{
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
		Attachments:  msg.Attachments,
		Metadata:     msg.Metadata,
		Channel:      msg.Channel,
		ThreadId:     msg.ThreadID,
		Visibility:   msg.Visibility,
	}
}

func healthStatusToProto(hs *plugin.HealthStatus) *brokerv1.HealthStatus {
	if hs == nil {
		return nil
	}
	return &brokerv1.HealthStatus{
		Status:  hs.Status,
		Message: hs.Message,
		Details: hs.Details,
	}
}

func pluginInfoToProto(info *plugin.PluginInfo) *brokerv1.PluginInfo {
	if info == nil {
		return nil
	}
	return &brokerv1.PluginInfo{
		Name:            info.Name,
		Version:         info.Version,
		MinScionVersion: info.MinScionVersion,
		ChannelId:       info.ChannelID,
		Capabilities:    info.Capabilities,
	}
}
