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
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server wraps any MessageBrokerPluginInterface as a BrokerServiceServer.
// Integration services (5C/5D) pass their broker implementation here to
// expose it over gRPC with near-zero per-integration boilerplate.
type Server struct {
	brokerv1.UnimplementedBrokerServiceServer
	impl plugin.MessageBrokerPluginInterface
}

// NewServer creates a BrokerServiceServer wrapping the given broker implementation.
func NewServer(impl plugin.MessageBrokerPluginInterface) *Server {
	return &Server{impl: impl}
}

func (s *Server) Configure(_ context.Context, req *brokerv1.ConfigureRequest) (*brokerv1.ConfigureResponse, error) {
	cfg := req.GetConfig()
	if cfg == nil {
		cfg = make(map[string]string)
	}
	if err := s.impl.Configure(cfg); err != nil {
		return nil, status.Errorf(codes.Internal, "configure failed: %v", err)
	}
	return &brokerv1.ConfigureResponse{}, nil
}

func (s *Server) Publish(ctx context.Context, req *brokerv1.PublishRequest) (*brokerv1.PublishResponse, error) {
	msg := ProtoToStructuredMessage(req.GetMessage())
	if msg == nil {
		return nil, status.Error(codes.InvalidArgument, "message is required")
	}
	if err := s.impl.Publish(ctx, req.GetTopic(), msg); err != nil {
		return nil, status.Errorf(codes.Internal, "publish failed: %v", err)
	}
	return &brokerv1.PublishResponse{}, nil
}

func (s *Server) Subscribe(_ context.Context, req *brokerv1.SubscribeRequest) (*brokerv1.SubscribeResponse, error) {
	if err := s.impl.Subscribe(req.GetPattern()); err != nil {
		return nil, status.Errorf(codes.Internal, "subscribe failed: %v", err)
	}
	return &brokerv1.SubscribeResponse{}, nil
}

func (s *Server) Unsubscribe(_ context.Context, req *brokerv1.UnsubscribeRequest) (*brokerv1.UnsubscribeResponse, error) {
	if err := s.impl.Unsubscribe(req.GetPattern()); err != nil {
		return nil, status.Errorf(codes.Internal, "unsubscribe failed: %v", err)
	}
	return &brokerv1.UnsubscribeResponse{}, nil
}

func (s *Server) HealthCheck(_ context.Context, _ *brokerv1.HealthCheckRequest) (*brokerv1.HealthCheckResponse, error) {
	hs, err := s.impl.HealthCheck()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "health check failed: %v", err)
	}
	return HealthStatusToProto(hs), nil
}

func (s *Server) GetInfo(_ context.Context, _ *brokerv1.GetInfoRequest) (*brokerv1.GetInfoResponse, error) {
	info, err := s.impl.GetInfo()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get info failed: %v", err)
	}
	return PluginInfoToProto(info), nil
}
